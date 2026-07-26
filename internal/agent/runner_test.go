package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	v1 "github.com/amirulashraf/parrot-coder/internal/api/v1"
	"github.com/amirulashraf/parrot-coder/internal/compaction"
	"github.com/amirulashraf/parrot-coder/internal/event"
	"github.com/amirulashraf/parrot-coder/internal/protocol"
	"github.com/amirulashraf/parrot-coder/internal/provider"
	"github.com/amirulashraf/parrot-coder/internal/security"
	"github.com/amirulashraf/parrot-coder/internal/session"
	statusinfo "github.com/amirulashraf/parrot-coder/internal/status"
	"github.com/amirulashraf/parrot-coder/internal/store"
	"github.com/amirulashraf/parrot-coder/internal/systemcontext"
	"github.com/amirulashraf/parrot-coder/internal/tool"
)

type fakeProvider struct {
	mu          sync.Mutex
	requests    []protocol.Request
	stream      func(int, context.Context, protocol.Request) (provider.Stream, error)
	inputPrice  float64
	outputPrice float64
}

func (*fakeProvider) ID() string { return "fake" }
func (p *fakeProvider) Models() []provider.Model {
	return []provider.Model{{ID: "model", InputPrice: p.inputPrice, OutputPrice: p.outputPrice, Capabilities: provider.Capabilities{Tools: true}}}
}
func (p *fakeProvider) Stream(ctx context.Context, request protocol.Request) (provider.Stream, error) {
	p.mu.Lock()
	index := len(p.requests)
	p.requests = append(p.requests, request)
	p.mu.Unlock()
	return p.stream(index, ctx, request)
}
func (p *fakeProvider) Requests() []protocol.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]protocol.Request(nil), p.requests...)
}

type sliceStream struct {
	events []protocol.Event
	index  int
}

func (s *sliceStream) Next(context.Context) (protocol.Event, error) {
	if s.index == len(s.events) {
		return protocol.Event{}, io.EOF
	}
	event := s.events[s.index]
	s.index++
	return event, nil
}
func (*sliceStream) Close() error { return nil }

func events(items ...protocol.Event) provider.Stream { return &sliceStream{events: items} }

func TestProfileInstructionsArePartOfStatusProvider(t *testing.T) {
	profile := Profile{Prompt: "profile prompt", HardRules: []string{"rule"}, Status: statusinfo.Static{ProviderKey: "profile:test", Text: "profile status"}}
	observation, err := newProfileStatus(profile).Observe(context.Background(), statusinfo.Query{})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := observation.Text, "profile prompt\n\nHard rules:\n- rule\n\nprofile status"; got != want {
		t.Fatalf("profile status = %q, want %q", got, want)
	}
	if got, want := runnerInstructions("baseline", "/state/session/ses_test/scratch", false), "baseline\n\nScratch directory: /state/session/ses_test/scratch"; got != want {
		t.Fatalf("runner instructions = %q, want %q", got, want)
	}
}

type recordingSessionRuntime struct {
	SessionRuntime
	mu  sync.Mutex
	ids []string
}

func (r *recordingSessionRuntime) GetSession(sessionID string) session.UserSession {
	r.mu.Lock()
	r.ids = append(r.ids, sessionID)
	r.mu.Unlock()
	return r.SessionRuntime.GetSession(sessionID)
}

func (r *recordingSessionRuntime) IDs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.ids...)
}

type failingGetSessionRuntime struct {
	SessionRuntime
	sessionID string
}

type failingGetUserSession struct{ session.UserSession }

func (r failingGetSessionRuntime) GetSession(sessionID string) session.UserSession {
	if sessionID == r.sessionID {
		return failingGetUserSession{}
	}
	return r.SessionRuntime.GetSession(sessionID)
}

func (failingGetUserSession) Get(context.Context) (session.AgentSessionDto, error) {
	return session.AgentSessionDto{}, session.ErrNotFound
}

func TestStatusQueryRetainsParentIDWhenParentCannotBeLoaded(t *testing.T) {
	h := newRunnerHarness(t, &fakeProvider{}, nil)
	created, err := NewUserSession(t.Context(), failingGetSessionRuntime{SessionRuntime: h.sessions, sessionID: "ses_deleted_parent"}, h.agentSessions.config)
	if err != nil {
		t.Fatal(err)
	}
	runner := mustGetAgentSession(t, created.(*userSession), h.sessionID).(*agentSession)
	query := runner.statusQuery(context.Background(), session.AgentSessionDto{
		ParentSessionID: "ses_deleted_parent",
		Provider:        "openai",
		Model:           "gpt",
	}, Profile{ID: "build"})
	if query.ParentSessionID != "ses_deleted_parent" || query.ParentSessionName != "" {
		t.Fatalf("parent status = %q (%q), want %q with no details", query.ParentSessionID, query.ParentSessionName, "ses_deleted_parent")
	}
}

