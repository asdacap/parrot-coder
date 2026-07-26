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

// Lifecycle statuses emitted as flat task events on a session's event stream.
const (
	EventStart    = "task.start"
	EventWorking  = "task.working"
	EventIdle     = "task.idle"
	EventFinished = "task.finished"
)

type Snapshot struct {
	Name      string    `json:"name,omitempty"`
	SessionID string    `json:"session_id,omitempty"`
	ProcessID string    `json:"process_id,omitempty"`
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
	ErrDuplicate     = errors.New("task: duplicate identity")
	ErrWrongKind     = errors.New("task: wrong kind")
	ErrAmbiguousName = errors.New("task: ambiguous name")
)

// Visibility decides whether a caller session may access a registered task.
type Visibility func(callerSession string) bool

type entry struct {
	task    Task
	visible Visibility
}

type domainKey struct {
	kind      Kind
	sessionID string
	processID string
}

// Manager is the session-scoped index of all managed shell and agent tasks.
// Execution remains owned by each Task implementation.
type Manager struct {
	mu    sync.RWMutex
	tasks map[domainKey]entry
}

func NewManager() *Manager { return &Manager{tasks: make(map[domainKey]entry)} }

func (m *Manager) Register(item Task, visible Visibility) error {
	if m == nil || item == nil || visible == nil {
		return errors.New("task: task and visibility are required")
	}
	key, err := domainKeyFor(item.Snapshot())
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.tasks[key]; ok {
		return ErrDuplicate
	}
	m.tasks[key] = entry{task: item, visible: visible}
	return nil
}

func (m *Manager) Unregister(snapshot Snapshot) {
	if m == nil {
		return
	}
	key, err := domainKeyFor(snapshot)
	if err != nil {
		return
	}
	m.mu.Lock()
	delete(m.tasks, key)
	m.mu.Unlock()
}

// Resolve returns a caller-visible task of the expected kind. Exact canonical
// IDs take precedence over friendly-name lookup.
func (m *Manager) Resolve(callerSession, identifier string, kind Kind) (Task, error) {
	item, err := m.resolve(callerSession, identifier, kind)
	if err != nil {
		return nil, err
	}
	return observedTask(item), nil
}

func (m *Manager) resolve(callerSession, identifier string, kind Kind) (Task, error) {
	if m == nil || callerSession == "" || identifier == "" {
		return nil, ErrNotFound
	}
	m.mu.RLock()
	items := make([]entry, 0, len(m.tasks))
	for _, item := range m.tasks {
		items = append(items, item)
	}
	m.mu.RUnlock()
	// Visibility callbacks can consult their owning managers, so invoke them
	// without holding the task index lock.
	wrongKind := false
	for _, item := range items {
		snapshot := item.task.Snapshot()
		if !item.visible(callerSession) || domainIdentifier(snapshot) != identifier {
			continue
		}
		if snapshot.Kind != kind {
			wrongKind = true
			continue
		}
		return item.task, nil
	}
	if wrongKind {
		return nil, ErrWrongKind
	}
	var found Task
	for _, item := range items {
		snapshot := item.task.Snapshot()
		if !item.visible(callerSession) || snapshot.Name != identifier {
			continue
		}
		if snapshot.Kind != kind {
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
	sort.Slice(result, func(i, j int) bool { return domainIdentifier(result[i]) < domainIdentifier(result[j]) })
	return result
}

// InterruptKind stops a caller-visible task resolved by its domain identity or
// friendly name and returns its latest snapshot.
func (m *Manager) InterruptKind(ctx context.Context, callerSession, identifier string, kind Kind) (Snapshot, error) {
	item, err := m.Resolve(callerSession, identifier, kind)
	if err != nil {
		return Snapshot{}, err
	}
	return item.Interrupt(ctx)
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
			return resultFromSnapshot(snapshot), err
		}
		return Result{}, err
	}
	result := resultFromSnapshot(completion.Task)
	result.ExitCode, result.Output, result.Error = completion.ExitCode, completion.Output, completion.Error
	return result, nil
}

func resultFromSnapshot(snapshot Snapshot) Result {
	return Result{SessionID: snapshot.SessionID, ProcessID: snapshot.ProcessID, Name: snapshot.Name, Kind: snapshot.Kind, Status: snapshot.Status}
}

// Result describes the state observed by a task wait. Output is populated for
// agent tasks; shell output remains available through the process tools.
type Result struct {
	SessionID string `json:"session_id,omitempty"`
	ProcessID string `json:"process_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Kind      Kind   `json:"kind"`
	Status    string `json:"status"`
	Yielded   bool   `json:"yielded,omitempty"`
	ElapsedMS int64  `json:"elapsed_ms"`
	ExitCode  *int   `json:"exit_code,omitempty"`
	Output    string `json:"output,omitempty"`
	Error     string `json:"error,omitempty"`
}

func domainKeyFor(snapshot Snapshot) (domainKey, error) {
	if snapshot.SessionID == "" {
		return domainKey{}, errors.New("task: session ID is required")
	}
	key := domainKey{kind: snapshot.Kind, sessionID: snapshot.SessionID, processID: snapshot.ProcessID}
	switch snapshot.Kind {
	case KindAgent:
		if snapshot.ProcessID != "" {
			return domainKey{}, errors.New("task: agent cannot have a process ID")
		}
	case KindShell:
		if snapshot.ProcessID == "" {
			return domainKey{}, errors.New("task: shell process ID is required")
		}
	default:
		return domainKey{}, errors.New("task: unsupported kind")
	}
	return key, nil
}

func domainIdentifier(snapshot Snapshot) string {
	if snapshot.Kind == KindShell {
		return snapshot.ProcessID
	}
	return snapshot.SessionID
}
