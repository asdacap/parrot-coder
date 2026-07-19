// Package monitor delivers asynchronous managed-process completion notices to
// the session which requested them.
package monitor

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/amirulashraf/parrot-coder/internal/diagnostics"
	"github.com/amirulashraf/parrot-coder/internal/id"
	"github.com/amirulashraf/parrot-coder/internal/process"
	"github.com/amirulashraf/parrot-coder/internal/session"
)

type sessionAdmitter interface {
	Admit(context.Context, string, session.AdmitParams) (session.Admission, error)
}

type sessionWaker interface{ Wake(string) }

type monitorKey struct {
	sessionID string
	processID int32
}

type activeMonitor struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// Service owns process monitors independently of the tool calls which create
// them. A monitor timeout only stops observation; it never stops the process.
type Service struct {
	processes *process.Runner
	sessions  sessionAdmitter

	mu     sync.Mutex
	ctx    context.Context
	cancel context.CancelFunc
	waker  sessionWaker
	closed bool
	active map[monitorKey]*activeMonitor
	paused map[string]int
	wg     sync.WaitGroup
}

func NewService(processes *process.Runner, sessions sessionAdmitter) *Service {
	ctx, cancel := context.WithCancel(context.Background())
	return &Service{
		processes: processes, sessions: sessions, ctx: ctx, cancel: cancel,
		active: make(map[monitorKey]*activeMonitor), paused: make(map[string]int),
	}
}

// SetWaker completes composition after the agent coordinator is available.
func (s *Service) SetWaker(waker sessionWaker) {
	s.mu.Lock()
	s.waker = waker
	s.mu.Unlock()
}

// Start validates and captures the process before returning, then waits in the
// background and eventually steers a notification into the caller session.
func (s *Service) Start(sessionID string, processID int32, timeout time.Duration) error {
	if s == nil || s.processes == nil || s.sessions == nil {
		return errors.New("monitor: service is unavailable")
	}
	if sessionID == "" || processID <= 0 || timeout < 0 {
		return errors.New("monitor: session, positive process ID, and nonnegative timeout are required")
	}
	observer, err := s.processes.ObservePersistent(sessionID, processID)
	if err != nil {
		return err
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("monitor: service is closed")
	}
	if s.waker == nil {
		s.mu.Unlock()
		return errors.New("monitor: session waker is unavailable")
	}
	if s.paused[sessionID] > 0 {
		s.mu.Unlock()
		return errors.New("monitor: session monitoring is paused")
	}
	key := monitorKey{sessionID: sessionID, processID: processID}
	if s.active[key] != nil {
		s.mu.Unlock()
		return fmt.Errorf("monitor: process session %d is already being monitored", processID)
	}
	monitorCtx, cancel := context.WithCancel(s.ctx)
	active := &activeMonitor{cancel: cancel, done: make(chan struct{})}
	s.active[key] = active
	s.wg.Add(1)
	s.mu.Unlock()
	go s.run(monitorCtx, key, timeout, observer, active)
	return nil
}

func (s *Service) run(ctx context.Context, key monitorKey, timeout time.Duration, observer *process.PersistentObserver, active *activeMonitor) {
	defer func() {
		s.mu.Lock()
		if s.active[key] == active {
			delete(s.active, key)
		}
		close(active.done)
		s.mu.Unlock()
		s.wg.Done()
	}()
	waitCtx := ctx
	var cancel context.CancelFunc
	if timeout > 0 {
		waitCtx, cancel = context.WithTimeout(waitCtx, timeout)
		defer cancel()
	}
	completion, err := observer.Wait(waitCtx)
	if errors.Is(err, context.Canceled) {
		return
	}

	var content string
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		content = fmt.Sprintf("Process monitor notification: monitoring process session %d timed out after %s. The process was not stopped and may still be running.", key.processID, timeout)
	case err != nil:
		content = fmt.Sprintf("Process monitor notification: waiting for process session %d failed: %v.", key.processID, err)
	case completion.ExitCode != nil:
		content = fmt.Sprintf("Process monitor notification: process session %d exited with code %d.", key.processID, *completion.ExitCode)
	case completion.WaitError != nil:
		content = fmt.Sprintf("Process monitor notification: process session %d finished with an unknown exit status: %v.", key.processID, completion.WaitError)
	default:
		content = fmt.Sprintf("Process monitor notification: process session %d finished.", key.processID)
	}
	if err := s.notify(ctx, key.sessionID, content); err != nil && !errors.Is(err, context.Canceled) {
		diagnostics.Error("process_monitor_notification_failed", "session_id", key.sessionID, "process_id", key.processID, "error_type", diagnostics.ErrorType(err))
	}
}

func (s *Service) notify(monitorCtx context.Context, sessionID, content string) error {
	messageID, err := id.New("msg")
	if err != nil {
		return err
	}
	s.mu.Lock()
	paused := s.paused[sessionID] > 0
	waker := s.waker
	s.mu.Unlock()
	if paused {
		return context.Canceled
	}
	ctx, cancel := context.WithTimeout(monitorCtx, 5*time.Second)
	defer cancel()
	if _, err := s.sessions.Admit(ctx, sessionID, session.AdmitParams{MessageID: messageID, Content: content, Delivery: session.DeliverySteer}); err != nil {
		return err
	}
	s.mu.Lock()
	paused = s.paused[sessionID] > 0
	s.mu.Unlock()
	if paused {
		return context.Canceled
	}
	if err := monitorCtx.Err(); err != nil {
		return err
	}
	if waker == nil {
		return errors.New("monitor: session waker is unavailable")
	}
	waker.Wake(sessionID)
	return nil
}

// SuspendSession prevents new observations and stops outstanding observations
// for one Parrot session without affecting the processes themselves.
func (s *Service) SuspendSession(ctx context.Context, sessionID string) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	s.paused[sessionID]++
	done := make([]<-chan struct{}, 0)
	for key, active := range s.active {
		if key.sessionID == sessionID {
			active.cancel()
			done = append(done, active.done)
		}
	}
	s.mu.Unlock()
	for _, finished := range done {
		select {
		case <-finished:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// ResumeSession permits new observations after lifecycle cleanup completes.
func (s *Service) ResumeSession(sessionID string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.paused[sessionID] <= 1 {
		delete(s.paused, sessionID)
	} else {
		s.paused[sessionID]--
	}
	s.mu.Unlock()
}

// InterruptSession delegates retained-process interruption to the runner.
func (s *Service) InterruptSession(sessionID string) error {
	if s == nil || s.processes == nil {
		return errors.New("monitor: process runner is unavailable")
	}
	return s.processes.InterruptSession(sessionID)
}

// DeleteSession delegates process-owned session cleanup to the runner.
func (s *Service) DeleteSession(sessionID string) error {
	if s == nil || s.processes == nil {
		return errors.New("monitor: process runner is unavailable")
	}
	return s.processes.DeleteSession(sessionID)
}

// Close cancels outstanding monitors and waits for their goroutines to exit.
func (s *Service) Close(ctx context.Context) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		s.cancel()
	}
	s.mu.Unlock()
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
