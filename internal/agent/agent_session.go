package agent

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/amirulashraf/parrot-coder/internal/id"
	"github.com/amirulashraf/parrot-coder/internal/session"
	"github.com/amirulashraf/parrot-coder/internal/tool"
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
	Send(context.Context, string) (string, error)
	SendAgentMessage(context.Context, tool.AgentMessage) (string, error)
	Wake()
	Resume(context.Context) error
	Interrupt(context.Context) error
	Details(context.Context) (session.AgentSessionDto, error)
	UpdateSelection(context.Context, session.SelectionPatch, session.SelectionValidator) (session.AgentSessionDto, error)
	ListMessages(context.Context) ([]session.Message, error)
	Admit(context.Context, session.AdmitParams) (session.Admission, error)
	HasPendingInputs(context.Context) (bool, error)
	LatestSequence(context.Context) (int64, error)
	CurrentContextEpoch(context.Context) (session.ContextEpoch, error)
	InitializeContext(context.Context, string, json.RawMessage, int64) (session.ContextEpoch, error)
	ReconcileContext(context.Context, string, json.RawMessage) error
	ReplaceContext(context.Context, string, json.RawMessage, int64) (session.ContextEpoch, error)
}

func (s *agentSession) ID() string           { return s.dto.ID }
func (s *agentSession) Name() string         { return s.dto.Name }
func (s *agentSession) Parent() AgentSession { return s.parent }

func (s *agentSession) Details(ctx context.Context) (session.AgentSessionDto, error) {
	return s.store.Get(ctx)
}

func (s *agentSession) UpdateSelection(ctx context.Context, patch session.SelectionPatch, validate session.SelectionValidator) (session.AgentSessionDto, error) {
	return s.store.UpdateSelection(ctx, patch, validate)
}

func (s *agentSession) ListMessages(ctx context.Context) ([]session.Message, error) {
	return s.store.ListMessages(ctx)
}
func (s *agentSession) Admit(ctx context.Context, params session.AdmitParams) (session.Admission, error) {
	return s.store.Admit(ctx, params)
}
func (s *agentSession) HasPendingInputs(ctx context.Context) (bool, error) {
	return s.store.HasPendingInputs(ctx)
}
func (s *agentSession) LatestSequence(ctx context.Context) (int64, error) {
	return s.store.LatestSequence(ctx)
}
func (s *agentSession) CurrentContextEpoch(ctx context.Context) (session.ContextEpoch, error) {
	return s.store.CurrentContextEpoch(ctx)
}
func (s *agentSession) InitializeContext(ctx context.Context, baseline string, sources json.RawMessage, cutoff int64) (session.ContextEpoch, error) {
	return s.store.InitializeContext(ctx, baseline, sources, cutoff)
}
func (s *agentSession) ReconcileContext(ctx context.Context, text string, sources json.RawMessage) error {
	return s.store.ReconcileContext(ctx, text, sources)
}
func (s *agentSession) ReplaceContext(ctx context.Context, baseline string, sources json.RawMessage, cutoff int64) (session.ContextEpoch, error) {
	return s.store.ReplaceContext(ctx, baseline, sources, cutoff)
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
	s.mu.Unlock()

	status := Status{SessionID: s.dto.ID, RootSession: s.dto.ID, Agent: s.dto.Agent, Model: s.dto.Model, Name: s.dto.Name, State: state}
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
	if s.user == nil || s.parent == nil {
		return nil, ErrChildNotFound
	}
	return s.user.observeChild(s.parent.ID(), s.dto.ID)
}

func (s *agentSession) ResolveChild(identifier string) (AgentSession, error) {
	if s.user == nil {
		return nil, ErrChildNotFound
	}
	return s.user.resolveChild(s.ID(), identifier)
}

// Prompt admits input, runs the session to idle, and returns the assistant
// message produced by that execution lifecycle.
func (s *agentSession) Prompt(ctx context.Context, content string) (string, error) {
	messages, err := s.store.ListMessages(ctx)
	if err != nil {
		return "", err
	}
	var cutoff int64
	for _, message := range messages {
		cutoff = max(cutoff, message.Sequence)
	}
	_, state, err := s.admitAndStart(ctx, content)
	if err != nil {
		return "", err
	}
	if err := s.wait(ctx, state); err != nil {
		if ctx.Err() != nil {
			cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = s.interruptExecution(cleanup)
			cancel()
		}
		return "", err
	}
	messages, err = s.store.ListMessages(ctx)
	if err != nil {
		return "", err
	}
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != "assistant" || messages[i].Sequence <= cutoff {
			continue
		}
		if messages[i].Error != "" {
			return messages[i].Content, errors.New(messages[i].Error)
		}
		return messages[i].Content, nil
	}
	return "", errors.New("agent: session produced no assistant output")
}

// Send admits steer input and wakes the session without waiting for it to idle.
func (s *agentSession) Send(ctx context.Context, content string) (string, error) {
	if s.user != nil && s.parent != nil {
		return s.user.sendManagedTurn(ctx, s, content, content)
	}
	messageID, _, err := s.admitAndStart(ctx, content)
	return messageID, err
}

func (s *agentSession) SendAgentMessage(ctx context.Context, message tool.AgentMessage) (string, error) {
	content := message.String()
	if s.user != nil && s.parent != nil {
		return s.user.sendManagedTurn(ctx, s, content, message.Content)
	}
	messageID, _, err := s.admitAndStart(ctx, content)
	return messageID, err
}

func (s *agentSession) admit(ctx context.Context, content string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.admitLocked(ctx, content)
}

func (s *agentSession) admitAndStart(ctx context.Context, content string) (string, *drainState, error) {
	s.mu.Lock()
	messageID, err := s.admitLocked(ctx, content)
	if err != nil {
		s.mu.Unlock()
		return "", nil, err
	}
	state := s.startOrJoinLocked(true)
	s.mu.Unlock()
	return messageID, state, nil
}

func (s *agentSession) admitLocked(ctx context.Context, content string) (string, error) {
	if s.removed {
		return "", ErrAgentSessionRemoved
	}
	messageID, err := id.New("msg")
	if err != nil {
		return "", err
	}
	if _, err := s.store.Admit(ctx, session.AdmitParams{MessageID: messageID, Content: content, Delivery: session.DeliverySteer}); err != nil {
		return "", err
	}
	return messageID, nil
}

// Wake coalesces with an active drain and returns immediately.
func (s *agentSession) Wake() {
	s.mu.Lock()
	if !s.removed {
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
			prepared, err = s.prepareContinuation(ctx)
			if err == nil && prepared {
				continue
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
