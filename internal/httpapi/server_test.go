package httpapi

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/amirulashraf/parrot-coder/internal/agent"
	v1 "github.com/amirulashraf/parrot-coder/internal/api/v1"
	"github.com/amirulashraf/parrot-coder/internal/client"
	"github.com/amirulashraf/parrot-coder/internal/event"
	"github.com/amirulashraf/parrot-coder/internal/provider"
	"github.com/amirulashraf/parrot-coder/internal/session"
	"github.com/amirulashraf/parrot-coder/internal/store"
	"github.com/amirulashraf/parrot-coder/internal/tool"
	"github.com/amirulashraf/parrot-coder/internal/transport/inproc"
)

type stubBackend struct {
	mu         sync.Mutex
	order      []string
	backendErr error
	stream     *EventStream
}

func (b *stubBackend) Runtime(context.Context) (v1.Runtime, error) {
	return v1.Runtime{Version: "test", Active: []v1.RuntimeSession{}}, b.backendErr
}
func (b *stubBackend) ListSessions(context.Context) (v1.SessionList, error) {
	return v1.SessionList{Items: []v1.Session{}}, b.backendErr
}
func (b *stubBackend) CreateSession(context.Context, v1.CreateSessionRequest) (v1.Session, error) {
	return v1.Session{ID: "ses_test"}, b.backendErr
}

func (b *stubBackend) ClaimSession(context.Context, v1.ClaimSessionRequest) (v1.ClaimSessionResponse, error) {
	return v1.ClaimSessionResponse{}, nil
}
func (b *stubBackend) GetSession(context.Context, string) (v1.Session, error) {
	return v1.Session{ID: "ses_test"}, b.backendErr
}
func (b *stubBackend) UpdateSessionSelection(context.Context, string, v1.UpdateSessionSelectionRequest) (v1.SessionSelection, error) {
	return v1.SessionSelection{Agent: "plan", Provider: "local", Model: "code"}, b.backendErr
}
func (b *stubBackend) DeleteSession(context.Context, string) error { return b.backendErr }
func (b *stubBackend) ListMessages(context.Context, string) (v1.MessageList, error) {
	return v1.MessageList{Items: []v1.Message{}}, b.backendErr
}
func (b *stubBackend) ListTodos(context.Context, string) (v1.TodoList, error) {
	return v1.TodoList{Items: []v1.Todo{}}, b.backendErr
}
func (b *stubBackend) GetGoal(context.Context, string) (v1.Goal, error) {
	return v1.Goal{ID: "goal_test", SessionID: "ses_test", Objective: "ship it", Status: "active"}, b.backendErr
}
func (b *stubBackend) PutGoal(context.Context, string, v1.PutGoalRequest) (v1.Goal, error) {
	return v1.Goal{ID: "goal_test", SessionID: "ses_test", Objective: "ship it", Status: "active"}, b.backendErr
}
func (b *stubBackend) DeleteGoal(context.Context, string) error { return b.backendErr }
func (b *stubBackend) AdmitPrompt(context.Context, string, v1.PromptRequest) (v1.PromptAccepted, error) {
	b.mu.Lock()
	b.order = append(b.order, "admit")
	b.mu.Unlock()
	return v1.PromptAccepted{InputID: "inp_test", MessageID: "msg_test", Delivery: "steer", Status: "pending", Created: true}, b.backendErr
}
func (b *stubBackend) Wake(string) {
	b.mu.Lock()
	b.order = append(b.order, "wake")
	b.mu.Unlock()
}
func (b *stubBackend) Interrupt(context.Context, string) error { return b.backendErr }
func (b *stubBackend) OpenEvents(context.Context, string, int64) (*EventStream, error) {
	if b.backendErr != nil {
		return nil, b.backendErr
	}
	return b.stream, nil
}
func (b *stubBackend) ListPermissions(context.Context, string) (v1.PermissionList, error) {
	return v1.PermissionList{Items: []v1.Permission{}}, b.backendErr
}
func (b *stubBackend) ReplyPermission(context.Context, string, string, v1.PermissionReply) error {
	return b.backendErr
}
func (b *stubBackend) ListQuestions(context.Context, string) (v1.QuestionList, error) {
	return v1.QuestionList{Items: []v1.QuestionRequest{}}, b.backendErr
}
func (b *stubBackend) ReplyQuestion(context.Context, string, string, v1.QuestionReply) error {
	return b.backendErr
}
func (b *stubBackend) ListModels(context.Context) (v1.ModelList, error) {
	return v1.ModelList{Items: []v1.Model{}}, b.backendErr
}
func (b *stubBackend) GetModelInfo(_ context.Context, _ string, _ string) (v1.Model, error) {
	return v1.Model{}, b.backendErr
}
func (b *stubBackend) SubscriptionUsage(context.Context) (v1.SubscriptionUsage, error) {
	return v1.SubscriptionUsage{Provider: "chatgpt"}, b.backendErr
}
func (b *stubBackend) ListAgents(context.Context) (v1.AgentList, error) {
	return v1.AgentList{Items: []v1.Agent{}}, b.backendErr
}

