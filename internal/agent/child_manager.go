package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/amirulashraf/parrot-coder/internal/session"
	managedtask "github.com/amirulashraf/parrot-coder/internal/task"
	petname "github.com/dustinkirkland/golang-petname"
)

type childTurnState struct {
	ctx          context.Context
	cancel       context.CancelFunc
	done         chan struct{}
	result       Status
	releaseQuota func()
	drain        *drainState
	messageID    string
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
	if s.childTurn != nil {
		lineage = append(append([]string(nil), s.childStatus.Lineage...), s.childStatus.Agent)
		root = s.childStatus.RootSession
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
	releaseQuota, blocked, err := created.tryAcquireWorkerQuotaWait()
	if err != nil {
		return nil, errors.Join(err, user.discardChild(context.WithoutCancel(ctx), child.ID()))
	}
	turnCtx, cancel := context.WithCancel(context.Background())
	turn := &childTurnState{ctx: turnCtx, cancel: cancel, done: make(chan struct{}), releaseQuota: releaseQuota}
	status := StatusBlocked
	var startedAt time.Time
	if !blocked {
		status = StatusRunning
		startedAt = time.Now().UTC()
	}
	created.mu.Lock()
	if created.shuttingDown {
		created.mu.Unlock()
		cancel()
		if releaseQuota != nil {
			releaseQuota()
		}
		return nil, errors.Join(ErrUserSessionClosed, user.discardChild(context.WithoutCancel(ctx), child.ID()))
	}
	created.childStatus = Status{SessionID: child.ID(), ParentSession: s.ID(), RootSession: root, Agent: created.dto.Agent, Provider: created.dto.Provider, Model: created.dto.Model, Variant: created.dto.Variant, Name: name, Lineage: append([]string(nil), lineage...), Depth: len(lineage), Turn: 1, State: status, StartedAt: startedAt}
	created.childRequest, created.childTurn, created.cancelChild = request, turn, cancel
	created.mu.Unlock()
	if err := user.registerChild(created); err != nil {
		created.mu.Lock()
		created.childStatus, created.childRequest = Status{}, ChildRequest{}
		created.childTurn, created.cancelChild = nil, nil
		created.mu.Unlock()
		cancel()
		if releaseQuota != nil {
			releaseQuota()
		}
		close(turn.done)
		return nil, errors.Join(err, user.discardChild(context.WithoutCancel(ctx), child.ID()))
	}
	created.emitChild(ChildLifecycleEvent{Kind: ChildLifecycleStart, Task: created.statusSnapshot()})
	if !blocked {
		go created.runChild(turn)
	} else {
		go created.waitForWorkerQuota(turn)
	}
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

func (s *agentSession) waitForWorkerQuota(turn *childTurnState) {
	parent := s.parent.(*agentSession)
	parentPermit, err := parent.childTurns.acquire(turn.ctx)
	if err == nil {
		var globalPermit ChildTurnPermit
		globalPermit, err = s.user.AcquireWorkerQuota(turn.ctx)
		if err == nil {
			var once sync.Once
			turn.releaseQuota = func() { once.Do(func() { parentPermit.Release(); globalPermit.Release() }) }
		} else {
			parentPermit.Release()
		}
	}
	if turn.ctx.Err() != nil {
		if turn.releaseQuota != nil {
			turn.releaseQuota()
		}
		s.finishChild(turn, "", context.Canceled)
		return
	}
	if err != nil {
		s.finishChild(turn, "", err)
		return
	}
	s.mu.Lock()
	if turn.ctx.Err() != nil {
		s.mu.Unlock()
		s.finishChild(turn, "", context.Canceled)
		return
	}
	s.childStatus.State = StatusRunning
	s.childStatus.StartedAt = time.Now().UTC()
	s.mu.Unlock()
	s.runChild(turn)
}

func (s *agentSession) runChild(turn *childTurnState) {
	s.mu.Lock()
	task := cloneStatus(s.childStatus)
	request := s.childRequest
	s.mu.Unlock()
	s.emitChild(ChildLifecycleEvent{Kind: ChildLifecycleWorking, Task: task})
	stop := func() {}
	if s.observeChildProgress != nil {
		stop = s.observeChildProgress(s.ID(), s.reportChildProgress)
	}
	var output string
	var runErr error
	if turn.drain == nil {
		output, runErr = s.sendAndWait(turn.ctx, request.Prompt)
	} else {
		output, runErr = s.awaitMessage(turn.ctx, turn.drain, turn.messageID)
	}
	stop()
	s.finishChild(turn, output, runErr)
}

func (s *agentSession) finishChild(turn *childTurnState, output string, runErr error) {
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
	s.childStatus.State, s.childStatus.Output, s.childStatus.Error = status, output, errText
	s.childStatus.Truncated = outputTruncated || errorTruncated
	s.childStatus.FinishedAt = time.Now().UTC()
	result := cloneStatus(s.childStatus)
	turn.result = result
	s.mu.Unlock()
	if turn.releaseQuota != nil {
		turn.releaseQuota()
	}
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

func (s *agentSession) sendManagedTurn(ctx context.Context, messageID, content, measuredContent string) (session.Admission, error) {
	if strings.TrimSpace(measuredContent) == "" {
		return session.Admission{}, ErrInvalidChildRequest
	}
	if len(measuredContent) > s.maxChildPromptBytes {
		return session.Admission{}, ErrChildRequestLimit
	}
retry:
	s.childOp.Lock()
	s.mu.Lock()
	if s.childTurn == nil {
		s.mu.Unlock()
		s.childOp.Unlock()
		return session.Admission{}, ErrChildNotFound
	}
	if childTurnActive(s.childStatus.State) {
		if s.drain == nil {
			turn := s.childTurn
			s.mu.Unlock()
			s.childOp.Unlock()
			select {
			case <-turn.done:
				goto retry
			case <-ctx.Done():
				return session.Admission{}, ctx.Err()
			}
		}
		admission, err := s.admitLocked(ctx, messageID, content)
		if err == nil && admission.Created {
			s.drain.wake = true
		}
		s.mu.Unlock()
		s.childOp.Unlock()
		return admission, err
	}
	s.mu.Unlock()
	defer s.childOp.Unlock()
	releaseQuota, err := s.tryAcquireWorkerQuota()
	if err != nil {
		return session.Admission{}, err
	}
	s.mu.Lock()
	if s.removed {
		s.mu.Unlock()
		releaseQuota()
		return session.Admission{}, ErrAgentSessionRemoved
	}
	if s.shuttingDown {
		s.mu.Unlock()
		releaseQuota()
		return session.Admission{}, ErrUserSessionClosed
	}
	admission, err := s.admitLocked(ctx, messageID, content)
	if err != nil {
		s.mu.Unlock()
		releaseQuota()
		return session.Admission{}, err
	}
	if !admission.Created {
		s.mu.Unlock()
		releaseQuota()
		return admission, nil
	}
	s.childStatus.Turn++
	s.childStatus.State, s.childStatus.StartedAt, s.childStatus.FinishedAt = StatusRunning, time.Now().UTC(), time.Time{}
	s.childStatus.Output, s.childStatus.Error, s.childStatus.Truncated = "", "", false
	s.childStatus.Usage, s.childStatus.ToolUses = ChildUsage{}, 0
	s.childRequest = ChildRequest{Prompt: content, Agent: s.childStatus.Agent, Model: s.childStatus.Model, Name: s.childStatus.Name}
	turnCtx, cancel := context.WithCancel(context.Background())
	turn := &childTurnState{ctx: turnCtx, cancel: cancel, done: make(chan struct{}), releaseQuota: releaseQuota, messageID: admission.Input.MessageID}
	s.childTurn, s.cancelChild = turn, cancel
	turn.drain = s.startOrJoinLocked(true)
	s.mu.Unlock()
	go s.runChild(turn)
	return admission, nil
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
	turn := t.child.childTurn
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
	current := t.child.childTurn == t.turn
	t.child.mu.Unlock()
	if !current {
		return t.Snapshot(), nil
	}
	return managedChildTask{t.child}.Interrupt(ctx)
}

func (s *agentSession) statusSnapshot() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.childTurn == nil {
		return Status{}
	}
	return cloneStatus(s.childStatus)
}

func (s *agentSession) reportChildProgress(progress ChildProgress) {
	if progress.ToolUses < 0 || progress.Usage.InputTokens < 0 || progress.Usage.OutputTokens < 0 || progress.Usage.TotalTokens < 0 || progress.Usage.ReasoningTokens < 0 || progress.Usage.CachedInputTokens < 0 {
		return
	}
	s.mu.Lock()
	if s.childTurn == nil || s.childStatus.State != StatusRunning {
		s.mu.Unlock()
		return
	}
	s.childStatus.Usage.InputTokens += progress.Usage.InputTokens
	s.childStatus.Usage.OutputTokens += progress.Usage.OutputTokens
	s.childStatus.Usage.TotalTokens += progress.Usage.TotalTokens
	s.childStatus.Usage.ReasoningTokens += progress.Usage.ReasoningTokens
	s.childStatus.Usage.CachedInputTokens += progress.Usage.CachedInputTokens
	s.childStatus.ToolUses += progress.ToolUses
	task := cloneStatus(s.childStatus)
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
