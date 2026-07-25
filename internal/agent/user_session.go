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

// UserSession is the runtime aggregate for a user. It owns the agent runtimes
// participating in that user's session hierarchy.
type UserSession interface {
	CreateChild(context.Context, AgentSession, ChildSessionRequest) (AgentSession, error)
	AddChildCreatedObserver(ChildCreatedObserver)
	ChildRelation(sessionID string) (string, bool)
	ChildSessions(parentSessionID string) []ChildSession
	HasChildSessions(parentSessionID string) bool
	ForgetChild(sessionID string) error
	DiscardChild(context.Context, string) error
	Get(sessionID string) AgentSession
	Lookup(sessionID string) (AgentSession, bool)
	Active() []Active
	Interrupt(context.Context, string) error
	Status(sessionID string) Status
	Remove(sessionID string) error
}

type userSession struct {
	repository *agentSessionRepository
}

var _ UserSession = (*userSession)(nil)

// NewUserSession creates a user runtime with its own agent-session repository.
func NewUserSession(ctx context.Context, config AgentSessionConfig, observers ...LifecycleObserver) (UserSession, error) {
	repository, err := newAgentSessionRepository(ctx, config, observers...)
	if err != nil {
		return nil, err
	}
	return &userSession{repository: repository}, nil
}

func (s *userSession) CreateChild(ctx context.Context, parent AgentSession, request ChildSessionRequest) (AgentSession, error) {
	return s.repository.CreateChild(ctx, parent, request)
}

func (s *userSession) AddChildCreatedObserver(observer ChildCreatedObserver) {
	s.repository.AddChildCreatedObserver(observer)
}

func (s *userSession) ChildRelation(sessionID string) (string, bool) {
	return s.repository.ChildRelation(sessionID)
}

func (s *userSession) ChildSessions(parentSessionID string) []ChildSession {
	return s.repository.ChildSessions(parentSessionID)
}

func (s *userSession) HasChildSessions(parentSessionID string) bool {
	return s.repository.HasChildSessions(parentSessionID)
}

func (s *userSession) ForgetChild(sessionID string) error {
	return s.repository.ForgetChild(sessionID)
}

func (s *userSession) DiscardChild(ctx context.Context, sessionID string) error {
	return s.repository.DiscardChild(ctx, sessionID)
}

func (s *userSession) Get(sessionID string) AgentSession { return s.repository.Get(sessionID) }

func (s *userSession) Lookup(sessionID string) (AgentSession, bool) {
	return s.repository.Lookup(sessionID)
}

func (s *userSession) Active() []Active { return s.repository.Active() }

func (s *userSession) Interrupt(ctx context.Context, sessionID string) error {
	return s.repository.Interrupt(ctx, sessionID)
}

func (s *userSession) Status(sessionID string) Status { return s.repository.Status(sessionID) }

func (s *userSession) Remove(sessionID string) error { return s.repository.Remove(sessionID) }

// agentSessionRepository owns the one runtime AgentSession object associated
// with each persisted session ID. Persistent state remains in SessionRuntime.
type agentSessionRepository struct {
	mu             sync.Mutex
	config         AgentSessionConfig
	observers      []LifecycleObserver
	childObservers []ChildCreatedObserver
	sessions       map[string]*agentSession
	dtos           map[string]session.AgentSessionDto
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
		sessions: make(map[string]*agentSession), dtos: make(map[string]session.AgentSessionDto), children: make(map[string]ChildSession),
	}
	for _, item := range items {
		repository.dtos[item.ID] = item
		if item.ParentSessionID != "" {
			repository.children[item.ID] = ChildSession{SessionID: item.ID, ParentSessionID: item.ParentSessionID}
		}
	}
	return repository, nil
}

type ChildSessionRequest struct {
	ProjectID        string
	Name             string
	Agent            string
	Model            string
	DefaultSelection session.Selection
}

