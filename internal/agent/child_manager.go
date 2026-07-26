package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	managedtask "github.com/amirulashraf/parrot-coder/internal/task"
	petname "github.com/dustinkirkland/golang-petname"
)

type childState struct {
	status  Status
	request ChildRequest
	turn    *childTurnState
	cancel  context.CancelFunc
}

type childTurnState struct {
	ctx          context.Context
	cancel       context.CancelFunc
	done         chan struct{}
	result       Status
	releaseQuota func()
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
	lineage := []string{s.dto.Agent}
	root := s.dto.ID
	if s.child != nil {
		lineage = append(append([]string(nil), s.child.status.Lineage...), s.child.status.Agent)
		root = s.child.status.RootSession
	}
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
	releaseQuota, err := created.tryAcquireWorkerQuota()
	if err != nil {
		return nil, errors.Join(err, user.discardChild(context.WithoutCancel(ctx), child.ID()))
	}
	now := time.Now().UTC()
	turnCtx, cancel := context.WithCancel(context.Background())
	turn := &childTurnState{ctx: turnCtx, cancel: cancel, done: make(chan struct{}), releaseQuota: releaseQuota}
	created.mu.Lock()
	if created.shuttingDown {
		created.mu.Unlock()
		releaseQuota()
		return nil, errors.Join(ErrUserSessionClosed, user.discardChild(context.WithoutCancel(ctx), child.ID()))
	}
	created.child = &childState{status: Status{SessionID: child.ID(), ParentSession: s.ID(), RootSession: root, Agent: request.Agent, Model: request.Model, Name: name, Lineage: append([]string(nil), lineage...), Depth: len(lineage), Turn: 1, State: StatusRunning, StartedAt: now}, request: request, turn: turn, cancel: cancel}
	created.mu.Unlock()
	if err := user.registerChild(created); err != nil {
		created.mu.Lock()
		created.child = nil
		created.mu.Unlock()
		cancel()
		releaseQuota()
		close(turn.done)
		return nil, errors.Join(err, user.discardChild(context.WithoutCancel(ctx), child.ID()))
	}
	created.emitChild(ChildLifecycleEvent{Kind: ChildLifecycleStart, Task: created.statusSnapshot()})
	go created.runChild(turn)
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
	if s.user == nil || s.parent == nil {
		return nil, ErrChildNotFound
	}
	releaseGlobal, err := s.user.TryAcquireWorkerQuota()
	if err != nil {
		return nil, err
	}
	parent, ok := s.parent.(*agentSession)
	if !ok || parent == nil {
		releaseGlobal()
		return nil, ErrChildNotFound
	}
	parentPermit, ok := parent.childTurns.tryAcquire()
	if !ok {
		releaseGlobal()
		return nil, ErrChildConcurrency
	}
	var once sync.Once
	return func() { once.Do(func() { parentPermit.Release(); releaseGlobal() }) }, nil
}

func (s *agentSession) runChild(turn *childTurnState) {
	s.mu.Lock()
	state := s.child
	task := cloneStatus(state.status)
	s.mu.Unlock()
	s.emitChild(ChildLifecycleEvent{Kind: ChildLifecycleWorking, Task: task})
	stop := func() {}
	if s.observeChildProgress != nil {
		stop = s.observeChildProgress(s.ID(), s.reportChildProgress)
	}
	output, runErr := s.Prompt(turn.ctx, state.request.Prompt)
	stop()
	turn.cancel()
	s.childOp.Lock()
	s.mu.Lock()
	status, errText := StatusSucceeded, ""
	if runErr != nil {
		status, errText = StatusFailed, runErr.Error()
	}
	if errors.Is(turn.ctx.Err(), context.Canceled) && errors.Is(runErr, context.Canceled) {
		status, errText = StatusCanceled, ErrChildCanceled.Error()
	}
	output, outputTruncated := truncateChild(output, s.maxChildResultBytes)
	errText, errorTruncated := truncateChild(errText, s.maxChildResultBytes)
	state.status.State, state.status.Output, state.status.Error = status, output, errText
	state.status.Truncated = outputTruncated || errorTruncated
	state.status.FinishedAt = time.Now().UTC()
	result := cloneStatus(state.status)
	turn.result = result
	s.mu.Unlock()
	turn.releaseQuota()
	// Publish terminal state while childOp still prevents a follow-up turn from
	// starting. Otherwise its task.working event can overtake this turn's final
	// events and make consumers attribute the old counters to the new turn.
	s.emitChild(ChildLifecycleEvent{Kind: ChildLifecycleFinished, Task: result})
	if s.onChildProgress != nil {
		s.onChildProgress(result)
	}
	close(turn.done)
	s.childOp.Unlock()
	if s.onChildComplete != nil {
		s.onChildComplete(result)
	}
}