func TestEveryRouteBasicAndMethodHandling(t *testing.T) {
	backend := &stubBackend{}
	server := New(backend, Config{})
	tests := []struct {
		method, path, body string
		status             int
	}{
		{"GET", "/api/v1/health", "", 200},
		{"GET", "/api/v1/runtime", "", 200},
		{"GET", "/api/v1/sessions", "", 200},
		{"POST", "/api/v1/sessions", `{}`, 201},
		{"POST", "/api/v1/interactive-sessions/claim", `{"working_directory":"/tmp/project","host_key":"host","pid":123}`, 200},
		{"GET", "/api/v1/sessions/ses_test", "", 200},
		{"DELETE", "/api/v1/sessions/ses_test", "", 204},
		{"PUT", "/api/v1/sessions/ses_test/selection", `{"agent":"plan"}`, 200},
		{"GET", "/api/v1/sessions/ses_test/messages", "", 200},
		{"GET", "/api/v1/sessions/ses_test/todos", "", 200},
		{"GET", "/api/v1/sessions/ses_test/goal", "", 200},
		{"PUT", "/api/v1/sessions/ses_test/goal", `{"objective":"ship it"}`, 200},
		{"DELETE", "/api/v1/sessions/ses_test/goal", "", 204},
		{"POST", "/api/v1/sessions/ses_test/prompts", `{"message_id":"msg_test","content":"hello","delivery":"steer"}`, 202},
		{"POST", "/api/v1/sessions/ses_test/interrupt", "", 204},
		{"GET", "/api/v1/sessions/ses_test/permissions", "", 200},
		{"POST", "/api/v1/sessions/ses_test/permissions/per_test/reply", `{"decision":"allow"}`, 204},
		{"POST", "/api/v1/sessions/ses_test/permissions/per_test/reply", `{"decision":"deny","reason":"not now"}`, 204},
		{"GET", "/api/v1/sessions/ses_test/questions", "", 200},
		{"POST", "/api/v1/sessions/ses_test/questions/qst_test/reply", `{"reject":true}`, 204},
		{"GET", "/api/v1/models", "", 200},
		{"GET", "/api/v1/usage", "", 200},
		{"GET", "/api/v1/agents", "", 200},
		{"GET", "/openapi.json", "", 200},
	}
	for _, test := range tests {
		t.Run(test.method+" "+test.path, func(t *testing.T) {
			request := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
			if test.body != "" {
				request.Header.Set("Content-Type", v1.MediaTypeJSON)
			}
			response := httptest.NewRecorder()
			server.ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
			}
			if response.Header().Get("X-Request-ID") == "" {
				t.Fatal("missing request ID")
			}
		})
	}

	for _, route := range Routes() {
		path := strings.NewReplacer("{id}", "ses_test", "{request}", "req_test").Replace(route.Path)
		request := httptest.NewRequest("PATCH", path, nil)
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if response.Code != http.StatusMethodNotAllowed {
			t.Errorf("PATCH %s status = %d", path, response.Code)
		}
		assertProblem(t, response, "method_not_allowed")
	}
}

type selectionResolver struct{}

func (selectionResolver) Resolve(providerID, modelID string) (provider.Provider, provider.Model, error) {
	if providerID != "local" || modelID != "code" && modelID != "reasoning" {
		return nil, provider.Model{}, errors.New("unknown model")
	}
	return nil, provider.Model{ID: modelID}, nil
}

func newSelectionBackend(t *testing.T) (*DomainBackend, *session.Service) {
	t.Helper()
	db := store.NewRegistry(t.TempDir(), "host-test")
	t.Cleanup(func() { _ = db.Close() })
	repository := event.NewRepository(db)
	sessions := session.NewService(db, repository)
	agents, err := agent.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	return &DomainBackend{Sessions: sessions, Goals: session.NewGoalService(db, repository), Agents: agents, ProviderResolver: selectionResolver{}}, sessions
}

