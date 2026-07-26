package agent

import (
	"context"
	"errors"
	"strings"
	"sync"

	"github.com/amirulashraf/parrot-coder/internal/id"
	managedtask "github.com/amirulashraf/parrot-coder/internal/task"
	petname "github.com/dustinkirkland/golang-petname"
)

type managedTurnPolicy struct {
	maxPromptBytes int
	maxResultBytes int
	tryAcquire     func() (func(), bool, error)
	acquire        func(context.Context) (func(), error)
	observe        func(func(ChildProgress)) func()
	onProgress     func(Status)
	onComplete     func(Status)
	onLifecycle    func(ChildLifecycleEvent)
}

func (p managedTurnPolicy) Validate(content string) error {
	if strings.TrimSpace(content) == "" {
		return ErrInvalidChildRequest
	}
	if len(content) > p.maxPromptBytes {
		return ErrChildRequestLimit
	}
	return nil
}

func applyChildDefaults(config *UserSessionConfig) {
	if config.MaxChildDepth <= 0 {
		config.MaxChildDepth = 4
	}
	if config.MaxChildTasks <= 0 {
		config.MaxChildTasks = 1024
	}
	if config.MaxChildPromptBytes <= 0 {
		config.MaxChildPromptBytes = 1 << 20
	}
	if config.MaxChildResultBytes <= 0 {
		config.MaxChildResultBytes = 1 << 20
	}
}

func (s *agentSession) CreateChild(ctx context.Context, request ChildRequest) (AgentSession, error) {
	if s.user == nil {
		return nil, ErrChildNotFound
	}
	return s.user.CreateChild(ctx, s, request)
}

func (user *userSession) CreateChild(ctx context.Context, parent AgentSession, request ChildRequest) (AgentSession, error) {
	s, ok := parent.(*agentSession)
	if !ok || s == nil {
		return nil, errors.New("agent: child parent runtime is not owned by this user session")
	}
	if err := s.reserveChildCreation(); err != nil {
		return nil, err
	}
	defer s.releaseChildCreation()

	owned, ok := user.repository.Lookup(s.ID())
	if !ok || owned != parent {
		return nil, errors.New("agent: child parent runtime is not owned by this user session")
	}
	user.childMu.Lock()
	defer user.childMu.Unlock()
	user.quotaMu.Lock()
	closed := user.closed
	user.quotaMu.Unlock()
	if closed {
		return nil, ErrUserSessionClosed
	}

	s.mu.Lock()
	lineage := append(append([]string(nil), s.status.Lineage...), s.status.Agent)
	s.mu.Unlock()
	if err := user.validateChild(s.ID(), lineage, request); err != nil {
		return nil, err
	}
	name := user.uniqueChildName(request)
	request.Name = name
	child, err := user.repository.CreateChild(ctx, s, ChildSessionRequest{ProjectID: user.config.ProjectID, Name: name, Agent: request.Agent, Model: request.Model, DefaultSelection: user.config.DefaultSelection})
	if err != nil {
		return nil, err
	}
	created := child.(*agentSession)
	messageID, err := id.New("msg")
	var turn *turnState
	if err == nil {
		_, turn, err = created.admitAndStart(ctx, messageID, request.Prompt, request.Prompt, true, false)
	}
	if err != nil {
		return nil, errors.Join(err, user.discardChild(context.WithoutCancel(ctx), child.ID()))
	}
	created.mu.Lock()
	created.managedTask = true
	created.mu.Unlock()
	if err := user.registerChild(created); err != nil {
		created.mu.Lock()
		created.managedTask = false
		created.mu.Unlock()
		created.abortTurn(turn)
		return nil, errors.Join(err, user.discardChild(context.WithoutCancel(ctx), child.ID()))
	}
	created.startTurn(turn)
	return child, nil
}

func (s *userSession) validateChild(parent string, lineage []string, request ChildRequest) error {
	if strings.TrimSpace(parent) == "" || strings.TrimSpace(request.Prompt) == "" || !validChildAgent(request.Agent) {
		return ErrInvalidChildRequest
	}
	if len(request.Prompt) > s.config.MaxChildPromptBytes {
		return ErrChildRequestLimit
	}
	if len(lineage) > s.config.MaxChildDepth {
		return ErrChildDepth
	}
	identity := s.childAgentIdentity(request.Agent)
	recursions := 1
	for _, ancestor := range lineage {
		if !validChildAgent(ancestor) {
			return ErrInvalidChildRequest
		}
		if s.childAgentIdentity(ancestor) == identity {
			recursions++
		}
	}
	if recursions > s.childRecursionLimit(identity) {
		return ErrChildRecursion
	}
	if s.repository.ManagedChildTasks() >= s.config.MaxChildTasks {
		return ErrChildTaskLimit
	}
	return nil
}

