package agent

import (
	"context"
	"sort"
	"sync"
)

type Drainer interface {
	Drain(context.Context, string) error
}

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

type Coordinator struct {
	mu        sync.Mutex
	drainer   Drainer
	observers []LifecycleObserver
	active    map[string]*drainState
}

func NewCoordinator(drainer Drainer, observers ...LifecycleObserver) *Coordinator {
	return &Coordinator{drainer: drainer, observers: observers, active: make(map[string]*drainState)}
}

// Wake coalesces with an active drain and returns immediately.
func (c *Coordinator) Wake(sessionID string) {
	c.startOrJoin(sessionID, true)
}

func (c *Coordinator) startOrJoin(sessionID string, requestWake bool) *drainState {
	c.mu.Lock()
	if state := c.active[sessionID]; state != nil {
		if requestWake {
			state.wake = true
		}
		c.mu.Unlock()
		return state
	}
	ctx, cancel := context.WithCancel(context.Background())
	state := &drainState{done: make(chan struct{}), cancel: cancel, status: StatusRunning}
	c.active[sessionID] = state
	for _, observer := range c.observers {
		if starter, ok := observer.(LifecycleStartObserver); ok {
			starter.LifecycleStarted(sessionID)
		}
	}
	c.mu.Unlock()
	go c.run(ctx, sessionID, state)
	return state
}

// Resume starts an idle drain or joins the complete lifetime of an active one.
func (c *Coordinator) Resume(ctx context.Context, sessionID string) error {
	state := c.startOrJoin(sessionID, false)
	done := state.done
	select {
	case <-done:
		c.mu.Lock()
		err := state.err
		c.mu.Unlock()
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Coordinator) Interrupt(ctx context.Context, sessionID string) error {
	c.mu.Lock()
	state := c.active[sessionID]
	if state == nil {
		c.mu.Unlock()
		return nil
	}
	state.status = StatusInterrupting
	state.wake = false
	state.cancel()
	done := state.done
	c.mu.Unlock()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Coordinator) Status(sessionID string) Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	if state := c.active[sessionID]; state != nil {
		return state.status
	}
	return StatusIdle
}

func (c *Coordinator) Active() []Active {
	c.mu.Lock()
	defer c.mu.Unlock()
	result := make([]Active, 0, len(c.active))
	for id, state := range c.active {
		result = append(result, Active{id, state.status})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].SessionID < result[j].SessionID })
	return result
}

func (c *Coordinator) run(ctx context.Context, sessionID string, state *drainState) {
	for {
		err := c.drainer.Drain(ctx, sessionID)
		c.mu.Lock()
		state.err = err
		if state.wake && ctx.Err() == nil {
			state.wake = false
			c.mu.Unlock()
			continue
		}
		restart := state.wake
		if restart {
			nextCtx, cancel := context.WithCancel(context.Background())
			next := &drainState{done: make(chan struct{}), cancel: cancel, status: StatusRunning}
			c.active[sessionID] = next
			for _, observer := range c.observers {
				if starter, ok := observer.(LifecycleStartObserver); ok {
					starter.LifecycleStarted(sessionID)
				}
			}
			go c.run(nextCtx, sessionID, next)
		} else {
			delete(c.active, sessionID)
			for _, observer := range c.observers {
				if observer != nil {
					observer.LifecycleComplete(sessionID, state.err)
				}
			}
		}
		close(state.done)
		c.mu.Unlock()
		return
	}
}
