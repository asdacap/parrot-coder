package agent

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/amirulashraf/parrot-coder/internal/session"
)

var ErrAgentSessionActive = errors.New("agent: session is active")

// ChildSession is the canonical relationship between a child agent session and
// its direct parent session.
type ChildSession struct {
	SessionID       string
	ParentSessionID string
}

// ChildCreatedObserver receives child relationships after they have been
// registered with the repository.
type ChildCreatedObserver interface {
	ChildCreated(ChildSession)
}

// UserSession is the runtime aggregate for a user. It owns the repository of
// agent runtimes participating in that user's session hierarchy.
type UserSession struct {
	repository *agentSessionRepository
}

// NewUserSession creates a user runtime with its own agent-session repository.
func NewUserSession(ctx context.Context, config AgentSessionConfig, observers ...LifecycleObserver) (*UserSession, error) {
	repository, err := newAgentSessionRepository(ctx, config, observers...)
	if err != nil {
		return nil, err
	}
	return &UserSession{repository: repository}, nil
}

func (s *UserSession) CreateChild(ctx context.Context, request ChildSessionRequest) (AgentSession, error) {
	return s.repository.CreateChild(ctx, request)
}

func (s *UserSession) AddChildCreatedObserver(observer ChildCreatedObserver) {
	s.repository.AddChildCreatedObserver(observer)
}

func (s *UserSession) ChildRelation(sessionID string) (string, bool) {
	return s.repository.ChildRelation(sessionID)
}

func (s *UserSession) ChildSessions(parentSessionID string) []ChildSession {
	return s.repository.ChildSessions(parentSessionID)
}

func (s *UserSession) HasChildSessions(parentSessionID string) bool {
	return s.repository.HasChildSessions(parentSessionID)
}

func (s *UserSession) ForgetChild(sessionID string) error {
	return s.repository.ForgetChild(sessionID)
}

func (s *UserSession) DiscardChild(ctx context.Context, sessionID string) error {
	return s.repository.DiscardChild(ctx, sessionID)
}

func (s *UserSession) Get(sessionID string) AgentSession { return s.repository.Get(sessionID) }

func (s *UserSession) Lookup(sessionID string) (AgentSession, bool) {
	return s.repository.Lookup(sessionID)
}

func (s *UserSession) Active() []Active { return s.repository.Active() }

func (s *UserSession) Interrupt(ctx context.Context, sessionID string) error {
	return s.repository.Interrupt(ctx, sessionID)
}

func (s *UserSession) Status(sessionID string) Status { return s.repository.Status(sessionID) }

func (s *UserSession) Remove(sessionID string) error { return s.repository.Remove(sessionID) }

// agentSessionRepository owns the one runtime AgentSession object associated
// with each persisted session ID. Persistent state remains in SessionRuntime.
type agentSessionRepository struct {
	mu             sync.Mutex
	config         AgentSessionConfig
	observers      []LifecycleObserver
	childObservers []ChildCreatedObserver
	sessions       map[string]*agentSession
	children       map[string]ChildSession
}

func newAgentSessionRepository(ctx context.Context, config AgentSessionConfig, observers ...LifecycleObserver) (*agentSessionRepository, error) {
	if err := validateAgentSessionConfig(&config); err != nil {
		return nil, err
	}
	items, err := config.Sessions.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("agent: list sessions: %w", err)
	}
	repository := &agentSessionRepository{
		config: config, observers: observers,
		sessions: make(map[string]*agentSession), children: make(map[string]ChildSession),
	}
	for _, item := range items {
		if item.ParentSessionID != "" {
			repository.children[item.ID] = ChildSession{SessionID: item.ID, ParentSessionID: item.ParentSessionID}
		}
	}
	return repository, nil
}

type ChildSessionRequest struct {
	ParentSessionID  string
	ProjectID        string
	Name             string
	Agent            string
	Model            string
	DefaultSelection session.Selection
}