func (s *agentSession) tryAcquireWorkerQuota() (func(), error) {
	release, blocked, err := s.tryAcquireWorkerQuotaWait()
	if err != nil {
		return nil, err
	}
	if blocked {
		return nil, ErrChildConcurrency
	}
	return release, nil
}

func (s *agentSession) tryAcquireWorkerQuotaWait() (func(), bool, error) {
	if s.user == nil || s.parent == nil {
		return nil, false, ErrChildNotFound
	}
	parent, ok := s.parent.(*agentSession)
	if !ok || parent == nil {
		return nil, false, ErrChildNotFound
	}
	parentPermit, ok := parent.childTurns.tryAcquire()
	if !ok {
		return nil, true, nil
	}
	globalRelease, err := s.user.TryAcquireWorkerQuota()
	if errors.Is(err, ErrChildConcurrency) {
		parentPermit.Release()
		return nil, true, nil
	}
	if err != nil {
		parentPermit.Release()
		return nil, false, err
	}
	var once sync.Once
	return func() { once.Do(func() { parentPermit.Release(); globalRelease() }) }, false, nil
}

func (p managedTurnPolicy) TryAcquire() (func(), bool, error)           { return p.tryAcquire() }
func (p managedTurnPolicy) Acquire(ctx context.Context) (func(), error) { return p.acquire(ctx) }
func (p managedTurnPolicy) CapturesOutput() bool                        { return true }
func (p managedTurnPolicy) Started(status Status) {
	if p.onLifecycle != nil {
		p.onLifecycle(ChildLifecycleEvent{Kind: ChildLifecycleStart, Task: status})
	}
}
func (p managedTurnPolicy) Working(status Status, report func(ChildProgress)) func() {
	if p.onLifecycle != nil {
		p.onLifecycle(ChildLifecycleEvent{Kind: ChildLifecycleWorking, Task: status})
	}
	if p.observe == nil {
		return func() {}
	}
	return p.observe(report)
}
func (p managedTurnPolicy) Finished(_ Status, output string, runErr error, canceled bool) (string, error, bool) {
	if canceled && errors.Is(runErr, context.Canceled) {
		runErr = ErrChildCanceled
	}
	output, outputTruncated := truncateChild(output, p.maxResultBytes)
	if runErr == nil {
		return output, nil, outputTruncated
	}
	errText, errorTruncated := truncateChild(runErr.Error(), p.maxResultBytes)
	if errorTruncated {
		runErr = errors.New(errText)
	}
	return output, runErr, outputTruncated || errorTruncated
}
func (p managedTurnPolicy) Published(result Status) {
	if p.onLifecycle != nil {
		p.onLifecycle(ChildLifecycleEvent{Kind: ChildLifecycleFinished, Task: result})
	}
	if p.onProgress != nil {
		p.onProgress(result)
	}
}
func (p managedTurnPolicy) Completed(result Status) {
	if p.onComplete != nil {
		p.onComplete(result)
	}
}

type turnObserver struct{ turn *turnState }

func (o turnObserver) Wait(ctx context.Context) (Status, error) {
	if o.turn == nil {
		return Status{}, ErrChildNotFound
	}
	select {
	case <-o.turn.done:
		return cloneStatus(o.turn.result), nil
	case <-ctx.Done():
		return Status{}, ctx.Err()
	}
}

func (s *userSession) discardChild(ctx context.Context, sessionID string) error {
	if err := s.repository.DiscardChild(ctx, sessionID); err != nil {
		return err
	}
	if s.config.OnChildDiscard != nil {
		s.config.OnChildDiscard(sessionID)
	}
	return nil
}

func (s *userSession) registerChild(child *agentSession) error {
	if s.config.ChildTasks == nil {
		return nil
	}
	return s.config.ChildTasks.Register(managedChildTask{child}, func(caller string) bool { return s.childVisible(caller, child.ID()) })
}

func (s *userSession) childVisible(caller, target string) bool {
	current := target
	for {
		parent, ok := s.repository.ChildRelation(current)
		if !ok {
			return false
		}
		if parent == caller {
			return true
		}
		current = parent
	}
}

type managedChildTask struct{ child *agentSession }
type managedChildTurn struct {
	child *agentSession
	turn  *turnState
}

