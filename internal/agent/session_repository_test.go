package agent

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/amirulashraf/parrot-coder/internal/protocol"
	"github.com/amirulashraf/parrot-coder/internal/provider"
	"github.com/amirulashraf/parrot-coder/internal/session"
	"github.com/amirulashraf/parrot-coder/internal/systemcontext"
	"github.com/amirulashraf/parrot-coder/internal/tool"
)

func emptyToolProviders(t *testing.T) tool.Providers {
	t.Helper()
	providers, err := tool.NewProviders()
	if err != nil {
		t.Fatal(err)
	}
	return providers
}

func mustGetRepositorySession(t *testing.T, repository *agentSessionRepository, id string) AgentSession {
	t.Helper()
	created, err := repository.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	return created
}

func TestAgentSessionRepositoryIdentityLifecycleAndRemoval(t *testing.T) {
	repository := &agentSessionRepository{sessions: make(map[string]*agentSession), bindings: make(map[string]*sessionBinding), toolProviders: emptyToolProviders(t)}
	first := mustGetRepositorySession(t, repository, "a")
	if first != mustGetRepositorySession(t, repository, "a") || first == mustGetRepositorySession(t, repository, "b") {
		t.Fatal("repository did not preserve one runtime object per session ID")
	}
	if _, ok := repository.Lookup("missing"); ok {
		t.Fatal("Lookup created a missing session")
	}

	for _, id := range []string{"a", "b"} {
		session := mustGetRepositorySession(t, repository, id).(*agentSession)
		session.mu.Lock()
		session.turn = &turnState{status: StatusRunning}
		session.mu.Unlock()
	}
	active := repository.Active()
	if len(active) != 2 || active[0].SessionID != "a" || active[1].SessionID != "b" {
		t.Fatalf("Active = %#v", active)
	}
	if !errors.Is(repository.Remove("a"), ErrAgentSessionActive) {
		t.Fatal("removed an active session")
	}
	for _, id := range []string{"a", "b"} {
		session := mustGetRepositorySession(t, repository, id).(*agentSession)
		session.mu.Lock()
		session.turn.status = StatusSucceeded
		session.mu.Unlock()
	}
	cleanup := &retrySessionStateDirectories{err: errors.New("cleanup failed")}
	repository.stateDirectories = cleanup
	if err := repository.Remove("a"); !errors.Is(err, cleanup.err) {
		t.Fatalf("first Remove error = %v, want %v", err, cleanup.err)
	}
	if retained, ok := repository.Lookup("a"); !ok || retained != first {
		t.Fatal("cleanup failure discarded the runtime needed for retry")
	}
	cleanup.err = nil
	if err := repository.Remove("a"); err != nil {
		t.Fatal(err)
	}
	if cleanup.removes != 2 {
		t.Fatalf("cleanup attempts = %d, want 2", cleanup.removes)
	}
	if recreated := mustGetRepositorySession(t, repository, "a"); recreated == first {
		t.Fatal("removed session was not recreated")
	}
}

type retrySessionStateDirectories struct {
	err     error
	removes int
}

func (*retrySessionStateDirectories) Directory(string) (UserSessionStateDirectory, error) {
	return UserSessionStateDirectory{}, nil
}

func (*retrySessionStateDirectories) Prepare(string) (UserSessionStateDirectory, error) {
	return UserSessionStateDirectory{}, nil
}

func (d *retrySessionStateDirectories) Remove(string) error {
	d.removes++
	return d.err
}

func TestAgentSessionRepositoryRepairsCompactionBeforeBindingAndCanRetry(t *testing.T) {
	failures := 1
	compactor := &fakeCompactor{repair: func(string) error {
		if failures > 0 {
			failures--
			return errors.New("repair failed")
		}
		return nil
	}}
	repository := &agentSessionRepository{
		toolProviders: emptyToolProviders(t), compactor: compactor,
		sessions: make(map[string]*agentSession), bindings: make(map[string]*sessionBinding),
	}
	if created, err := repository.Get("repair"); err == nil || created != nil || !strings.Contains(err.Error(), "repair compaction") {
		t.Fatalf("first Get = %#v, %v", created, err)
	}
	if _, ok := repository.Lookup("repair"); ok {
		t.Fatal("failed repair exposed a runtime")
	}
	mustGetRepositorySession(t, repository, "repair")
	if !slices.Equal(compactor.repairs, []string{"repair", "repair"}) {
		t.Fatalf("repair calls = %#v", compactor.repairs)
	}
}