func TestGoalCRUDThroughTypedClient(t *testing.T) {
	backend, _ := newSelectionBackend(t)
	backend.DefaultSelection = session.Selection{Agent: "build", Provider: "local", Model: "code"}
	apiClient, _ := client.New("http://inproc", inproc.New(New(backend, Config{})))
	ctx := context.Background()
	created, err := apiClient.CreateSession(ctx, v1.CreateSessionRequest{Title: "goal"})
	if err != nil {
		t.Fatal(err)
	}
	budget := int64(100)
	goal, err := apiClient.PutGoal(ctx, created.ID, v1.PutGoalRequest{Objective: pointer("ship it"), TokenBudget: &budget})
	if err != nil || goal.Objective != "ship it" || goal.Status != "active" || goal.RemainingTokens == nil || *goal.RemainingTokens != budget {
		t.Fatalf("PutGoal create = %#v, %v", goal, err)
	}
	loaded, err := apiClient.Goal(ctx, created.ID)
	if err != nil || loaded.ID != goal.ID {
		t.Fatalf("Goal = %#v, %v", loaded, err)
	}
	paused := "paused"
	loaded, err = apiClient.PutGoal(ctx, created.ID, v1.PutGoalRequest{Status: &paused, ClearTokenBudget: true})
	if err != nil || loaded.Status != paused || loaded.TokenBudget != nil {
		t.Fatalf("PutGoal update = %#v, %v", loaded, err)
	}
	if err := apiClient.DeleteGoal(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	_, err = apiClient.Goal(ctx, created.ID)
	assertAPIProblem(t, err, http.StatusNotFound, "session_not_found")
}

func pointer[T any](value T) *T { return &value }

func TestSelectedCreationIsValidatedAndAtomic(t *testing.T) {
	backend, sessions := newSelectionBackend(t)
	server := httptest.NewServer(New(backend, Config{}))
	defer server.Close()
	apiClient, err := client.New(server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	_, err = apiClient.CreateSession(ctx, v1.CreateSessionRequest{Title: "missing"})
	assertAPIProblem(t, err, http.StatusConflict, "model_required")
	items, err := sessions.List(ctx)
	if err != nil || len(items) != 0 {
		t.Fatalf("sessions after missing selection = %#v, %v", items, err)
	}

	created, err := apiClient.CreateSession(ctx, v1.CreateSessionRequest{Title: "selected", Agent: "plan", Model: "local/code"})
	if err != nil {
		t.Fatal(err)
	}
	if created.Agent != "plan" || created.Provider != "local" || created.Model != "code" {
		t.Fatalf("created selection = %#v", created)
	}

	_, err = apiClient.CreateSession(ctx, v1.CreateSessionRequest{Agent: "missing", Model: "local/code"})
	assertAPIProblem(t, err, http.StatusBadRequest, "invalid_selection")
	_, err = apiClient.CreateSession(ctx, v1.CreateSessionRequest{Agent: "build", Model: "local/missing"})
	assertAPIProblem(t, err, http.StatusBadRequest, "invalid_selection")
	items, err = sessions.List(ctx)
	if err != nil || len(items) != 1 {
		t.Fatalf("sessions after invalid selections = %#v, %v", items, err)
	}
}

func TestSessionDTOIncludesName(t *testing.T) {
	item := sessionDTO(session.AgentSessionDto{ID: "ses_child", Name: "inspect", Title: "Subtask inspect [build]"})
	if item.Name != "inspect" || item.Title != "Subtask inspect [build]" {
		t.Fatalf("session DTO = %#v", item)
	}
}

func TestChildCreationMapsAndValidatesParent(t *testing.T) {
	backend, sessions := newSelectionBackend(t)
	backend.DefaultSelection = session.Selection{Agent: "build", Provider: "local", Model: "code"}
	apiClient, _ := client.New("http://inproc", inproc.New(New(backend, Config{})))
	ctx := context.Background()
	parent, err := apiClient.CreateSession(ctx, v1.CreateSessionRequest{ProjectID: "project", Title: "parent"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := apiClient.CreateSession(ctx, v1.CreateSessionRequest{ParentSessionID: parent.ID, ProjectID: "project", Title: "child"})
	if err != nil || child.ParentSessionID != parent.ID {
		t.Fatalf("CreateSession child = %#v, %v", child, err)
	}
	loaded, err := sessions.Get(ctx, child.ID)
	if err != nil || loaded.ParentSessionID != parent.ID {
		t.Fatalf("stored child = %#v, %v", loaded, err)
	}
	_, err = apiClient.CreateSession(ctx, v1.CreateSessionRequest{ParentSessionID: "ses_missing", ProjectID: "project"})
	assertAPIProblem(t, err, http.StatusNotFound, "session_not_found")
	_, err = apiClient.CreateSession(ctx, v1.CreateSessionRequest{ParentSessionID: parent.ID, ProjectID: "other"})
	assertAPIProblem(t, err, http.StatusBadRequest, "invalid_request")
}

func TestDefaultCreationAndTypedPartialSelectionUpdate(t *testing.T) {
	backend, _ := newSelectionBackend(t)
	backend.DefaultSelection = session.Selection{Agent: "build", Provider: "local", Model: "code"}
	apiClient, _ := client.New("http://inproc", inproc.New(New(backend, Config{})))
	ctx := context.Background()
	created, err := apiClient.CreateSession(ctx, v1.CreateSessionRequest{Title: "default"})
	if err != nil || created.Agent != "build" || created.Provider != "local" || created.Model != "code" {
		t.Fatalf("default CreateSession = %#v, %v", created, err)
	}
	selected, err := apiClient.UpdateSessionSelection(ctx, created.ID, v1.UpdateSessionSelectionRequest{Agent: "plan"})
	if err != nil || selected.Agent != "plan" || selected.Provider != "local" || selected.Model != "code" {
		t.Fatalf("agent selection = %#v, %v", selected, err)
	}
	selected, err = apiClient.UpdateSessionSelection(ctx, created.ID, v1.UpdateSessionSelectionRequest{Model: "reasoning"})
	if err != nil || selected.Agent != "plan" || selected.Provider != "local" || selected.Model != "reasoning" {
		t.Fatalf("model selection = %#v, %v", selected, err)
	}
}

type testDrainer interface {
	Drain(context.Context, string) error
}

type testSessionController struct {
	mu      sync.Mutex
	drainer testDrainer
	active  map[string]*testDrainState
}

type testDrainState struct {
	cancel context.CancelFunc
	done   chan struct{}
	wake   bool
	status agent.AgentStatus
}

func newTestSessionController(drainer testDrainer) *testSessionController {
	return &testSessionController{drainer: drainer, active: make(map[string]*testDrainState)}
}

func (c *testSessionController) Get(id string) (agent.AgentSession, error) {
	return testAgentSession{controller: c, id: id}, nil
}

type testAgentSession struct {
	controller *testSessionController
	id         string
}

func (s testAgentSession) ID() string                 { return s.id }
func (s testAgentSession) Name() string               { return "" }
func (s testAgentSession) Parent() agent.AgentSession { return nil }
func (s testAgentSession) CreateChild(context.Context, agent.ChildRequest) (agent.AgentSession, error) {
	return nil, errors.New("not implemented")
}
func (s testAgentSession) Observe() (agent.ChildTurnObserver, error) {
	return nil, errors.New("not implemented")
}
func (s testAgentSession) ResolveChild(string) (agent.AgentSession, error) {
	return nil, errors.New("not implemented")
}
func (s testAgentSession) Prompt(context.Context, string) (string, error) {
	return "", errors.New("not implemented")
}
func (s testAgentSession) Send(context.Context, string) (string, error) {
	return "", errors.New("not implemented")
}
func (s testAgentSession) SendAgentMessage(context.Context, tool.AgentMessage) (string, error) {
	return "", errors.New("not implemented")
}
func (s testAgentSession) Wake() { s.controller.wake(s.id) }
func (s testAgentSession) Resume(context.Context) error {
	return errors.New("not implemented")
}
func (s testAgentSession) Interrupt(ctx context.Context) error {
	return s.controller.Interrupt(ctx, s.id)
}
func (s testAgentSession) Status() agent.Status {
	return agent.Status{SessionID: s.id, State: s.controller.Status(s.id)}
}

func (c *testSessionController) wake(id string) {
	c.mu.Lock()
	if state := c.active[id]; state != nil {
		state.wake = true
		c.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	state := &testDrainState{cancel: cancel, done: make(chan struct{}), status: agent.StatusRunning}
	c.active[id] = state
	c.mu.Unlock()
	go c.run(ctx, id, state)
}

func (c *testSessionController) run(ctx context.Context, id string, state *testDrainState) {
	_ = c.drainer.Drain(ctx, id)
	c.mu.Lock()
	if state.wake {
		nextCtx, cancel := context.WithCancel(context.Background())
		next := &testDrainState{cancel: cancel, done: make(chan struct{}), status: agent.StatusRunning}
		c.active[id] = next
		close(state.done)
		c.mu.Unlock()
		go c.run(nextCtx, id, next)
		return
	}
	delete(c.active, id)
	close(state.done)
	c.mu.Unlock()
}

func (c *testSessionController) Interrupt(ctx context.Context, id string) error {
	c.mu.Lock()
	state := c.active[id]
	if state == nil {
		c.mu.Unlock()
		return nil
	}
	state.status = agent.StatusInterrupting
	state.wake = false
	state.cancel()
	done := state.done
	c.mu.Unlock()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *testSessionController) Status(id string) agent.AgentStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	if state := c.active[id]; state != nil {
		return state.status
	}
	return agent.StatusIdle
}

func (c *testSessionController) Active() []agent.Active {
	c.mu.Lock()
	defer c.mu.Unlock()
	items := make([]agent.Active, 0, len(c.active))
	for id, state := range c.active {
		items = append(items, agent.Active{SessionID: id, Status: state.status})
	}
	return items
}

func (c *testSessionController) Remove(string) error { return nil }

type blockingDrainer struct {
	started chan struct{}
}

func (d blockingDrainer) Drain(ctx context.Context, _ string) error {
	select {
	case d.started <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return ctx.Err()
}

func TestSelectionRejectsActiveSession(t *testing.T) {
	backend, sessions := newSelectionBackend(t)
	backend.DefaultSelection = session.Selection{Agent: "build", Provider: "local", Model: "code"}
	started := make(chan struct{}, 1)
	backend.AgentSessions = newTestSessionController(blockingDrainer{started: started})
	created, err := backend.CreateSession(context.Background(), v1.CreateSessionRequest{})
	if err != nil {
		t.Fatal(err)
	}
	runtime, _ := backend.AgentSessions.Get(created.ID)
	runtime.Wake()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("session did not become active")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = backend.AgentSessions.Interrupt(ctx, created.ID)
	})

	apiClient, _ := client.New("http://inproc", inproc.New(New(backend, Config{})))
	_, err = apiClient.UpdateSessionSelection(context.Background(), created.ID, v1.UpdateSessionSelectionRequest{Agent: "plan"})
	assertAPIProblem(t, err, http.StatusConflict, "session_active")
	loaded, err := sessions.Get(context.Background(), created.ID)
	if err != nil || loaded.Agent != "build" {
		t.Fatalf("selection changed while active = %#v, %v", loaded, err)
	}
}

func waitForCondition(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition was not met")
		}
		time.Sleep(time.Millisecond)
	}
}

// restartOnceDrainer blocks on the first drain until its context is canceled,
// then returns the context error. On the second drain it reports that it
// restarted and completes, so a test can observe the auto-resume triggered by
// Interrupt when pending inputs remain. The coordinator runs drains for one
// session sequentially, so the boolean is never read concurrently.
type restartOnceDrainer struct {
	started   chan struct{}
	restarted chan struct{}
	drained   bool
}

func (d *restartOnceDrainer) Drain(ctx context.Context, _ string) error {
	if !d.drained {
		d.drained = true
		select {
		case d.started <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return ctx.Err()
	}
	select {
	case d.restarted <- struct{}{}:
	default:
	}
	return nil
}

func TestInterruptAutoResumesWhenPendingInputsRemain(t *testing.T) {
	backend, sessions := newSelectionBackend(t)
	backend.DefaultSelection = session.Selection{Agent: "build", Provider: "local", Model: "code"}
	drainer := &restartOnceDrainer{started: make(chan struct{}, 2), restarted: make(chan struct{}, 1)}
	backend.AgentSessions = newTestSessionController(drainer)

	ctx := context.Background()
	created, err := backend.CreateSession(ctx, v1.CreateSessionRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sessions.Admit(ctx, created.ID, session.AdmitParams{MessageID: "msg_steer", Content: "queued", Delivery: session.DeliverySteer}); err != nil {
		t.Fatal(err)
	}

	runtime, _ := backend.AgentSessions.Get(created.ID)
	runtime.Wake()
	select {
	case <-drainer.started:
	case <-time.After(time.Second):
		t.Fatal("first drain did not start")
	}

	interruptCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := backend.Interrupt(interruptCtx, created.ID); err != nil {
		t.Fatalf("Interrupt = %v", err)
	}

	select {
	case <-drainer.restarted:
	case <-time.After(3 * time.Second):
		t.Fatal("queued steer did not auto-resume the drain after interrupt")
	}
	waitForCondition(t, func() bool { return backend.AgentSessions.Status(created.ID) == agent.StatusIdle })
}

func TestInterruptDoesNotAutoResumeWithoutPendingInputs(t *testing.T) {
	backend, _ := newSelectionBackend(t)
	backend.DefaultSelection = session.Selection{Agent: "build", Provider: "local", Model: "code"}
	drainer := &restartOnceDrainer{started: make(chan struct{}, 2), restarted: make(chan struct{}, 1)}
	backend.AgentSessions = newTestSessionController(drainer)

	ctx := context.Background()
	created, err := backend.CreateSession(ctx, v1.CreateSessionRequest{})
	if err != nil {
		t.Fatal(err)
	}

	runtime, _ := backend.AgentSessions.Get(created.ID)
	runtime.Wake()
	select {
	case <-drainer.started:
	case <-time.After(time.Second):
		t.Fatal("first drain did not start")
	}

	interruptCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := backend.Interrupt(interruptCtx, created.ID); err != nil {
		t.Fatalf("Interrupt = %v", err)
	}

	select {
	case <-drainer.restarted:
		t.Fatal("drain restarted without pending inputs")
	case <-time.After(200 * time.Millisecond):
	}
	waitForCondition(t, func() bool { return backend.AgentSessions.Status(created.ID) == agent.StatusIdle })
}

func TestStrictJSONContentTypeAndLimit(t *testing.T) {
	server := New(&stubBackend{}, Config{MaxBodyBytes: 32})
	tests := []struct {
		name, method, path, contentType, body, code string
		status                                      int
	}{
		{"content type", http.MethodPost, "/api/v1/sessions", "text/plain", `{}`, "unsupported_media_type", 415},
		{"unknown create field", http.MethodPost, "/api/v1/sessions", v1.MediaTypeJSON, `{"unknown":true}`, "invalid_request", 400},
		{"unknown selection field", http.MethodPut, "/api/v1/sessions/ses_test/selection", v1.MediaTypeJSON, `{"unknown":true}`, "invalid_request", 400},
		{"trailing", http.MethodPost, "/api/v1/sessions", v1.MediaTypeJSON, `{} {}`, "invalid_request", 400},
		{"too large", http.MethodPost, "/api/v1/sessions", v1.MediaTypeJSON, `{"title":"` + strings.Repeat("x", 64) + `"}`, "body_too_large", 413},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
			request.Header.Set("Content-Type", test.contentType)
			response := httptest.NewRecorder()
			server.ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
			}
			assertProblem(t, response, test.code)
		})
	}
}