func TestRunnerIncludesDirectParentInStatusPrompt(t *testing.T) {
	fake := &fakeProvider{stream: func(_ int, _ context.Context, request protocol.Request) (provider.Stream, error) {
		if !containsRoleSubstring(request.Messages, protocol.RoleSystem, "Parent session: ") {
			return nil, fmt.Errorf("request missing parent status: %#v", request.Messages)
		}
		return events(protocol.Event{Type: protocol.EventFinish, FinishReason: protocol.FinishStop}), nil
	}}
	h := newRunnerHarness(t, fake, nil)
	if _, err := h.sessions.GetSession(h.sessionID).UpdateSelection(context.Background(), session.SelectionPatch{Agent: "root-agent"}, nil); err != nil {
		t.Fatal(err)
	}
	parent := mustGetAgentSession(t, h.agentSessions, h.sessionID)
	child, err := h.agentSessions.repository.CreateChild(context.Background(), parent, ChildSessionRequest{
		ProjectID: h.runner.dto.ProjectID, Name: "inspect", Agent: BuildID,
		DefaultSelection: session.Selection{Provider: "fake", Model: "model"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.sessions.GetSession(child.ID()).UpdateSelection(context.Background(), session.SelectionPatch{Agent: "direct-parent-agent"}, nil); err != nil {
		t.Fatal(err)
	}
	nested, err := h.agentSessions.repository.CreateChild(context.Background(), child, ChildSessionRequest{
		ProjectID: h.runner.dto.ProjectID, Name: "nested", Agent: BuildID,
		DefaultSelection: session.Selection{Provider: "fake", Model: "model"},
	})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := statusinfo.NewRegistry(statusinfo.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	nestedRunner := nested.(*agentSession)
	nestedRunner.config.Status = registry
	if _, err := nested.Prompt(context.Background(), "work"); err != nil {
		t.Fatal(err)
	}

	requests := fake.Requests()
	if len(requests) != 1 {
		t.Fatalf("request count = %d, want 1", len(requests))
	}
	want := "Parent session: " + child.ID() + " (inspect)"
	if !containsRoleSubstring(requests[0].Messages, protocol.RoleSystem, want) {
		t.Fatalf("status does not contain %q: %#v", want, requests[0].Messages)
	}
	if containsRoleSubstring(requests[0].Messages, protocol.RoleSystem, "Parent agent:") {
		t.Fatalf("status contains parent agent profile: %#v", requests[0].Messages)
	}
	if containsRoleSubstring(requests[0].Messages, protocol.RoleSystem, "Parent session: "+h.sessionID) {
		t.Fatalf("status contains root instead of direct parent: %#v", requests[0].Messages)
	}
}

func TestRunnerAppendsComposedStatusOnlyWhenPending(t *testing.T) {
	fake := &fakeProvider{stream: func(_ int, _ context.Context, _ protocol.Request) (provider.Stream, error) {
		return events(protocol.Event{Type: protocol.EventTextDelta, Text: "done"}, protocol.Event{Type: protocol.EventFinish, FinishReason: protocol.FinishStop}), nil
	}}
	profiles := []Profile{
		{ID: "build", Prompt: "build prompt", MaxTurns: 10, Status: statusinfo.Static{ProviderKey: "profile:mode", Text: "Build mode status"}},
		{ID: "plan", Prompt: "plan prompt", MaxTurns: 10, Status: statusinfo.Static{ProviderKey: "profile:mode", Text: "Plan mode status"}},
	}
	h := newRunnerHarness(t, fake, profiles)
	registry, err := statusinfo.NewRegistry(statusinfo.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	h.runner.config.Status = registry
	live := &recordingPublisher{}
	h.runner.config.Live = live
	if err := h.runner.drainOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	pending, err := h.sessions.GetSession(h.sessionID).StatusPromptPending(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !pending || len(fake.Requests()) != 0 {
		t.Fatalf("idle drain consumed status: pending=%t requests=%d", pending, len(fake.Requests()))
	}
	for _, text := range []string{"first", "second"} {
		h.admit(t, text, text, session.DeliverySteer)
		if err := h.runner.drainOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.sessions.GetSession(h.sessionID).UpdateSelection(context.Background(), session.SelectionPatch{Agent: "plan"}, nil); err != nil {
		t.Fatal(err)
	}
	h.admit(t, "user", "third", session.DeliverySteer)
	if err := h.runner.drainOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	requests := fake.Requests()
	if len(requests) != 3 {
		t.Fatalf("request count = %d, want 3", len(requests))
	}
	buildStatus := "build prompt\n\nBuild mode status\n\nActive profile: build\nModel: fake/model"
	planStatus := "plan prompt\n\nPlan mode status\n\nActive profile: plan\nModel: fake/model"
	for index, request := range requests {
		if !strings.HasPrefix(request.Instructions, "baseline\n\nScratch directory: ") || !strings.HasSuffix(request.Instructions, "/session/"+h.sessionID+"/scratch") {
			t.Errorf("request %d instructions = %q, want baseline and session scratch path", index, request.Instructions)
		}
		var statuses []string
		for _, message := range request.Messages {
			if message.Role == protocol.RoleSystem {
				statuses = append(statuses, message.Content[0].Text)
			}
		}
		want := []string{buildStatus}
		if index == 2 {
			want = append(want, planStatus)
		}
		if !reflect.DeepEqual(statuses, want) {
			t.Errorf("request %d statuses = %#v, want %#v", index, statuses, want)
		}
	}
	var statusEvents int
	for _, item := range live.events {
		if item.Type == protocol.EventStatusPromptInjected {
			statusEvents++
			if item.Text != "Status prompt injected" {
				t.Fatalf("status prompt event text = %q", item.Text)
			}
		}
	}
	if statusEvents != 2 {
		t.Fatalf("status prompt event count = %d, want 2", statusEvents)
	}
}

// recordingPublisher captures everything the runner puts on the live stream.
type recordingPublisher struct {
	mu     sync.Mutex
	events []protocol.Event
}

func (p *recordingPublisher) Publish(_ string, item protocol.Event) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if item.Usage != nil {
		copied := *item.Usage
		item.Usage = &copied
	}
	p.events = append(p.events, item)
}

func TestToolDisplayPublisherAddsExecutionIdentity(t *testing.T) {
	live := &recordingPublisher{}
	toolDisplayPublisher{live: live, sessionID: "session", callID: "call"}.DisplayCode(tool.CodeDisplay{
		Source: "package main\n", Path: "main.go", Language: "go", StartLine: 3,
	})
	if len(live.events) != 1 || live.events[0].Type != protocol.EventCodeDisplay || live.events[0].CodeDisplay == nil {
		t.Fatalf("events = %#v", live.events)
	}
	display := live.events[0].CodeDisplay
	if display.ToolCallID != "call" || display.Source != "package main\n" || display.Path != "main.go" || display.StartLine != 3 {
		t.Fatalf("display = %#v", display)
	}
}

// The live usage event is the only carrier of cost to clients, so it must be
// priced before publication rather than afterwards on a copy the runner keeps.
func TestRunnerPublishesPricedUsage(t *testing.T) {
	fake := &fakeProvider{inputPrice: 0.001, outputPrice: 0.002, stream: func(_ int, _ context.Context, _ protocol.Request) (provider.Stream, error) {
		return events(
			protocol.Event{Type: protocol.EventTextDelta, Text: "done"},
			protocol.Event{Type: protocol.EventUsage, Usage: &protocol.Usage{InputTokens: 100, OutputTokens: 50, TotalTokens: 150}},
			protocol.Event{Type: protocol.EventFinish, FinishReason: protocol.FinishStop},
		), nil
	}}
	live := &recordingPublisher{}
	h := newRunnerHarness(t, fake, nil)
	h.runner.config.Live = live
	h.admit(t, "user", "work", session.DeliverySteer)
	if err := h.runner.drainOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	var published *protocol.Usage
	for _, item := range live.events {
		if item.Type == protocol.EventUsage {
			published = item.Usage
		}
	}
	if published == nil {
		t.Fatal("no usage event published")
	}
	if published.InputCost != 0.1 || published.OutputCost != 0.1 {
		t.Fatalf("published usage costs = %v/%v, want 0.1/0.1", published.InputCost, published.OutputCost)
	}
}

func TestReasoningSummaryAccumulatorPreservesPartOrder(t *testing.T) {
	var summary reasoningSummaryAccumulator
	summary.Write("reasoning:0", "First")
	summary.Write("reasoning:1", "Second item")
	summary.Write("reasoning:0", " item")
	summary.Set("reasoning:1", "Final second item")
	if got, want := summary.String(), "First itemFinal second item"; got != want {
		t.Fatalf("summary = %q, want %q", got, want)
	}
	if summary.Len() != len("First itemFinal second item") {
		t.Fatalf("summary length = %d", summary.Len())
	}
}

type blockingStream struct {
	started chan struct{}
	release <-chan struct{}
	events  *sliceStream
	once    sync.Once
}

func (s *blockingStream) Next(ctx context.Context) (protocol.Event, error) {
	s.once.Do(func() { close(s.started) })
	select {
	case <-s.release:
		return s.events.Next(ctx)
	case <-ctx.Done():
		return protocol.Event{}, ctx.Err()
	}
}
func (*blockingStream) Close() error { return nil }

type callThenBlockingStream struct {
	call    protocol.ToolCall
	started chan struct{}
	emitted bool
}

func (s *callThenBlockingStream) Next(ctx context.Context) (protocol.Event, error) {
	if !s.emitted {
		s.emitted = true
		return protocol.Event{Type: protocol.EventToolCallComplete, ToolCall: &s.call}, nil
	}
	close(s.started)
	<-ctx.Done()
	return protocol.Event{}, ctx.Err()
}

func (*callThenBlockingStream) Close() error { return nil }

type fakeTool struct {
	tool.BasePresentation
	id      string
	execute func(context.Context) (tool.Result, error)
}

func (t *fakeTool) ID() string { return t.id }
func (t *fakeTool) Descriptor() tool.Descriptor {
	return tool.Descriptor{ID: t.ID(), Description: t.id, Schema: t.JSONSchema(), Presentation: t.Presentation()}
}
func (t *fakeTool) DescribeRequest(json.RawMessage) (string, error) { return t.id, nil }
func (t *fakeTool) JSONSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","additionalProperties":true}`)
}
func (t *fakeTool) Plan(_ context.Context, raw json.RawMessage, _ tool.CallContext) (tool.Plan, error) {
	return tool.NewPlan(t.id, raw, nil, nil, nil)
}
func (t *fakeTool) Execute(ctx context.Context, _ tool.Plan, call tool.CallContext) (tool.Result, error) {
	if call.SecurityProfile != nil && call.SecurityProfile.IsReadOnly() {
		return tool.Result{}, errors.New("tool is not permitted by the current security profile")
	}
	if t.execute == nil {
		return tool.Result{Text: "ok", ModelText: "ok"}, nil
	}
	return t.execute(ctx)
}

type runnerHarness struct {
	db            *store.DB
	sessions      *session.Service
	goals         *session.GoalService
	repository    *event.Repository
	agentSessions *userSession
	sessionID     string
	runner        *agentSession
}

type fakeCompactor struct {
	requests []compaction.Request
	compact  func(compaction.Request) (compaction.Result, error)
}

func (c *fakeCompactor) Compact(_ context.Context, request compaction.Request) (compaction.Result, error) {
	c.requests = append(c.requests, request)
	if c.compact != nil {
		return c.compact(request)
	}
	return compaction.Result{Status: "skipped"}, nil
}

func mustGetAgentSession(t *testing.T, sessions *userSession, id string) AgentSession {
	t.Helper()
	created, err := sessions.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	return created
}

func newRunnerHarness(t *testing.T, fake *fakeProvider, profiles []Profile, tools ...tool.Tool) *runnerHarness {
	t.Helper()
	ctx := context.Background()
	db := store.NewRegistry(t.TempDir(), "host-test")
	t.Cleanup(func() { db.Close() })
	repository := event.NewRepository(db)
	sessions := session.NewService(db, repository)
	goals := session.NewGoalService(db, repository)
	created, err := sessions.Create(ctx, session.CreateParams{Title: "runner"})
	if err != nil {
		t.Fatal(err)
	}
	agents, err := NewRegistry(profiles...)
	if err != nil {
		t.Fatal(err)
	}
	profile := BuildID
	if len(profiles) > 0 {
		profile = profiles[0].ID
	}
	if err := sessions.GetSession(created.ID).SetSelection(ctx, session.Selection{Agent: profile, Provider: "fake", Model: "model"}); err != nil {
		t.Fatal(err)
	}
	providers, err := NewProviderRegistry(fake)
	if err != nil {
		t.Fatal(err)
	}
	providersList := make([]tool.ToolProvider, 0, len(tools))
	for _, item := range tools {
		item := item
		providersList = append(providersList, &tool.ProviderFunc{ToolDescriptor: tool.DescriptorOf(item), CreateTool: func(tool.AgentSession) (tool.Tool, error) { return item, nil }})
	}
	toolProviders, err := tool.NewProviders(providersList...)
	if err != nil {
		t.Fatal(err)
	}
	contextRegistry, _ := systemcontext.NewRegistry(systemcontext.StaticSource{SourceKey: "agent:context", Text: "baseline"})
	createdAgentSessions, err := NewUserSession(ctx, sessions, UserSessionConfig{AgentSession: AgentSessionConfig{
		Contexts:           systemcontext.Manager{Registry: contextRegistry, Store: sessions},
		StateDirectories:   testSessionStateDirectories(t),
		Agents:             agents,
		Providers:          providers,
		ToolProviders:      toolProviders,
		Goals:              goals,
		MaxConcurrentTools: 2,
		CleanupTimeout:     time.Second,
	}, MaxConcurrentChildTurns: 8, MaxConcurrentChildTurnsPerParent: 4})
	if err != nil {
		t.Fatal(err)
	}
	agentSessions := createdAgentSessions.(*userSession)
	runtimeSession, err := agentSessions.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	runner := runtimeSession.(*agentSession)
	sessionDB, err := db.Session(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	return &runnerHarness{db: sessionDB, sessions: sessions, goals: goals, repository: repository, agentSessions: agentSessions, sessionID: created.ID, runner: runner}
}

func TestRunningSendCannotEscapeCompletingManagedTurn(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	fake := &fakeProvider{stream: func(index int, _ context.Context, request protocol.Request) (provider.Stream, error) {
		if index == 0 {
			close(started)
			<-release
		}
		last := request.Messages[len(request.Messages)-1]
		return events(
			protocol.Event{Type: protocol.EventTextDelta, Text: "answer-" + last.Content[0].Text},
			protocol.Event{Type: protocol.EventFinish, FinishReason: protocol.FinishStop},
		), nil
	}}
	h := newRunnerHarness(t, fake, nil)
	parent := mustGetAgentSession(t, h.agentSessions, h.sessionID)
	child, err := parent.CreateChild(context.Background(), ChildRequest{Prompt: "initial", Agent: BuildID, Name: "serialized"})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	item := child.(*agentSession)
	item.childOp.Lock()
	close(release)
	for deadline := time.Now().Add(time.Second); ; {
		item.mu.Lock()
		idle := item.drain == nil
		item.mu.Unlock()
		if idle {
			break
		}
		if time.Now().After(deadline) {
			item.childOp.Unlock()
			t.Fatal("child drain did not become idle")
		}
		runtime.Gosched()
	}

	type sendResult struct {
		status    Status
		messageID string
		err       error
	}
	sent := make(chan sendResult, 1)
	go func() {
		messageID, sendErr := child.Send(context.Background(), "follow-up")
		sent <- sendResult{status: child.Status(), messageID: messageID, err: sendErr}
	}()
	select {
	case result := <-sent:
		item.childOp.Unlock()
		t.Fatalf("Send escaped managed completion: %#v", result)
	case <-time.After(20 * time.Millisecond):
	}
	item.childOp.Unlock()
	result := <-sent
	if result.err != nil || result.messageID != "" || result.status.Turn != 2 || result.status.State != StatusRunning {
		t.Fatalf("serialized Send = %#v", result)
	}
	observation, err := child.Observe()
	if err != nil {
		t.Fatal(err)
	}
	completed, err := observation.Wait(context.Background())
	if err != nil || completed.Output != "answer-follow-up" {
		t.Fatalf("follow-up = %#v, %v", completed, err)
	}
}

func TestAgentSessionOwnsReusableChildLifecycle(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	fake := &fakeProvider{stream: func(index int, _ context.Context, request protocol.Request) (provider.Stream, error) {
		last := request.Messages[len(request.Messages)-1]
		prompt := last.Content[0].Text
		if index == 0 {
			started <- struct{}{}
			<-release
		}
		return events(
			protocol.Event{Type: protocol.EventTextDelta, Text: "answer-" + prompt},
			protocol.Event{Type: protocol.EventFinish, FinishReason: protocol.FinishStop},
		), nil
	}}
	h := newRunnerHarness(t, fake, nil)
	parent := mustGetAgentSession(t, h.agentSessions, h.sessionID)
	parentStatus := parent.Status()
	if parentStatus.SessionID != h.sessionID || parentStatus.RootSession != h.sessionID || parentStatus.ParentSession != "" || parentStatus.State != StatusIdle {
		t.Fatalf("parent status = %#v", parentStatus)
	}
	child, err := parent.CreateChild(context.Background(), ChildRequest{Prompt: "initial", Agent: BuildID, Name: "inspect"})
	if err != nil {
		t.Fatal(err)
	}
	observation, err := child.Observe()
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("child turn did not start")
	}

	messageID, err := child.Send(context.Background(), "steer")
	status := child.Status()
	if err != nil || messageID == "" || status.Turn != 1 || status.State != StatusRunning {
		t.Fatalf("running Send = %#v, %q, %v", status, messageID, err)
	}
	close(release)
	completed, err := observation.Wait(context.Background())
	status = child.Status()
	if err != nil || completed.State != StatusSucceeded || completed.Output != "answer-steer" || status.State != StatusSucceeded || status.Output != completed.Output || status.SessionID != child.ID() || status.ParentSession != parent.ID() || status.RootSession != parent.ID() {
		t.Fatalf("first turn = %#v, status = %#v, %v", completed, status, err)
	}

	messageID, err = child.Send(context.Background(), "follow-up")
	status = child.Status()
	if err != nil || messageID != "" || status.Turn != 2 || status.State != StatusRunning {
		t.Fatalf("follow-up Send = %#v, %q, %v", status, messageID, err)
	}
	observation, err = child.Observe()
	if err != nil {
		t.Fatal(err)
	}
	completed, err = observation.Wait(context.Background())
	if err != nil || completed.Turn != 2 || completed.Output != "answer-follow-up" {
		t.Fatalf("second turn = %#v, %v", completed, err)
	}
	resolved, err := parent.ResolveChild("inspect")
	if err != nil || resolved != child {
		t.Fatalf("ResolveChild = %#v, %v", resolved, err)
	}
	nested, err := child.CreateChild(context.Background(), ChildRequest{Prompt: "nested", Agent: BuildID, Name: "nested-inspect"})
	if err != nil {
		t.Fatal(err)
	}
	nestedObservation, err := nested.Observe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := nestedObservation.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, identifier := range []string{nested.ID(), "nested-inspect"} {
		resolved, err = parent.ResolveChild(identifier)
		if err != nil || resolved != nested {
			t.Fatalf("ResolveChild(%q) = %#v, %v", identifier, resolved, err)
		}
	}
	if err := nested.(*agentSession).forget(); err != nil {
		t.Fatal(err)
	}
	if err := child.(*agentSession).forget(); err != nil {
		t.Fatal(err)
	}
	if _, err := parent.ResolveChild(child.ID()); !errors.Is(err, ErrChildNotFound) {
		t.Fatalf("ResolveChild after Forget = %v", err)
	}
}

func TestAgentToolSessionResolvesDirectParentAndDescendantsOnly(t *testing.T) {
	fake := &fakeProvider{stream: func(_ int, _ context.Context, request protocol.Request) (provider.Stream, error) {
		prompt := request.Messages[len(request.Messages)-1].Content[0].Text
		return events(
			protocol.Event{Type: protocol.EventTextDelta, Text: "answer-" + prompt},
			protocol.Event{Type: protocol.EventFinish, FinishReason: protocol.FinishStop},
		), nil
	}}
	h := newRunnerHarness(t, fake, nil)
	root := mustGetAgentSession(t, h.agentSessions, h.sessionID)
	createCompleted := func(parent AgentSession, prompt, name, agent string) AgentSession {
		t.Helper()
		child, err := parent.CreateChild(context.Background(), ChildRequest{Prompt: prompt, Agent: agent, Name: name})
		if err != nil {
			t.Fatal(err)
		}
		observation, err := child.Observe()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := observation.Wait(context.Background()); err != nil {
			t.Fatal(err)
		}
		return child
	}
	parent := createCompleted(root, "parent", "direct-parent", WorkerID)
	subject := createCompleted(parent, "subject", "subject", ExplorerID)
	descendant := createCompleted(subject, "descendant", "descendant", WorkerID)
	sibling := createCompleted(parent, "sibling", "sibling", ExplorerID)
	unrelated := createCompleted(root, "unrelated", "unrelated", ExplorerID)
	session := agentToolSession{session: subject.(*agentSession)}
	if !session.IsSubagent() || (agentToolSession{session: root.(*agentSession)}).IsSubagent() {
		t.Fatal("agent tool sessions did not reflect their parent hierarchy")
	}

	for _, testCase := range []struct {
		identifier   string
		wantID       string
		relationship tool.AgentRelationship
	}{
		{identifier: parent.ID(), wantID: parent.ID(), relationship: tool.AgentRelationshipParent},
		{identifier: "direct-parent", wantID: parent.ID(), relationship: tool.AgentRelationshipParent},
		{identifier: descendant.ID(), wantID: descendant.ID(), relationship: tool.AgentRelationshipDescendant},
		{identifier: "descendant", wantID: descendant.ID(), relationship: tool.AgentRelationshipDescendant},
	} {
		resolved, err := session.ResolveAgent(testCase.identifier)
		if err != nil || resolved.Agent.Status().SessionID != testCase.wantID || resolved.Relationship != testCase.relationship {
			t.Fatalf("ResolveAgent(%q) = %#v, %v", testCase.identifier, resolved, err)
		}
	}
	for _, identifier := range []string{sibling.ID(), "sibling", unrelated.ID(), "unrelated", root.ID()} {
		if _, err := session.ResolveAgent(identifier); err == nil {
			t.Fatalf("ResolveAgent(%q) unexpectedly succeeded", identifier)
		}
	}

	send := &tool.AgentTool{Kind: "agent_send", Session: session, Agents: func(string) (bool, error) { return false, nil }}
	call := tool.CallContext{SessionID: subject.ID(), Agent: ExplorerID}
	plan, err := send.Plan(context.Background(), json.RawMessage(`{"session_id":"direct-parent","message":"report upward"}`), call)
	if err != nil {
		t.Fatal(err)
	}
	result, err := send.Execute(context.Background(), plan, call)
	if err != nil || result.Metadata["message_id"] != nil || result.Metadata["session_id"] != parent.ID() || result.Metadata["status"] != "running" {
		t.Fatalf("Execute() = %#v, %v", result, err)
	}
	observation, err := parent.Observe()
	if err != nil {
		t.Fatal(err)
	}
	completed, err := observation.Wait(context.Background())
	if err != nil || completed.Output != "answer-Agent message from subject:\n\nreport upward" {
		t.Fatalf("parent follow-up = %#v, %v", completed, err)
	}
}

func TestManagedAgentSessionInterruptsItself(t *testing.T) {
	started := make(chan struct{})
	fake := &fakeProvider{stream: func(_ int, ctx context.Context, _ protocol.Request) (provider.Stream, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	h := newRunnerHarness(t, fake, nil)
	child, err := mustGetAgentSession(t, h.agentSessions, h.sessionID).CreateChild(context.Background(), ChildRequest{Prompt: "initial", Agent: BuildID})
	if err != nil {
		t.Fatal(err)
	}
	<-started

	if err := child.Interrupt(context.Background()); err != nil {
		t.Fatal(err)
	}
	status := child.Status()
	if status.State != StatusCanceled || status.Error != ErrChildCanceled.Error() {
		t.Fatalf("status = %#v", status)
	}
}

func TestAgentSessionPromptAndSendOwnAdmissionAndResultHandling(t *testing.T) {
	fake := &fakeProvider{stream: func(index int, _ context.Context, request protocol.Request) (provider.Stream, error) {
		want := []string{"initial", "steer"}[index]
		last := request.Messages[len(request.Messages)-1]
		if last.Role != protocol.RoleUser || last.Content[0].Text != want {
			return nil, fmt.Errorf("request %d last message = %#v, want user %q", index, last, want)
		}
		return events(
			protocol.Event{Type: protocol.EventTextDelta, Text: "answer-" + want},
			protocol.Event{Type: protocol.EventFinish, FinishReason: protocol.FinishStop},
		), nil
	}}
	h := newRunnerHarness(t, fake, nil)

	result, err := h.runner.Prompt(context.Background(), "initial")
	if err != nil || result != "answer-initial" {
		t.Fatalf("Prompt = %q, %v; want answer-initial", result, err)
	}
	messageID, err := h.runner.Send(context.Background(), "steer")
	if err != nil || !strings.HasPrefix(messageID, "msg_") {
		t.Fatalf("Send = %q, %v; want message ID", messageID, err)
	}
	if err := h.runner.Resume(context.Background()); err != nil {
		t.Fatal(err)
	}
	messages, err := h.sessions.GetSession(h.sessionID).ListMessages(context.Background())
	if err != nil || messages[len(messages)-1].Content != "answer-steer" {
		t.Fatalf("last message = %#v, %v; want answer-steer", messages[len(messages)-1], err)
	}
}

type childCreatedObserverFunc func(ChildSession)

func (f childCreatedObserverFunc) ChildCreated(child ChildSession) { f(child) }

func TestAgentSessionRepositoryCreatesSelectedChild(t *testing.T) {
	h := newRunnerHarness(t, &fakeProvider{}, nil)
	parent, err := h.sessions.GetSession(h.sessionID).Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var observed ChildSession
	h.agentSessions.AddChildCreatedObserver(childCreatedObserverFunc(func(child ChildSession) {
		observed = child
		if relationParent, ok := h.agentSessions.ChildRelation(child.SessionID); !ok || relationParent != child.ParentSessionID {
			t.Fatalf("relationship unavailable to observer: %q, %v", relationParent, ok)
		}
		if _, ok := h.agentSessions.Lookup(child.SessionID); !ok {
			t.Fatal("runtime unavailable to observer")
		}
	}))
	parentRuntime := mustGetAgentSession(t, h.agentSessions, parent.ID)
	child, err := h.agentSessions.repository.CreateChild(context.Background(), parentRuntime, ChildSessionRequest{
		ProjectID:        parent.ProjectID,
		Name:             "inspect",
		Agent:            BuildID,
		Model:            "fake/model",
		DefaultSelection: session.Selection{Provider: "fake", Model: "model"},
	})
	if err != nil {
		t.Fatal(err)
	}
	selected, err := h.sessions.GetSession(child.ID()).Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if selected.ParentSessionID != parent.ID || selected.Name != "inspect" || selected.Agent != BuildID || selected.Provider != "fake" || selected.Model != "model" || selected.Title != "Subtask inspect [build]" {
		t.Fatalf("child = %#v", selected)
	}
	if bound, ok := h.agentSessions.Lookup(selected.ID); !ok || bound != child {
		t.Fatal("created child was not bound in the repository")
	}
	if child.Name() != "inspect" || child.Parent() != parentRuntime {
		t.Fatalf("created child name/parent = %q, %#v", child.Name(), child.Parent())
	}
	wantRelation := ChildSession{SessionID: selected.ID, ParentSessionID: parent.ID}
	if observed != wantRelation {
		t.Fatalf("observed child = %#v, want %#v", observed, wantRelation)
	}
	children := h.agentSessions.ChildSessions(parent.ID)
	if len(children) != 1 || children[0] != wantRelation {
		t.Fatalf("ChildSessions = %#v, want %#v", children, []ChildSession{wantRelation})
	}
}

type deleteFailSessionRuntime struct {
	SessionRuntime
	err error
}

type deleteFailUserSession struct {
	session.UserSession
	err error
}

func (r deleteFailSessionRuntime) GetSession(sessionID string) session.UserSession {
	return deleteFailUserSession{UserSession: r.SessionRuntime.GetSession(sessionID), err: r.err}
}

func (r deleteFailUserSession) Delete(context.Context) error { return r.err }

func TestAgentSessionsResolvePersistenceThroughOwningUserSession(t *testing.T) {
	h := newRunnerHarness(t, &fakeProvider{}, nil)
	recording := &recordingSessionRuntime{SessionRuntime: h.sessions}
	created, err := NewUserSession(t.Context(), recording, h.agentSessions.config)
	if err != nil {
		t.Fatal(err)
	}
	agentSessions := created.(*userSession)
	parent := mustGetAgentSession(t, agentSessions, h.sessionID)
	if _, err := parent.(*agentSession).admit(context.Background(), "root input"); err != nil {
		t.Fatal(err)
	}
	child, err := agentSessions.repository.CreateChild(context.Background(), parent, ChildSessionRequest{
		ProjectID: parent.(*agentSession).dto.ProjectID, Name: "bound", Agent: BuildID,
		DefaultSelection: session.Selection{Provider: "fake", Model: "model"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := child.(*agentSession).admit(context.Background(), "child input"); err != nil {
		t.Fatal(err)
	}
	ids := recording.IDs()
	if len(ids) != 3 || ids[0] != parent.ID() || ids[1] != parent.ID() || ids[2] != child.ID() {
		t.Fatalf("persistent session resolutions = %q, want [%q %q %q]", ids, parent.ID(), parent.ID(), child.ID())
	}
}

func TestAgentSessionRepositoryRollsBackChildWhenToolsFail(t *testing.T) {
	h := newRunnerHarness(t, &fakeProvider{}, nil)
	providerErr := errors.New("child provider failed")
	provider := &tool.ProviderFunc{ToolDescriptor: tool.DescriptorOf(&fakeTool{id: "bound"}), CreateTool: func(state tool.AgentSession) (tool.Tool, error) {
		if state.SessionID() != h.sessionID {
			return nil, providerErr
		}
		return &fakeTool{id: "bound"}, nil
	}}
	providers, err := tool.NewProviders(provider)
	if err != nil {
		t.Fatal(err)
	}
	h.agentSessions.repository.config.ToolProviders = providers
	parent := mustGetAgentSession(t, h.agentSessions, h.sessionID)
	before, err := h.sessions.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if child, err := h.agentSessions.repository.CreateChild(context.Background(), parent, ChildSessionRequest{ProjectID: parent.(*agentSession).dto.ProjectID, Name: "rollback", Agent: BuildID, DefaultSelection: session.Selection{Provider: "fake", Model: "model"}}); !errors.Is(err, providerErr) || child != nil {
		t.Fatalf("CreateChild = %#v, %v", child, err)
	}
	after, err := h.sessions.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) || h.agentSessions.HasChildSessions(h.sessionID) {
		t.Fatalf("failed child remained: sessions=%d want=%d children=%#v", len(after), len(before), h.agentSessions.ChildSessions(h.sessionID))
	}

	cleanupErr := errors.New("delete failed")
	cleanupConfig := h.agentSessions.config
	cleanupConfig.AgentSession.ToolProviders = providers
	created, err := NewUserSession(t.Context(), deleteFailSessionRuntime{SessionRuntime: h.sessions, err: cleanupErr}, cleanupConfig)
	if err != nil {
		t.Fatal(err)
	}
	cleanupSessions := created.(*userSession)
	parent = mustGetAgentSession(t, cleanupSessions, h.sessionID)
	if child, err := cleanupSessions.repository.CreateChild(context.Background(), parent, ChildSessionRequest{ProjectID: parent.(*agentSession).dto.ProjectID, Name: "retained", Agent: BuildID, DefaultSelection: session.Selection{Provider: "fake", Model: "model"}}); !errors.Is(err, providerErr) || !errors.Is(err, cleanupErr) || child != nil {
		t.Fatalf("CreateChild cleanup failure = %#v, %v", child, err)
	}
	children := cleanupSessions.ChildSessions(h.sessionID)
	if len(children) != 1 {
		t.Fatalf("cleanup failure children = %#v, want retained relation", children)
	}
	if _, err := h.sessions.GetSession(children[0].SessionID).Get(context.Background()); err != nil {
		t.Fatalf("cleanup failure did not retain durable child: %v", err)
	}
}

func TestAgentSessionRepositoryDiscardsPreparedChild(t *testing.T) {
	h := newRunnerHarness(t, &fakeProvider{}, nil)
	parent, err := h.sessions.GetSession(h.sessionID).Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	child, err := h.agentSessions.repository.CreateChild(context.Background(), mustGetAgentSession(t, h.agentSessions, parent.ID), ChildSessionRequest{
		ProjectID:        parent.ProjectID,
		Name:             "discard",
		Agent:            BuildID,
		DefaultSelection: session.Selection{Provider: "fake", Model: "model"},
	})
	if err != nil {
		t.Fatal(err)
	}
	childID := child.ID()
	if err := h.agentSessions.repository.DiscardChild(context.Background(), childID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.sessions.GetSession(childID).Get(context.Background()); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("durable child lookup error = %v, want session not found", err)
	}
	if _, ok := h.agentSessions.Lookup(childID); ok {
		t.Fatal("discarded child retained its runtime")
	}
	if _, ok := h.agentSessions.ChildRelation(childID); ok {
		t.Fatal("discarded child retained its hierarchy relation")
	}
}

func TestCreateChildIsAtomicWithParentRemoval(t *testing.T) {
	fake := &fakeProvider{stream: func(_ int, _ context.Context, _ protocol.Request) (provider.Stream, error) {
		return events(protocol.Event{Type: protocol.EventFinish, FinishReason: protocol.FinishStop}), nil
	}}
	h := newRunnerHarness(t, fake, nil)
	parent := mustGetAgentSession(t, h.agentSessions, h.sessionID).(*agentSession)

	// Block after CreateChild reserves the parent but before it performs durable
	// child creation. Remove must observe that reservation without either side
	// holding the repository lock while waiting for the runtime boundary.
	h.agentSessions.childMu.Lock()
	created := make(chan struct {
		child AgentSession
		err   error
	}, 1)
	go func() {
		child, err := parent.CreateChild(context.Background(), ChildRequest{Prompt: "race", Agent: BuildID, Name: "reserved"})
		created <- struct {
			child AgentSession
			err   error
		}{child: child, err: err}
	}()
	for deadline := time.Now().Add(time.Second); ; {
		parent.mu.Lock()
		reserved := parent.childCreations
		parent.mu.Unlock()
		if reserved != 0 {
			break
		}
		if time.Now().After(deadline) {
			h.agentSessions.childMu.Unlock()
			t.Fatal("CreateChild did not reserve its parent")
		}
		runtime.Gosched()
	}
	if err := h.agentSessions.Remove(parent.ID()); !errors.Is(err, ErrAgentSessionActive) {
		h.agentSessions.childMu.Unlock()
		t.Fatalf("Remove during admitted CreateChild = %v", err)
	}
	h.agentSessions.childMu.Unlock()
	result := <-created
	if result.err != nil {
		t.Fatal(result.err)
	}
	observation, err := result.child.Observe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := observation.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}

	other := newRunnerHarness(t, fake, nil)
	retired := mustGetAgentSession(t, other.agentSessions, other.sessionID)
	if err := other.agentSessions.Remove(retired.ID()); err != nil {
		t.Fatal(err)
	}
	if _, err := retired.CreateChild(context.Background(), ChildRequest{Prompt: "late", Agent: BuildID}); !errors.Is(err, ErrAgentSessionRemoved) {
		t.Fatalf("CreateChild on retired parent = %v", err)
	}
}

func TestRemoveIsAtomicWithIdleSessionAdmission(t *testing.T) {
	fake := &fakeProvider{stream: func(_ int, _ context.Context, _ protocol.Request) (provider.Stream, error) {
		return events(protocol.Event{Type: protocol.EventFinish, FinishReason: protocol.FinishStop}), nil
	}}
	h := newRunnerHarness(t, fake, nil)
	runtime := mustGetAgentSession(t, h.agentSessions, h.sessionID).(*agentSession)
	release := make(chan struct{})
	runtime.execute = func(context.Context) error {
		<-release
		return nil
	}

	// Hold the runtime boundary until both operations are waiting on it. Whichever
	// operation acquires it first must exclude the other: removal retires this
	// runtime, while admission makes it active before Remove can inspect it.
	runtime.mu.Lock()
	sendResult := make(chan error, 1)
	removeResult := make(chan error, 1)
	go func() {
		_, err := runtime.Send(context.Background(), "race")
		sendResult <- err
	}()
	go func() { removeResult <- h.agentSessions.Remove(h.sessionID) }()
	runtime.mu.Unlock()

	sendErr, removeErr := <-sendResult, <-removeResult
	switch {
	case sendErr == nil:
		if !errors.Is(removeErr, ErrAgentSessionActive) {
			t.Fatalf("admitted Send escaped removal: Send=%v Remove=%v", sendErr, removeErr)
		}
		close(release)
		if err := runtime.Resume(context.Background()); err != nil {
			t.Fatal(err)
		}
	case removeErr == nil:
		if !errors.Is(sendErr, ErrAgentSessionRemoved) {
			t.Fatalf("removed runtime admitted work: Send=%v Remove=%v", sendErr, removeErr)
		}
		close(release)
	default:
		close(release)
		t.Fatalf("Send=%v Remove=%v", sendErr, removeErr)
	}
}

func TestForgetAndRemoveDoNotDeadlock(t *testing.T) {
	for range 100 {
		fake := &fakeProvider{stream: func(_ int, _ context.Context, _ protocol.Request) (provider.Stream, error) {
			return events(protocol.Event{Type: protocol.EventFinish, FinishReason: protocol.FinishStop}), nil
		}}
		h := newRunnerHarness(t, fake, nil)
		parent := mustGetAgentSession(t, h.agentSessions, h.sessionID)
		child, err := parent.CreateChild(context.Background(), ChildRequest{Prompt: "done", Agent: BuildID, Name: "forget"})
		if err != nil {
			t.Fatal(err)
		}
		observation, err := child.Observe()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := observation.Wait(context.Background()); err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		results := make(chan error, 2)
		go func() { <-start; results <- child.(*agentSession).forget() }()
		go func() { <-start; results <- h.agentSessions.Remove(child.ID()) }()
		close(start)
		for range 2 {
			select {
			case err := <-results:
				if err != nil && !errors.Is(err, ErrChildNotFound) {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("Forget and Remove deadlocked")
			}
		}
	}
}

func TestAgentSessionRepositoryRestoresPersistedChildHierarchy(t *testing.T) {
	ctx := context.Background()
	fake := &fakeProvider{stream: func(_ int, _ context.Context, _ protocol.Request) (provider.Stream, error) {
		return events(protocol.Event{Type: protocol.EventFinish, FinishReason: protocol.FinishStop}), nil
	}}
	h := newRunnerHarness(t, fake, nil)
	parent, err := h.sessions.GetSession(h.sessionID).Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	selection := session.Selection{Agent: BuildID, Provider: "fake", Model: "model"}
	createChild := func(parentID, title string) session.AgentSessionDto {
		t.Helper()
		child, err := h.sessions.CreateSelected(ctx, session.CreateParams{
			ParentSessionID: parentID,
			Name:            title,
			ProjectID:       parent.ProjectID,
			ProjectRoot:     parent.ProjectRoot,
			Title:           title,
		}, selection)
		if err != nil {
			t.Fatal(err)
		}
		return child
	}
	childB := createChild(parent.ID, "child b")
	childA := createChild(parent.ID, "child a")
	nested := createChild(childA.ID, "nested")

	restartedConfig := h.agentSessions.config
	restartedConfig.MaxChildTasks = 1
	createdRestarted, err := NewUserSession(ctx, h.sessions, restartedConfig)
	if err != nil {
		t.Fatal(err)
	}
	restarted := createdRestarted.(*userSession)
	if restarted.repository == nil || restarted.repository == h.agentSessions.repository {
		t.Fatal("UserSession did not create its own agent session repository")
	}
	for _, relation := range []ChildSession{
		{SessionID: childA.ID, ParentSessionID: parent.ID},
		{SessionID: childB.ID, ParentSessionID: parent.ID},
		{SessionID: nested.ID, ParentSessionID: childA.ID},
	} {
		if got, ok := restarted.ChildRelation(relation.SessionID); !ok || got != relation.ParentSessionID {
			t.Errorf("ChildRelation(%q) = %q, %v; want %q, true", relation.SessionID, got, ok, relation.ParentSessionID)
		}
	}
	wantChildren := []ChildSession{
		{SessionID: childA.ID, ParentSessionID: parent.ID},
		{SessionID: childB.ID, ParentSessionID: parent.ID},
	}
	sort.Slice(wantChildren, func(i, j int) bool { return wantChildren[i].SessionID < wantChildren[j].SessionID })
	if got := restarted.ChildSessions(parent.ID); !reflect.DeepEqual(got, wantChildren) {
		t.Fatalf("ChildSessions(%q) = %#v, want %#v", parent.ID, got, wantChildren)
	}
	if !restarted.HasChildSessions(parent.ID) || !restarted.HasChildSessions(childA.ID) || restarted.HasChildSessions(nested.ID) {
		t.Fatalf("HasChildSessions = parent:%t child:%t nested:%t", restarted.HasChildSessions(parent.ID), restarted.HasChildSessions(childA.ID), restarted.HasChildSessions(nested.ID))
	}
	managed, err := mustGetAgentSession(t, restarted, parent.ID).CreateChild(ctx, ChildRequest{Prompt: "managed", Agent: BuildID, Name: "managed"})
	if err != nil {
		t.Fatalf("historical children consumed MaxChildTasks: %v", err)
	}
	managedObservation, err := managed.Observe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := managedObservation.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := mustGetAgentSession(t, restarted, parent.ID).CreateChild(ctx, ChildRequest{Prompt: "over limit", Agent: BuildID, Name: "over-limit"}); !errors.Is(err, ErrChildTaskLimit) {
		t.Fatalf("CreateChild over managed retention limit = %v", err)
	}
	if got, ok := restarted.ChildRelation(nested.ID); !ok || got != childA.ID {
		t.Fatalf("historical nested hierarchy after managed create = %q, %v", got, ok)
	}
	nestedRuntime := mustGetAgentSession(t, restarted, nested.ID)
	if nestedRuntime.Name() != "nested" || mustGetAgentSession(t, restarted, childA.ID).Name() != "child a" {
		t.Fatalf("restored names = nested:%q child:%q", nestedRuntime.Name(), mustGetAgentSession(t, restarted, childA.ID).Name())
	}
	childRuntime := nestedRuntime.Parent()
	if childRuntime == nil || childRuntime.ID() != childA.ID || childRuntime != mustGetAgentSession(t, restarted, childA.ID) {
		t.Fatalf("restored nested parent = %#v, want canonical runtime %q", childRuntime, childA.ID)
	}
	parentRuntime := childRuntime.Parent()
	if parentRuntime == nil || parentRuntime.ID() != parent.ID || parentRuntime != mustGetAgentSession(t, restarted, parent.ID) || parentRuntime.Parent() != nil {
		t.Fatalf("restored root parent chain = %#v, want canonical root runtime %q", parentRuntime, parent.ID)
	}

	live := event.NewBroker(h.repository, nil)
	live.SetSessionHierarchy(restarted)
	parentEvents, unsubscribe := live.Subscribe(parent.ID, 1)
	defer unsubscribe()
	var replayed []ChildSession
	restarted.AddChildCreatedObserver(childCreatedObserverFunc(func(child ChildSession) {
		replayed = append(replayed, child)
		live.ObserveSession(child.SessionID)
	}))
	wantReplayed := append(append([]ChildSession(nil), wantChildren...),
		ChildSession{SessionID: nested.ID, ParentSessionID: childA.ID},
		ChildSession{SessionID: managed.ID(), ParentSessionID: parent.ID},
	)
	sort.Slice(wantReplayed, func(i, j int) bool { return wantReplayed[i].SessionID < wantReplayed[j].SessionID })
	if !reflect.DeepEqual(replayed, wantReplayed) {
		t.Fatalf("restored observer replay = %#v, want %#v", replayed, wantReplayed)
	}

	data := json.RawMessage(`{"kind":"running"}`)
	if _, err := h.repository.Append(ctx, nested.ID, []event.NewEvent{{Type: v1.EventSessionStatus, Data: data}}, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case item := <-parentEvents:
		if item.Type != v1.EventSessionStatus || item.SessionID != parent.ID || item.TaskID != nested.ID || string(item.Data) != string(data) {
			t.Fatalf("restored durable projection = %#v", item)
		}
	case <-time.After(time.Second):
		t.Fatal("restored child durable event was not projected to its ancestor")
	}
}

func TestRunnerMarksActiveGoalOnStructuredUsageExhaustionOnly(t *testing.T) {
	tests := []struct {
		name       string
		stream     func(context.Context) (provider.Stream, error)
		wantStatus session.GoalStatus
	}{
		{"http quota", func(context.Context) (provider.Stream, error) {
			return nil, &provider.HTTPError{StatusCode: 429, Code: "insufficient_quota"}
		}, session.GoalUsageLimited},
		{"stream quota", func(context.Context) (provider.Stream, error) {
			return events(protocol.Event{Type: protocol.EventProviderError, ProviderError: &protocol.ProviderError{Type: "usage_limit_reached", Message: "limited"}}), nil
		}, session.GoalUsageLimited},
		{"transient rate limit", func(context.Context) (provider.Stream, error) {
			return nil, &provider.HTTPError{StatusCode: 429, Code: "rate_limit_exceeded"}
		}, session.GoalActive},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := &fakeProvider{stream: func(_ int, ctx context.Context, _ protocol.Request) (provider.Stream, error) { return test.stream(ctx) }}
			h := newRunnerHarness(t, fake, nil)
			if _, err := h.goals.Create(context.Background(), h.sessionID, "continue", nil); err != nil {
				t.Fatal(err)
			}
			h.admit(t, "user", "work", session.DeliverySteer)
			if err := h.runner.drainOnce(context.Background()); err == nil {
				t.Fatal("Drain succeeded")
			}
			goal, err := h.goals.Get(context.Background(), h.sessionID)
			if err != nil || goal.Status != test.wantStatus {
				t.Fatalf("goal = %#v, %v; want %s", goal, err, test.wantStatus)
			}
		})
	}
}

func (h *runnerHarness) admit(t *testing.T, id, content string, delivery session.Delivery) {
	t.Helper()
	if _, err := h.sessions.GetSession(h.sessionID).Admit(context.Background(), session.AdmitParams{MessageID: id, Content: content, Delivery: delivery}); err != nil {
		t.Fatal(err)
	}
}

func TestRunnerPersistsStreamedFinalText(t *testing.T) {
	fake := &fakeProvider{stream: func(_ int, _ context.Context, _ protocol.Request) (provider.Stream, error) {
		return events(
			protocol.Event{Type: protocol.EventTextDelta, Text: "hello "},
			protocol.Event{Type: protocol.EventTextDelta, Text: "world"},
			protocol.Event{Type: protocol.EventUsage, Usage: &protocol.Usage{TotalTokens: 3}},
			protocol.Event{Type: protocol.EventFinish, FinishReason: protocol.FinishStop},
		), nil
	}}
	h := newRunnerHarness(t, fake, nil)
	h.admit(t, "user", "question", session.DeliverySteer)
	if err := h.runner.drainOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	messages, err := h.sessions.GetSession(h.sessionID).ListMessages(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	last := messages[len(messages)-1]
	if last.Role != "assistant" || last.Content != "hello world" || last.Status != "complete" || last.FinishReason != string(protocol.FinishStop) {
		t.Fatalf("assistant = %#v", last)
	}
	var usage protocol.Usage
	if err := json.Unmarshal(last.Usage, &usage); err != nil || usage.TotalTokens != 3 {
		t.Fatalf("usage = %#v, %v", usage, err)
	}
}

func TestRunnerPersistsToolBeforeSideEffectAndContinuesAfterSettlement(t *testing.T) {
	var h *runnerHarness
	executed := make(chan struct{}, 1)
	item := &fakeTool{id: "mutate", execute: func(context.Context) (tool.Result, error) {
		var status string
		if err := h.db.SQL().QueryRow(`SELECT status FROM session_tool_call WHERE session_id=? AND id='call-1'`, h.sessionID).Scan(&status); err != nil {
			return tool.Result{}, err
		}
		if status != "running" {
			return tool.Result{}, errors.New("tool call was not durably running")
		}
		executed <- struct{}{}
		return tool.Result{Text: "tool output", ModelText: "tool output"}, nil
	}}
	fake := &fakeProvider{stream: func(index int, _ context.Context, request protocol.Request) (provider.Stream, error) {
		if index == 0 {
			call := protocol.ToolCall{ID: "call-1", Name: "mutate", Input: json.RawMessage(`{}`)}
			return events(protocol.Event{Type: protocol.EventToolCallComplete, ToolCall: &call}, protocol.Event{Type: protocol.EventFinish, FinishReason: protocol.FinishToolCalls}), nil
		}
		if len(request.Messages) == 0 || request.Messages[len(request.Messages)-1].Role != protocol.RoleTool {
			return nil, errors.New("continuation started before tool result")
		}
		return events(protocol.Event{Type: protocol.EventTextDelta, Text: "final"}, protocol.Event{Type: protocol.EventFinish, FinishReason: protocol.FinishStop}), nil
	}}
	h = newRunnerHarness(t, fake, nil, item)
	observations := 0
	h.runner.config.Status = statusObserverFunc(func() string {
		observations++
		return "Active tasks: none"
	})
	h.admit(t, "user", "run tool", session.DeliverySteer)
	if err := h.runner.drainOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-executed:
	default:
		t.Fatal("tool did not execute")
	}
	status, result := toolState(t, h, "call-1")
	if status != "success" || result != "tool output" {
		t.Fatalf("tool state = %q, %q", status, result)
	}
	requests := fake.Requests()
	if len(requests) != 2 {
		t.Fatalf("provider turns = %d", len(requests))
	}
	for index, request := range requests {
		if !containsRoleText(request.Messages, protocol.RoleSystem, "Active tasks: none") {
			t.Errorf("request %d lost durable status message: %#v", index, request.Messages)
		}
	}
	if observations != 1 {
		t.Fatalf("status observations = %d, want 1", observations)
	}
}

type statusObserver string

func (s statusObserver) Observe(context.Context, statusinfo.Query, statusinfo.Provider) (string, error) {
	return string(s), nil
}

type statusObserverFunc func() string

func (observe statusObserverFunc) Observe(context.Context, statusinfo.Query, statusinfo.Provider) (string, error) {
	return observe(), nil
}

func TestRunnerBoundsConcurrentToolsAndSettlesAllBeforeContinuation(t *testing.T) {
	gate := make(chan struct{})
	started := make(chan struct{}, 5)
	var active, maximum atomic.Int32
	item := &fakeTool{id: "parallel", execute: func(context.Context) (tool.Result, error) {
		current := active.Add(1)
		for {
			old := maximum.Load()
			if current <= old || maximum.CompareAndSwap(old, current) {
				break
			}
		}
		started <- struct{}{}
		<-gate
		active.Add(-1)
		return tool.Result{Text: "ok", ModelText: "ok"}, nil
	}}
	fake := &fakeProvider{stream: func(index int, _ context.Context, request protocol.Request) (provider.Stream, error) {
		if index == 0 {
			stream := &sliceStream{}
			for i := 0; i < 5; i++ {
				call := protocol.ToolCall{ID: "call-" + string(rune('a'+i)), Name: "parallel", Input: json.RawMessage(`{}`)}
				stream.events = append(stream.events, protocol.Event{Type: protocol.EventToolCallComplete, ToolCall: &call})
			}
			stream.events = append(stream.events, protocol.Event{Type: protocol.EventFinish, FinishReason: protocol.FinishToolCalls})
			return stream, nil
		}
		for _, message := range request.Messages {
			if message.Role == protocol.RoleTool {
				continue
			}
		}
		return events(protocol.Event{Type: protocol.EventFinish, FinishReason: protocol.FinishStop}), nil
	}}
	h := newRunnerHarness(t, fake, nil, item)
	h.admit(t, "user", "parallel", session.DeliverySteer)
	done := make(chan error, 1)
	go func() { done <- h.runner.drainOnce(context.Background()) }()
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("tools did not start")
		}
	}
	select {
	case <-started:
		t.Fatal("more than two tools ran concurrently")
	case <-time.After(20 * time.Millisecond):
	}
	close(gate)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if maximum.Load() != 2 {
		t.Fatalf("maximum concurrency = %d", maximum.Load())
	}
	for i := 0; i < 5; i++ {
		status, _ := toolState(t, h, "call-"+string(rune('a'+i)))
		if status != "success" {
			t.Fatalf("tool %d status = %s", i, status)
		}
	}
}

func TestRunnerSteerDuringBlockedTurnPromotesOnNextTurnOnly(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	item := &fakeTool{id: "read", execute: nil}
	fake := &fakeProvider{stream: func(index int, _ context.Context, _ protocol.Request) (provider.Stream, error) {
		if index == 0 {
			call := protocol.ToolCall{ID: "call", Name: "read", Input: json.RawMessage(`{}`)}
			return &blockingStream{started: started, release: release, events: &sliceStream{events: []protocol.Event{{Type: protocol.EventToolCallComplete, ToolCall: &call}, {Type: protocol.EventFinish, FinishReason: protocol.FinishToolCalls}}}}, nil
		}
		return events(protocol.Event{Type: protocol.EventFinish, FinishReason: protocol.FinishStop}), nil
	}}
	h := newRunnerHarness(t, fake, nil, item)
	h.admit(t, "initial", "initial", session.DeliverySteer)
	done := make(chan error, 1)
	go func() { done <- h.runner.drainOnce(context.Background()) }()
	<-started
	h.admit(t, "steer", "late steer", session.DeliverySteer)
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	requests := fake.Requests()
	if len(requests) != 2 || containsText(requests[0].Messages, "late steer") || !containsText(requests[1].Messages, "late steer") {
		t.Fatalf("steer request placement = %#v", requests)
	}
}

func TestRunnerQueueWaitsUntilCurrentContinuationIsIdle(t *testing.T) {
	fake := &fakeProvider{stream: func(_ int, _ context.Context, _ protocol.Request) (provider.Stream, error) {
		return events(protocol.Event{Type: protocol.EventFinish, FinishReason: protocol.FinishStop}), nil
	}}
	h := newRunnerHarness(t, fake, nil)
	h.admit(t, "initial", "initial", session.DeliverySteer)
	h.admit(t, "queued", "queued", session.DeliveryQueue)
	if err := h.runner.drainOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	requests := fake.Requests()
	if len(requests) != 2 || containsText(requests[0].Messages, "queued") || !containsText(requests[1].Messages, "queued") {
		t.Fatalf("queue request placement = %#v", requests)
	}
}

func TestRunnerCancellationSettlesAssistantAndTools(t *testing.T) {
	t.Run("assistant", func(t *testing.T) {
		started := make(chan struct{})
		release := make(chan struct{})
		fake := &fakeProvider{stream: func(_ int, _ context.Context, _ protocol.Request) (provider.Stream, error) {
			return &blockingStream{started: started, release: release, events: &sliceStream{}}, nil
		}}
		h := newRunnerHarness(t, fake, nil)
		h.admit(t, "user", "cancel", session.DeliverySteer)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- h.runner.drainOnce(ctx) }()
		<-started
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("Drain error = %v", err)
		}
		messages, _ := h.sessions.GetSession(h.sessionID).ListMessages(context.Background())
		if messages[len(messages)-1].Status != "interrupted" {
			t.Fatalf("assistant status = %s", messages[len(messages)-1].Status)
		}
	})

	t.Run("tools", func(t *testing.T) {
		started := make(chan struct{})
		item := &fakeTool{id: "block", execute: func(ctx context.Context) (tool.Result, error) {
			close(started)
			<-ctx.Done()
			return tool.Result{}, ctx.Err()
		}}
		fake := &fakeProvider{stream: func(_ int, _ context.Context, _ protocol.Request) (provider.Stream, error) {
			call := protocol.ToolCall{ID: "blocked", Name: "block", Input: json.RawMessage(`{}`)}
			return events(protocol.Event{Type: protocol.EventToolCallComplete, ToolCall: &call}, protocol.Event{Type: protocol.EventFinish, FinishReason: protocol.FinishToolCalls}), nil
		}}
		h := newRunnerHarness(t, fake, nil, item)
		h.admit(t, "user", "cancel tool", session.DeliverySteer)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- h.runner.drainOnce(ctx) }()
		<-started
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("Drain error = %v", err)
		}
		status, _ := toolState(t, h, "blocked")
		if status != "interrupted" {
			t.Fatalf("tool status = %s", status)
		}
	})
}

func TestRunnerCancellationPersistsToolOutputBeforeNextProviderTurn(t *testing.T) {
	started := make(chan struct{})
	item := &fakeTool{id: "block", execute: func(ctx context.Context) (tool.Result, error) {
		close(started)
		<-ctx.Done()
		return tool.Result{}, ctx.Err()
	}}
	fake := &fakeProvider{stream: func(index int, _ context.Context, request protocol.Request) (provider.Stream, error) {
		if index == 0 {
			call := protocol.ToolCall{ID: "call_cancelled", Name: "block", Input: json.RawMessage(`{}`)}
			return events(protocol.Event{Type: protocol.EventToolCallComplete, ToolCall: &call}, protocol.Event{Type: protocol.EventFinish, FinishReason: protocol.FinishToolCalls}), nil
		}
		var callSeen, outputSeen bool
		for _, message := range request.Messages {
			for _, part := range message.Content {
				if part.Type == protocol.ContentToolCall && part.ToolCall != nil && part.ToolCall.ID == "call_cancelled" {
					callSeen = true
				}
				if part.Type == protocol.ContentToolResult && part.ToolCallID == "call_cancelled" {
					outputSeen = true
					if !strings.Contains(part.Text, "interrupted") {
						return nil, errors.New("interrupted tool output did not explain cancellation")
					}
				}
			}
		}
		if !callSeen || !outputSeen {
			return nil, errors.New("resumed provider request contains an orphaned tool call")
		}
		return events(protocol.Event{Type: protocol.EventFinish, FinishReason: protocol.FinishStop}), nil
	}}
	h := newRunnerHarness(t, fake, nil, item)
	h.admit(t, "first", "run", session.DeliverySteer)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.runner.drainOnce(ctx) }()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Drain error = %v", err)
	}

	h.admit(t, "second", "try something else", session.DeliverySteer)
	if err := h.runner.drainOnce(context.Background()); err != nil {
		t.Fatalf("resumed Drain error = %v", err)
	}
}

