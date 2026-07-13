// Package subagent manages bounded asynchronous subagent executions.
package subagent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	ErrInvalid      = errors.New("subagent: invalid request")
	ErrConcurrency  = errors.New("subagent: concurrency limit reached")
	ErrDepth        = errors.New("subagent: maximum depth reached")
	ErrCycle        = errors.New("subagent: agent cycle detected")
	ErrNotFound     = errors.New("subagent: task not found")
	ErrCanceled     = errors.New("subagent: task canceled")
	ErrTimeout      = errors.New("subagent: task timed out")
	ErrTaskLimit    = errors.New("subagent: retained task limit reached")
	ErrRequestLimit = errors.New("subagent: request limit reached")
)

// Request intentionally contains no parent permission grants. Every executor
// invocation must establish its own authorization context.
type Request struct {
	Prompt string
	Agent  string
	Model  string
}

// Execution is the complete executor contract. Lineage contains ancestor agent
// names, from the root toward the immediate parent.
type Execution struct {
	TaskID        string
	ParentSession string
	Lineage       []string
	Request       Request
}

type Executor interface {
	Execute(context.Context, Execution) (string, error)
}

type Config struct {
	MaxConcurrent          int
	MaxConcurrentPerParent int
	MaxDepth               int
	MaxTasks               int
	MaxPromptBytes         int
	MaxResultBytes         int
	Timeout                time.Duration
}

type Status string

const (
	StatusRunning   Status = "running"
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
	StatusCanceled  Status = "canceled"
	StatusTimedOut  Status = "timed_out"
)

type Task struct {
	ID            string
	ParentSession string
	Agent         string
	Model         string
	Depth         int
	Status        Status
	StartedAt     time.Time
	FinishedAt    time.Time
	Output        string
	Error         string
	Truncated     bool
}

type taskState struct {
	task   Task
	done   chan struct{}
	cancel context.CancelFunc
}

type Manager struct {
	mu       sync.Mutex
	executor Executor
	config   Config
	tasks    map[string]*taskState
	running  int
	byParent map[string]int
}

func NewManager(executor Executor, config Config) *Manager {
	if config.MaxConcurrent <= 0 {
		config.MaxConcurrent = 8
	}
	if config.MaxConcurrentPerParent <= 0 {
		config.MaxConcurrentPerParent = 4
	}
	if config.MaxDepth <= 0 {
		config.MaxDepth = 4
	}
	if config.MaxTasks <= 0 {
		config.MaxTasks = 1024
	}
	if config.MaxPromptBytes <= 0 {
		config.MaxPromptBytes = 1 << 20
	}
	if config.MaxResultBytes <= 0 {
		config.MaxResultBytes = 1 << 20
	}
	if config.Timeout <= 0 {
		config.Timeout = 10 * time.Minute
	}
	return &Manager{executor: executor, config: config, tasks: make(map[string]*taskState), byParent: make(map[string]int)}
}

// Launch reserves concurrency immediately and starts the execution
// asynchronously. Lineage is validated and copied before this method returns.
func (m *Manager) Launch(parentSession string, lineage []string, request Request) (string, error) {
	if m == nil || m.executor == nil || strings.TrimSpace(parentSession) == "" || strings.TrimSpace(request.Prompt) == "" || !validAgent(request.Agent) {
		return "", ErrInvalid
	}
	if len(request.Prompt) > m.config.MaxPromptBytes {
		return "", ErrRequestLimit
	}
	if len(lineage)+1 > m.config.MaxDepth {
		return "", ErrDepth
	}
	for _, ancestor := range lineage {
		if !validAgent(ancestor) {
			return "", ErrInvalid
		}
		if ancestor == request.Agent {
			return "", ErrCycle
		}
	}
	id, err := taskID()
	if err != nil {
		return "", err
	}
	lineage = append([]string(nil), lineage...)
	now := time.Now().UTC()
	m.mu.Lock()
	if len(m.tasks) >= m.config.MaxTasks {
		m.mu.Unlock()
		return "", ErrTaskLimit
	}
	if m.running >= m.config.MaxConcurrent || m.byParent[parentSession] >= m.config.MaxConcurrentPerParent {
		m.mu.Unlock()
		return "", ErrConcurrency
	}
	ctx, cancel := context.WithTimeout(context.Background(), m.config.Timeout)
	state := &taskState{task: Task{ID: id, ParentSession: parentSession, Agent: request.Agent, Model: request.Model, Depth: len(lineage) + 1, Status: StatusRunning, StartedAt: now}, done: make(chan struct{}), cancel: cancel}
	m.tasks[id] = state
	m.running++
	m.byParent[parentSession]++
	m.mu.Unlock()
	go m.run(ctx, state, lineage, request)
	return id, nil
}

