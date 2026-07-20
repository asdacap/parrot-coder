// Package subagent manages bounded, reusable child-agent sessions.
package subagent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	ErrInvalid      = errors.New("subagent: invalid request")
	ErrConcurrency  = errors.New("subagent: concurrency limit reached")
	ErrDepth        = errors.New("subagent: maximum depth reached")
	ErrRecursion    = errors.New("subagent: agent recursion limit reached")
	ErrNotFound     = errors.New("subagent: task not found")
	ErrCanceled     = errors.New("subagent: task canceled")
	ErrTimeout      = errors.New("subagent: task timed out")
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

// Execution is the complete executor contract. Lineage contains ancestor agent
// names, from the root toward the immediate parent.
type Execution struct {
	TaskID          string
	SessionID       string
	ParentSession   string
	RootSession     string
	ParentAgentID   string
	Lineage         []string
	Turn            int
	Request         Request
	ReportProgress  func(Progress)
	RegisterSession func(string)
}

type Executor interface {
	Execute(context.Context, Execution) (string, error)
}

// MessageSender admits a steer message without starting a new turn.
type MessageSender interface {
	Send(context.Context, Execution, string) (string, error)
}

type Config struct {
	MaxConcurrent          int
	MaxConcurrentPerParent int
	MaxDepth               int
	MaxTasks               int
	MaxPromptBytes         int
	MaxResultBytes         int
	Timeout                time.Duration
	AgentIdentity          func(string) string
	AgentRecursionLimit    func(string) int
	OnProgress             func(Task)
	OnEvent                func(LifecycleEvent)
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
	StatusTimedOut  Status = "timed_out"
)

type Task struct {
	ID            string
	SessionID     string
	ParentSession string
	RootSession   string
	ParentAgentID string
	Agent         string
	Model         string
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
	task           Task
	request        Request
	turn           *turnState
	registered     chan struct{}
	registeredDone bool
	discard        bool
	cancel         context.CancelFunc
	op             sync.Mutex
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
	mu        sync.Mutex
	executor  Executor
	config    Config
	tasks     map[string]*taskState
	bySession map[string]*taskState
	running   int
	byParent  map[string]int
	closed    bool
	workers   sync.WaitGroup
}

func NewManager(executor Executor, config Config) *Manager {
	if config.MaxConcurrent <= 0 {
		config.MaxConcurrent = 8
	}
	if config.MaxConcurrentPerParent <= 0 {
		config.MaxConcurrentPerParent = 4
	}
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
	if config.Timeout <= 0 {
		config.Timeout = 10 * time.Minute
	}
	return &Manager{executor: executor, config: config, tasks: make(map[string]*taskState), bySession: make(map[string]*taskState), byParent: make(map[string]int)}
}