func TestRunnerCancellationDuringProviderStreamDropsUnexecutedToolCalls(t *testing.T) {
	started := make(chan struct{})
	call := protocol.ToolCall{ID: "partial_call", Name: "read", Input: json.RawMessage(`{}`)}
	fake := &fakeProvider{stream: func(index int, _ context.Context, request protocol.Request) (provider.Stream, error) {
		if index == 0 {
			return &callThenBlockingStream{call: call, started: started}, nil
		}
		for _, message := range request.Messages {
			for _, part := range message.Content {
				if part.Type == protocol.ContentToolCall && part.ToolCall != nil && part.ToolCall.ID == call.ID {
					return nil, errors.New("unexecuted tool call survived interrupted provider stream")
				}
			}
		}
		return events(protocol.Event{Type: protocol.EventFinish, FinishReason: protocol.FinishStop}), nil
	}}
	h := newRunnerHarness(t, fake, nil, &fakeTool{id: "read"})
	h.admit(t, "first", "read", session.DeliverySteer)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.runner.drainOnce(ctx) }()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Drain error = %v", err)
	}
	h.admit(t, "second", "continue", session.DeliverySteer)
	if err := h.runner.drainOnce(context.Background()); err != nil {
		t.Fatalf("resumed Drain error = %v", err)
	}
}

