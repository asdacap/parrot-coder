package agent

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sort"
	"strings"
	"sync"

	"github.com/amirulashraf/parrot-coder/internal/session"
	managedtask "github.com/amirulashraf/parrot-coder/internal/task"
	"github.com/amirulashraf/parrot-coder/internal/tool"
)

var (
	ErrAgentSessionActive  = errors.New("agent: session is active")
	ErrAgentSessionRemoved = errors.New("agent: session was removed")
)

type SessionRuntime interface {
	GetSession(string) session.AgentSessionStore
	List(context.Context) ([]session.AgentSessionDto, error)
	CreateSelected(context.Context, session.CreateParams, session.Selection) (session.AgentSessionDto, error)
	ClaimInteractive(context.Context, session.InteractiveOwner, session.CreateParams, session.Selection, bool, func(int) bool) (session.InteractiveClaim, error)
}

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
	AddChildCreatedObserver(ChildCreatedObserver)
	ChildRelation(sessionID string) (string, bool)
	ChildSessions(parentSessionID string) []ChildSession
	HasChildSessions(parentSessionID string) bool
	Get(sessionID string) (AgentSession, error)
	Lookup(sessionID string) (AgentSession, bool)
	Active() []Active
	Interrupt(context.Context, string) error
	Status(sessionID string) AgentStatus
	Remove(sessionID string) error
	Delete(context.Context, string) error
	List(context.Context) ([]session.AgentSessionDto, error)
	CreateSelected(context.Context, session.CreateParams, session.Selection) (AgentSession, error)
	ClaimInteractive(context.Context, session.InteractiveOwner, session.CreateParams, session.Selection, bool, func(int) bool) (session.InteractiveClaim, error)
	Shutdown(context.Context) error
	CreateChild(context.Context, AgentSession, ChildRequest) (AgentSession, error)
	TryAcquireWorkerQuota() (func(), error)
}

type UserSessionConfig struct {
	AgentSession                     AgentSessionConfig
	MaxConcurrentChildTurns          int
	MaxConcurrentChildTurnsPerParent int
	MaxChildDepth                    int
	MaxChildTasks                    int
	MaxChildPromptBytes              int
	MaxChildResultBytes              int
	ChildAgentIdentity               func(string) string
	ChildAgentRecursionLimit         func(string) int
	ChildNameGenerator               func() string
	ChildTasks                       *managedtask.Manager
	ProjectID                        string
	DefaultSelection                 session.Selection
	ObserveChildProgress             func(sessionID string, report func(ChildProgress)) func()
	OnChildProgress                  func(Status)
	OnChildComplete                  func(Status)
	OnChildLifecycle                 func(ChildLifecycleEvent)
	OnChildDiscard                   func(string)
}

type userSession struct {
	config     UserSessionConfig
	sessions   SessionRuntime
	repository *agentSessionRepository
	childTurns childTurnSemaphore
	quotaMu    sync.Mutex
	childMu    sync.Mutex
	closed     bool
}

var _ UserSession = (*userSession)(nil)

// NewUserSession creates a user runtime with its own agent-session repository.
func NewUserSession(ctx context.Context, sessions SessionRuntime, config UserSessionConfig, observers ...LifecycleObserver) (UserSession, error) {
	if config.MaxConcurrentChildTurns <= 0 || config.MaxConcurrentChildTurnsPerParent <= 0 || config.MaxConcurrentChildTurnsPerParent > config.MaxConcurrentChildTurns {
		return nil, errors.New("agent: child turn concurrency limits are invalid")
	}
	if sessions == nil {
		return nil, errors.New("agent: session persistence is required")
	}
	if err := validateAgentSessionConfig(&config.AgentSession); err != nil {
		return nil, err
	}
	created := &userSession{config: config, sessions: sessions, childTurns: newChildTurnSemaphore(config.MaxConcurrentChildTurns)}
	applyChildDefaults(&created.config)
	repository, err := newAgentSessionRepository(ctx, created, config.MaxConcurrentChildTurnsPerParent, observers...)
	if err != nil {
		return nil, err
	}
	created.repository = repository
	return created, nil
}

