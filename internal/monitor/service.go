// Package monitor delivers asynchronous managed-task completion notices to
// the session which requested them.
package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	v1 "github.com/amirulashraf/parrot-coder/internal/api/v1"
	"github.com/amirulashraf/parrot-coder/internal/diagnostics"
	"github.com/amirulashraf/parrot-coder/internal/process"
	managedtask "github.com/amirulashraf/parrot-coder/internal/task"
)

// AgentSession is the object-level capability monitors need after resolving a
// session ID. Composition adapts the application's richer agent session type.
type AgentSession interface {
	Send(context.Context, string) (string, error)
}

type agentSessionResolver interface {
	Get(string) AgentSession
}

type lifecyclePublisher interface{ PublishEvent(v1.Event) }

// Request identifies one background observation and the tool call which created it.
type Request struct {
	SessionID  string
	CallerTask string
	ToolCallID string
	TaskID     string
	Timeout    time.Duration
}

type monitorKey struct {
	sessionID string
	taskID    string
}

type activeMonitor struct {
	cancel  context.CancelFunc
	done    chan struct{}
	request Request
}

// Service owns process monitors independently of the tool calls which create
// them. A monitor timeout only stops observation; it never stops the process.
type Service struct {
	processes *process.Runner
	tasks     *managedtask.Manager
	lifecycle lifecyclePublisher

	mu     sync.Mutex
	ctx    context.Context
	cancel context.CancelFunc
	agents agentSessionResolver
	closed bool
	active map[monitorKey]*activeMonitor
	paused map[string]int
	wg     sync.WaitGroup
}

func NewService(processes *process.Runner, tasks *managedtask.Manager, lifecycle lifecyclePublisher) *Service {
	ctx, cancel := context.WithCancel(context.Background())
	return &Service{
		processes: processes, tasks: tasks, lifecycle: lifecycle, ctx: ctx, cancel: cancel,
		active: make(map[monitorKey]*activeMonitor), paused: make(map[string]int),
	}
}

// SetAgentSessions completes composition after the agent coordinator is available.
func (s *Service) SetAgentSessions(agents agentSessionResolver) {
	s.mu.Lock()
	s.agents = agents
	s.mu.Unlock()
}

// Start validates and captures the task before returning, then waits in the
// background and eventually steers a notification into the caller session.
func (s *Service) Start(request Request) error {
	if s == nil || s.tasks == nil {
		return errors.New("monitor: service is unavailable")
	}
	if request.SessionID == "" || request.TaskID == "" || request.Timeout < 0 {
		return errors.New("monitor: session, task ID, and nonnegative timeout are required")
	}
	wait, err := s.observe(request.SessionID, request.TaskID)
	if err != nil {
		return err
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("monitor: service is closed")
	}
	if s.agents == nil {
		s.mu.Unlock()
		return errors.New("monitor: agent sessions are unavailable")
	}
	if s.paused[request.SessionID] > 0 {
		s.mu.Unlock()
		return errors.New("monitor: session monitoring is paused")
	}
	key := monitorKey{sessionID: request.SessionID, taskID: request.TaskID}
	if s.active[key] != nil {
		s.mu.Unlock()
		return fmt.Errorf("monitor: task %s is already being monitored", request.TaskID)
	}
	monitorCtx, cancel := context.WithCancel(s.ctx)
	active := &activeMonitor{cancel: cancel, done: make(chan struct{}), request: request}
	s.active[key] = active
	s.wg.Add(1)
	s.mu.Unlock()
	s.publishLifecycle(v1.EventMonitorStarted, request, "", "")
	go s.run(monitorCtx, key, request.Timeout, wait, active)
	return nil
}

type taskWait func(context.Context) (string, error)

func (s *Service) observe(sessionID, taskID string) (taskWait, error) {
	item, err := s.tasks.Get(sessionID, taskID)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context) (string, error) {
		completion, err := item.Wait(ctx)
		if err != nil {
			return "", err
		}
		content := fmt.Sprintf("Task monitor notification: %s task %s finished with status %s.", completion.Task.Kind, completion.Task.ID, completion.Task.Status)
		if completion.ExitCode != nil {
			content = fmt.Sprintf("Task monitor notification: shell task %s exited with code %d.", completion.Task.ID, *completion.ExitCode)
		}
		if completion.Output != "" {
			content += "\n\n" + completion.Output
		}
		if completion.Error != "" {
			content += "\n\nError: " + completion.Error
		}
		return content, nil
	}, nil
}

func (s *Service) run(ctx context.Context, key monitorKey, timeout time.Duration, wait taskWait, active *activeMonitor) {
	status, lifecycleError := "canceled", ""
	defer func() {
		s.publishLifecycle(v1.EventMonitorFinished, active.request, status, lifecycleError)
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
	content, err := wait(waitCtx)
	if errors.Is(err, context.Canceled) {
		return
	}

	switch {
	case errors.Is(err, context.DeadlineExceeded):
		status = "timed_out"
		content = fmt.Sprintf("Task monitor notification: monitoring task %s timed out after %s. The task was not stopped and may still be running.", key.taskID, timeout)
	case err != nil:
		status, lifecycleError = "failed", err.Error()
		content = fmt.Sprintf("Task monitor notification: waiting for task %s failed: %v.", key.taskID, err)
	default:
		status = "completed"
	}
	if notifyErr := s.notify(ctx, key.sessionID, content); notifyErr != nil {
		if errors.Is(notifyErr, context.Canceled) {
			status = "canceled"
			return
		}
		status, lifecycleError = "failed", notifyErr.Error()
		diagnostics.Error("task_monitor_notification_failed", "session_id", key.sessionID, "task_id", key.taskID, "error_type", diagnostics.ErrorType(notifyErr))
	}
}

func (s *Service) publishLifecycle(eventType string, request Request, status, errorText string) {
	if s.lifecycle == nil {
		return
	}
	data, err := json.Marshal(v1.MonitorEvent{ToolCallID: request.ToolCallID, TaskID: request.TaskID, TimeoutMS: request.Timeout.Milliseconds(), Status: status, Error: errorText})
	if err != nil {
		return
	}
	s.lifecycle.PublishEvent(v1.Event{Type: eventType, SessionID: request.SessionID, TaskID: request.CallerTask, Data: data})
}

func (s *Service) notify(monitorCtx context.Context, sessionID, content string) error {
	s.mu.Lock()
	paused := s.paused[sessionID] > 0
	agents := s.agents
	s.mu.Unlock()
	if paused {
		return context.Canceled
	}
	if agents == nil {
		return errors.New("monitor: agent sessions are unavailable")
	}
	ctx, cancel := context.WithTimeout(monitorCtx, 5*time.Second)
	defer cancel()
	_, err := agents.Get(sessionID).Send(ctx, content)
	return err
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