type preparedProfileResolver struct{ base Profile }

func (*preparedProfileResolver) GetProfile(string) (Profile, error) {
	return Profile{}, errors.New("GetProfile called before PrepareTurn")
}
func (r *preparedProfileResolver) PrepareTurn(string, string) (TurnProfile, error) {
	return NewTurnProfile(r.base, security.Rule{Path: "/tmp/plan.md", Action: security.ActionAllowWrite}), nil
}

type profileCaptureTool struct {
	fakeTool
	paths []string
}

func (t *profileCaptureTool) Execute(_ context.Context, _ tool.Plan, call tool.CallContext) (tool.Result, error) {
	for _, rule := range call.SecurityProfile.Rules() {
		if rule.Action == security.ActionAllowWrite {
			t.paths = append(t.paths, rule.Path)
		}
	}
	return tool.Result{Text: "ok", ModelText: "ok"}, nil
}

func TestRunnerPreparesProfileBeforeUseAndKeepsItAcrossToolContinuations(t *testing.T) {
	capture := &profileCaptureTool{fakeTool: fakeTool{id: "capture"}}
	fake := &fakeProvider{stream: func(index int, _ context.Context, _ protocol.Request) (provider.Stream, error) {
		if index < 2 {
			name := []string{"status", "capture"}[index]
			call := protocol.ToolCall{ID: name, Name: name, Input: json.RawMessage(`{}`)}
			return events(protocol.Event{Type: protocol.EventToolCallComplete, ToolCall: &call}, protocol.Event{Type: protocol.EventFinish, FinishReason: protocol.FinishToolCalls}), nil
		}
		return events(protocol.Event{Type: protocol.EventFinish, FinishReason: protocol.FinishStop}), nil
	}}
	base := Profile{ID: "plan", Prompt: "plan", ReadOnly: true, MaxTurns: 10}
	h := newRunnerHarness(t, fake, []Profile{base}, &fakeTool{id: "status"}, capture)
	resolver := &preparedProfileResolver{base: base}
	h.runner.config.Profiles = resolver
	h.admit(t, "user", "plan", session.DeliverySteer)
	if err := h.runner.drainOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(capture.paths) != 2 || capture.paths[0] != "/tmp/plan.md" || !strings.HasSuffix(capture.paths[1], "/session/"+h.sessionID+"/scratch") {
		t.Fatalf("session profile allow_write rules = %#v", capture.paths)
	}
	if len(resolver.base.SandboxRules) != 0 {
		t.Fatalf("reusable profile rules = %#v", resolver.base.SandboxRules)
	}
}

