package agent

import (
	"context"
	"errors"
	"time"

	"github.com/amirulashraf/parrot-coder/internal/id"
	"github.com/amirulashraf/parrot-coder/internal/protocol"
	"github.com/amirulashraf/parrot-coder/internal/session"
)

type LifecycleObserver interface {
	LifecycleComplete(sessionID string, err error)
}

type LifecycleStartObserver interface {
	LifecycleStarted(sessionID string)
}

type AgentStatus string

const (
	StatusIdle         AgentStatus = "idle"
	StatusPending      AgentStatus = "pending"
	StatusBlocked      AgentStatus = "blocked"
	StatusRunning      AgentStatus = "running"
	StatusInterrupting AgentStatus = "interrupting"
	StatusSucceeded    AgentStatus = "succeeded"
	StatusFailed       AgentStatus = "failed"
	StatusCanceled     AgentStatus = "canceled"
)

type Active struct {
	SessionID string
	Status    AgentStatus
}

type turnState struct {
	ctx          context.Context
	cancel       context.CancelFunc
	done         chan struct{}
	wake         bool
	status       AgentStatus
	err          error
	messageID    string
	result       Status
	releaseQuota func()
}

type turnPolicy interface {
	Validate(string) error
	TryAcquire() (func(), bool, error)
	Acquire(context.Context) (func(), error)
	CapturesOutput() bool
	Started(Status)
	Working(Status, func(ChildProgress)) func()
	Finished(Status, string, error, bool) (string, error, bool)
	Published(Status)
	Completed(Status)
}

type ordinaryTurnPolicy struct{}

func (ordinaryTurnPolicy) Validate(string) error                      { return nil }
func (ordinaryTurnPolicy) TryAcquire() (func(), bool, error)          { return nil, false, nil }
func (ordinaryTurnPolicy) Acquire(context.Context) (func(), error)    { return nil, nil }
func (ordinaryTurnPolicy) CapturesOutput() bool                       { return false }
func (ordinaryTurnPolicy) Started(Status)                             {}
func (ordinaryTurnPolicy) Working(Status, func(ChildProgress)) func() { return func() {} }
func (ordinaryTurnPolicy) Finished(_ Status, output string, err error, _ bool) (string, error, bool) {
	return output, err, false
}
func (ordinaryTurnPolicy) Published(Status) {}
func (ordinaryTurnPolicy) Completed(Status) {}

// AgentSession is the runtime for one persisted agent session. It owns both
// execution and the synchronization which ensures only one execution lifecycle
// is active for its bound session ID.
type AgentSession interface {
	ID() string
	Name() string
	Parent() AgentSession
	CreateChild(context.Context, ChildRequest) (AgentSession, error)
	Status() Status
	Observe() (ChildTurnObserver, error)
	ResolveChild(string) (AgentSession, error)
	Prompt(context.Context, string) (string, error)
	Send(context.Context, string, string) (session.Admission, error)
	Wake()
	Resume(context.Context) error
	Interrupt(context.Context) error
	Shutdown(context.Context) error
	Details(context.Context) (session.AgentSessionDto, error)
	UpdateSelection(context.Context, session.SelectionPatch, session.SelectionValidator) (session.AgentSessionDto, error)
	ListMessages(context.Context) ([]session.Message, error)
	HasPendingInputs(context.Context) (bool, error)
	LatestSequence(context.Context) (int64, error)
}

func (s *agentSession) ID() string           { return s.dto.ID }
func (s *agentSession) Name() string         { return s.dto.Name }
func (s *agentSession) Parent() AgentSession { return s.parent }

func (s *agentSession) Details(ctx context.Context) (session.AgentSessionDto, error) {
	return s.store.Get(ctx)
}