func TestPromptAdmissionPrecedesWake(t *testing.T) {
	backend := &stubBackend{}
	server := New(backend, Config{})
	request := httptest.NewRequest(http.MethodPost, "/api/v1/sessions/ses_test/prompts", strings.NewReader(`{"message_id":"msg_test","content":"hello","delivery":"queue"}`))
	request.Header.Set("Content-Type", v1.MediaTypeJSON)
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d", response.Code)
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if strings.Join(backend.order, ",") != "admit,wake" {
		t.Fatalf("order = %v", backend.order)
	}
}

func TestPromptExactRetryThroughHTTP(t *testing.T) {
	ctx := context.Background()
	db := store.NewRegistry(t.TempDir(), "host-test")
	defer db.Close()
	repository := event.NewRepository(db)
	sessions := session.NewService(db, repository)
	created, err := sessions.CreateSelected(ctx, session.CreateParams{Title: "test"}, session.Selection{Agent: "build", Provider: "local", Model: "code"})
	if err != nil {
		t.Fatal(err)
	}
	server := New(&DomainBackend{Sessions: sessions, Events: event.NewBroker(repository, event.NewTransientRepository())}, Config{})
	body := `{"message_id":"msg_retry","content":"hello","delivery":"steer"}`
	for attempt := 0; attempt < 2; attempt++ {
		request := httptest.NewRequest(http.MethodPost, "/api/v1/sessions/"+created.ID+"/prompts", strings.NewReader(body))
		request.Header.Set("Content-Type", v1.MediaTypeJSON)
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if response.Code != http.StatusAccepted {
			t.Fatalf("attempt %d status = %d: %s", attempt, response.Code, response.Body.String())
		}
		var accepted v1.PromptAccepted
		if err := json.Unmarshal(response.Body.Bytes(), &accepted); err != nil {
			t.Fatal(err)
		}
		if accepted.Created != (attempt == 0) {
			t.Fatalf("attempt %d created = %v", attempt, accepted.Created)
		}
	}
	items, err := repository.List(ctx, created.ID, -1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Type != "session.input.admitted" {
		t.Fatalf("events = %#v", items)
	}

	request := httptest.NewRequest(http.MethodPost, "/api/v1/sessions/"+created.ID+"/prompts", strings.NewReader(`{"message_id":"msg_retry","content":"different","delivery":"steer"}`))
	request.Header.Set("Content-Type", v1.MediaTypeJSON)
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusConflict {
		t.Fatalf("conflict status = %d: %s", response.Code, response.Body.String())
	}
}

func TestOpaqueCursorPagination(t *testing.T) {
	list := v1.SessionList{Items: []v1.Session{{ID: "ses_3"}, {ID: "ses_2"}, {ID: "ses_1"}}}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/sessions?limit=2", nil)
	first, err := paginateSessions(request, list)
	if err != nil || len(first.Items) != 2 || first.NextCursor == "" || strings.Contains(first.NextCursor, "ses_2") {
		t.Fatalf("first page = %#v, %v", first, err)
	}
	request = httptest.NewRequest(http.MethodGet, "/api/v1/sessions?cursor="+first.NextCursor+"&limit=2", nil)
	second, err := paginateSessions(request, list)
	if err != nil || len(second.Items) != 1 || second.Items[0].ID != "ses_1" {
		t.Fatalf("second page = %#v, %v", second, err)
	}
	bad := httptest.NewRequest(http.MethodGet, "/api/v1/sessions?cursor=not-a-cursor", nil)
	if _, err := paginateSessions(bad, list); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid cursor error = %v", err)
	}
}