func (t managedChildTask) Snapshot() managedtask.Snapshot {
	return childSnapshot(t.child.statusSnapshot())
}
func (t managedChildTask) Wait(ctx context.Context) (managedtask.Completion, error) {
	return t.Observe().Wait(ctx)
}
func (t managedChildTask) Observe() managedtask.Task {
	t.child.mu.Lock()
	turn := t.child.turn
	t.child.mu.Unlock()
	return managedChildTurn{child: t.child, turn: turn}
}
func (t managedChildTask) Interrupt(ctx context.Context) (managedtask.Snapshot, error) {
	err := t.child.Interrupt(ctx)
	return childSnapshot(t.child.Status()), err
}
func (t managedChildTurn) Snapshot() managedtask.Snapshot {
	select {
	case <-t.turn.done:
		return childSnapshot(t.turn.result)
	default:
		return childSnapshot(t.child.statusSnapshot())
	}
}
func (t managedChildTurn) Wait(ctx context.Context) (managedtask.Completion, error) {
	item, err := turnObserver{turn: t.turn}.Wait(ctx)
	return managedtask.Completion{Task: childSnapshot(item), Output: item.Output, Error: item.Error}, err
}
func (t managedChildTurn) Interrupt(ctx context.Context) (managedtask.Snapshot, error) {
	t.child.mu.Lock()
	current := t.child.turn == t.turn
	t.child.mu.Unlock()
	if !current {
		return t.Snapshot(), nil
	}
	return managedChildTask{t.child}.Interrupt(ctx)
}

func (s *agentSession) statusSnapshot() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneStatus(s.status)
}

func (s *agentSession) reportChildProgress(progress ChildProgress) {
	s.mu.Lock()
	turn := s.turn
	s.mu.Unlock()
	s.reportTurnProgress(turn, progress)
}

func (s *agentSession) reportTurnProgress(turn *turnState, progress ChildProgress) {
	if progress.ToolUses < 0 || progress.Usage.InputTokens < 0 || progress.Usage.OutputTokens < 0 || progress.Usage.TotalTokens < 0 || progress.Usage.ReasoningTokens < 0 || progress.Usage.CachedInputTokens < 0 {
		return
	}
	s.mu.Lock()
	if turn == nil || s.turn != turn || turn.status != StatusRunning {
		s.mu.Unlock()
		return
	}
	s.status.Usage.InputTokens += progress.Usage.InputTokens
	s.status.Usage.OutputTokens += progress.Usage.OutputTokens
	s.status.Usage.TotalTokens += progress.Usage.TotalTokens
	s.status.Usage.ReasoningTokens += progress.Usage.ReasoningTokens
	s.status.Usage.CachedInputTokens += progress.Usage.CachedInputTokens
	s.status.ToolUses += progress.ToolUses
	task := cloneStatus(s.status)
	s.mu.Unlock()
	if s.onChildProgress != nil {
		s.onChildProgress(task)
	}
}

func (s *userSession) uniqueChildName(request ChildRequest) string {
	name := managedtask.SanitizeName(request.Name)
	if name == "" {
		generated := petname.Generate(2, "-")
		if s.config.ChildNameGenerator != nil {
			generated = s.config.ChildNameGenerator()
		}
		name = managedtask.SanitizeName(request.Agent + "-" + generated)
	}
	return managedtask.UniqueName(name, func(candidate string) bool {
		for _, relation := range s.repository.children {
			if child, ok := s.repository.Lookup(relation.SessionID); ok && child.Name() == candidate {
				return false
			}
		}
		return true
	})
}

func (s *userSession) childAgentIdentity(id string) string {
	if s.config.ChildAgentIdentity != nil {
		if identity := s.config.ChildAgentIdentity(id); identity != "" {
			return identity
		}
	}
	return id
}
func (s *userSession) childRecursionLimit(id string) int {
	if s.config.ChildAgentRecursionLimit != nil {
		if limit := s.config.ChildAgentRecursionLimit(id); limit > 0 {
			return limit
		}
	}
	return 3
}
func (s *agentSession) emitChild(event ChildLifecycleEvent) {
	if s.onChildLifecycle != nil {
		s.onChildLifecycle(event)
	}
}

func childSnapshot(item Status) managedtask.Snapshot {
	return managedtask.Snapshot{ID: item.SessionID, Name: item.Name, SessionID: item.SessionID, Kind: managedtask.KindAgent, Status: string(item.State), StartedAt: item.StartedAt, Agent: item.Agent, Turn: item.Turn, Depth: item.Depth}
}
func cloneStatus(task Status) Status {
	task.Lineage = append([]string(nil), task.Lineage...)
	return task
}
func truncateChild(value string, limit int) (string, bool) {
	if len(value) <= limit {
		return value, false
	}
	return value[:limit], true
}
func validChildAgent(agent string) bool {
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