func (s *agentSession) UpdateSelection(ctx context.Context, patch session.SelectionPatch, validate session.SelectionValidator) (session.AgentSessionDto, error) {
	s.selectionMu.Lock()
	defer s.selectionMu.Unlock()
	updated, err := s.store.UpdateSelection(ctx, patch, validate)
	if err != nil {
		if reconciled, getErr := s.store.Get(ctx); getErr == nil {
			s.applySelection(reconciled)
		}
		return session.AgentSessionDto{}, err
	}
	s.applySelection(updated)
	return updated, nil
}

func (s *agentSession) applySelection(updated session.AgentSessionDto) {
	s.mu.Lock()
	s.dto.Agent = updated.Agent
	s.dto.Provider = updated.Provider
	s.dto.Model = updated.Model
	s.dto.Variant = updated.Variant
	s.status.Agent = updated.Agent
	s.status.Provider = updated.Provider
	s.status.Model = updated.Model
	s.status.Variant = updated.Variant
	s.mu.Unlock()
	if s.agentSessionRepository != nil {
		s.agentSessionRepository.updateSelection(s, updated)
	}
}

func (s *agentSession) ListMessages(ctx context.Context) ([]session.Message, error) {
	return s.store.ListMessages(ctx)
}
func (s *agentSession) HasPendingInputs(ctx context.Context) (bool, error) {
	return s.store.HasPendingInputs(ctx)
}
func (s *agentSession) LatestSequence(ctx context.Context) (int64, error) {
	return s.store.LatestSequence(ctx)
}
func (s *agentSession) Status() Status {
	s.mu.Lock()
	status := cloneStatus(s.status)
	if s.turn != nil && turnActive(s.turn.status) {
		status.State = s.turn.status
	}
	s.mu.Unlock()
	return status
}

func (s *agentSession) executionStatus() AgentStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.turn != nil && turnActive(s.turn.status) {
		return s.turn.status
	}
	return StatusIdle
}

func (s *agentSession) Observe() (ChildTurnObserver, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.turn == nil {
		return nil, ErrChildNotFound
	}
	return turnObserver{turn: s.turn}, nil
}

func (s *agentSession) ResolveChild(identifier string) (AgentSession, error) {
	if identifier == "" || s.agentSessionRepository == nil {
		return nil, ErrChildNotFound
	}
	pending := s.agentSessionRepository.ChildSessions(s.ID())
	for len(pending) > 0 {
		relation := pending[0]
		pending = pending[1:]
		child, ok := s.agentSessionRepository.Lookup(relation.SessionID)
		if !ok {
			continue
		}
		if relation.SessionID == identifier || child.Name() == identifier {
			return child, nil
		}
		pending = append(pending, s.agentSessionRepository.ChildSessions(relation.SessionID)...)
	}
	return nil, ErrChildNotFound
}

// Prompt sends input with a generated message ID and waits for that input's
// terminal assistant response.
func (s *agentSession) Prompt(ctx context.Context, content string) (string, error) {
	return s.sendAndWait(ctx, content)
}

// Send admits steer input with the caller's idempotency key and wakes the
// session without waiting for it to idle.
func (s *agentSession) Send(ctx context.Context, messageID, content string) (session.Admission, error) {
	return s.send(ctx, messageID, content, content)
}

func (s *agentSession) send(ctx context.Context, messageID, content, measuredContent string) (session.Admission, error) {
	admission, _, err := s.admitAndStart(ctx, messageID, content, measuredContent, false, true)
	return admission, err
}

func (s *agentSession) sendAndWait(ctx context.Context, content string) (string, error) {
	messageID, err := id.New("msg")
	if err != nil {
		return "", err
	}
	_, state, err := s.admitAndStart(ctx, messageID, content, content, false, true)
	if err != nil {
		return "", err
	}
	return s.awaitMessage(ctx, state, messageID)
}

func (s *agentSession) awaitMessage(ctx context.Context, state *turnState, messageID string) (string, error) {
	if err := s.wait(ctx, state); err != nil {
		if ctx.Err() != nil {
			cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = s.interruptExecution(cleanup)
			cancel()
		}
		return "", err
	}
	return s.messageResponse(ctx, messageID)
}

