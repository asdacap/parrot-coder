// Package subagent manages bounded, reusable child-agent sessions.
package subagent

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	managedtask "github.com/amirulashraf/parrot-coder/internal/task"
	petname "github.com/dustinkirkland/golang-petname"
)

var (
	ErrInvalid      = errors.New("subagent: invalid request")
	ErrConcurrency  = errors.New("subagent: concurrency limit reached")
	ErrDepth        = errors.New("subagent: maximum depth reached")
	ErrRecursion    = errors.New("subagent: agent recursion limit reached")
	ErrNotFound     = errors.New("subagent: task not found")
	ErrCanceled     = errors.New("subagent: task canceled")
	ErrTaskLimit    = errors.New("subagent: retained task limit reached")
	ErrRequestLimit = errors.New("subagent: request limit reached")
	ErrRunning      = errors.New("subagent: agent is already running")
	ErrNotRunning   = errors.New("subagent: agent is not running")
	ErrClosed       = errors.New("subagent: manager is closed")
)

// Request intentionally contains no parent permission grants. Every executor
// invocation must establish its own authorization context.
type Request struct {
	Prompt     string
	Agent      string
	Model      string
	Name       string
	ToolCallID string
}

// Usage is cumulative provider-reported token accounting for a task.
type Usage struct {
	InputTokens       int `json:"input_tokens"`
	OutputTokens      int `json:"output_tokens"`
	TotalTokens       int `json:"total_tokens"`
	ReasoningTokens   int `json:"reasoning_tokens"`
	CachedInputTokens int `json:"cached_input_tokens"`
}

// Progress is a delta reported by an executor while a task is running.
type Progress struct {
	Usage    Usage
	ToolUses int
}

// Preparation describes a child session before it has a durable identity.
type Preparation struct {
	ParentSession string
	RootSession   string
	Lineage       []string
	Turn          int
	Request       Request
}

// Execution is the complete executor contract. Lineage contains ancestor agent
// names, from the root toward the immediate parent.
type Execution struct {
	SessionID      string
	ParentSession  string
	RootSession    string
	Lineage        []string
	Turn           int
	Request        Request
	ReportProgress func(Progress)
}

type Executor interface {
	Execute(context.Context, Execution) (string, error)
}

// Preparer synchronously creates or obtains the child session used by Spawn.
// Execute is not called, and lifecycle events are not emitted, until Prepare
// returns a nonempty session ID.
type Preparer interface {
	Prepare(context.Context, Preparation) (string, error)
}

// PreparationDiscarder removes a child session which was prepared but could
// not be admitted as a managed task.
type PreparationDiscarder interface {
	DiscardPreparation(context.Context, string) error
}

// MessageSender admits a steer message without starting a new turn.
type MessageSender interface {
	Send(context.Context, Execution, string) (string, error)
}

type SessionHierarchy interface {
	HasChildSessions(parentSessionID string) bool
	ForgetChild(sessionID string) error
}

type Config struct {
	MaxConcurrent          int
	MaxConcurrentPerParent int
	MaxDepth               int
	MaxTasks               int
	MaxPromptBytes         int
	MaxResultBytes         int
	AgentIdentity          func(string) string
	AgentRecursionLimit    func(string) int
	NameGenerator          func() string
	OnProgress             func(Task)
	OnComplete             func(Task)
	OnEvent                func(LifecycleEvent)
	Tasks                  *managedtask.Manager
	Sessions               SessionHierarchy
}

// Lifecycle kinds a task reports as flat events on its parent session's
// stream. Working marks the start of a turn, Finished its terminal state, and
// Idle a retained agent waiting for follow-up work.
const (
	LifecycleStart    = "start"
	LifecycleWorking  = "working"
	LifecycleIdle     = "idle"
	LifecycleFinished = "finished"
)

// LifecycleEvent is one flat task lifecycle emission. Task is a snapshot taken
// when the event fired.
type LifecycleEvent struct {
	Kind string
	Task Task
}

type Status string