func (s *userSession) resolveAgentSessionStore(sessionID string) session.AgentSessionStore {
	return s.sessions.GetSession(sessionID)
}

func (s *userSession) list(ctx context.Context) ([]session.AgentSessionDto, error) {
	return s.sessions.List(ctx)
}

func (s *userSession) createSelected(ctx context.Context, params session.CreateParams, selection session.Selection) (session.AgentSessionDto, error) {
	return s.sessions.CreateSelected(ctx, params, selection)
}

func (s *userSession) delete(ctx context.Context, sessionID string) error {
	return s.resolveAgentSessionStore(sessionID).Delete(ctx)
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

func (s *userSession) Get(sessionID string) (AgentSession, error) { return s.repository.Get(sessionID) }

func (s *userSession) Lookup(sessionID string) (AgentSession, bool) {
	return s.repository.Lookup(sessionID)
}

func (s *userSession) Active() []Active { return s.repository.Active() }

func (s *userSession) Interrupt(ctx context.Context, sessionID string) error {
	return s.repository.Interrupt(ctx, sessionID)
}

func (s *userSession) Status(sessionID string) AgentStatus { return s.repository.Status(sessionID) }

func (s *userSession) Remove(sessionID string) error { return s.repository.Remove(sessionID) }

func (s *userSession) Delete(ctx context.Context, sessionID string) error {
	if err := s.repository.Interrupt(ctx, sessionID); err != nil {
		return err
	}
	if err := s.delete(ctx, sessionID); err != nil {
		return err
	}
	return s.repository.Remove(sessionID)
}

func (s *userSession) List(ctx context.Context) ([]session.AgentSessionDto, error) {
	return s.list(ctx)
}

func (s *userSession) CreateSelected(ctx context.Context, params session.CreateParams, selection session.Selection) (AgentSession, error) {
	created, err := s.createSelected(ctx, params, selection)
	if err != nil {
		return nil, err
	}
	var parent AgentSession
	if created.ParentSessionID != "" {
		parent, err = s.repository.Get(created.ParentSessionID)
		if err != nil {
			return nil, errors.Join(err, s.delete(ctx, created.ID))
		}
	}
	return s.repository.bind(created, parent, s.resolveAgentSessionStore(created.ID), func() error { return s.delete(ctx, created.ID) })
}

func (s *userSession) ClaimInteractive(ctx context.Context, owner session.InteractiveOwner, params session.CreateParams, selection session.Selection, forceNew bool, alive func(int) bool) (session.InteractiveClaim, error) {
	claim, err := s.sessions.ClaimInteractive(ctx, owner, params, selection, forceNew, alive)
	if err != nil {
		return session.InteractiveClaim{}, err
	}
	if _, err := s.repository.Get(claim.Session.ID); err != nil {
		return session.InteractiveClaim{}, err
	}
	return claim, nil
}

func (s *userSession) TryAcquireWorkerQuota() (func(), error) {
	s.quotaMu.Lock()
	defer s.quotaMu.Unlock()
	if s.closed {
		return nil, ErrUserSessionClosed
	}
	permit, ok := s.childTurns.tryAcquire()
	if !ok {
		return nil, ErrChildConcurrency
	}
	return permit.Release, nil
}

func (s *userSession) Shutdown(ctx context.Context) error {
	s.quotaMu.Lock()
	s.closed = true
	s.quotaMu.Unlock()

	sessions, err := s.repository.closeAndSnapshot(ctx)
	if err != nil {
		return err
	}

	errs := make(chan error, len(sessions))
	for _, item := range sessions {
		go func() { errs <- item.Shutdown(ctx) }()
	}
	for range sessions {
		err = errors.Join(err, <-errs)
	}
	return err
}

// agentSessionRepository owns the one runtime AgentSession object associated
// with each persisted session ID. Persistent state remains owned by userSession.
type agentSessionRepository struct {
	mu                      sync.Mutex
	user                    *userSession
	config                  AgentSessionConfig
	observers               []LifecycleObserver
	childObservers          []ChildCreatedObserver
	sessions                map[string]*agentSession
	bindings                map[string]*sessionBinding
	dtos                    map[string]session.AgentSessionDto
	children                map[string]ChildSession
	maxConcurrentChildTurns int
	closed                  bool
}

func newAgentSessionRepository(ctx context.Context, user *userSession, maxConcurrentChildTurns int, observers ...LifecycleObserver) (*agentSessionRepository, error) {
	items, err := user.list(ctx)
	if err != nil {
		return nil, fmt.Errorf("agent: list sessions: %w", err)
	}
	repository := &agentSessionRepository{
		user: user, config: user.config.AgentSession, observers: observers, maxConcurrentChildTurns: maxConcurrentChildTurns,
		sessions: make(map[string]*agentSession), bindings: make(map[string]*sessionBinding), dtos: make(map[string]session.AgentSessionDto), children: make(map[string]ChildSession),
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
	boundParent, ok := r.Lookup(parent.ID())
	if !ok || boundParent != parent {
		return nil, errors.New("agent: child parent runtime is not owned by this user session")
	}
	selectedParent, err := boundParent.(*agentSession).store.Get(ctx)
	if err != nil {
		return nil, fmt.Errorf("agent: child parent session: %w", err)
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
	child, err := r.user.createSelected(ctx, session.CreateParams{
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
	runtime, err := r.bind(child, parent, r.user.resolveAgentSessionStore(child.ID), func() error { return r.user.delete(ctx, child.ID) })
	if err != nil {
		if _, retained := r.ChildRelation(child.ID); retained {
			for _, observer := range observers {
				if observer != nil {
					observer.ChildCreated(relation)
				}
			}
		}
		return nil, err
	}
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

// ManagedChildTasks returns the retained children created and managed by this
// user-session lifetime. Persisted historical relations remain available for
// hierarchy traversal, but do not consume the managed task retention limit.
func (r *agentSessionRepository) ManagedChildTasks() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	count := 0
	for _, runtime := range r.sessions {
		if runtime.child != nil {
			count++
		}
	}
	return count
}

// DiscardChild removes a child runtime, relationship, and durable session when
// task admission fails immediately after creation. The runtime hierarchy is
// retained if durable deletion fails, so a persisted child never becomes
// unreachable from its parent.
func (r *agentSessionRepository) DiscardChild(ctx context.Context, sessionID string) error {
	if err := r.user.delete(ctx, sessionID); err != nil {
		return err
	}
	r.mu.Lock()
	delete(r.sessions, sessionID)
	delete(r.children, sessionID)
	delete(r.dtos, sessionID)
	r.mu.Unlock()
	return nil
}

type sessionBinding struct {
	done chan struct{}
	err  error
}

func (r *agentSessionRepository) Get(sessionID string) (AgentSession, error) {
	r.mu.Lock()
	if existing := r.sessions[sessionID]; existing != nil {
		r.mu.Unlock()
		return existing, nil
	}
	relation, child := r.children[sessionID]
	dto, known := r.dtos[sessionID]
	r.mu.Unlock()

	var store session.AgentSessionStore
	if r.user != nil {
		store = r.user.resolveAgentSessionStore(sessionID)
	}
	if !known && store != nil {
		loaded, err := store.Get(context.Background())
		if err != nil {
			return nil, fmt.Errorf("agent: get session %s: %w", sessionID, err)
		}
		dto, known = loaded, true
		if dto.ParentSessionID != "" {
			relation = ChildSession{SessionID: dto.ID, ParentSessionID: dto.ParentSessionID}
			child = true
			r.mu.Lock()
			if r.dtos == nil {
				r.dtos = make(map[string]session.AgentSessionDto)
			}
			if r.children == nil {
				r.children = make(map[string]ChildSession)
			}
			r.dtos[dto.ID] = dto
			r.children[dto.ID] = relation
			r.mu.Unlock()
		}
	}
	if !known {
		dto.ID = sessionID
	}
	var parent AgentSession
	if child {
		dto.ParentSessionID = relation.ParentSessionID
		var err error
		parent, err = r.Get(relation.ParentSessionID)
		if err != nil {
			return nil, fmt.Errorf("agent: bind parent %s: %w", relation.ParentSessionID, err)
		}
	}
	return r.bind(dto, parent, store, nil)
}

func (r *agentSessionRepository) bind(dto session.AgentSessionDto, parent AgentSession, store session.AgentSessionStore, rollback func() error) (created AgentSession, err error) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		err = ErrUserSessionClosed
		if rollback != nil {
			if rollbackErr := rollback(); rollbackErr != nil {
				err = errors.Join(err, rollbackErr)
			} else {
				r.mu.Lock()
				delete(r.children, dto.ID)
				delete(r.dtos, dto.ID)
				r.mu.Unlock()
			}
		}
		return nil, err
	}
	if existing := r.sessions[dto.ID]; existing != nil {
		r.mu.Unlock()
		return existing, nil
	}
	if binding := r.bindings[dto.ID]; binding != nil {
		done := binding.done
		r.mu.Unlock()
		<-done
		r.mu.Lock()
		created, err := r.sessions[dto.ID], binding.err
		r.mu.Unlock()
		return created, err
	}
	binding := &sessionBinding{done: make(chan struct{})}
	if r.bindings == nil {
		r.bindings = make(map[string]*sessionBinding)
	}
	r.bindings[dto.ID] = binding
	r.mu.Unlock()

	var (
		user                                     UserSession
		maxChildPromptBytes, maxChildResultBytes int
		observeChildProgress                     func(string, func(ChildProgress)) func()
		onChildProgress, onChildComplete         func(Status)
		onChildLifecycle                         func(ChildLifecycleEvent)
	)
	if r.user != nil {
		user = r.user
		maxChildPromptBytes, maxChildResultBytes = r.user.config.MaxChildPromptBytes, r.user.config.MaxChildResultBytes
		observeChildProgress = r.user.config.ObserveChildProgress
		onChildProgress, onChildComplete = r.user.config.OnChildProgress, r.user.config.OnChildComplete
		onChildLifecycle = r.user.config.OnChildLifecycle
	}
	candidate := newAgentSession(
		dto, parent, user, r, store, r.config, r.maxConcurrentChildTurns, r.observers,
		maxChildPromptBytes, maxChildResultBytes, observeChildProgress, onChildProgress, onChildComplete, onChildLifecycle,
	)
	rolledBack := false
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("agent: materialize tools for session %s: panic: %v\n%s", dto.ID, recovered, debug.Stack())
		}
		if err != nil && rollback != nil {
			if rollbackErr := rollback(); rollbackErr != nil {
				err = errors.Join(err, fmt.Errorf("agent: rollback child session: %w", rollbackErr))
			} else {
				rolledBack = true
			}
		}
		r.mu.Lock()
		if err == nil {
			if r.dtos == nil {
				r.dtos = make(map[string]session.AgentSessionDto)
			}
			r.sessions[dto.ID] = candidate
			r.dtos[dto.ID] = dto
			created = candidate
		} else if rolledBack {
			delete(r.sessions, dto.ID)
			delete(r.children, dto.ID)
			delete(r.dtos, dto.ID)
		}
		binding.err = err
		delete(r.bindings, dto.ID)
		close(binding.done)
		r.mu.Unlock()
	}()

	snapshot, err := r.config.ToolProviders.Materialize(agentToolSession{candidate})
	if err != nil {
		return nil, err
	}
	candidate.toolSnapshot = snapshot
	candidate.toolExecutor = tool.Executor{Snapshot: snapshot, Permissions: r.config.ToolPermissionAuthorizer, ErrorAdvisor: r.config.ToolErrorAdvisor, MaxInputBytes: r.config.ToolMaxInputBytes, MaxOutputBytes: r.config.ToolMaxOutputBytes}
	return candidate, nil
}

type agentToolSession struct{ session *agentSession }

func (s agentToolSession) SessionID() string   { return s.session.ID() }
func (s agentToolSession) SessionName() string { return s.session.Name() }
func (s agentToolSession) IsSubagent() bool    { return s.session.Parent() != nil }
func (s agentToolSession) CreateAgent(ctx context.Context, callerAgent, prompt, target, model, name string) (tool.ChildAgent, error) {
	child, err := s.session.CreateChild(ctx, ChildRequest{Prompt: prompt, Agent: target, Model: model, Name: name})
	if err != nil {
		return nil, err
	}
	return agentToolChild{session: child}, nil
}
func (s agentToolSession) ResolveAgent(identifier string) (tool.ResolvedAgent, error) {
	if parent := s.session.Parent(); parent != nil && (identifier == parent.ID() || parent.Name() != "" && identifier == parent.Name()) {
		return tool.ResolvedAgent{Agent: agentToolChild{session: parent}, Relationship: tool.AgentRelationshipParent}, nil
	}
	child, err := s.session.ResolveChild(identifier)
	if err != nil {
		return tool.ResolvedAgent{}, err
	}
	return tool.ResolvedAgent{Agent: agentToolChild{session: child}, Relationship: tool.AgentRelationshipDescendant}, nil
}

type agentToolChild struct{ session AgentSession }

func (c agentToolChild) Status() tool.AgentTask { return toolAgentTask(c.session.Status()) }
func (c agentToolChild) Send(ctx context.Context, message tool.AgentMessage) (tool.AgentTask, string, error) {
	messageID, err := c.session.SendAgentMessage(ctx, message)
	return c.Status(), messageID, err
}

func toolAgentTask(status Status) tool.AgentTask {
	return tool.AgentTask{SessionID: status.SessionID, Agent: status.Agent, Name: status.Name, Status: string(status.State), Turn: status.Turn, Depth: status.Depth, Output: status.Output, Error: status.Error}
}

func (r *agentSessionRepository) closeAndSnapshot(ctx context.Context) ([]*agentSession, error) {
	for {
		r.mu.Lock()
		r.closed = true
		pending := make([]<-chan struct{}, 0, len(r.bindings))
		for _, binding := range r.bindings {
			pending = append(pending, binding.done)
		}
		if len(pending) == 0 {
			items := make([]*agentSession, 0, len(r.sessions))
			for _, item := range r.sessions {
				items = append(items, item)
			}
			r.mu.Unlock()
			return items, nil
		}
		r.mu.Unlock()
		for _, done := range pending {
			select {
			case <-done:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}
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
		if status := item.executionStatus(); status != StatusIdle {
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

func (r *agentSessionRepository) Status(sessionID string) AgentStatus {
	item, ok := r.Lookup(sessionID)
	if !ok {
		return StatusIdle
	}
	return item.(*agentSession).executionStatus()
}

func (r *agentSessionRepository) Remove(sessionID string) error {
	r.mu.Lock()
	item := r.sessions[sessionID]
	r.mu.Unlock()
	if item == nil {
		return nil
	}
	return item.removeIfIdle(func() error {
		if r.config.StateDirectories != nil {
			if err := r.config.StateDirectories.Remove(sessionID); err != nil {
				return err
			}
		}
		r.mu.Lock()
		if r.sessions[sessionID] == item {
			delete(r.sessions, sessionID)
		}
		r.mu.Unlock()
		return nil
	})
}