func (s *agentSession) messageResponse(ctx context.Context, messageID string) (string, error) {
	messages, err := s.store.ListMessages(ctx)
	if err != nil {
		return "", err
	}
	found := false
	for _, message := range messages {
		if !found {
			found = message.Role == "user" && message.ID == messageID
			continue
		}
		if message.Role == "user" {
			break
		}
		if message.Role != "assistant" || message.FinishReason == string(protocol.FinishToolCalls) {
			continue
		}
		if message.Error != "" {
			return message.Content, errors.New(message.Error)
		}
		return message.Content, nil
	}
	return "", errors.New("agent: session produced no assistant output")
}

func (s *agentSession) effectiveTurnPolicy() turnPolicy {
	if s.turnPolicy == nil {
		return ordinaryTurnPolicy{}
	}
	return s.turnPolicy
}

func (s *agentSession) admitAndStart(ctx context.Context, messageID, content, measuredContent string, waitForPermit, start bool) (session.Admission, *turnState, error) {
	policy := s.effectiveTurnPolicy()
	if err := policy.Validate(measuredContent); err != nil {
		return session.Admission{}, nil, err
	}
retry:
	s.mu.Lock()
	if s.removed {
		s.mu.Unlock()
		return session.Admission{}, nil, ErrAgentSessionRemoved
	}
	if s.shuttingDown {
		s.mu.Unlock()
		return session.Admission{}, nil, ErrUserSessionClosed
	}
	if s.turn != nil && (s.turn.status == StatusBlocked || s.turn.status == StatusInterrupting) {
		turn := s.turn
		s.mu.Unlock()
		select {
		case <-turn.done:
			goto retry
		case <-ctx.Done():
			return session.Admission{}, nil, ctx.Err()
		}
	}
	if s.turn != nil && turnActive(s.turn.status) {
		admission, err := s.admitLocked(ctx, messageID, content)
		if err == nil && admission.Created {
			s.turn.wake = true
		}
		state := s.turn
		s.mu.Unlock()
		return admission, state, err
	}
	s.mu.Unlock()

	release, blocked, err := policy.TryAcquire()
	if err != nil || blocked && !waitForPermit {
		if blocked {
			err = ErrChildConcurrency
		}
		return session.Admission{}, nil, err
	}
	s.mu.Lock()
	if s.turn != nil && turnActive(s.turn.status) {
		s.mu.Unlock()
		if release != nil {
			release()
		}
		goto retry
	}
	admission, err := s.admitLocked(ctx, messageID, content)
	if err != nil || !admission.Created {
		state := s.turn
		s.mu.Unlock()
		if release != nil {
			release()
		}
		return admission, state, err
	}
	state := s.newTurnLocked(admission.Input.MessageID, release, blocked)
	s.mu.Unlock()
	if start {
		s.startTurn(state)
	}
	return admission, state, nil
}

func (s *agentSession) startTurn(state *turnState) {
	s.mu.Lock()
	status := state.status
	task := cloneStatus(s.status)
	s.mu.Unlock()
	if status == StatusInterrupting {
		go s.finishTurn(state, "", context.Canceled)
		return
	}
	s.effectiveTurnPolicy().Started(task)
	if status == StatusBlocked {
		go s.waitForTurnPermit(state)
	} else {
		go s.runTurn(state)
	}
}

func (s *agentSession) abortTurn(state *turnState) {
	state.cancel()
	s.mu.Lock()
	if state.releaseQuota != nil {
		state.releaseQuota()
		state.releaseQuota = nil
	}
	state.err = context.Canceled
	if s.turn == state {
		s.turn = nil
	}
	close(state.done)
	s.mu.Unlock()
}

