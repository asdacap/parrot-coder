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
	TryAcquireChildTurn() (ChildTurnPermit, bool)
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
func (s *agentSession) TryAcquireChildTurn() (ChildTurnPermit, bool) {
	return s.childTurns.tryAcquire()
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
	if _, err := s.admit(ctx, content); err != nil {
		return "", err
	}
	if err := s.Resume(ctx); err != nil {
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
	messageID, err := s.admit(ctx, content)
	if err != nil {
		return "", err
	}
	s.Wake()
	return messageID, nil
}

func (s *agentSession) admit(ctx context.Context, content string) (string, error) {
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
func (s *agentSession) Wake() { s.startOrJoin(true) }

// Resume starts an idle drain or joins the complete lifetime of an active one.
func (s *agentSession) Resume(ctx context.Context) error {
	state := s.startOrJoin(false)
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

func (s *agentSession) startOrJoin(requestWake bool) *drainState {
	s.mu.Lock()
	if s.drain != nil {
		if requestWake {
			s.drain.wake = true
		}
		state := s.drain
		s.mu.Unlock()
		return state
	}
	ctx, cancel := context.WithCancel(context.Background())
	state := &drainState{done: make(chan struct{}), cancel: cancel, status: StatusRunning}
	s.drain = state
	s.mu.Unlock()
	s.started()
	go s.run(ctx, state)
	return state
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