func TestProblemDoesNotLeakBackendError(t *testing.T) {
	secret := "provider credential sk-secret raw body"
	server := New(&stubBackend{backendErr: errors.New(secret)}, Config{})
	response := httptest.NewRecorder()
	server.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/runtime", nil))
	if response.Code != 500 {
		t.Fatalf("status = %d", response.Code)
	}
	if strings.Contains(response.Body.String(), secret) {
		t.Fatal("problem leaked backend error")
	}
	var item v1.Problem
	if err := json.Unmarshal(response.Body.Bytes(), &item); err != nil {
		t.Fatal(err)
	}
	if item.ErrorRef == "" || item.Code != "internal_error" {
		t.Fatalf("problem = %#v", item)
	}
}

func TestOpenAPIHasExactlyDeclaredRoutesAndProblems(t *testing.T) {
	var document struct {
		OpenAPI string                    `json:"openapi"`
		Paths   map[string]map[string]any `json:"paths"`
		Events  []v1.EventDefinition      `json:"x-event-manifest"`
	}
	if err := json.Unmarshal(buildOpenAPI(), &document); err != nil {
		t.Fatal(err)
	}
	if document.OpenAPI != "3.1.0" || len(document.Events) != len(v1.EventManifest) {
		t.Fatalf("document metadata is incomplete")
	}
	if len(operationSchemas) != len(Routes()) {
		t.Fatalf("operation schema count = %d, route count = %d", len(operationSchemas), len(Routes()))
	}
	expected := make(map[string]map[string]bool)
	for _, route := range Routes() {
		if expected[route.Path] == nil {
			expected[route.Path] = make(map[string]bool)
		}
		expected[route.Path][strings.ToLower(route.Method)] = true
	}
	if len(document.Paths) != len(expected) {
		t.Fatalf("OpenAPI path count = %d, route path count = %d", len(document.Paths), len(expected))
	}
	seen := 0
	for _, route := range Routes() {
		operation, ok := document.Paths[route.Path][strings.ToLower(route.Method)].(map[string]any)
		if !ok {
			t.Fatalf("missing %s %s", route.Method, route.Path)
		}
		responses := operation["responses"].(map[string]any)
		if _, ok := responses["500"]; !ok {
			t.Fatalf("%s has no problem responses", route.OperationID)
		}
		seen++
	}
	if seen != len(Routes()) {
		t.Fatalf("route count = %d", seen)
	}
	for path, methods := range document.Paths {
		if len(methods) != len(expected[path]) {
			t.Fatalf("OpenAPI methods for %s = %#v, routes = %#v", path, methods, expected[path])
		}
		for method := range methods {
			if !expected[path][method] {
				t.Fatalf("unexpected OpenAPI operation %s %s", method, path)
			}
		}
	}
}