const (
	StatusPending   Status = "pending"
	StatusRunning   Status = "running"
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
	StatusCanceled  Status = "canceled"
)

type Task struct {
	SessionID     string
	ParentSession string
	RootSession   string
	Agent         string
	Model         string
	Name          string
	Lineage       []string
	Depth         int
	Turn          int
	Status        Status
	StartedAt     time.Time
	FinishedAt    time.Time
	Output        string
	Error         string
	Truncated     bool
	ToolCallID    string
	Usage         Usage
	ToolUses      int
}

type taskState struct {
	task    Task
	request Request
	turn    *turnState
	cancel  context.CancelFunc
	op      sync.Mutex
}

type turnState struct {
	done   chan struct{}
	result Task
}

// Observer waits for the turn which was current when it was created. It stays
// bound to that turn if the retained agent subsequently starts a follow-up.
type Observer struct {
	turn *turnState
}

type Manager struct {
	mu       sync.Mutex
	executor Executor
	config   Config
	tasks    map[string]*taskState
	running  int
	byParent map[string]int
	closed   bool
	workers  sync.WaitGroup
}

func NewManager(executor Executor, config Config) *Manager {
	if config.MaxDepth <= 0 {
		config.MaxDepth = 4
	}
	if config.MaxTasks <= 0 {
		config.MaxTasks = 1024
	}
	if config.MaxPromptBytes <= 0 {
		config.MaxPromptBytes = 1 << 20
	}
	if config.MaxResultBytes <= 0 {
		config.MaxResultBytes = 1 << 20
	}
	return &Manager{executor: executor, config: config, tasks: make(map[string]*taskState), byParent: make(map[string]int)}
}

func (m *Manager) validate(parentSession string, lineage []string, request Request) error {
	if m == nil || m.executor == nil || strings.TrimSpace(parentSession) == "" || strings.TrimSpace(request.Prompt) == "" || !validAgent(request.Agent) {
		return ErrInvalid
	}
	if len(request.Prompt) > m.config.MaxPromptBytes {
		return ErrRequestLimit
	}
	if len(lineage)+1 > m.config.MaxDepth {
		return ErrDepth
	}
	targetIdentity := m.agentIdentity(request.Agent)
	recursions := 1
	for _, ancestor := range lineage {
		if !validAgent(ancestor) {
			return ErrInvalid
		}
		if m.agentIdentity(ancestor) == targetIdentity {
			recursions++
		}
	}
	if recursions > m.agentRecursionLimit(targetIdentity) {
		return ErrRecursion
	}
	return nil
}

func (m *Manager) launch(sessionID, parentSession string, lineage []string, request Request) (string, error) {
	if strings.TrimSpace(sessionID) == "" {
		return "", ErrInvalid
	}
	lineage = append([]string(nil), lineage...)
	now := time.Now().UTC()
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return "", ErrClosed
	}
	if _, exists := m.tasks[sessionID]; exists {
		m.mu.Unlock()
		return "", managedtask.ErrDuplicate
	}
	if len(m.tasks) >= m.config.MaxTasks {
		m.mu.Unlock()
		return "", ErrTaskLimit
	}
	if m.running >= m.config.MaxConcurrent || m.byParent[parentSession] >= m.config.MaxConcurrentPerParent {
		m.mu.Unlock()
		return "", ErrConcurrency
	}
	name := managedtask.SanitizeName(request.Name)
	if name == "" {
		generated := petname.Generate(2, "-")
		if m.config.NameGenerator != nil {
			generated = m.config.NameGenerator()
		}
		name = managedtask.SanitizeName(request.Agent + "-" + generated)
	}
	name = m.uniqueNameLocked(name)
	rootSession := parentSession
	if parent := m.taskForSessionLocked(parentSession); parent != nil {
		rootSession = parent.task.RootSession
	}
	ctx, cancel := context.WithCancel(context.Background())
	request.Name = name
	state := &taskState{task: Task{SessionID: sessionID, ParentSession: parentSession, RootSession: rootSession, Agent: request.Agent, Model: request.Model, Name: name, Lineage: lineage, ToolCallID: request.ToolCallID, Depth: len(lineage) + 1, Turn: 1, Status: StatusRunning, StartedAt: now}, request: request, turn: &turnState{done: make(chan struct{})}, cancel: cancel}
	m.tasks[sessionID] = state
	m.running++
	m.byParent[parentSession]++
	m.workers.Add(1)
	start := LifecycleEvent{Kind: LifecycleStart, Task: cloneTask(state.task)}
	m.mu.Unlock()
	if m.config.Tasks != nil {
		item := &managedAgentTask{manager: m, state: state}
		if err := m.config.Tasks.Register(item, func(caller string) bool { return m.isVisible(caller, state) }); err != nil {
			cancel()
			m.mu.Lock()
			delete(m.tasks, sessionID)
			m.running--
			m.byParent[parentSession]--
			m.workers.Done()
			m.mu.Unlock()
			return "", err
		}
	}
	m.emit(start)
	go m.run(ctx, state)
	return sessionID, nil
}