func (s *agentSession) admitLocked(ctx context.Context, messageID, content string) (session.Admission, error) {
	if s.removed {
		return session.Admission{}, ErrAgentSessionRemoved
	}
	if s.shuttingDown {
		return session.Admission{}, ErrUserSessionClosed
	}
	return s.store.Admit(ctx, session.AdmitParams{MessageID: messageID, Content: content, Delivery: session.DeliverySteer})
}

func (s *agentSession) startEmptyTurn(wake bool) (*turnState, error) {
retry:
	s.mu.Lock()
	if s.removed {
		s.mu.Unlock()
		return nil, ErrAgentSessionRemoved
	}
	if s.shuttingDown {
		s.mu.Unlock()
		return nil, ErrUserSessionClosed
	}
	if s.turn != nil && turnActive(s.turn.status) {
		if wake {
			s.turn.wake = true
		}
		state := s.turn
		s.mu.Unlock()
		return state, nil
	}
	s.mu.Unlock()

	release, blocked, err := s.effectiveTurnPolicy().TryAcquire()
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.removed || s.shuttingDown || s.turn != nil && turnActive(s.turn.status) {
		s.mu.Unlock()
		if release != nil {
			release()
		}
		goto retry
	}
	state := s.newTurnLocked("", release, blocked)
	s.mu.Unlock()
	s.startTurn(state)
	return state, nil
}

// Wake coalesces with an active turn and returns immediately.
func (s *agentSession) Wake() {
	_, _ = s.startEmptyTurn(true)
}

// Resume starts an idle turn or joins the complete lifetime of an active one.
func (s *agentSession) Resume(ctx context.Context) error {
	state, err := s.startEmptyTurn(false)
	if err != nil {
		return err
	}
	return s.wait(ctx, state)
}

func (s *agentSession) wait(ctx context.Context, state *turnState) error {
	if state == nil {
		return nil
	}
	select {
	case <-state.done:
		return state.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *agentSession) Interrupt(ctx context.Context) error {
	if err := s.interruptExecution(ctx); ctx.Err() != nil {
		return err
	}
	return nil
}

func (s *agentSession) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.shuttingDown = true
	state := s.turn
	if state == nil || !turnActive(state.status) {
		s.mu.Unlock()
		return nil
	}
	state.status = StatusInterrupting
	state.wake = false
	state.cancel()
	s.mu.Unlock()
	if err := s.wait(ctx, state); ctx.Err() != nil {
		return err
	}
	return nil
}

func (s *agentSession) interruptExecution(ctx context.Context) error {
	s.mu.Lock()
	state := s.turn
	if state == nil || !turnActive(state.status) {
		s.mu.Unlock()
		return nil
	}
	state.status = StatusInterrupting
	state.wake = false
	state.cancel()
	s.mu.Unlock()
	return s.wait(ctx, state)
}

func (s *agentSession) newTurnLocked(messageID string, release func(), blocked bool) *turnState {
	ctx, cancel := context.WithCancel(context.Background())
	status := StatusRunning
	if blocked {
		status = StatusBlocked
	}
	state := &turnState{ctx: ctx, cancel: cancel, done: make(chan struct{}), status: status, messageID: messageID, releaseQuota: release}
	s.turn = state
	s.status.Turn++
	s.status.State, s.status.FinishedAt = status, time.Time{}
	if !blocked {
		s.status.StartedAt = time.Now().UTC()
	} else {
		s.status.StartedAt = time.Time{}
	}
	s.status.Output, s.status.Error, s.status.Truncated = "", "", false
	s.status.Usage, s.status.ToolUses = ChildUsage{}, 0
	return state
}

func (s *agentSession) reserveChildCreation() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.removed {
		return ErrAgentSessionRemoved
	}
	if s.shuttingDown {
		return ErrUserSessionClosed
	}
	s.childCreations++
	return nil
}

func (s *agentSession) releaseChildCreation() {
	s.mu.Lock()
	s.childCreations--
	s.mu.Unlock()
}

