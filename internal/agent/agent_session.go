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

type drainState struct {
	done   chan struct{}
	cancel context.CancelFunc
	wake   bool
	status AgentStatus
	err    error
}

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
	if s.child != nil {
		s.child.status.Agent = updated.Agent
		s.child.status.Provider = updated.Provider
		s.child.status.Model = updated.Model
		s.child.status.Variant = updated.Variant
	}
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
	if s.child != nil {
		status := cloneStatus(s.child.status)
		s.mu.Unlock()
		return status
	}
	state := StatusIdle
	if s.drain != nil {
		state = s.drain.status
	}
	status := Status{SessionID: s.dto.ID, RootSession: s.dto.ID, Agent: s.dto.Agent, Provider: s.dto.Provider, Model: s.dto.Model, Variant: s.dto.Variant, Name: s.dto.Name, State: state}
	s.mu.Unlock()

	if s.parent != nil {
		parent := s.parent.Status()
		status.ParentSession = parent.SessionID
		status.RootSession = parent.RootSession
		status.Lineage = append(parent.Lineage, parent.Agent)
		status.Depth = parent.Depth + 1
	}
	return status
}

func (s *agentSession) executionStatus() AgentStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.drain != nil {
		return s.drain.status
	}
	return StatusIdle
}

func (s *agentSession) Observe() (ChildTurnObserver, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.child == nil {
		return nil, ErrChildNotFound
	}
	return childObserver{turn: s.child.turn}, nil
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
	if s.user != nil && s.parent != nil {
		return s.sendManagedTurn(ctx, messageID, content, measuredContent)
	}
	admission, _, err := s.admitAndStart(ctx, messageID, content)
	return admission, err
}

func (s *agentSession) sendAndWait(ctx context.Context, content string) (string, error) {
	messageID, err := id.New("msg")
	if err != nil {
		return "", err
	}
	_, state, err := s.admitAndStart(ctx, messageID, content)
	if err != nil {
		return "", err
	}
	return s.awaitMessage(ctx, state, messageID)
}

func (s *agentSession) awaitMessage(ctx context.Context, state *drainState, messageID string) (string, error) {
	if err := s.wait(ctx, state); err != nil {
		if ctx.Err() != nil {
			cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = s.interruptExecution(cleanup)
			cancel()
		}
		return "", err
	}
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

func (s *agentSession) admitAndStart(ctx context.Context, messageID, content string) (session.Admission, *drainState, error) {
	s.mu.Lock()
	admission, err := s.admitLocked(ctx, messageID, content)
	if err != nil {
		s.mu.Unlock()
		return session.Admission{}, nil, err
	}
	if !admission.Created {
		s.mu.Unlock()
		return admission, nil, nil
	}
	state := s.startOrJoinLocked(true)
	s.mu.Unlock()
	return admission, state, nil
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

// Wake coalesces with an active drain and returns immediately.
func (s *agentSession) Wake() {
	s.mu.Lock()
	if !s.removed && !s.shuttingDown {
		s.startOrJoinLocked(true)
	}
	s.mu.Unlock()
}

// Resume starts an idle drain or joins the complete lifetime of an active one.
func (s *agentSession) Resume(ctx context.Context) error {
	s.mu.Lock()
	if s.removed {
		s.mu.Unlock()
		return ErrAgentSessionRemoved
	}
	if s.shuttingDown {
		s.mu.Unlock()
		return ErrUserSessionClosed
	}
	state := s.startOrJoinLocked(false)
	s.mu.Unlock()
	return s.wait(ctx, state)
}

func (s *agentSession) wait(ctx context.Context, state *drainState) error {
	select {
	case <-state.done:
		s.mu.Lock()
		err := state.err
		s.mu.Unlock()
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *agentSession) Interrupt(ctx context.Context) error {
	s.mu.Lock()
	if s.child != nil && (s.child.status.State == StatusRunning || s.child.status.State == StatusPending) {
		s.child.cancel()
		done := s.child.turn.done
		s.mu.Unlock()
		select {
		case <-done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	s.mu.Unlock()
	return s.interruptExecution(ctx)
}

func (s *agentSession) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.shuttingDown = true
	var childDone <-chan struct{}
	if s.child != nil && s.child.turn != nil {
		childDone = s.child.turn.done
		if s.child.status.State == StatusRunning || s.child.status.State == StatusPending {
			s.child.cancel()
		}
	}
	var drainDone <-chan struct{}
	if s.drain != nil {
		s.drain.status = StatusInterrupting
		s.drain.wake = false
		s.drain.cancel()
		drainDone = s.drain.done
	}
	s.mu.Unlock()

	for _, done := range []<-chan struct{}{childDone, drainDone} {
		if done == nil {
			continue
		}
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (s *agentSession) interruptExecution(ctx context.Context) error {
	s.mu.Lock()
	state := s.drain
	if state == nil {
		s.mu.Unlock()
		return nil
	}
	state.status = StatusInterrupting
	state.wake = false
	state.cancel()
	done := state.done
	s.mu.Unlock()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *agentSession) startOrJoinLocked(requestWake bool) *drainState {
	if s.drain != nil {
		if requestWake {
			s.drain.wake = true
		}
		return s.drain
	}
	ctx, cancel := context.WithCancel(context.Background())
	state := &drainState{done: make(chan struct{}), cancel: cancel, status: StatusRunning}
	s.drain = state
	go func() {
		s.started()
		s.run(ctx, state)
	}()
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
	if s.drain != nil || s.childCreations != 0 || s.child != nil && (s.child.status.State == StatusRunning || s.child.status.State == StatusPending) {
		return ErrAgentSessionActive
	}
	if err := remove(); err != nil {
		return err
	}
	s.removed = true
	return nil
}

func (s *agentSession) run(ctx context.Context, state *drainState) {
	for {
		var err error
		if s.execute != nil {
			err = s.execute(ctx)
		} else {
			err = s.drainOnce(ctx)
		}
		if err == nil && ctx.Err() == nil {
			var prepared bool
			prepared, err = s.prepareQueueNotification(ctx)
			if err == nil && prepared {
				continue
			}
			if err == nil {
				prepared, err = s.prepareContinuation(ctx)
				if err == nil && prepared {
					continue
				}
			}
		}

		s.mu.Lock()
		state.err = err
		if state.wake && ctx.Err() == nil {
			state.wake = false
			s.mu.Unlock()
			continue
		}
		restart := state.wake
		if restart {
			nextCtx, cancel := context.WithCancel(context.Background())
			next := &drainState{done: make(chan struct{}), cancel: cancel, status: StatusRunning}
			s.drain = next
			close(state.done)
			s.mu.Unlock()
			s.started()
			go s.run(nextCtx, next)
			return
		}
		if s.drain == state {
			s.drain = nil
		}
		close(state.done)
		s.mu.Unlock()
		s.completed(err)
		return
	}
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
