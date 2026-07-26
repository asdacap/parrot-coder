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

// MainTaskID identifies the main task every session starts with. Agent tasks
// use their child session ID, while shell tasks use their process ID.
const MainTaskID = "task_main"

// Lifecycle statuses emitted as flat task events on a session's event stream.
const (
	EventStart    = "task.start"
	EventWorking  = "task.working"
	EventIdle     = "task.idle"
	EventFinished = "task.finished"
)

type Snapshot struct {
	ID        string    `json:"task_id"`
	Name      string    `json:"name,omitempty"`
	SessionID string    `json:"session_id"`
	Kind      Kind      `json:"kind"`
	Status    string    `json:"status"`
	StartedAt time.Time `json:"started_at,omitempty"`
	Agent     string    `json:"agent,omitempty"`
	Turn      int       `json:"turn,omitempty"`
	Depth     int       `json:"depth,omitempty"`
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

// ObserverFactory captures the current process or turn for stable waiting.
// Tasks which do not implement it are already stable for their lifetime.
type ObserverFactory interface {
	Observe() Task
}

var (
	ErrNotFound      = errors.New("task: not found")
	ErrDuplicate     = errors.New("task: duplicate ID")
	ErrWrongKind     = errors.New("task: wrong kind")
	ErrAmbiguousName = errors.New("task: ambiguous name")
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
	if m == nil || item == nil || visible == nil {
		return errors.New("task: task and visibility are required")
	}
	snapshot := item.Snapshot()
	if snapshot.ID == "" || snapshot.SessionID == "" {
		return errors.New("task: task ID and session ID are required")
	}
	id := snapshot.ID
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
	if observer, ok := item.task.(ObserverFactory); ok {
		return observer.Observe(), nil
	}
	return item.task, nil
}

// Resolve returns a caller-visible task of the expected kind. Exact canonical
// IDs take precedence over friendly-name lookup.
func (m *Manager) Resolve(callerSession, identifier string, kind Kind) (Task, error) {
	item, err := m.resolve(callerSession, identifier, kind, true)
	if err != nil {
		return nil, err
	}
	return observedTask(item), nil
}

// ResolveAny returns a caller-visible task by canonical ID or friendly name.
func (m *Manager) ResolveAny(callerSession, identifier string) (Task, error) {
	item, err := m.resolve(callerSession, identifier, "", false)
	if err != nil {
		return nil, err
	}
	return observedTask(item), nil
}

func (m *Manager) resolve(callerSession, identifier string, kind Kind, enforceKind bool) (Task, error) {
	if m == nil || callerSession == "" || identifier == "" {
		return nil, ErrNotFound
	}
	m.mu.RLock()
	exact, exactFound := m.tasks[identifier]
	items := make([]entry, 0, len(m.tasks))
	for _, item := range m.tasks {
		items = append(items, item)
	}
	m.mu.RUnlock()
	// Visibility callbacks can consult their owning managers, so invoke them
	// without holding the task index lock.
	if exactFound && exact.visible(callerSession) {
		if enforceKind && exact.task.Snapshot().Kind != kind {
			return nil, ErrWrongKind
		}
		return exact.task, nil
	}
	var found Task
	wrongKind := false
	for _, item := range items {
		snapshot := item.task.Snapshot()
		if !item.visible(callerSession) || snapshot.Name != identifier {
			continue
		}
		if enforceKind && snapshot.Kind != kind {
			wrongKind = true
			continue
		}
		if found != nil {
			return nil, ErrAmbiguousName
		}
		found = item.task
	}
	if found != nil {
		return found, nil
	}
	if wrongKind {
		return nil, ErrWrongKind
	}
	return nil, ErrNotFound
}

func observedTask(item Task) Task {
	if observer, ok := item.(ObserverFactory); ok {
		return observer.Observe()
	}
	return item
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
		if item.visible(callerSession) && (snapshot.Status == "blocked" || snapshot.Status == "running" || snapshot.Status == "pending") {
			result = append(result, snapshot)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

// Interrupt stops a caller-visible task and returns its latest snapshot.
func (m *Manager) Interrupt(ctx context.Context, callerSession, id string) (Snapshot, error) {
	item, err := m.ResolveAny(callerSession, id)
	if err != nil {
		return Snapshot{}, err
	}
	return item.Interrupt(ctx)
}

// Wait observes a caller-visible task without affecting its execution. If the
// wait context expires, the latest snapshot is returned with the context error.
func (m *Manager) Wait(ctx context.Context, callerSession, id string) (Result, error) {
	item, err := m.Get(callerSession, id)
	if err != nil {
		return Result{}, err
	}
	return waitTask(ctx, item)
}

// WaitKind waits for a task resolved by canonical ID or friendly name and
// enforces that it has the expected kind.
func (m *Manager) WaitKind(ctx context.Context, callerSession, identifier string, kind Kind) (Result, error) {
	item, err := m.Resolve(callerSession, identifier, kind)
	if err != nil {
		return Result{}, err
	}
	return waitTask(ctx, item)
}

func waitTask(ctx context.Context, item Task) (Result, error) {
	completion, err := item.Wait(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			snapshot := item.Snapshot()
			return Result{ID: snapshot.ID, Name: snapshot.Name, Kind: snapshot.Kind, Status: snapshot.Status}, err
		}
		return Result{}, err
	}
	return Result{ID: completion.Task.ID, Name: completion.Task.Name, Kind: completion.Task.Kind, Status: completion.Task.Status, ExitCode: completion.ExitCode, Output: completion.Output, Error: completion.Error}, nil
}

// Result describes the state observed by a task wait. Output is populated for
// agent tasks; shell output remains available through the process tools.
type Result struct {
	ID        string `json:"task_id"`
	Name      string `json:"name,omitempty"`
	Kind      Kind   `json:"kind"`
	Status    string `json:"status"`
	Yielded   bool   `json:"yielded,omitempty"`
	ElapsedMS int64  `json:"elapsed_ms"`
	ExitCode  *int   `json:"exit_code,omitempty"`
	Output    string `json:"output,omitempty"`
	Error     string `json:"error,omitempty"`
}