func TestSSEHeadersFieldsReplayHeartbeatAndCancel(t *testing.T) {
	durable := make(chan v1.Event)
	live := make(chan v1.Event)
	closed := make(chan struct{})
	sequence := int64(4)
	backend := &stubBackend{stream: &EventStream{
		Replay:  []v1.Event{{ID: "evt_replay", Type: "session.input.admitted", SessionID: "ses_test", Sequence: &sequence, Data: json.RawMessage(`{"ok":true}`)}},
		Durable: durable, Live: live, Close: func() { close(closed) },
	}}
	server := httptest.NewServer(New(backend, Config{HeartbeatInterval: 10 * time.Millisecond}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/api/v1/sessions/ses_test/events?after=3", nil)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if response.Header.Get("Content-Type") != v1.MediaTypeSSE || response.Header.Get("X-Accel-Buffering") != "no" || !strings.Contains(response.Header.Get("Cache-Control"), "no-transform") {
		t.Fatalf("SSE headers = %#v", response.Header)
	}
	reader := bufio.NewReader(response.Body)
	connected, err := readSSEBlock(reader)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(connected, "event: server.connected\n") || !strings.Contains(connected, "id: evt_") {
		t.Fatalf("connected block = %q", connected)
	}
	replay, err := readSSEBlock(reader)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(replay, "id: evt_replay\n") || !strings.Contains(replay, "event: session.input.admitted\n") || !strings.Contains(replay, `"sequence":4`) {
		t.Fatalf("replay block = %q", replay)
	}
	heartbeat, err := readSSEBlock(reader)
	if err != nil {
		t.Fatal(err)
	}
	if heartbeat != ": heartbeat\n\n" {
		t.Fatalf("heartbeat = %q", heartbeat)
	}
	cancel()
	_ = response.Body.Close()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("stream did not close after cancellation")
	}
}