func (m *Manager) emit(event LifecycleEvent) {
	if m.config.OnEvent != nil {
		m.config.OnEvent(event)
	}
}

// ResolveTask returns a caller-visible agent task by session ID or friendly
// name. Session IDs take precedence, matching the shared task resolver.
func (m *Manager) ResolveTask(callerSession, identifier string) (Task, error) {
	if m == nil || callerSession == "" || identifier == "" {
		return Task{}, ErrNotFound
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if state := m.tasks[identifier]; state != nil && m.visibleLocked(callerSession, state) {
		return cloneTask(state.task), nil
	}
	for _, state := range m.tasks {
		if state.task.Name == identifier && m.visibleLocked(callerSession, state) {
			return cloneTask(state.task), nil
		}
	}
	return Task{}, ErrNotFound
}

func (m *Manager) agentIdentity(id string) string {
	if m.config.AgentIdentity == nil {
		return id
	}
	if identity := m.config.AgentIdentity(id); identity != "" {
		return identity
	}
	return id
}

func (m *Manager) agentRecursionLimit(id string) int {
	if m.config.AgentRecursionLimit != nil {
		if limit := m.config.AgentRecursionLimit(id); limit > 0 {
			return limit
		}
	}
	return 3
}

// Spawn derives durable ancestry from the caller session, synchronously
// prepares the child session, and only then registers and launches the task.
func (m *Manager) Spawn(ctx context.Context, parentSession, callerAgent string, request Request) (string, error) {
	if m == nil || m.executor == nil {
		return "", ErrInvalid
	}
	preparer, ok := m.executor.(Preparer)
	if !ok {
		return "", errors.New("subagent: executor does not support session preparation")
	}
	lineage := []string{callerAgent}
	rootSession := parentSession
	m.mu.Lock()
	if parent := m.taskForSessionLocked(parentSession); parent != nil {
		lineage = append(append([]string(nil), parent.task.Lineage...), parent.task.Agent)
		rootSession = parent.task.RootSession
	}
	m.mu.Unlock()
	if err := m.validate(parentSession, lineage, request); err != nil {
		return "", err
	}
	prepared := Preparation{ParentSession: parentSession, RootSession: rootSession, Lineage: append([]string(nil), lineage...), Turn: 1, Request: request}
	sessionID, err := preparer.Prepare(ctx, prepared)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(sessionID) == "" {
		return "", errors.New("subagent: preparer returned an empty child session")
	}
	id, err := m.launch(sessionID, parentSession, lineage, request)
	if err == nil {
		return id, nil
	}
	if discarder, ok := m.executor.(PreparationDiscarder); ok {
		if discardErr := discarder.DiscardPreparation(context.WithoutCancel(ctx), sessionID); discardErr != nil {
			return "", errors.Join(err, fmt.Errorf("subagent: discard prepared session %s: %w", sessionID, discardErr))
		}
	}
	return "", err
}

func (m *Manager) run(ctx context.Context, state *taskState) {
	turn := state.task.Turn
	report := func(progress Progress) { m.reportProgress(state, turn, progress) }
	m.emit(LifecycleEvent{Kind: LifecycleWorking, Task: cloneTask(state.task)})
	execution := Execution{SessionID: state.task.SessionID, ParentSession: state.task.ParentSession, RootSession: state.task.RootSession, Lineage: append([]string(nil), state.task.Lineage...), Turn: turn, Request: state.request, ReportProgress: report}
	output, executeErr := m.executor.Execute(ctx, execution)
	state.op.Lock()
	status := StatusSucceeded
	errText := ""
	if executeErr != nil {
		status, errText = StatusFailed, executeErr.Error()
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		status, errText = StatusCanceled, ErrCanceled.Error()
	}
	truncated := false
	if len(output) > m.config.MaxResultBytes {
		output = output[:m.config.MaxResultBytes]
		truncated = true
	}
	if len(errText) > m.config.MaxResultBytes {
		errText = errText[:m.config.MaxResultBytes]
		truncated = true
	}
	m.mu.Lock()
	state.task.Status = status
	state.task.Output = output
	state.task.Error = errText
	state.task.Truncated = truncated
	state.task.FinishedAt = time.Now().UTC()
	m.running--
	m.byParent[state.task.ParentSession]--
	if m.byParent[state.task.ParentSession] == 0 {
		delete(m.byParent, state.task.ParentSession)
	}
	state.cancel()
	snapshot := cloneTask(state.task)
	state.turn.result = snapshot
	callback := m.config.OnProgress
	complete := m.config.OnComplete
	finished := LifecycleEvent{Kind: LifecycleFinished, Task: snapshot}
	done := state.turn.done
	m.mu.Unlock()
	close(done)
	m.workers.Done()
	m.emit(finished)
	if callback != nil {
		callback(snapshot)
	}
	if complete != nil {
		complete(snapshot)
	}
	state.op.Unlock()
}

func (m *Manager) reportProgress(state *taskState, turn int, progress Progress) {
	if progress.ToolUses < 0 || progress.Usage.InputTokens < 0 || progress.Usage.OutputTokens < 0 || progress.Usage.TotalTokens < 0 || progress.Usage.ReasoningTokens < 0 || progress.Usage.CachedInputTokens < 0 {
		return
	}
	m.mu.Lock()
	if state.task.Status != StatusRunning || state.task.Turn != turn {
		m.mu.Unlock()
		return
	}
	state.task.Usage.InputTokens += progress.Usage.InputTokens
	state.task.Usage.OutputTokens += progress.Usage.OutputTokens
	state.task.Usage.TotalTokens += progress.Usage.TotalTokens
	state.task.Usage.ReasoningTokens += progress.Usage.ReasoningTokens
	state.task.Usage.CachedInputTokens += progress.Usage.CachedInputTokens
	state.task.ToolUses += progress.ToolUses
	snapshot := cloneTask(state.task)
	callback := m.config.OnProgress
	m.mu.Unlock()
	if callback != nil {
		callback(snapshot)
	}
}

// Await observes a task. Cancellation of ctx only stops waiting and does not
// cancel the underlying execution.
func (m *Manager) Await(ctx context.Context, parentSession, id string) (Task, error) {
	observer, err := m.Observe(parentSession, id)
	if err != nil {
		return Task{}, err
	}
	task, err := observer.Wait(ctx)
	if err != nil {
		return Task{}, err
	}
	return task, taskError(task)
}

// Observe captures the caller-visible task's current turn for stable waiting.
func (m *Manager) Observe(callerSession, id string) (*Observer, error) {
	if m == nil || callerSession == "" || id == "" {
		return nil, ErrNotFound
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	state := m.tasks[id]
	if state == nil || !m.visibleLocked(callerSession, state) {
		return nil, ErrNotFound
	}
	return &Observer{turn: state.turn}, nil
}

// Wait waits for the captured turn without affecting the underlying agent.
func (o *Observer) Wait(ctx context.Context) (Task, error) {
	if o == nil || o.turn == nil {
		return Task{}, ErrNotFound
	}
	select {
	case <-o.turn.done:
		return cloneTask(o.turn.result), nil
	case <-ctx.Done():
		return Task{}, ctx.Err()
	}
}

// Send delivers a steer message to an active agent without ending its turn.
func (m *Manager) Send(ctx context.Context, callerSession, id, message string) (string, error) {
	if strings.TrimSpace(message) == "" {
		return "", ErrInvalid
	}
	if len(message) > m.config.MaxPromptBytes {
		return "", ErrRequestLimit
	}
	sender, ok := m.executor.(MessageSender)
	if !ok {
		return "", errors.New("subagent: executor does not support messages")
	}
	state, err := m.lookup(callerSession, id)
	if err != nil {
		return "", err
	}
	state.op.Lock()
	defer state.op.Unlock()
	m.mu.Lock()
	if state.task.Status != StatusRunning {
		m.mu.Unlock()
		return "", ErrNotRunning
	}
	execution := m.executionLocked(state)
	m.mu.Unlock()
	messageID, err := sender.Send(ctx, execution, message)
	if err != nil {
		return "", err
	}
	return messageID, nil
}

// FollowUp starts another turn in the same child session.
func (m *Manager) FollowUp(callerSession, id string, request Request) (Task, error) {
	if strings.TrimSpace(request.Prompt) == "" {
		return Task{}, ErrInvalid
	}
	if len(request.Prompt) > m.config.MaxPromptBytes {
		return Task{}, ErrRequestLimit
	}
	state, err := m.lookup(callerSession, id)
	if err != nil {
		return Task{}, err
	}
	state.op.Lock()
	defer state.op.Unlock()
	m.mu.Lock()
	if m.tasks[id] != state {
		m.mu.Unlock()
		return Task{}, ErrNotFound
	}
	if m.closed {
		m.mu.Unlock()
		return Task{}, ErrClosed
	}
	if state.task.Status == StatusRunning || state.task.Status == StatusPending {
		m.mu.Unlock()
		return Task{}, ErrRunning
	}
	if m.running >= m.config.MaxConcurrent || m.byParent[state.task.ParentSession] >= m.config.MaxConcurrentPerParent {
		m.mu.Unlock()
		return Task{}, ErrConcurrency
	}
	ctx, cancel := context.WithCancel(context.Background())
	state.task.Turn++
	state.task.Status = StatusRunning
	state.task.StartedAt = time.Now().UTC()
	state.task.FinishedAt = time.Time{}
	state.task.Output, state.task.Error = "", ""
	state.task.Truncated = false
	state.task.ToolCallID = request.ToolCallID
	state.task.Usage = Usage{}
	state.task.ToolUses = 0
	request.Agent, request.Model, request.Name = state.task.Agent, state.task.Model, state.task.Name
	state.request = request
	state.turn = &turnState{done: make(chan struct{})}
	state.cancel = cancel
	m.running++
	m.byParent[state.task.ParentSession]++
	m.workers.Add(1)
	task := cloneTask(state.task)
	m.mu.Unlock()
	go m.run(ctx, state)
	return task, nil
}

// Interrupt stops the current turn but retains the agent for follow-up work.
func (m *Manager) Interrupt(ctx context.Context, callerSession, id string) (Task, error) {
	state, err := m.lookup(callerSession, id)
	if err != nil {
		return Task{}, err
	}
	m.mu.Lock()
	if state.task.Status != StatusRunning && state.task.Status != StatusPending {
		task := cloneTask(state.task)
		m.mu.Unlock()
		return task, nil
	}
	state.cancel()
	turn := state.turn
	m.mu.Unlock()
	select {
	case <-turn.done:
		return cloneTask(turn.result), nil
	case <-ctx.Done():
		return Task{}, ctx.Err()
	}
}

// Cancel requests cancellation and waits for the executor to return. If ctx is
// canceled first, the task remains canceled but the wait returns ctx.Err().
func (m *Manager) Cancel(ctx context.Context, parentSession, id string) error {
	_, err := m.Interrupt(ctx, parentSession, id)
	return err
}

func (m *Manager) Status(parentSession, id string) (Task, error) {
	if m == nil {
		return Task{}, ErrNotFound
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	state := m.tasks[id]
	if state == nil || !m.visibleLocked(parentSession, state) {
		return Task{}, ErrNotFound
	}
	return cloneTask(state.task), nil
}

// Resolve returns a caller-visible task by canonical child session ID or
// friendly name. An exact session ID takes precedence over a name match.
func (m *Manager) Resolve(callerSession, identifier string) (Task, error) {
	if m == nil || callerSession == "" || identifier == "" {
		return Task{}, ErrNotFound
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if state := m.tasks[identifier]; state != nil && m.visibleLocked(callerSession, state) {
		return cloneTask(state.task), nil
	}
	for _, state := range m.tasks {
		if state.task.Name == identifier && m.visibleLocked(callerSession, state) {
			return cloneTask(state.task), nil
		}
	}
	return Task{}, ErrNotFound
}

// List is stable by agent ID and returns the caller-visible subtree.
func (m *Manager) List(callerSession string) []Task {
	return m.list(callerSession, false)
}

// ListActive is stable by agent ID and returns active tasks in the
// caller-visible subtree.
func (m *Manager) ListActive(callerSession string) []Task {
	return m.list(callerSession, true)
}

func (m *Manager) list(callerSession string, activeOnly bool) []Task {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	out := make([]Task, 0)
	for _, state := range m.tasks {
		active := state.task.Status == StatusRunning || state.task.Status == StatusPending
		if m.visibleLocked(callerSession, state) && (!activeOnly || active) {
			out = append(out, cloneTask(state.task))
		}
	}
	m.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].SessionID < out[j].SessionID })
	return out
}

// Forget removes a completed task so long-lived managers can explicitly
// reclaim retained-task capacity.
func (m *Manager) Forget(parentSession, id string) error {
	if m == nil {
		return ErrNotFound
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	state := m.tasks[id]
	if state == nil || !m.visibleLocked(parentSession, state) {
		return ErrNotFound
	}
	if state.task.Status == StatusRunning || state.task.Status == StatusPending {
		return errors.New("subagent: cannot forget a running task")
	}
	if m.hasChildrenLocked(state.task.SessionID) {
		return errors.New("subagent: cannot forget an agent with retained children")
	}
	if m.config.Sessions != nil {
		if err := m.config.Sessions.ForgetChild(state.task.SessionID); err != nil {
			return err
		}
	}
	delete(m.tasks, id)
	if m.config.Tasks != nil {
		m.config.Tasks.Unregister(id)
	}
	return nil
}

func (m *Manager) lookup(parentSession, id string) (*taskState, error) {
	if m == nil || parentSession == "" || id == "" {
		return nil, ErrNotFound
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	state := m.tasks[id]
	if state == nil || !m.visibleLocked(parentSession, state) {
		return nil, ErrNotFound
	}
	return state, nil
}

// Shutdown rejects new turns, interrupts active turns, and waits for every
// executor invocation to return.
func (m *Manager) Shutdown(ctx context.Context) error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	if !m.closed {
		m.closed = true
		for _, state := range m.tasks {
			if state.cancel != nil && (state.task.Status == StatusRunning || state.task.Status == StatusPending) {
				state.cancel()
			}
		}
	}
	m.mu.Unlock()
	done := make(chan struct{})
	go func() {
		m.workers.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Manager) executionLocked(state *taskState) Execution {
	turn := state.task.Turn
	return Execution{SessionID: state.task.SessionID, ParentSession: state.task.ParentSession, RootSession: state.task.RootSession, Lineage: append([]string(nil), state.task.Lineage...), Turn: turn, Request: state.request, ReportProgress: func(progress Progress) { m.reportProgress(state, turn, progress) }}
}

func (m *Manager) isVisible(callerSession string, target *taskState) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.visibleLocked(callerSession, target)
}

func (m *Manager) visibleLocked(callerSession string, target *taskState) bool {
	caller := m.taskForSessionLocked(callerSession)
	if caller == nil {
		return target.task.RootSession == callerSession
	}
	for current := target; current != nil; current = m.taskForSessionLocked(current.task.ParentSession) {
		if current.task.ParentSession == caller.task.SessionID {
			return true
		}
	}
	return false
}

func (m *Manager) taskForSessionLocked(sessionID string) *taskState {
	return m.tasks[sessionID]
}

func (m *Manager) hasChildrenLocked(sessionID string) bool {
	if m.config.Sessions != nil {
		return m.config.Sessions.HasChildSessions(sessionID)
	}
	for _, state := range m.tasks {
		if state.task.ParentSession == sessionID {
			return true
		}
	}
	return false
}

type managedAgentTask struct {
	manager *Manager
	state   *taskState
}

type managedAgentTurn struct {
	manager *Manager
	state   *taskState
	turn    *turnState
}

func (t *managedAgentTask) Snapshot() managedtask.Snapshot {
	t.manager.mu.Lock()
	item := cloneTask(t.state.task)
	t.manager.mu.Unlock()
	return agentSnapshot(item)
}

func (t *managedAgentTask) Wait(ctx context.Context) (managedtask.Completion, error) {
	return t.Observe().Wait(ctx)
}

func (t *managedAgentTask) Observe() managedtask.Task {
	t.manager.mu.Lock()
	turn := t.state.turn
	t.manager.mu.Unlock()
	return &managedAgentTurn{manager: t.manager, state: t.state, turn: turn}
}

func (t *managedAgentTurn) Snapshot() managedtask.Snapshot {
	t.manager.mu.Lock()
	item := cloneTask(t.state.task)
	if result := t.turn.result; result.SessionID != "" {
		item = cloneTask(result)
	}
	t.manager.mu.Unlock()
	return agentSnapshot(item)
}

func (t *managedAgentTurn) Wait(ctx context.Context) (managedtask.Completion, error) {
	select {
	case <-t.turn.done:
		result := cloneTask(t.turn.result)
		return managedtask.Completion{Task: agentSnapshot(result), Output: result.Output, Error: result.Error}, nil
	case <-ctx.Done():
		return managedtask.Completion{}, ctx.Err()
	}
}

func (t *managedAgentTurn) Interrupt(ctx context.Context) (managedtask.Snapshot, error) {
	t.manager.mu.Lock()
	current := t.state.turn == t.turn
	t.manager.mu.Unlock()
	if !current {
		return t.Snapshot(), nil
	}
	item, err := t.manager.Interrupt(ctx, t.state.task.RootSession, t.state.task.SessionID)
	return agentSnapshot(item), err
}

func (t *managedAgentTask) Interrupt(ctx context.Context) (managedtask.Snapshot, error) {
	item, err := t.manager.Interrupt(ctx, t.state.task.RootSession, t.state.task.SessionID)
	return agentSnapshot(item), err
}

func agentSnapshot(item Task) managedtask.Snapshot {
	return managedtask.Snapshot{ID: item.SessionID, Name: item.Name, SessionID: item.SessionID, Kind: managedtask.KindAgent, Status: string(item.Status), StartedAt: item.StartedAt, Agent: item.Agent, Turn: item.Turn, Depth: item.Depth}
}

func cloneTask(task Task) Task {
	task.Lineage = append([]string(nil), task.Lineage...)
	return task
}

func taskError(task Task) error {
	switch task.Status {
	case StatusCanceled:
		return ErrCanceled
	case StatusFailed:
		return errors.New(task.Error)
	default:
		return nil
	}
}

func (m *Manager) uniqueNameLocked(name string) string {
	return managedtask.UniqueName(name, func(candidate string) bool {
		for _, state := range m.tasks {
			if state.task.Name == candidate {
				return false
			}
		}
		return true
	})
}

func validAgent(agent string) bool {
	if agent == "" || len(agent) > 128 {
		return false
	}
	for _, char := range agent {
		if char > 127 || !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '-' || char == '_') {
			return false
		}
	}
	return true
}