// CreateChild persists a selected child session and returns its bound runtime.
func (r *agentSessionRepository) CreateChild(ctx context.Context, request ChildSessionRequest) (AgentSession, error) {
	parent, err := r.config.Sessions.Get(ctx, request.ParentSessionID)
	if err != nil {
		return nil, fmt.Errorf("agent: child parent session: %w", err)
	}
	if parent.ProjectID != request.ProjectID {
		return nil, errors.New("agent: child parent belongs to another project")
	}
	selection := request.DefaultSelection
	if parent.Provider != "" && parent.Model != "" {
		selection.Provider, selection.Model, selection.Variant = parent.Provider, parent.Model, parent.Variant
	}
	selection.Agent = request.Agent
	if request.Model != "" {
		if providerID, modelID, found := strings.Cut(request.Model, "/"); found {
			selection.Provider, selection.Model = providerID, modelID
		} else {
			selection.Model = request.Model
		}
		selection.Variant = ""
	}
	if selection.Provider == "" || selection.Model == "" {
		return nil, errors.New("agent: child has no default model")
	}
	if _, model, err := r.config.Providers.Resolve(selection.Provider, selection.Model); err != nil {
		return nil, fmt.Errorf("agent: child model: %w", err)
	} else if selection.Variant != "" {
		if _, ok := model.Capabilities.Variant(selection.Variant); !ok {
			return nil, fmt.Errorf("agent: child model: unknown model variant %q", selection.Variant)
		}
	}
	child, err := r.config.Sessions.CreateSelected(ctx, session.CreateParams{
		ParentSessionID: parent.ID,
		ProjectID:       parent.ProjectID,
		ProjectRoot:     parent.ProjectRoot,
		Title:           "Subtask " + request.Name + " [" + request.Agent + "]",
	}, selection)
	if err != nil {
		return nil, err
	}
	relation := ChildSession{SessionID: child.ID, ParentSessionID: parent.ID}
	r.mu.Lock()
	if r.children == nil {
		r.children = make(map[string]ChildSession)
	}
	r.children[child.ID] = relation
	observers := append([]ChildCreatedObserver(nil), r.childObservers...)
	r.mu.Unlock()
	runtime := r.Get(child.ID)
	for _, observer := range observers {
		if observer != nil {
			observer.ChildCreated(relation)
		}
	}
	return runtime, nil
}

// AddChildCreatedObserver registers an observer and replays the current child
// snapshot. Notifications are synchronous and occur outside the repository
// lock, after each relationship can be queried.
func (r *agentSessionRepository) AddChildCreatedObserver(observer ChildCreatedObserver) {
	if observer == nil {
		return
	}
	r.mu.Lock()
	r.childObservers = append(r.childObservers, observer)
	children := make([]ChildSession, 0, len(r.children))
	for _, relation := range r.children {
		children = append(children, relation)
	}
	r.mu.Unlock()
	sort.Slice(children, func(i, j int) bool { return children[i].SessionID < children[j].SessionID })
	for _, relation := range children {
		observer.ChildCreated(relation)
	}
}

// ChildRelation returns the direct parent for sessionID.
func (r *agentSessionRepository) ChildRelation(sessionID string) (parentSessionID string, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	relation, ok := r.children[sessionID]
	return relation.ParentSessionID, ok
}

// ChildSessions returns a stable snapshot of a parent's direct children.
func (r *agentSessionRepository) ChildSessions(parentSessionID string) []ChildSession {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([]ChildSession, 0)
	for _, relation := range r.children {
		if relation.ParentSessionID == parentSessionID {
			result = append(result, relation)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].SessionID < result[j].SessionID })
	return result
}

func (r *agentSessionRepository) HasChildSessions(parentSessionID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, relation := range r.children {
		if relation.ParentSessionID == parentSessionID {
			return true
		}
	}
	return false
}

// ForgetChild removes the relationship for sessionID.
func (r *agentSessionRepository) ForgetChild(sessionID string) error {
	r.mu.Lock()
	delete(r.children, sessionID)
	r.mu.Unlock()
	return nil
}

// DiscardChild removes a child runtime, relationship, and durable session when
// task admission fails immediately after creation. The runtime hierarchy is
// retained if durable deletion fails, so a persisted child never becomes
// unreachable from its parent.
func (r *agentSessionRepository) DiscardChild(ctx context.Context, sessionID string) error {
	if err := r.config.Sessions.Delete(ctx, sessionID); err != nil {
		return err
	}
	r.mu.Lock()
	delete(r.sessions, sessionID)
	delete(r.children, sessionID)
	r.mu.Unlock()
	return nil
}

func (r *agentSessionRepository) Get(sessionID string) AgentSession {
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing := r.sessions[sessionID]; existing != nil {
		return existing
	}
	created := &agentSession{id: sessionID, config: r.config, observers: r.observers}
	r.sessions[sessionID] = created
	return created
}

func (r *agentSessionRepository) Lookup(sessionID string) (AgentSession, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.sessions[sessionID]
	return item, ok
}

func (r *agentSessionRepository) Active() []Active {
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

func (r *agentSessionRepository) Interrupt(ctx context.Context, sessionID string) error {
	item, ok := r.Lookup(sessionID)
	if !ok {
		return nil
	}
	return item.Interrupt(ctx)
}

func (r *agentSessionRepository) Status(sessionID string) Status {
	item, ok := r.Lookup(sessionID)
	if !ok {
		return StatusIdle
	}
	return item.Status()
}

func (r *agentSessionRepository) Remove(sessionID string) error {
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
