package agent

import (
	"context"
	"errors"
	"strings"
	"time"

	managedtask "github.com/amirulashraf/parrot-coder/internal/task"
	petname "github.com/dustinkirkland/golang-petname"
)

type childState struct {
	task    ChildTask
	request ChildRequest
	turn    *childTurnState
	cancel  context.CancelFunc
}

type childTurnState struct {
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	result ChildTask
	permit childPermit
}

type childPermit struct {
	user   ChildTurnPermit
	parent ChildTurnPermit
}

func (p childPermit) Release() {
	p.parent.Release()
	p.user.Release()
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

func (s *userSession) createChild(ctx context.Context, parent *agentSession, request ChildRequest) (AgentSession, error) {
	if err := parent.reserveChildCreation(); err != nil {
		return nil, err
	}
	defer parent.releaseChildCreation()
	s.childMu.Lock()
	defer s.childMu.Unlock()
	if s.closed {
		return nil, ErrUserSessionClosed
	}
	lineage := []string{parent.dto.Agent}
	root := parent.ID()
	if task, ok := parent.ChildTask(); ok {
		lineage = append(append([]string(nil), task.Lineage...), task.Agent)
		root = task.RootSession
	}
	if err := s.validateChild(parent.ID(), lineage, request); err != nil {
		return nil, err
	}
	name := s.uniqueChildName(request)
	request.Name = name
	child, err := s.repository.CreateChild(ctx, parent, ChildSessionRequest{ProjectID: s.config.ProjectID, Name: name, Agent: request.Agent, Model: request.Model, DefaultSelection: s.config.DefaultSelection})
	if err != nil {
		return nil, err
	}
	permit, err := s.admitChildTurn(parent)
	if err != nil {
		return nil, errors.Join(err, s.discardChild(context.WithoutCancel(ctx), child.ID()))
	}
	created := child.(*agentSession)
	now := time.Now().UTC()
	turnCtx, cancel := context.WithCancel(context.Background())
	turn := &childTurnState{ctx: turnCtx, cancel: cancel, done: make(chan struct{}), permit: permit}
	created.child = &childState{task: ChildTask{SessionID: child.ID(), ParentSession: parent.ID(), RootSession: root, Agent: request.Agent, Model: request.Model, Name: name, Lineage: append([]string(nil), lineage...), Depth: len(lineage), Turn: 1, Status: ChildStatusRunning, StartedAt: now, ToolCallID: request.ToolCallID}, request: request, turn: turn, cancel: cancel}
	if err := s.registerChild(created); err != nil {
		permit.Release()
		return nil, errors.Join(err, s.discardChild(context.WithoutCancel(ctx), child.ID()))
	}
	s.workers.Add(1)
	s.emitChild(ChildLifecycleEvent{Kind: ChildLifecycleStart, Task: created.childTaskSnapshot()})
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

func (s *userSession) admitChildTurn(parent *agentSession) (childPermit, error) {
	userPermit, ok := s.childTurns.tryAcquire()
	if !ok {
		return childPermit{}, ErrChildConcurrency
	}
	parentPermit, ok := parent.childTurns.tryAcquire()
	if !ok {
		userPermit.Release()
		return childPermit{}, ErrChildConcurrency
	}
	return childPermit{user: userPermit, parent: parentPermit}, nil
}

func (s *agentSession) runChild(turn *childTurnState) {
	s.mu.Lock()
	state := s.child
	task := cloneChildTask(state.task)
	s.mu.Unlock()
	s.user.emitChild(ChildLifecycleEvent{Kind: ChildLifecycleWorking, Task: task})
	stop := func() {}
	if s.user.config.ObserveChildProgress != nil {
		stop = s.user.config.ObserveChildProgress(s.ID(), s.reportChildProgress)
	}
	output, runErr := s.Prompt(turn.ctx, state.request.Prompt)
	stop()
	turn.cancel()
	s.childOp.Lock()
	s.mu.Lock()
	status, errText := ChildStatusSucceeded, ""
	if runErr != nil {
		status, errText = ChildStatusFailed, runErr.Error()
	}
	if errors.Is(turn.ctx.Err(), context.Canceled) && errors.Is(runErr, context.Canceled) {
		status, errText = ChildStatusCanceled, ErrChildCanceled.Error()
	}
	output, outputTruncated := truncateChild(output, s.user.config.MaxChildResultBytes)
	errText, errorTruncated := truncateChild(errText, s.user.config.MaxChildResultBytes)
	state.task.Status, state.task.Output, state.task.Error = status, output, errText
	state.task.Truncated = outputTruncated || errorTruncated
	state.task.FinishedAt = time.Now().UTC()
	result := cloneChildTask(state.task)
	turn.result = result
	s.mu.Unlock()
	turn.permit.Release()
	close(turn.done)
	s.user.workers.Done()
	s.childOp.Unlock()
	s.user.emitChild(ChildLifecycleEvent{Kind: ChildLifecycleFinished, Task: result})
	if s.user.config.OnChildProgress != nil {
		s.user.config.OnChildProgress(result)
	}
	if s.user.config.OnChildComplete != nil {
		s.user.config.OnChildComplete(result)
	}
}

func (s *agentSession) sendChild(ctx context.Context, request ChildRequest) (ChildTask, string, error) {
	if strings.TrimSpace(request.Prompt) == "" {
		return ChildTask{}, "", ErrInvalidChildRequest
	}
	if len(request.Prompt) > s.user.config.MaxChildPromptBytes {
		return ChildTask{}, "", ErrChildRequestLimit
	}
retry:
	s.childOp.Lock()
	s.mu.Lock()
	state := s.child
	if state == nil {
		s.mu.Unlock()
		s.childOp.Unlock()
		return ChildTask{}, "", ErrChildNotFound
	}
	if state.task.Status == ChildStatusRunning || state.task.Status == ChildStatusPending {
		if s.drain == nil {
			turn := state.turn
			s.mu.Unlock()
			s.childOp.Unlock()
			select {
			case <-turn.done:
				goto retry
			case <-ctx.Done():
				return ChildTask{}, "", ctx.Err()
			}
		}
		messageID, err := s.admitLocked(ctx, request.Prompt)
		if err == nil {
			s.drain.wake = true
		}
		task := cloneChildTask(state.task)
		s.mu.Unlock()
		s.childOp.Unlock()
		return task, messageID, err
	}
	s.mu.Unlock()
	defer s.childOp.Unlock()
	s.user.childMu.Lock()
	defer s.user.childMu.Unlock()
	if s.user.closed {
		return ChildTask{}, "", ErrUserSessionClosed
	}
	permit, err := s.user.admitChildTurn(s.parent.(*agentSession))
	if err != nil {
		return ChildTask{}, "", err
	}
	s.mu.Lock()
	if s.removed {
		s.mu.Unlock()
		permit.Release()
		return ChildTask{}, "", ErrAgentSessionRemoved
	}
	state.task.Turn++
	state.task.Status, state.task.StartedAt, state.task.FinishedAt = ChildStatusRunning, time.Now().UTC(), time.Time{}
	state.task.Output, state.task.Error, state.task.Truncated = "", "", false
	state.task.ToolCallID, state.task.Usage, state.task.ToolUses = request.ToolCallID, ChildUsage{}, 0
	request.Agent, request.Model, request.Name = state.task.Agent, state.task.Model, state.task.Name
	state.request = request
	turnCtx, cancel := context.WithCancel(context.Background())
	turn := &childTurnState{ctx: turnCtx, cancel: cancel, done: make(chan struct{}), permit: permit}
	state.turn, state.cancel = turn, cancel
	task := cloneChildTask(state.task)
	s.mu.Unlock()
	s.user.workers.Add(1)
	go s.runChild(turn)
	return task, "", nil
}

func (s *agentSession) interruptChild(ctx context.Context) (ChildTask, error) {
	s.mu.Lock()
	state := s.child
	if state == nil {
		s.mu.Unlock()
		return ChildTask{}, ErrChildNotFound
	}
	if state.task.Status != ChildStatusRunning && state.task.Status != ChildStatusPending {
		task := cloneChildTask(state.task)
		s.mu.Unlock()
		return task, nil
	}
	if state.cancel != nil {
		state.cancel()
	}
	turn := state.turn
	s.mu.Unlock()
	select {
	case <-turn.done:
		return cloneChildTask(turn.result), nil
	case <-ctx.Done():
		return ChildTask{}, ctx.Err()
	}
}

func (s *userSession) childTask(sessionID string) (ChildTask, bool) {
	child, ok := s.repository.Lookup(sessionID)
	if !ok {
		return ChildTask{}, false
	}
	item := child.(*agentSession)
	item.mu.Lock()
	defer item.mu.Unlock()
	if item.child == nil {
		return ChildTask{}, false
	}
	return cloneChildTask(item.child.task), true
}

func (s *userSession) resolveChild(callerSessionID, identifier string) (AgentSession, error) {
	if identifier == "" {
		return nil, ErrChildNotFound
	}
	pending := s.repository.ChildSessions(callerSessionID)
	for len(pending) > 0 {
		relation := pending[0]
		pending = pending[1:]
		child, ok := s.repository.Lookup(relation.SessionID)
		if !ok {
			continue
		}
		if relation.SessionID == identifier || child.Name() == identifier {
			return child, nil
		}
		pending = append(pending, s.repository.ChildSessions(relation.SessionID)...)
	}
	return nil, ErrChildNotFound
}

func (s *userSession) observeChild(callerSessionID, sessionID string) (ChildTurnObserver, error) {
	child, err := s.resolveChild(callerSessionID, sessionID)
	if err != nil {
		return nil, err
	}
	item := child.(*agentSession)
	item.mu.Lock()
	defer item.mu.Unlock()
	if item.child == nil {
		return nil, ErrChildNotFound
	}
	return childObserver{turn: item.child.turn}, nil
}

type childObserver struct{ turn *childTurnState }

func (o childObserver) Wait(ctx context.Context) (ChildTask, error) {
	if o.turn == nil {
		return ChildTask{}, ErrChildNotFound
	}
	select {
	case <-o.turn.done:
		return cloneChildTask(o.turn.result), nil
	case <-ctx.Done():
		return ChildTask{}, ctx.Err()
	}
}

func (s *agentSession) forget() error {
	s.childOp.Lock()
	defer s.childOp.Unlock()
	s.user.childMu.Lock()
	defer s.user.childMu.Unlock()
	s.mu.Lock()
	if s.child == nil {
		s.mu.Unlock()
		return ErrChildNotFound
	}
	if s.child.task.Status == ChildStatusRunning || s.child.task.Status == ChildStatusPending {
		s.mu.Unlock()
		return ErrChildRunning
	}
	s.mu.Unlock()
	if s.user.repository.HasChildSessions(s.ID()) {
		return errors.New("agent: cannot forget an agent with retained children")
	}
	if err := s.user.repository.ForgetChild(s.ID()); err != nil {
		return err
	}
	if s.user.config.ChildTasks != nil {
		s.user.config.ChildTasks.Unregister(s.ID())
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
	return childSnapshot(t.child.childTaskSnapshot())
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
	item, err := t.child.InterruptChild(ctx)
	return childSnapshot(item), err
}
func (t managedChildTurn) Snapshot() managedtask.Snapshot {
	select {
	case <-t.turn.done:
		return childSnapshot(t.turn.result)
	default:
		return childSnapshot(t.child.childTaskSnapshot())
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

func (s *agentSession) childTaskSnapshot() ChildTask {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.child == nil {
		return ChildTask{}
	}
	return cloneChildTask(s.child.task)
}

func (s *agentSession) reportChildProgress(progress ChildProgress) {
	if progress.ToolUses < 0 || progress.Usage.InputTokens < 0 || progress.Usage.OutputTokens < 0 || progress.Usage.TotalTokens < 0 || progress.Usage.ReasoningTokens < 0 || progress.Usage.CachedInputTokens < 0 {
		return
	}
	s.mu.Lock()
	if s.child == nil || s.child.task.Status != ChildStatusRunning {
		s.mu.Unlock()
		return
	}
	s.child.task.Usage.InputTokens += progress.Usage.InputTokens
	s.child.task.Usage.OutputTokens += progress.Usage.OutputTokens
	s.child.task.Usage.TotalTokens += progress.Usage.TotalTokens
	s.child.task.Usage.ReasoningTokens += progress.Usage.ReasoningTokens
	s.child.task.Usage.CachedInputTokens += progress.Usage.CachedInputTokens
	s.child.task.ToolUses += progress.ToolUses
	task := cloneChildTask(s.child.task)
	s.mu.Unlock()
	if s.user.config.OnChildProgress != nil {
		s.user.config.OnChildProgress(task)
	}
}

func (s *userSession) shutdownChildren(ctx context.Context) error {
	s.childMu.Lock()
	if !s.closed {
		s.closed = true
		for _, relation := range s.repository.children {
			if child, ok := s.repository.Lookup(relation.SessionID); ok {
				item := child.(*agentSession)
				item.mu.Lock()
				if item.child != nil && item.child.cancel != nil {
					item.child.cancel()
				}
				item.mu.Unlock()
			}
		}
	}
	s.childMu.Unlock()
	done := make(chan struct{})
	go func() { s.workers.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
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
func (s *userSession) emitChild(event ChildLifecycleEvent) {
	if s.config.OnChildLifecycle != nil {
		s.config.OnChildLifecycle(event)
	}
}
func childSnapshot(item ChildTask) managedtask.Snapshot {
	return managedtask.Snapshot{ID: item.SessionID, Name: item.Name, SessionID: item.SessionID, Kind: managedtask.KindAgent, Status: string(item.Status), StartedAt: item.StartedAt, Agent: item.Agent, Turn: item.Turn, Depth: item.Depth}
}
func cloneChildTask(task ChildTask) ChildTask {
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
