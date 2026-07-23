package agent

import (
	"context"
	"errors"
	"sort"
	"sync"
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
	Wake()
	Resume(context.Context) error
	Interrupt(context.Context) error
	Status() Status
}

func (s *agentSession) ID() string { return s.id }

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
			starter.LifecycleStarted(s.id)
		}
	}
}

func (s *agentSession) completed(err error) {
	for _, observer := range s.observers {
		if observer != nil {
			observer.LifecycleComplete(s.id, err)
		}
	}
}

var ErrAgentSessionActive = errors.New("agent: session is active")

// AgentSessionRepository owns the one runtime AgentSession object associated
// with each persisted session ID. Persistent state remains in SessionRuntime.
type AgentSessionRepository struct {
	mu        sync.Mutex
	config    AgentSessionConfig
	observers []LifecycleObserver
	sessions  map[string]*agentSession
}

func NewAgentSessionRepository(config AgentSessionConfig, observers ...LifecycleObserver) (*AgentSessionRepository, error) {
	if err := validateAgentSessionConfig(&config); err != nil {
		return nil, err
	}
	return &AgentSessionRepository{config: config, observers: observers, sessions: make(map[string]*agentSession)}, nil
}

func (r *AgentSessionRepository) Get(sessionID string) AgentSession {
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing := r.sessions[sessionID]; existing != nil {
		return existing
	}
	created := &agentSession{id: sessionID, config: r.config, observers: r.observers}
	r.sessions[sessionID] = created
	return created
}

func (r *AgentSessionRepository) Lookup(sessionID string) (AgentSession, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.sessions[sessionID]
	return item, ok
}

func (r *AgentSessionRepository) Active() []Active {
	r.mu.Lock()
	items := make([]*agentSession, 0, len(r.sessions))
	for _, item := range r.sessions {
		items = append(items, item)
	}
	r.mu.Unlock()

	result := make([]Active, 0, len(items))
	for _, item := range items {
		if status := item.Status(); status != StatusIdle {
			result = append(result, Active{SessionID: item.ID(), Status: status})
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].SessionID < result[j].SessionID })
	return result
}

func (r *AgentSessionRepository) Wake(sessionID string) { r.Get(sessionID).Wake() }

func (r *AgentSessionRepository) Resume(ctx context.Context, sessionID string) error {
	return r.Get(sessionID).Resume(ctx)
}

func (r *AgentSessionRepository) Interrupt(ctx context.Context, sessionID string) error {
	item, ok := r.Lookup(sessionID)
	if !ok {
		return nil
	}
	return item.Interrupt(ctx)
}

func (r *AgentSessionRepository) Status(sessionID string) Status {
	item, ok := r.Lookup(sessionID)
	if !ok {
		return StatusIdle
	}
	return item.Status()
}

func (r *AgentSessionRepository) Remove(sessionID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	item := r.sessions[sessionID]
	if item == nil {
		return nil
	}
	if item.Status() != StatusIdle {
		return ErrAgentSessionActive
	}
	if r.sessions[sessionID] == item {
		delete(r.sessions, sessionID)
	}
	return nil
}