func TestSSEValidatesBeforeCommittingHeaders(t *testing.T) {
	server := New(&stubBackend{backendErr: ErrNotFound}, Config{})
	response := httptest.NewRecorder()
	server.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/sessions/missing/events", nil))
	if response.Code != 404 || response.Header().Get("Content-Type") != v1.MediaTypeProblem {
		t.Fatalf("response = %d %#v", response.Code, response.Header())
	}
}

func TestSSEWritesReadyLiveEventsBeforeDurableCompletion(t *testing.T) {
	durable := make(chan v1.Event, 1)
	live := make(chan v1.Event, 1)
	live <- v1.Event{ID: "evt_live", Type: v1.EventMessagePartDelta, SessionID: "ses_test", Data: json.RawMessage(`{"kind":"reasoning_summary","delta":"Checking"}`)}
	durable <- v1.Event{ID: "evt_complete", Type: "session.assistant.complete", SessionID: "ses_test", Data: json.RawMessage(`{"message_id":"msg_test"}`)}
	backend := &stubBackend{stream: &EventStream{Durable: durable, Live: live}}
	server := httptest.NewServer(New(backend, Config{HeartbeatInterval: time.Second}))
	defer server.Close()

	response, err := http.Get(server.URL + "/api/v1/sessions/ses_test/events")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	if _, err := readSSEBlock(reader); err != nil { // server.connected
		t.Fatal(err)
	}
	first, err := readSSEBlock(reader)
	if err != nil {
		t.Fatal(err)
	}
	second, err := readSSEBlock(reader)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(first, "id: evt_live\n") || !strings.Contains(second, "id: evt_complete\n") {
		t.Fatalf("events emitted out of causal order: first=%q second=%q", first, second)
	}
}