func TestRunnerPlanDeniesMutationEvenWhenToolIsRegistered(t *testing.T) {
	var executed atomic.Bool
	mutation := &fakeTool{id: "mutate", execute: func(context.Context) (tool.Result, error) {
		executed.Store(true)
		return tool.Result{Text: "mutated", ModelText: "mutated"}, nil
	}}
	fake := &fakeProvider{stream: func(index int, _ context.Context, _ protocol.Request) (provider.Stream, error) {
		if index == 0 {
			call := protocol.ToolCall{ID: "denied", Name: "mutate", Input: json.RawMessage(`{}`)}
			return events(protocol.Event{Type: protocol.EventToolCallComplete, ToolCall: &call}, protocol.Event{Type: protocol.EventFinish, FinishReason: protocol.FinishToolCalls}), nil
		}
		return events(protocol.Event{Type: protocol.EventFinish, FinishReason: protocol.FinishStop}), nil
	}}
	h := newRunnerHarness(t, fake, []Profile{Builtins()[1]}, mutation)
	h.admit(t, "user", "plan", session.DeliverySteer)
	if err := h.runner.drainOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if executed.Load() {
		t.Fatal("mutation tool executed")
	}
	if len(fake.Requests()[0].Tools) == 0 {
		t.Fatalf("plan request had no tools")
	}
	status, _ := toolState(t, h, "denied")
	if status != "failure" {
		t.Fatalf("denied tool status = %s", status)
	}
}

