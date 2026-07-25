package agent

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/amirulashraf/parrot-coder/internal/id"
	"github.com/amirulashraf/parrot-coder/internal/session"
)

type LifecycleObserver interface {
	LifecycleComplete(sessionID string, err error)
}

type LifecycleStartObserver interface {
	LifecycleStarted(sessionID string)
}

type Status string

const (
	StatusIdle         Status = "idle"
	StatusRunning      Status = "running"
	StatusInterrupting Status = "interrupting"
)

type Active struct {
	SessionID string
	Status    Status
}

type drainState struct {
	done   chan struct{}
	cancel context.CancelFunc
	wake   bool
	status Status
	err    error
}

// AgentSession is the runtime for one persisted agent session. It owns both
// execution and the synchronization which ensures only one execution lifecycle
// is active for its bound session ID.
type AgentSession interface {
	ID() string
	Prompt(context.Context, string) (string, error)
	Send(context.Context, string) (string, error)
	Wake()
	Resume(context.Context) error
	Interrupt(context.Context) error
	Status() Status
}

func (s *agentSession) ID() string { return s.id }

// Prompt admits input, runs the session to idle, and returns the assistant
// message produced by that execution lifecycle.
func (s *agentSession) Prompt(ctx context.Context, content string) (string, error) {
	messages, err := s.config.Sessions.ListMessages(ctx, s.id)
	if err != nil {
		return "", err
	}
	var cutoff int64
	for _, message := range messages {
		cutoff = max(cutoff, message.Sequence)
	}
	if _, err := s.admit(ctx, content); err != nil {
		return "", err
	}
	if err := s.Resume(ctx); err != nil {
		if ctx.Err() != nil {
			cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = s.Interrupt(cleanup)
			cancel()
		}
		return "", err
	}
	messages, err = s.config.Sessions.ListMessages(ctx, s.id)
	if err != nil {
		return "", err
	}
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != "assistant" || messages[i].Sequence <= cutoff {
			continue
		}
		if messages[i].Error != "" {
			return messages[i].Content, errors.New(messages[i].Error)
		}
		return messages[i].Content, nil
	}
	return "", errors.New("agent: session produced no assistant output")
}

// Send admits steer input and wakes the session without waiting for it to idle.
func (s *agentSession) Send(ctx context.Context, content string) (string, error) {
	messageID, err := s.admit(ctx, content)
	if err != nil {
		return "", err
	}
	s.Wake()
	return messageID, nil
}

func (s *agentSession) admit(ctx context.Context, content string) (string, error) {
	messageID, err := id.New("msg")
	if err != nil {
		return "", err
	}
	if _, err := s.config.Sessions.Admit(ctx, s.id, session.AdmitParams{MessageID: messageID, Content: content, Delivery: session.DeliverySteer}); err != nil {
		return "", err
	}
	return messageID, nil
}

// Wake coalesces with an active drain and returns immediately.
func (s *agentSession) Wake() { s.startOrJoin(true) }

// Resume starts an idle drain or joins the complete lifetime of an active one.
func (s *agentSession) Resume(ctx context.Context) error {
	state := s.startOrJoin(false)
	select {
	case <-state.done:
		s.mu.Lock()
		err := state.err
		s.mu.Unlock()
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *agentSession) Interrupt(ctx context.Context) error {
	s.mu.Lock()
	state := s.drain
	if state == nil {
		s.mu.Unlock()
		return nil
	}
	state.status = StatusInterrupting
	state.wake = false
	state.cancel()
	done := state.done
	s.mu.Unlock()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *agentSession) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.drain != nil {
		return s.drain.status
	}
	return StatusIdle
}

func (s *agentSession) startOrJoin(requestWake bool) *drainState {
	s.mu.Lock()
	if s.drain != nil {
		if requestWake {
			s.drain.wake = true
		}
		state := s.drain
		s.mu.Unlock()
		return state
	}
	ctx, cancel := context.WithCancel(context.Background())
	state := &drainState{done: make(chan struct{}), cancel: cancel, status: StatusRunning}
	s.drain = state
	s.mu.Unlock()
	s.started()
	go s.run(ctx, state)
	return state
}

func (s *agentSession) run(ctx context.Context, state *drainState) {
	for {
		var err error
		if s.execute != nil {
			err = s.execute(ctx)
		} else {
			err = s.drainOnce(ctx)
		}
		if err == nil && ctx.Err() == nil {
			var prepared bool
			prepared, err = s.prepareContinuation(ctx)
			if err == nil && prepared {
				continue
			}
		}

		s.mu.Lock()
		state.err = err
		if state.wake && ctx.Err() == nil {
			state.wake = false
			s.mu.Unlock()
			continue
		}
		restart := state.wake
		if restart {
			nextCtx, cancel := context.WithCancel(context.Background())
			next := &drainState{done: make(chan struct{}), cancel: cancel, status: StatusRunning}
			s.drain = next
			close(state.done)
			s.mu.Unlock()
			s.started()
			go s.run(nextCtx, next)
			return
		}
		if s.drain == state {
			s.drain = nil
		}
		close(state.done)
		s.mu.Unlock()
		s.completed(err)
		return
	}
}

func (s *agentSession) started() {
	for _, observer := range s.observers {
		if starter, ok := observer.(LifecycleStartObserver); ok {
			starter.LifecycleStarted(s.id)
		}
	}
}

func (s *agentSession) completed(err error) {
	for _, observer := range s.observers {
		if observer != nil {
			observer.LifecycleComplete(s.id, err)
		}
	}
}

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