func TestSSEDefersNextAssistantDeltaUntilPriorAssistantCompletes(t *testing.T) {
	durable := make(chan v1.Event, 2)
	live := make(chan v1.Event, 2)
	backend := &stubBackend{stream: &EventStream{
		Replay: []v1.Event{{
			ID: "evt_first_started", Type: "session.assistant.started", SessionID: "ses_test",
			Data: json.RawMessage(`{"message_id":"msg_first"}`),
		}},
		Durable: durable,
		Live:    live,
		Close:   func() {},
	}}
	server := httptest.NewServer(New(backend, Config{HeartbeatInterval: time.Second}))
	defer server.Close()

	response, err := http.Get(server.URL + "/api/v1/sessions/ses_test/events")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	if _, err := readSSEBlock(reader); err != nil { // server.connected
		t.Fatal(err)
	}
	if replay, err := readSSEBlock(reader); err != nil || !strings.Contains(replay, "id: evt_first_started\n") {
		t.Fatalf("assistant-start replay = %q, %v", replay, err)
	}

	// Establish the first live renderer stream before making both queues ready.
	live <- v1.Event{
		ID: "evt_first_delta", Type: v1.EventMessagePartDelta, SessionID: "ses_test",
		Data: json.RawMessage(`{"message_id":"msg_first","kind":"text","delta":"first"}`),
	}
	if delta, err := readSSEBlock(reader); err != nil || !strings.Contains(delta, "id: evt_first_delta\n") {
		t.Fatalf("first assistant delta = %q, %v", delta, err)
	}

	// Live and durable events are deliberately ready together. Regardless of
	// which select case wins, the next assistant's delta must remain behind the
	// prior completion and its own durable start event.
	live <- v1.Event{
		ID: "evt_second_delta", Type: v1.EventMessagePartDelta, SessionID: "ses_test",
		Data: json.RawMessage(`{"message_id":"msg_second","kind":"text","delta":"second"}`),
	}
	durable <- v1.Event{
		ID: "evt_first_complete", Type: "session.assistant.complete", SessionID: "ses_test",
		Data: json.RawMessage(`{"message_id":"msg_first"}`),
	}
	durable <- v1.Event{
		ID: "evt_second_started", Type: "session.assistant.started", SessionID: "ses_test",
		Data: json.RawMessage(`{"message_id":"msg_second"}`),
	}

	for i, want := range []string{"evt_first_complete", "evt_second_started", "evt_second_delta"} {
		block, err := readSSEBlock(reader)
		if err != nil {
			t.Fatalf("event %d: %v", i, err)
		}
		if !strings.Contains(block, "id: "+want+"\n") {
			t.Fatalf("event %d = %q; want %s", i, block, want)
		}
	}
}

func TestClientParityNetworkAndInProcess(t *testing.T) {
	for _, transport := range []string{"network", "inproc"} {
		t.Run(transport, func(t *testing.T) {
			backend := &stubBackend{}
			handler := New(backend, Config{})
			var apiClient *client.Client
			if transport == "network" {
				server := httptest.NewServer(handler)
				defer server.Close()
				apiClient, _ = client.New(server.URL, nil)
			} else {
				apiClient, _ = client.New("http://inproc", inproc.New(handler))
			}
			ctx := context.Background()
			health, err := apiClient.Health(ctx)
			if err != nil || health.Status != "ok" {
				t.Fatalf("Health = %#v, %v", health, err)
			}
			created, err := apiClient.CreateSession(ctx, v1.CreateSessionRequest{Title: "test"})
			if err != nil || created.ID != "ses_test" {
				t.Fatalf("CreateSession = %#v, %v", created, err)
			}
			selected, err := apiClient.UpdateSessionSelection(ctx, "ses_test", v1.UpdateSessionSelectionRequest{Agent: "plan"})
			if err != nil || selected.Agent != "plan" || selected.Model != "code" {
				t.Fatalf("UpdateSessionSelection = %#v, %v", selected, err)
			}
			accepted, err := apiClient.Prompt(ctx, "ses_test", v1.PromptRequest{MessageID: "msg_test", Content: "hello", Delivery: "steer"})
			if err != nil || accepted.InputID != "inp_test" {
				t.Fatalf("Prompt = %#v, %v", accepted, err)
			}
			durable := make(chan v1.Event)
			live := make(chan v1.Event)
			var closeOnce sync.Once
			backend.stream = &EventStream{Durable: durable, Live: live, Close: func() {
				closeOnce.Do(func() { close(durable); close(live) })
			}}
			stream, err := apiClient.Events(ctx, "ses_test", nil)
			if err != nil {
				t.Fatal(err)
			}
			item, err := stream.Next()
			if err != nil || item.Type != v1.EventServerConnected {
				t.Fatalf("connected event = %#v, %v", item, err)
			}
			if err := stream.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func readSSEBlock(reader *bufio.Reader) (string, error) {
	var out strings.Builder
	for {
		line, err := reader.ReadString('\n')
		out.WriteString(line)
		if err != nil {
			return out.String(), err
		}
		if line == "\n" {
			return out.String(), nil
		}
	}
}

func assertProblem(t *testing.T, response *httptest.ResponseRecorder, code string) {
	t.Helper()
	if response.Header().Get("Content-Type") != v1.MediaTypeProblem {
		t.Errorf("content type = %q", response.Header().Get("Content-Type"))
	}
	data, _ := io.ReadAll(response.Result().Body)
	var item v1.Problem
	if err := json.Unmarshal(data, &item); err != nil {
		t.Fatalf("decode problem: %v (%s)", err, data)
	}
	if item.Code != code || item.RequestID == "" {
		t.Errorf("problem = %#v", item)
	}
}

func assertAPIProblem(t *testing.T, err error, status int, code string) {
	t.Helper()
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %T %v, want API problem", err, err)
	}
	if apiErr.Problem.Status != status || apiErr.Problem.Code != code {
		t.Fatalf("problem = %#v, want status %d code %q", apiErr.Problem, status, code)
	}
}
