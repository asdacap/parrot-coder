package agent

import (
	"context"
	"errors"
	"time"

	"github.com/amirulashraf/parrot-coder/internal/id"
	"github.com/amirulashraf/parrot-coder/internal/session"
)

type LifecycleObserver interface {
	LifecycleComplete(sessionID string, err error)
}

type LifecycleStartObserver interface {
	LifecycleStarted(sessionID string)
}

type Status string

const (
	StatusIdle         Status = "idle"
	StatusRunning      Status = "running"
	StatusInterrupting Status = "interrupting"
)

type Active struct {
	SessionID string
	Status    Status
}

type drainState struct {
	done   chan struct{}
	cancel context.CancelFunc
	wake   bool
	status Status
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
	ChildTask() (ChildTask, bool)
	Observe() (ChildTurnObserver, error)
	ResolveChild(string) (AgentSession, error)
	SendChild(context.Context, ChildRequest) (ChildTask, string, error)
	InterruptChild(context.Context) (ChildTask, error)
	Forget() error
	Prompt(context.Context, string) (string, error)
	Send(context.Context, string) (string, error)
	Wake()
	Resume(context.Context) error
	Interrupt(context.Context) error
	Status() Status
}

func (s *agentSession) ID() string           { return s.dto.ID }
func (s *agentSession) Name() string         { return s.dto.Name }
func (s *agentSession) Parent() AgentSession { return s.parent }
func (s *agentSession) CreateChild(ctx context.Context, request ChildRequest) (AgentSession, error) {
	if s.user == nil {
		return nil, ErrChildNotFound
	}
	return s.user.createChild(ctx, s, request)
}

func (s *agentSession) ChildTask() (ChildTask, bool) {
	if s.user == nil {
		return ChildTask{}, false
	}
	return s.user.childTask(s.dto.ID)
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

func (s *agentSession) SendChild(ctx context.Context, request ChildRequest) (ChildTask, string, error) {
	if s.user == nil || s.parent == nil {
		return ChildTask{}, "", ErrChildNotFound
	}
	return s.sendChild(ctx, request)
}

func (s *agentSession) InterruptChild(ctx context.Context) (ChildTask, error) {
	if s.user == nil || s.parent == nil {
		return ChildTask{}, ErrChildNotFound
	}
	return s.interruptChild(ctx)
}

func (s *agentSession) Forget() error {
	if s.user == nil || s.parent == nil {
		return ErrChildNotFound
	}
	return s.forget()
}

// Prompt admits input, runs the session to idle, and returns the assistant
// message produced by that execution lifecycle.
func (s *agentSession) Prompt(ctx context.Context, content string) (string, error) {
	messages, err := s.config.Sessions.ListMessages(ctx, s.dto.ID)
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
			_ = s.Interrupt(cleanup)
			cancel()
		}
		return "", err
	}
	messages, err = s.config.Sessions.ListMessages(ctx, s.dto.ID)
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
	if _, err := s.config.Sessions.Admit(ctx, s.dto.ID, session.AdmitParams{MessageID: messageID, Content: content, Delivery: session.DeliverySteer}); err != nil {
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

func (s *agentSession) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.drain != nil {
		return s.drain.status
	}
	return StatusIdle
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
	if s.drain != nil || s.childCreations != 0 || s.child != nil && (s.child.task.Status == ChildStatusRunning || s.child.task.Status == ChildStatusPending) {
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