func TestRunnerMaxTurnsOmitsToolsAndPublishesNotice(t *testing.T) {
	for _, maxTurns := range []int{1, 2} {
		t.Run(fmt.Sprintf("turns_%d", maxTurns), func(t *testing.T) {
			item := &fakeTool{id: "available"}
			fake := &fakeProvider{stream: func(index int, _ context.Context, request protocol.Request) (provider.Stream, error) {
				if index == 0 && maxTurns == 2 {
					if len(request.Tools) == 0 {
						return nil, errors.New("tools were omitted before final turn")
					}
					call := protocol.ToolCall{ID: "call-1", Name: item.id, Input: json.RawMessage(`{}`)}
					return events(protocol.Event{Type: protocol.EventToolCallComplete, ToolCall: &call}, protocol.Event{Type: protocol.EventFinish, FinishReason: protocol.FinishToolCalls}), nil
				}
				if len(request.Tools) != 0 {
					return nil, errors.New("tools were present on final turn")
				}
				return events(protocol.Event{Type: protocol.EventFinish, FinishReason: protocol.FinishStop}), nil
			}}
			profile := Profile{ID: "limited", Prompt: "finish", MaxTurns: maxTurns}
			h := newRunnerHarness(t, fake, []Profile{profile}, item)
			live := &recordingPublisher{}
			h.runner.config.Live = live
			h.admit(t, "user", "finish", session.DeliverySteer)
			if err := h.runner.drainOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(fake.Requests()) != maxTurns {
				t.Fatalf("provider turns = %d", len(fake.Requests()))
			}
			var notices []protocol.Event
			for _, event := range live.events {
				if event.Type == protocol.EventMaxTurnsReached {
					notices = append(notices, event)
				}
			}
			if len(notices) != 1 || !strings.Contains(notices[0].Text, fmt.Sprint(maxTurns)) {
				t.Fatalf("max-turn notices = %#v", notices)
			}
		})
	}
}