// Launch reserves concurrency immediately and starts the execution
// asynchronously. Lineage is validated and copied before this method returns.
func (m *Manager) Launch(parentSession string, lineage []string, request Request) (string, error) {
	if m == nil || m.executor == nil || strings.TrimSpace(parentSession) == "" || strings.TrimSpace(request.Prompt) == "" || !validAgent(request.Agent) {
		return "", ErrInvalid
	}
	if len(request.Prompt) > m.config.MaxPromptBytes {
		return "", ErrRequestLimit
	}
	if len(lineage)+1 > m.config.MaxDepth {
		return "", ErrDepth
	}
	targetIdentity := m.agentIdentity(request.Agent)
	recursions := 1
	for _, ancestor := range lineage {
		if !validAgent(ancestor) {
			return "", ErrInvalid
		}
		if m.agentIdentity(ancestor) == targetIdentity {
			recursions++
		}
	}
	if recursions > m.agentRecursionLimit(targetIdentity) {
		return "", ErrRecursion
	}
	id, err := taskID()
	if err != nil {
		return "", err
	}
	lineage = append([]string(nil), lineage...)
	now := time.Now().UTC()
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return "", ErrClosed
	}
	if len(m.tasks) >= m.config.MaxTasks {
		m.mu.Unlock()
		return "", ErrTaskLimit
	}
	if m.running >= m.config.MaxConcurrent || m.byParent[parentSession] >= m.config.MaxConcurrentPerParent {
		m.mu.Unlock()
		return "", ErrConcurrency
	}
	rootSession, parentAgentID := parentSession, ""
	if parent := m.bySession[parentSession]; parent != nil {
		rootSession, parentAgentID = parent.task.RootSession, parent.task.ID
	}
	ctx, cancel := context.WithTimeout(context.Background(), m.config.Timeout)
	state := &taskState{task: Task{ID: id, ParentSession: parentSession, RootSession: rootSession, ParentAgentID: parentAgentID, Agent: request.Agent, Model: request.Model, Lineage: lineage, ToolCallID: request.ToolCallID, Depth: len(lineage) + 1, Turn: 1, Status: StatusRunning, StartedAt: now}, request: request, turn: &turnState{done: make(chan struct{})}, registered: make(chan struct{}), cancel: cancel}
	m.tasks[id] = state
	m.running++
	m.byParent[parentSession]++
	m.workers.Add(1)
	start := LifecycleEvent{Kind: LifecycleStart, Task: cloneTask(state.task)}
	m.mu.Unlock()
	m.emit(start)
	go m.run(ctx, state)
	return id, nil
}

func (m *Manager) emit(event LifecycleEvent) {
	if m.config.OnEvent != nil {
		m.config.OnEvent(event)
	}
}

