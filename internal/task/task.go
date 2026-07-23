// Package task defines the public model shared by managed shell and agent work.
package task

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"
)

type Kind string

const (
	KindMain  Kind = "main"
	KindAgent Kind = "agent"
	KindShell Kind = "shell"
)

// MainTaskID identifies the main task every session starts with. It is unique
// within one session's event stream; subtasks receive generated identifiers.
const MainTaskID = "task_main"

// Lifecycle statuses emitted as flat task events on a session's event stream.
const (
	EventStart    = "task.start"
	EventWorking  = "task.working"
	EventIdle     = "task.idle"
	EventFinished = "task.finished"
)

type Snapshot struct {
	ID           string    `json:"task_id"`
	ParentTaskID string    `json:"parent_task_id,omitempty"`
	Kind         Kind      `json:"kind"`
	Status       string    `json:"status"`
	StartedAt    time.Time `json:"started_at,omitempty"`
	Agent        string    `json:"agent,omitempty"`
	Turn         int       `json:"turn,omitempty"`
	Depth        int       `json:"depth,omitempty"`
}

// Active is retained as an alias for callers which only consume task metadata.
type Active = Snapshot

type Completion struct {
	Task     Snapshot `json:"task"`
	Output   string   `json:"output,omitempty"`
	Error    string   `json:"error,omitempty"`
	ExitCode *int     `json:"exit_code,omitempty"`
}

// Task is a managed unit of work. Implementations retain a stable observation
// of the turn or process which was current when the task was registered.
type Task interface {
	Snapshot() Snapshot
	Wait(context.Context) (Completion, error)
	Interrupt(context.Context) (Snapshot, error)
}

var (
	ErrNotFound  = errors.New("task: not found")
	ErrDuplicate = errors.New("task: duplicate ID")
)

// Visibility decides whether a caller session may access a registered task.
type Visibility func(callerSession string) bool

type entry struct {
	task    Task
	visible Visibility
}

// Manager is the session-scoped index of all managed shell and agent tasks.
// Execution remains owned by each Task implementation.
type Manager struct {
	mu    sync.RWMutex
	tasks map[string]entry
}

func NewManager() *Manager { return &Manager{tasks: make(map[string]entry)} }

func (m *Manager) Register(item Task, visible Visibility) error {
	if m == nil || item == nil || item.Snapshot().ID == "" || visible == nil {
		return errors.New("task: task and visibility are required")
	}
	id := item.Snapshot().ID
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.tasks[id]; ok {
		return ErrDuplicate
	}
	m.tasks[id] = entry{task: item, visible: visible}
	return nil
}

func (m *Manager) Unregister(id string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	delete(m.tasks, id)
	m.mu.Unlock()
}

func (m *Manager) Get(callerSession, id string) (Task, error) {
	if m == nil || callerSession == "" || id == "" {
		return nil, ErrNotFound
	}
	m.mu.RLock()
	item, ok := m.tasks[id]
	m.mu.RUnlock()
	if !ok || !item.visible(callerSession) {
		return nil, ErrNotFound
	}
	return item.task, nil
}

func (m *Manager) ListActive(callerSession string) []Snapshot {
	if m == nil || callerSession == "" {
		return nil
	}
	m.mu.RLock()
	items := make([]entry, 0, len(m.tasks))
	for _, item := range m.tasks {
		items = append(items, item)
	}
	m.mu.RUnlock()
	result := make([]Snapshot, 0, len(items))
	for _, item := range items {
		snapshot := item.task.Snapshot()
		if item.visible(callerSession) && (snapshot.Status == "running" || snapshot.Status == "pending") {
			result = append(result, snapshot)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

// Result describes the state observed by a task wait. Output is populated for
// agent tasks; shell output remains available through the process tools.
type Result struct {
	ID       string `json:"task_id"`
	Kind     Kind   `json:"kind"`
	Status   string `json:"status"`
	ExitCode *int   `json:"exit_code,omitempty"`
	Output   string `json:"output,omitempty"`
	Error    string `json:"error,omitempty"`
}