func TestRunnerProviderErrorLeavesTerminalAssistant(t *testing.T) {
	fake := &fakeProvider{stream: func(_ int, _ context.Context, _ protocol.Request) (provider.Stream, error) {
		return events(
			protocol.Event{Type: protocol.EventTextDelta, Text: "partial"},
			protocol.Event{Type: protocol.EventProviderError, ProviderError: &protocol.ProviderError{Message: "provider failed"}},
		), nil
	}}
	h := newRunnerHarness(t, fake, nil)
	h.admit(t, "user", "error", session.DeliverySteer)
	if err := h.runner.drainOnce(context.Background()); err == nil {
		t.Fatal("Drain succeeded")
	}
	messages, _ := h.sessions.GetSession(h.sessionID).ListMessages(context.Background())
	last := messages[len(messages)-1]
	if last.Status != "error" || last.Content != "partial" || last.Error != "provider failed" {
		t.Fatalf("assistant = %#v", last)
	}
}

func TestRunnerInvokesAutomaticCompactionWithCompleteRequestCost(t *testing.T) {
	item := &fakeTool{id: "read"}
	fake := &fakeProvider{stream: func(_ int, _ context.Context, _ protocol.Request) (provider.Stream, error) {
		return events(protocol.Event{Type: protocol.EventFinish, FinishReason: protocol.FinishStop}), nil
	}}
	h := newRunnerHarness(t, fake, nil, item)
	compactor := &fakeCompactor{}
	h.runner.config.Compactor = compactor
	h.admit(t, "user", "automatic", session.DeliverySteer)
	if err := h.runner.drainOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(compactor.requests) != 1 || compactor.requests[0].Force || compactor.requests[0].ProviderID != "fake" || compactor.requests[0].Model.ID != "model" || len(compactor.requests[0].Tools) != 1 {
		t.Fatalf("compaction requests = %#v", compactor.requests)
	}
}

