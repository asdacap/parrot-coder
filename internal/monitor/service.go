// Package monitor delivers asynchronous managed-task completion notices to
// the session which requested them.
package monitor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/amirulashraf/parrot-coder/internal/diagnostics"
	"github.com/amirulashraf/parrot-coder/internal/id"
	"github.com/amirulashraf/parrot-coder/internal/process"
	"github.com/amirulashraf/parrot-coder/internal/session"
	"github.com/amirulashraf/parrot-coder/internal/subagent"
	managedtask "github.com/amirulashraf/parrot-coder/internal/task"
)

type sessionAdmitter interface {
	Admit(context.Context, string, session.AdmitParams) (session.Admission, error)
}

type sessionWaker interface{ Wake(string) }

type monitorKey struct {
	sessionID string
	taskID    string
}

type activeMonitor struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// Service owns process monitors independently of the tool calls which create
// them. A monitor timeout only stops observation; it never stops the process.
type Service struct {
	processes *process.Runner
	agents    *subagent.Manager
	tasks     *managedtask.Manager
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

func NewService(processes *process.Runner, agents *subagent.Manager, sessions sessionAdmitter, tasks ...*managedtask.Manager) *Service {
	ctx, cancel := context.WithCancel(context.Background())
	var taskManager *managedtask.Manager
	if len(tasks) > 0 {
		taskManager = tasks[0]
	}
	return &Service{
		processes: processes, agents: agents, tasks: taskManager, sessions: sessions, ctx: ctx, cancel: cancel,
		active: make(map[monitorKey]*activeMonitor), paused: make(map[string]int),
	}
}

// SetWaker completes composition after the agent coordinator is available.
func (s *Service) SetWaker(waker sessionWaker) {
	s.mu.Lock()
	s.waker = waker
	s.mu.Unlock()
}

// Start validates and captures the task before returning, then waits in the
// background and eventually steers a notification into the caller session.
func (s *Service) Start(sessionID, taskID string, timeout time.Duration) error {
	if s == nil || s.processes == nil || s.sessions == nil {
		return errors.New("monitor: service is unavailable")
	}
	if sessionID == "" || taskID == "" || timeout < 0 {
		return errors.New("monitor: session, task ID, and nonnegative timeout are required")
	}
	wait, err := s.observe(sessionID, taskID)
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
	key := monitorKey{sessionID: sessionID, taskID: taskID}
	if s.active[key] != nil {
		s.mu.Unlock()
		return fmt.Errorf("monitor: task %s is already being monitored", taskID)
	}
	monitorCtx, cancel := context.WithCancel(s.ctx)
	active := &activeMonitor{cancel: cancel, done: make(chan struct{})}
	s.active[key] = active
	s.wg.Add(1)
	s.mu.Unlock()
	go s.run(monitorCtx, key, timeout, wait, active)
	return nil
}

// Wait waits for a caller-visible shell or agent task without consuming its
// output or affecting its execution. On context cancellation it returns the
// task's last observable running state along with the context error.
func (s *Service) Wait(ctx context.Context, sessionID, taskID string) (managedtask.Result, error) {
	if s == nil || s.processes == nil {
		return managedtask.Result{}, errors.New("task: controller is unavailable")
	}
	switch {
	case strings.HasPrefix(taskID, "proc_"):
		observer, err := s.processes.ObservePersistent(sessionID, taskID)
		if err != nil {
			return managedtask.Result{}, err
		}
		completion, err := observer.Wait(ctx)
		if err != nil {
			return managedtask.Result{ID: taskID, Kind: managedtask.KindShell, Status: "running"}, err
		}
		result := managedtask.Result{ID: taskID, Kind: managedtask.KindShell, Status: "succeeded", ExitCode: completion.ExitCode}
		if completion.WaitError != nil || completion.ExitCode == nil || *completion.ExitCode != 0 {
			result.Status = "failed"
		}
		if completion.WaitError != nil {
			result.Error = completion.WaitError.Error()
		}
		return result, nil
	case strings.HasPrefix(taskID, "task_"):
		if s.agents == nil {
			return managedtask.Result{}, errors.New("task: agent manager is unavailable")
		}
		observer, err := s.agents.Observe(sessionID, taskID)
		if err != nil {
			return managedtask.Result{}, err
		}
		item, err := observer.Wait(ctx)
		if err != nil {
			return managedtask.Result{ID: taskID, Kind: managedtask.KindAgent, Status: "running"}, err
		}
		return managedtask.Result{ID: item.ID, Kind: managedtask.KindAgent, Status: string(item.Status), Output: item.Output, Error: item.Error}, nil
	default:
		return managedtask.Result{}, fmt.Errorf("task: unknown task ID %q", taskID)
	}
}

type taskWait func(context.Context) (string, error)

func (s *Service) observe(sessionID, taskID string) (taskWait, error) {
	if s.tasks != nil {
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
	switch {
	case strings.HasPrefix(taskID, "proc_"):
		observer, err := s.processes.ObservePersistent(sessionID, taskID)
		if err != nil {
			return nil, err
		}
		return func(ctx context.Context) (string, error) {
			completion, err := observer.Wait(ctx)
			if err != nil {
				return "", err
			}
			if completion.ExitCode != nil {
				return fmt.Sprintf("Task monitor notification: shell task %s exited with code %d.", completion.ProcessID, *completion.ExitCode), nil
			}
			if completion.WaitError != nil {
				return fmt.Sprintf("Task monitor notification: shell task %s finished with an unknown exit status: %v.", completion.ProcessID, completion.WaitError), nil
			}
			return fmt.Sprintf("Task monitor notification: shell task %s finished.", completion.ProcessID), nil
		}, nil
	case strings.HasPrefix(taskID, "task_"):
		if s.agents == nil {
			return nil, errors.New("monitor: agent manager is unavailable")
		}
		observer, err := s.agents.Observe(sessionID, taskID)
		if err != nil {
			return nil, err
		}
		return func(ctx context.Context) (string, error) {
			task, err := observer.Wait(ctx)
			if err != nil {
				return "", err
			}
			content := fmt.Sprintf("Task monitor notification: agent task %s finished with status %s.", task.ID, task.Status)
			if task.Output != "" {
				content += "\n\n" + task.Output
			}
			if task.Error != "" {
				content += "\n\nError: " + task.Error
			}
			return content, nil
		}, nil
	default:
		return nil, fmt.Errorf("monitor: unknown task ID %q", taskID)
	}
}

func (s *Service) run(ctx context.Context, key monitorKey, timeout time.Duration, wait taskWait, active *activeMonitor) {
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
	content, err := wait(waitCtx)
	if errors.Is(err, context.Canceled) {
		return
	}

	switch {
	case errors.Is(err, context.DeadlineExceeded):
		content = fmt.Sprintf("Task monitor notification: monitoring task %s timed out after %s. The task was not stopped and may still be running.", key.taskID, timeout)
	case err != nil:
		content = fmt.Sprintf("Task monitor notification: waiting for task %s failed: %v.", key.taskID, err)
	}
	if err := s.notify(ctx, key.sessionID, content); err != nil && !errors.Is(err, context.Canceled) {
		diagnostics.Error("task_monitor_notification_failed", "session_id", key.sessionID, "task_id", key.taskID, "error_type", diagnostics.ErrorType(err))
	}
}

// Interrupt stops one active shell or agent task visible to sessionID.
func (s *Service) Interrupt(ctx context.Context, sessionID, taskID string) (managedtask.Active, error) {
	if s == nil || s.processes == nil {
		return managedtask.Active{}, errors.New("task: controller is unavailable")
	}
	switch {
	case strings.HasPrefix(taskID, "proc_"):
		active, err := s.processes.InterruptPersistent(sessionID, taskID)
		if err != nil {
			return managedtask.Active{}, err
		}
		return managedtask.Active{ID: taskID, Kind: managedtask.KindShell, Status: "canceled", StartedAt: active.StartedAt}, nil
	case strings.HasPrefix(taskID, "task_"):
		if s.agents == nil {
			return managedtask.Active{}, errors.New("task: agent manager is unavailable")
		}
		task, err := s.agents.Interrupt(ctx, sessionID, taskID)
		return agentActive(task), err
	default:
		return managedtask.Active{}, fmt.Errorf("task: unknown task ID %q", taskID)
	}
}

// ListActive returns active shell and agent tasks visible to sessionID.
func (s *Service) ListActive(sessionID string) []managedtask.Active {
	if s == nil || s.processes == nil {
		return nil
	}
	processes := s.processes.ListActivePersistent(sessionID)
	var agents []subagent.Task
	if s.agents != nil {
		agents = s.agents.ListActive(sessionID)
	}
	result := make([]managedtask.Active, 0, len(processes)+len(agents))
	for _, item := range processes {
		result = append(result, shellActive(item))
	}
	for _, item := range agents {
		result = append(result, agentActive(item))
	}
	return result
}

func shellActive(item process.PersistentTask) managedtask.Active {
	return managedtask.Active{ID: item.ID, Kind: managedtask.KindShell, Status: "running", StartedAt: item.StartedAt}
}

func agentActive(item subagent.Task) managedtask.Active {
	return managedtask.Active{ID: item.ID, Kind: managedtask.KindAgent, Status: string(item.Status), StartedAt: item.StartedAt, Agent: item.Agent, Turn: item.Turn, Depth: item.Depth}
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