func (s *agentSession) sendManagedTurn(ctx context.Context, content, measuredContent string) (string, string, error) {
	if strings.TrimSpace(measuredContent) == "" {
		return "", "", ErrInvalidChildRequest
	}
	if len(measuredContent) > s.maxChildPromptBytes {
		return "", "", ErrChildRequestLimit
	}
retry:
	s.childOp.Lock()
	s.mu.Lock()
	state := s.child
	if state == nil {
		s.mu.Unlock()
		s.childOp.Unlock()
		return "", "", ErrChildNotFound
	}
	if state.status.State == StatusRunning || state.status.State == StatusPending {
		if s.drain == nil {
			turn := state.turn
			s.mu.Unlock()
			s.childOp.Unlock()
			select {
			case <-turn.done:
				goto retry
			case <-ctx.Done():
				return "", "", ctx.Err()
			}
		}
		admission, err := s.admitLocked(ctx, content)
		if err == nil {
			s.drain.wake = true
		}
		s.mu.Unlock()
		s.childOp.Unlock()
		return admission.Input.MessageID, admission.Input.ID, err
	}
	s.mu.Unlock()
	defer s.childOp.Unlock()
	releaseQuota, err := s.tryAcquireWorkerQuota()
	if err != nil {
		return "", "", err
	}
	s.mu.Lock()
	if s.removed {
		s.mu.Unlock()
		releaseQuota()
		return "", "", ErrAgentSessionRemoved
	}
	if s.shuttingDown {
		s.mu.Unlock()
		releaseQuota()
		return "", "", ErrUserSessionClosed
	}
	state.status.Turn++
	state.status.State, state.status.StartedAt, state.status.FinishedAt = StatusRunning, time.Now().UTC(), time.Time{}
	state.status.Output, state.status.Error, state.status.Truncated = "", "", false
	state.status.Usage, state.status.ToolUses = ChildUsage{}, 0
	state.request = ChildRequest{Prompt: content, Agent: state.status.Agent, Model: state.status.Model, Name: state.status.Name}
	turnCtx, cancel := context.WithCancel(context.Background())
	turn := &childTurnState{ctx: turnCtx, cancel: cancel, done: make(chan struct{}), releaseQuota: releaseQuota}
	state.turn, state.cancel = turn, cancel
	s.mu.Unlock()
	go s.runChild(turn)
	return "", "", nil
}

type childObserver struct{ turn *childTurnState }

func (o childObserver) Wait(ctx context.Context) (Status, error) {
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

func (s *agentSession) forget() error {
	if s.user == nil {
		return ErrChildNotFound
	}
	return s.user.forgetChild(s)
}

func (user *userSession) forgetChild(s *agentSession) error {
	s.childOp.Lock()
	defer s.childOp.Unlock()
	user.childMu.Lock()
	defer user.childMu.Unlock()
	s.mu.Lock()
	if s.child == nil {
		s.mu.Unlock()
		return ErrChildNotFound
	}
	if s.child.status.State == StatusRunning || s.child.status.State == StatusPending {
		s.mu.Unlock()
		return ErrChildRunning
	}
	s.mu.Unlock()
	if user.repository.HasChildSessions(s.ID()) {
		return errors.New("agent: cannot forget an agent with retained children")
	}
	if err := user.repository.ForgetChild(s.ID()); err != nil {
		return err
	}
	if user.config.ChildTasks != nil {
		user.config.ChildTasks.Unregister(s.ID())
	}
	return nil
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
	turn  *childTurnState
}

func (t managedChildTask) Snapshot() managedtask.Snapshot {
	return childSnapshot(t.child.statusSnapshot())
}
func (t managedChildTask) Wait(ctx context.Context) (managedtask.Completion, error) {
	return t.Observe().Wait(ctx)
}
func (t managedChildTask) Observe() managedtask.Task {
	t.child.mu.Lock()
	turn := t.child.child.turn
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
	item, err := childObserver{turn: t.turn}.Wait(ctx)
	return managedtask.Completion{Task: childSnapshot(item), Output: item.Output, Error: item.Error}, err
}
func (t managedChildTurn) Interrupt(ctx context.Context) (managedtask.Snapshot, error) {
	t.child.mu.Lock()
	current := t.child.child.turn == t.turn
	t.child.mu.Unlock()
	if !current {
		return t.Snapshot(), nil
	}
	return managedChildTask{t.child}.Interrupt(ctx)
}

func (s *agentSession) statusSnapshot() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.child == nil {
		return Status{}
	}
	return cloneStatus(s.child.status)
}

func (s *agentSession) reportChildProgress(progress ChildProgress) {
	if progress.ToolUses < 0 || progress.Usage.InputTokens < 0 || progress.Usage.OutputTokens < 0 || progress.Usage.TotalTokens < 0 || progress.Usage.ReasoningTokens < 0 || progress.Usage.CachedInputTokens < 0 {
		return
	}
	s.mu.Lock()
	if s.child == nil || s.child.status.State != StatusRunning {
		s.mu.Unlock()
		return
	}
	s.child.status.Usage.InputTokens += progress.Usage.InputTokens
	s.child.status.Usage.OutputTokens += progress.Usage.OutputTokens
	s.child.status.Usage.TotalTokens += progress.Usage.TotalTokens
	s.child.status.Usage.ReasoningTokens += progress.Usage.ReasoningTokens
	s.child.status.Usage.CachedInputTokens += progress.Usage.CachedInputTokens
	s.child.status.ToolUses += progress.ToolUses
	task := cloneStatus(s.child.status)
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