func (m *Manager) run(ctx context.Context, state *taskState, lineage []string, request Request) {
	output, executeErr := m.executor.Execute(ctx, Execution{TaskID: state.task.ID, ParentSession: state.task.ParentSession, Lineage: lineage, Request: request})
	status := StatusSucceeded
	errText := ""
	if executeErr != nil {
		status, errText = StatusFailed, executeErr.Error()
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		status, errText = StatusTimedOut, ErrTimeout.Error()
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		status, errText = StatusCanceled, ErrCanceled.Error()
	}
	truncated := false
	if len(output) > m.config.MaxResultBytes {
		output = output[:m.config.MaxResultBytes]
		truncated = true
	}
	if len(errText) > m.config.MaxResultBytes {
		errText = errText[:m.config.MaxResultBytes]
		truncated = true
	}
	m.mu.Lock()
	state.task.Status = status
	state.task.Output = output
	state.task.Error = errText
	state.task.Truncated = truncated
	state.task.FinishedAt = time.Now().UTC()
	m.running--
	m.byParent[state.task.ParentSession]--
	if m.byParent[state.task.ParentSession] == 0 {
		delete(m.byParent, state.task.ParentSession)
	}
	state.cancel()
	close(state.done)
	m.mu.Unlock()
}

// Await observes a task. Cancellation of ctx only stops waiting and does not
// cancel the underlying execution.
func (m *Manager) Await(ctx context.Context, parentSession, id string) (Task, error) {
	state, err := m.lookup(parentSession, id)
	if err != nil {
		return Task{}, err
	}
	select {
	case <-state.done:
		m.mu.Lock()
		task := state.task
		m.mu.Unlock()
		return task, taskError(task)
	case <-ctx.Done():
		return Task{}, ctx.Err()
	}
}

// Cancel requests cancellation and waits for the executor to return. If ctx is
// canceled first, the task remains canceled but the wait returns ctx.Err().
func (m *Manager) Cancel(ctx context.Context, parentSession, id string) error {
	state, err := m.lookup(parentSession, id)
	if err != nil {
		return err
	}
	m.mu.Lock()
	if state.task.Status == StatusRunning {
		state.cancel()
	}
	done := state.done
	m.mu.Unlock()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Manager) Status(parentSession, id string) (Task, error) {
	state, err := m.lookup(parentSession, id)
	if err != nil {
		return Task{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return state.task, nil
}

// List is stable by task ID and returns only tasks belonging to parentSession.
func (m *Manager) List(parentSession string) []Task {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	out := make([]Task, 0)
	for _, state := range m.tasks {
		if state.task.ParentSession == parentSession {
			out = append(out, state.task)
		}
	}
	m.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Forget removes a completed task so long-lived managers can explicitly
// reclaim retained-task capacity.
func (m *Manager) Forget(parentSession, id string) error {
	if m == nil {
		return ErrNotFound
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	state := m.tasks[id]
	if state == nil || state.task.ParentSession != parentSession {
		return ErrNotFound
	}
	if state.task.Status == StatusRunning {
		return errors.New("subagent: cannot forget a running task")
	}
	delete(m.tasks, id)
	return nil
}

func (m *Manager) lookup(parentSession, id string) (*taskState, error) {
	if m == nil || parentSession == "" || id == "" {
		return nil, ErrNotFound
	}
	m.mu.Lock()
	state := m.tasks[id]
	m.mu.Unlock()
	if state == nil || state.task.ParentSession != parentSession {
		return nil, ErrNotFound
	}
	return state, nil
}

func taskError(task Task) error {
	switch task.Status {
	case StatusCanceled:
		return ErrCanceled
	case StatusTimedOut:
		return ErrTimeout
	case StatusFailed:
		return errors.New(task.Error)
	default:
		return nil
	}
}

func validAgent(agent string) bool {
	if agent == "" || len(agent) > 128 {
		return false
	}
	for _, char := range agent {
		if char > 127 || !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '-' || char == '_') {
			return false
		}
	}
	return true
}

func taskID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("subagent: generate task ID: %w", err)
	}
	return "task_" + hex.EncodeToString(raw[:]), nil
}