func (s *agentSession) removeIfIdle(remove func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.removed {
		return nil
	}
	if s.turn != nil && turnActive(s.turn.status) || s.childCreations != 0 {
		return ErrAgentSessionActive
	}
	if err := remove(); err != nil {
		return err
	}
	s.removed = true
	return nil
}

func (s *agentSession) waitForTurnPermit(state *turnState) {
	release, err := s.effectiveTurnPolicy().Acquire(state.ctx)
	if err != nil {
		s.finishTurn(state, "", err)
		return
	}
	s.mu.Lock()
	if state.ctx.Err() != nil || s.turn != state {
		s.mu.Unlock()
		if release != nil {
			release()
		}
		s.finishTurn(state, "", context.Canceled)
		return
	}
	state.releaseQuota = release
	state.status = StatusRunning
	s.status.State, s.status.StartedAt = StatusRunning, time.Now().UTC()
	s.mu.Unlock()
	go s.runTurn(state)
}

func (s *agentSession) runTurn(state *turnState) {
	s.mu.Lock()
	canceled := state.ctx.Err() != nil || state.status == StatusInterrupting
	s.mu.Unlock()
	if canceled {
		s.finishTurn(state, "", context.Canceled)
		return
	}
	s.started()
	policy := s.effectiveTurnPolicy()
	stop := policy.Working(s.Status(), func(progress ChildProgress) { s.reportTurnProgress(state, progress) })
	var err error
	for {
		if s.execute != nil {
			err = s.execute(state.ctx)
		} else {
			err = s.drainOnce(state.ctx)
		}
		if err == nil && state.ctx.Err() == nil {
			var prepared bool
			prepared, err = s.prepareQueueNotification(state.ctx)
			if err == nil && prepared {
				continue
			}
			if err == nil {
				prepared, err = s.prepareContinuation(state.ctx)
				if err == nil && prepared {
					continue
				}
			}
		}

		s.mu.Lock()
		if state.wake && state.ctx.Err() == nil {
			state.wake = false
			s.mu.Unlock()
			continue
		}
		s.mu.Unlock()
		break
	}
	stop()
	output := ""
	if policy.CapturesOutput() && state.messageID != "" && err == nil {
		output, err = s.messageResponse(context.Background(), state.messageID)
	}
	s.finishTurn(state, output, err)
}

func (s *agentSession) finishTurn(state *turnState, output string, runErr error) {
	canceled := state.ctx.Err() != nil
	state.cancel()
	policy := s.effectiveTurnPolicy()
	output, runErr, truncated := policy.Finished(s.Status(), output, runErr, canceled)
	s.mu.Lock()
	s.status.Truncated = truncated
	state.err = runErr
	status, errText := StatusSucceeded, ""
	if runErr != nil {
		status, errText = StatusFailed, runErr.Error()
	}
	if state.status == StatusInterrupting && (errors.Is(runErr, context.Canceled) || errors.Is(runErr, ErrChildCanceled)) {
		status = StatusCanceled
	}
	s.status.State, s.status.Output, s.status.Error = status, output, errText
	s.status.FinishedAt = time.Now().UTC()
	state.result = cloneStatus(s.status)
	state.status = StatusInterrupting
	if state.releaseQuota != nil {
		state.releaseQuota()
		state.releaseQuota = nil
	}
	s.mu.Unlock()
	policy.Published(state.result)
	s.mu.Lock()
	state.status = status
	close(state.done)
	s.mu.Unlock()
	policy.Completed(state.result)
	s.completed(runErr)
}

func (s *agentSession) started() {
	for _, observer := range s.observers {
		if starter, ok := observer.(LifecycleStartObserver); ok {
			starter.LifecycleStarted(s.dto.ID)
		}
	}
}

func (s *agentSession) completed(err error) {
	for _, observer := range s.observers {
		if observer != nil {
			observer.LifecycleComplete(s.dto.ID, err)
		}
	}
}