// TaskForSession returns the agent task bound to a child session, if any. The
// main task of a subagent child session is the subagent task itself.
func (m *Manager) TaskForSession(sessionID string) (Task, bool) {
	if m == nil || sessionID == "" {
		return Task{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	state := m.bySession[sessionID]
	if state == nil {
		return Task{}, false
	}
	return cloneTask(state.task), true
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

// Spawn derives durable ancestry from the caller session and waits only until
// the executor has established the reusable child session.
func (m *Manager) Spawn(ctx context.Context, parentSession, callerAgent string, request Request) (string, error) {
	lineage := []string{callerAgent}
	m.mu.Lock()
	if parent := m.bySession[parentSession]; parent != nil {
		lineage = append(append([]string(nil), parent.task.Lineage...), parent.task.Agent)
	}
	m.mu.Unlock()
	id, err := m.Launch(parentSession, lineage, request)
	if err != nil {
		return "", err
	}
	m.mu.Lock()
	state := m.tasks[id]
	registered := state.registered
	m.mu.Unlock()
	select {
	case <-registered:
		return m.finishSpawn(parentSession, id, state)
	case <-ctx.Done():
		m.mu.Lock()
		if state.registeredDone {
			m.mu.Unlock()
			return m.finishSpawn(parentSession, id, state)
		}
		if state.cancel != nil {
			state.cancel()
		}
		state.discard = true
		m.mu.Unlock()
		return "", ctx.Err()
	}
}

func (m *Manager) finishSpawn(parentSession, id string, state *taskState) (string, error) {
	task, err := m.Status(parentSession, id)
	if err != nil {
		return "", err
	}
	if task.SessionID != "" {
		return id, nil
	}
	m.discard(state)
	if task.Error == "" {
		return "", errors.New("subagent: executor did not register a child session")
	}
	return "", errors.New(task.Error)
}

func (m *Manager) discard(state *taskState) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.tasks[state.task.ID] != state {
		return
	}
	for _, child := range m.tasks {
		if child.task.ParentAgentID == state.task.ID {
			return
		}
	}
	delete(m.tasks, state.task.ID)
	delete(m.bySession, state.task.SessionID)
}

func (m *Manager) run(ctx context.Context, state *taskState) {
	turn := state.task.Turn
	report := func(progress Progress) { m.reportProgress(state, turn, progress) }
	register := func(sessionID string) {
		if sessionID == "" {
			return
		}
		m.mu.Lock()
		if state.task.SessionID == "" {
			state.task.SessionID = sessionID
			m.bySession[sessionID] = state
		}
		m.closeRegisteredLocked(state)
		working := LifecycleEvent{Kind: LifecycleWorking, Task: cloneTask(state.task)}
		m.mu.Unlock()
		m.emit(working)
	}
	execution := Execution{TaskID: state.task.ID, SessionID: state.task.SessionID, ParentSession: state.task.ParentSession, RootSession: state.task.RootSession, ParentAgentID: state.task.ParentAgentID, Lineage: append([]string(nil), state.task.Lineage...), Turn: turn, Request: state.request, ReportProgress: report, RegisterSession: register}
	output, executeErr := m.executor.Execute(ctx, execution)
	state.op.Lock()
	status := StatusSucceeded
	errText := ""
	if executeErr != nil {
		status, errText = StatusFailed, executeErr.Error()
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		status, errText = StatusTimedOut, ErrTimeout.Error()
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
	m.closeRegisteredLocked(state)
	if state.discard {
		delete(m.tasks, state.task.ID)
		delete(m.bySession, state.task.SessionID)
	}
	snapshot := cloneTask(state.task)
	state.turn.result = snapshot
	callback := m.config.OnProgress
	finished := LifecycleEvent{Kind: LifecycleFinished, Task: snapshot}
	done := state.turn.done
	m.mu.Unlock()
	state.op.Unlock()
	close(done)
	m.workers.Done()
	m.emit(finished)
	if callback != nil {
		callback(snapshot)
	}
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
	ctx, cancel := context.WithTimeout(context.Background(), m.config.Timeout)
	state.task.Turn++
	state.task.Status = StatusRunning
	state.task.StartedAt = time.Now().UTC()
	state.task.FinishedAt = time.Time{}
	state.task.Output, state.task.Error = "", ""
	state.task.Truncated = false
	state.task.ToolCallID = request.ToolCallID
	state.task.Usage = Usage{}
	state.task.ToolUses = 0
	request.Agent, request.Model = state.task.Agent, state.task.Model
	state.request = request
	state.turn = &turnState{done: make(chan struct{})}
	state.cancel = cancel
	m.running++
	m.byParent[state.task.ParentSession]++
	m.workers.Add(1)
	task := cloneTask(state.task)
	m.mu.Unlock()
	m.emit(LifecycleEvent{Kind: LifecycleWorking, Task: task})
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
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
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
	for _, child := range m.tasks {
		if child.task.ParentAgentID == id {
			return errors.New("subagent: cannot forget an agent with retained children")
		}
	}
	delete(m.tasks, id)
	delete(m.bySession, state.task.SessionID)
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
	return Execution{TaskID: state.task.ID, SessionID: state.task.SessionID, ParentSession: state.task.ParentSession, RootSession: state.task.RootSession, ParentAgentID: state.task.ParentAgentID, Lineage: append([]string(nil), state.task.Lineage...), Turn: turn, Request: state.request, ReportProgress: func(progress Progress) { m.reportProgress(state, turn, progress) }}
}

func (m *Manager) visibleLocked(callerSession string, target *taskState) bool {
	caller := m.bySession[callerSession]
	if caller == nil {
		return target.task.RootSession == callerSession
	}
	for current := target; current != nil; current = m.tasks[current.task.ParentAgentID] {
		if current.task.ParentAgentID == caller.task.ID {
			return true
		}
	}
	return false
}

func (m *Manager) closeRegisteredLocked(state *taskState) {
	if !state.registeredDone {
		close(state.registered)
		state.registeredDone = true
	}
}

func cloneTask(task Task) Task {
	task.Lineage = append([]string(nil), task.Lineage...)
	return task
}

func taskError(task Task) error {
	switch task.Status {
	case StatusCanceled:
		return ErrCanceled
	case StatusTimedOut:
		return ErrTimeout
	case StatusFailed:
		return errors.New(task.Error)
	default:
		return nil
	}
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

func taskID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("subagent: generate task ID: %w", err)
	}
	return "task_" + hex.EncodeToString(raw[:]), nil
}