func TestAgentSessionRepositoryProviderBindingLifecycle(t *testing.T) {
	var mu sync.Mutex
	calls := map[string]int{}
	failures := map[string]int{"retry": 1}
	panics := map[string]int{"panic": 1, "panic-wait": 1}
	entered, release := make(chan struct{}), make(chan struct{})
	panicEntered, panicRelease := make(chan struct{}), make(chan struct{})
	provider := &tool.ProviderFunc{ToolDescriptor: tool.DescriptorOf(&fakeTool{id: "bound"}), CreateTool: func(state tool.AgentSession) (tool.Tool, error) {
		mu.Lock()
		calls[state.SessionID()]++
		call := calls[state.SessionID()]
		fail, panicNow := failures[state.SessionID()] >= call, panics[state.SessionID()] >= call
		mu.Unlock()
		if state.SessionID() == "same" {
			if call == 1 {
				close(entered)
			}
			<-release
		}
		if state.SessionID() == "panic-wait" && call == 1 {
			close(panicEntered)
			<-panicRelease
		}
		if panicNow {
			panic("provider panic")
		}
		if fail {
			return nil, errors.New("provider failed")
		}
		return &fakeTool{id: "bound"}, nil
	}}
	providers, err := tool.NewProviders(provider)
	if err != nil {
		t.Fatal(err)
	}
	repository := &agentSessionRepository{toolProviders: providers, sessions: make(map[string]*agentSession), bindings: make(map[string]*sessionBinding), dtos: make(map[string]session.AgentSessionDto), children: make(map[string]ChildSession)}

	const waiters = 16
	results := make(chan AgentSession, waiters)
	for range waiters {
		go func() {
			created, getErr := repository.Get("same")
			if getErr != nil {
				t.Errorf("concurrent Get: %v", getErr)
			}
			results <- created
		}()
	}
	<-entered
	close(release)
	first := <-results
	for range waiters - 1 {
		if got := <-results; got != first {
			t.Fatal("concurrent Get returned different runtimes")
		}
	}
	if other := mustGetRepositorySession(t, repository, "other"); other == first {
		t.Fatal("distinct sessions shared a runtime")
	}
	if _, err := repository.Get("retry"); err == nil {
		t.Fatal("provider failure was accepted")
	}
	mustGetRepositorySession(t, repository, "retry")
	if _, err := repository.Get("panic"); err == nil || !strings.Contains(err.Error(), "provider panic") {
		t.Fatalf("panic error = %v", err)
	}
	mustGetRepositorySession(t, repository, "panic")
	panicResults := make(chan error, 2)
	go func() { _, getErr := repository.Get("panic-wait"); panicResults <- getErr }()
	<-panicEntered
	go func() { _, getErr := repository.Get("panic-wait"); panicResults <- getErr }()
	time.Sleep(10 * time.Millisecond)
	close(panicRelease)
	for range 2 {
		select {
		case getErr := <-panicResults:
			if getErr == nil || !strings.Contains(getErr.Error(), "provider panic") {
				t.Fatalf("concurrent panic error = %v", getErr)
			}
		case <-time.After(time.Second):
			t.Fatal("concurrent panic waiter wedged")
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if calls["same"] != 1 || calls["other"] != 1 || calls["retry"] != 2 || calls["panic"] != 2 || calls["panic-wait"] != 1 {
		t.Fatalf("provider calls = %#v", calls)
	}
}

type bindingPromptProvider struct {
	materialize func(systemcontext.AgentSession) (systemcontext.SystemPrompt, error)
}

func (bindingPromptProvider) Key() string { return "test:binding-prompt" }
func (p bindingPromptProvider) MaterializeSystemPrompt(session systemcontext.AgentSession) (systemcontext.SystemPrompt, error) {
	return p.materialize(session)
}

type bindingPrompt string

func (p bindingPrompt) GetSystemPrompt(context.Context, systemcontext.ModelSelection) (string, error) {
	return string(p), nil
}

type guidedBindingTool struct{ fakeTool }

func (t guidedBindingTool) Descriptor() tool.Descriptor {
	descriptor := t.fakeTool.Descriptor()
	descriptor.SystemPromptGuidance = "bound tool guidance"
	return descriptor
}

func TestAgentSessionRepositoryMaterializesSystemPromptAfterToolsAndRetriesPanic(t *testing.T) {
	toolProvider := &tool.ProviderFunc{ToolDescriptor: tool.DescriptorOf(&guidedBindingTool{fakeTool{id: "guided"}}), CreateTool: func(tool.AgentSession) (tool.Tool, error) {
		return &guidedBindingTool{fakeTool{id: "guided"}}, nil
	}}
	tools, err := tool.NewProviders(toolProvider)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	promptProvider := bindingPromptProvider{materialize: func(session systemcontext.AgentSession) (systemcontext.SystemPrompt, error) {
		calls++
		if session.ModelSelector() != "provider/model" || session.ToolSystemGuidance() != "bound tool guidance" {
			t.Fatalf("materialized session = model %q, guidance %q", session.ModelSelector(), session.ToolSystemGuidance())
		}
		if calls == 1 {
			panic("prompt panic")
		}
		return bindingPrompt("bound prompt"), nil
	}}
	user := &userSession{systemPromptProvider: promptProvider}
	repository := &agentSessionRepository{
		user: user, toolProviders: tools, sessions: make(map[string]*agentSession), bindings: make(map[string]*sessionBinding),
		dtos: make(map[string]session.AgentSessionDto),
	}
	dto := session.AgentSessionDto{ID: "prompt", Model: "provider/model"}
	if created, err := repository.bind(dto, nil, nil, nil); created != nil || err == nil || !strings.Contains(err.Error(), "prompt panic") {
		t.Fatalf("panic bind = %#v, %v", created, err)
	}
	bound, err := repository.bind(dto, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	created := bound.(*agentSession)
	if calls != 2 {
		t.Fatalf("materialize calls = %d, want 2", calls)
	}
	prompt, err := created.systemPrompt.GetSystemPrompt(t.Context(), systemcontext.ModelSelection{})
	if err != nil || prompt != "bound prompt" {
		t.Fatalf("system prompt = %q, %v", prompt, err)
	}
}

func TestAgentSessionRepositoryRestoredChildPropagatesParentBindFailure(t *testing.T) {
	providerErr := errors.New("parent tools failed")
	provider := &tool.ProviderFunc{ToolDescriptor: tool.DescriptorOf(&fakeTool{id: "bound"}), CreateTool: func(state tool.AgentSession) (tool.Tool, error) {
		if state.SessionID() == "parent" {
			return nil, providerErr
		}
		return &fakeTool{id: "bound"}, nil
	}}
	providers, err := tool.NewProviders(provider)
	if err != nil {
		t.Fatal(err)
	}
	repository := &agentSessionRepository{
		toolProviders: providers, sessions: make(map[string]*agentSession), bindings: make(map[string]*sessionBinding),
		dtos:     map[string]session.AgentSessionDto{"parent": {ID: "parent"}, "child": {ID: "child", ParentSessionID: "parent"}},
		children: map[string]ChildSession{"child": {SessionID: "child", ParentSessionID: "parent"}},
	}
	if child, err := repository.Get("child"); !errors.Is(err, providerErr) || child != nil {
		t.Fatalf("restored child Get = %#v, %v", child, err)
	}
	if _, ok := repository.Lookup("child"); ok {
		t.Fatal("parentless child was cached")
	}
}

func TestAgentSessionRepositorySelectionUpdateDoesNotResurrectOrReplaceCache(t *testing.T) {
	bound := &agentSession{}
	replacement := &agentSession{}
	updated := session.AgentSessionDto{ID: "child", Agent: "plan", Model: "provider/model/high"}
	repository := &agentSessionRepository{
		sessions: map[string]*agentSession{"child": replacement},
		dtos:     map[string]session.AgentSessionDto{"child": {ID: "child", Agent: "build"}},
	}
	repository.updateSelection(bound, updated)
	if got := repository.dtos["child"].Agent; got != "build" {
		t.Fatalf("replacement cache agent = %q, want build", got)
	}
	delete(repository.dtos, "child")
	repository.sessions["child"] = bound
	repository.updateSelection(bound, updated)
	if _, ok := repository.dtos["child"]; ok {
		t.Fatal("missing cache entry was resurrected")
	}
}

func TestAgentSessionRepositoryChildHierarchy(t *testing.T) {
	repository := &agentSessionRepository{children: map[string]ChildSession{
		"child-b": {SessionID: "child-b", ParentSessionID: "parent"},
		"nested":  {SessionID: "nested", ParentSessionID: "child-a"},
		"child-a": {SessionID: "child-a", ParentSessionID: "parent"},
	}}

	parent, ok := repository.ChildRelation("child-a")
	if !ok || parent != "parent" {
		t.Fatalf("ChildRelation = %q, %v", parent, ok)
	}
	if _, ok := repository.ChildRelation("missing"); ok {
		t.Fatal("missing child relation was found")
	}
	children := repository.ChildSessions("parent")
	want := []ChildSession{
		{SessionID: "child-a", ParentSessionID: "parent"},
		{SessionID: "child-b", ParentSessionID: "parent"},
	}
	if !slices.Equal(children, want) {
		t.Fatalf("ChildSessions = %#v, want %#v", children, want)
	}
}

func TestAgentSessionConcurrentResumeJoinsLifecycle(t *testing.T) {
	var calls int
	var mu sync.Mutex
	entered := make(chan struct{})
	release := make(chan struct{})
	fake := &fakeProvider{stream: func(_ int, _ context.Context, _ protocol.Request) (provider.Stream, error) {
		mu.Lock()
		calls++
		if calls == 1 {
			close(entered)
		}
		mu.Unlock()
		<-release
		return events(protocol.Event{Type: protocol.EventFinish, FinishReason: protocol.FinishStop}), nil
	}}
	h := newRunnerHarness(t, fake, nil)
	h.admit(t, "user", "work", session.DeliverySteer)
	runtime := h.runner
	const waiters = 32
	results := make(chan error, waiters)
	go func() { results <- runtime.Resume(context.Background()) }()
	<-entered
	for range waiters - 1 {
		go func() { results <- runtime.Resume(context.Background()) }()
	}
	time.Sleep(10 * time.Millisecond)
	close(release)
	for range waiters {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("execution calls = %d, want 1", calls)
	}
}

func waitForAgentSession(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition was not met")
		}
		time.Sleep(time.Millisecond)
	}
}