// CreateChild persists a selected child session and returns its bound runtime.
func (r *agentSessionRepository) CreateChild(ctx context.Context, parent AgentSession, request ChildSessionRequest) (AgentSession, error) {
	if parent == nil || parent.ID() == "" {
		return nil, errors.New("agent: child parent session is required")
	}
	selectedParent, err := r.config.Sessions.Get(ctx, parent.ID())
	if err != nil {
		return nil, fmt.Errorf("agent: child parent session: %w", err)
	}
	boundParent, ok := r.Lookup(parent.ID())
	if !ok || boundParent != parent {
		return nil, errors.New("agent: child parent runtime is not owned by this user session")
	}
	if selectedParent.ProjectID != request.ProjectID {
		return nil, errors.New("agent: child parent belongs to another project")
	}
	selection := request.DefaultSelection
	if selectedParent.Provider != "" && selectedParent.Model != "" {
		selection.Provider, selection.Model, selection.Variant = selectedParent.Provider, selectedParent.Model, selectedParent.Variant
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
		ParentSessionID: selectedParent.ID,
		Name:            request.Name,
		ProjectID:       selectedParent.ProjectID,
		ProjectRoot:     selectedParent.ProjectRoot,
		Title:           "Subtask " + request.Name + " [" + request.Agent + "]",
	}, selection)
	if err != nil {
		return nil, err
	}
	relation := ChildSession{SessionID: child.ID, ParentSessionID: selectedParent.ID}
	r.mu.Lock()
	if r.children == nil {
		r.children = make(map[string]ChildSession)
	}
	r.children[child.ID] = relation
	r.dtos[child.ID] = child
	observers := append([]ChildCreatedObserver(nil), r.childObservers...)
	r.mu.Unlock()
	runtime := r.bind(child, parent)
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

// ForgetChild removes the retained child runtime and its relationship.
func (r *agentSessionRepository) ForgetChild(sessionID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if runtime := r.sessions[sessionID]; runtime != nil {
		if runtime.Status() != StatusIdle {
			return ErrAgentSessionActive
		}
		delete(r.sessions, sessionID)
	}
	delete(r.children, sessionID)
	delete(r.dtos, sessionID)
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
	delete(r.dtos, sessionID)
	r.mu.Unlock()
	return nil
}

func (r *agentSessionRepository) Get(sessionID string) AgentSession {
	r.mu.Lock()
	if existing := r.sessions[sessionID]; existing != nil {
		r.mu.Unlock()
		return existing
	}
	relation, child := r.children[sessionID]
	dto, known := r.dtos[sessionID]
	r.mu.Unlock()

	if !known && r.config.Sessions != nil {
		if loaded, err := r.config.Sessions.Get(context.Background(), sessionID); err == nil {
			dto, known = loaded, true
		}
	}
	if !known {
		dto.ID = sessionID
	}
	var parent AgentSession
	if child {
		dto.ParentSessionID = relation.ParentSessionID
		parent = r.Get(relation.ParentSessionID)
	}
	return r.bind(dto, parent)
}

func (r *agentSessionRepository) bind(dto session.AgentSessionDto, parent AgentSession) AgentSession {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.dtos == nil {
		r.dtos = make(map[string]session.AgentSessionDto)
	}
	if existing := r.sessions[dto.ID]; existing != nil {
		return existing
	}
	created := &agentSession{dto: dto, parent: parent, config: r.config, observers: r.observers}
	r.sessions[dto.ID] = created
	r.dtos[dto.ID] = dto
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
	item := r.sessions[sessionID]
	if item != nil && item.Status() != StatusIdle {
		r.mu.Unlock()
		return ErrAgentSessionActive
	}
	if r.config.StateDirectories != nil {
		if err := r.config.StateDirectories.Remove(sessionID); err != nil {
			r.mu.Unlock()
			return err
		}
	}
	if r.sessions[sessionID] == item {
		delete(r.sessions, sessionID)
	}
	r.mu.Unlock()
	return nil
}