func TestRunnerRetriesCanonicalOverflowExactlyOnce(t *testing.T) {
	fake := &fakeProvider{stream: func(index int, _ context.Context, _ protocol.Request) (provider.Stream, error) {
		if index == 0 {
			return events(protocol.Event{Type: protocol.EventProviderError, ProviderError: &protocol.ProviderError{Type: "invalid_request_error", Code: "context_length_exceeded", Message: "too long"}}), nil
		}
		return events(protocol.Event{Type: protocol.EventFinish, FinishReason: protocol.FinishStop}), nil
	}}
	h := newRunnerHarness(t, fake, nil)
	h.runner.config.Status = statusObserver("retry status")
	live := &recordingPublisher{}
	h.runner.config.Live = live
	compactor := &fakeCompactor{compact: func(request compaction.Request) (compaction.Result, error) {
		if request.Force {
			return compaction.Result{Status: "complete", RecordID: "cmpr_retry"}, nil
		}
		return compaction.Result{Status: "skipped"}, nil
	}}
	h.runner.config.Compactor = compactor
	h.admit(t, "user", "overflow", session.DeliverySteer)
	if err := h.runner.drainOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	requests := fake.Requests()
	if len(requests) != 2 || len(compactor.requests) != 2 || !compactor.requests[1].Force {
		t.Fatalf("provider=%d compactor=%#v", len(requests), compactor.requests)
	}
	for index, request := range requests {
		if !containsRoleText(request.Messages, protocol.RoleSystem, "retry status") {
			t.Errorf("request %d lost durable status message: %#v", index, request.Messages)
		}
	}
	events, err := h.repository.List(context.Background(), h.sessionID, -1, 100)
	if err != nil {
		t.Fatal(err)
	}
	retries := 0
	for _, item := range events {
		if item.Type == "session.compaction.retry" {
			retries++
		}
	}
	if retries != 1 {
		t.Fatalf("retry events = %d", retries)
	}
	injections := 0
	for _, item := range live.events {
		if item.Type == protocol.EventStatusPromptInjected {
			injections++
		}
	}
	if injections != 1 {
		t.Fatalf("status prompt injection events = %d, want 1", injections)
	}
}

func TestRunnerRetriesMessageOnlyOverflowExactlyOnce(t *testing.T) {
	// Kimi/Moonshot reports a context overflow with a generic
	// "invalid_request_error" type and the reason only in the message text.
	fake := &fakeProvider{stream: func(index int, _ context.Context, _ protocol.Request) (provider.Stream, error) {
		if index == 0 {
			return nil, &provider.HTTPError{StatusCode: 400, Type: "invalid_request_error", Message: "Invalid request: Your request exceeded model token limit: 262144 (requested: 265424)"}
		}
		return events(protocol.Event{Type: protocol.EventFinish, FinishReason: protocol.FinishStop}), nil
	}}
	h := newRunnerHarness(t, fake, nil)
	compactor := &fakeCompactor{compact: func(request compaction.Request) (compaction.Result, error) {
		if request.Force {
			return compaction.Result{Status: "complete", RecordID: "cmpr_retry"}, nil
		}
		return compaction.Result{Status: "skipped"}, nil
	}}
	h.runner.config.Compactor = compactor
	h.admit(t, "user", "overflow", session.DeliverySteer)
	if err := h.runner.drainOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(fake.Requests()) != 2 || len(compactor.requests) != 2 || !compactor.requests[1].Force {
		t.Fatalf("provider=%d compactor=%#v", len(fake.Requests()), compactor.requests)
	}
	events, err := h.repository.List(context.Background(), h.sessionID, -1, 100)
	if err != nil {
		t.Fatal(err)
	}
	retries := 0
	for _, item := range events {
		if item.Type == "session.compaction.retry" {
			retries++
		}
	}
	if retries != 1 {
		t.Fatalf("retry events = %d", retries)
	}
}

func TestRunnerDoesNotRetryUnknownProviderError(t *testing.T) {
	fake := &fakeProvider{stream: func(_ int, _ context.Context, _ protocol.Request) (provider.Stream, error) {
		return events(protocol.Event{Type: protocol.EventProviderError, ProviderError: &protocol.ProviderError{Code: "mystery", Message: "unknown"}}), nil
	}}
	h := newRunnerHarness(t, fake, nil)
	compactor := &fakeCompactor{}
	h.runner.config.Compactor = compactor
	h.admit(t, "user", "unknown", session.DeliverySteer)
	if err := h.runner.drainOnce(context.Background()); err == nil {
		t.Fatal("Drain succeeded")
	}
	if len(fake.Requests()) != 1 || len(compactor.requests) != 1 {
		t.Fatalf("provider=%d compactor=%d", len(fake.Requests()), len(compactor.requests))
	}
}

func toolState(t *testing.T, h *runnerHarness, callID string) (string, string) {
	t.Helper()
	var status, result string
	if err := h.db.SQL().QueryRow(`SELECT status,result_text FROM session_tool_call WHERE session_id=? AND id=?`, h.sessionID, callID).Scan(&status, &result); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("tool call %s was not persisted", callID)
		}
		t.Fatal(err)
	}
	return status, result
}

func testSessionStateDirectories(t *testing.T) UserSessionStateDirectories {
	t.Helper()
	directories, err := NewUserSessionStateDirectories(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return directories
}

func containsText(messages []protocol.Message, want string) bool {
	for _, message := range messages {
		for _, part := range message.Content {
			if part.Text == want {
				return true
			}
		}
	}
	return false
}

func containsRoleText(messages []protocol.Message, role protocol.Role, want string) bool {
	for _, message := range messages {
		if message.Role == role && containsText([]protocol.Message{message}, want) {
			return true
		}
	}
	return false
}

func containsRoleSubstring(messages []protocol.Message, role protocol.Role, want string) bool {
	for _, message := range messages {
		if message.Role != role {
			continue
		}
		for _, part := range message.Content {
			if strings.Contains(part.Text, want) {
				return true
			}
		}
	}
	return false
}
