package agent

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/amirulashraf/parrot-coder/internal/session"
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
	repository := &agentSessionRepository{sessions: make(map[string]*agentSession), bindings: make(map[string]*sessionBinding), config: AgentSessionConfig{ToolProviders: emptyToolProviders(t)}}
	first := mustGetRepositorySession(t, repository, "a")
	if first != mustGetRepositorySession(t, repository, "a") || first == mustGetRepositorySession(t, repository, "b") {
		t.Fatal("repository did not preserve one runtime object per session ID")
	}
	if _, ok := repository.Lookup("missing"); ok {
		t.Fatal("Lookup created a missing session")
	}

	started := make(chan string, 2)
	release := make(chan struct{})
	for _, id := range []string{"a", "b"} {
		session := mustGetRepositorySession(t, repository, id).(*agentSession)
		session.execute = func(context.Context) error {
			started <- session.ID()
			<-release
			return nil
		}
		session.Wake()
	}
	seen := map[string]bool{}
	for len(seen) != 2 {
		select {
		case id := <-started:
			seen[id] = true
		case <-time.After(time.Second):
			t.Fatal("different sessions did not execute concurrently")
		}
	}
	active := repository.Active()
	if len(active) != 2 || active[0].SessionID != "a" || active[1].SessionID != "b" {
		t.Fatalf("Active = %#v", active)
	}
	if !errors.Is(repository.Remove("a"), ErrAgentSessionActive) {
		t.Fatal("removed an active session")
	}
	close(release)
	waitForAgentSession(t, func() bool { return len(repository.Active()) == 0 })
	cleanup := &retrySessionStateDirectories{err: errors.New("cleanup failed")}
	repository.config.StateDirectories = cleanup
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

func TestAgentSessionRepositoryProviderBindingLifecycle(t *testing.T) {
	var mu sync.Mutex
	calls := map[string]int{}
	failures := map[string]int{"retry": 1}
	panics := map[string]int{"panic": 1, "panic-wait": 1}
	entered, release := make(chan struct{}), make(chan struct{})
	panicEntered, panicRelease := make(chan struct{}), make(chan struct{})
	provider := &tool.ProviderFunc{ToolDescriptor: tool.DescriptorOf(&fakeTool{id: "bound"}), CreateTool: func(state tool.SessionState) (tool.Tool, error) {
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
	repository := &agentSessionRepository{config: AgentSessionConfig{ToolProviders: providers}, sessions: make(map[string]*agentSession), bindings: make(map[string]*sessionBinding), dtos: make(map[string]session.AgentSessionDto), children: make(map[string]ChildSession)}

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

func TestAgentSessionRepositoryRestoredChildPropagatesParentBindFailure(t *testing.T) {
	providerErr := errors.New("parent tools failed")
	provider := &tool.ProviderFunc{ToolDescriptor: tool.DescriptorOf(&fakeTool{id: "bound"}), CreateTool: func(state tool.SessionState) (tool.Tool, error) {
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
		config: AgentSessionConfig{ToolProviders: providers}, sessions: make(map[string]*agentSession), bindings: make(map[string]*sessionBinding),
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
	if err := repository.ForgetChild("child-a"); err != nil {
		t.Fatal(err)
	}
	if err := repository.ForgetChild("missing"); err != nil {
		t.Fatal(err)
	}
	if _, ok := repository.ChildRelation("child-a"); ok {
		t.Fatal("child relation was not forgotten")
	}
}

func TestAgentSessionConcurrentResumeJoinsLifecycle(t *testing.T) {
	var calls int
	var mu sync.Mutex
	entered := make(chan struct{})
	release := make(chan struct{})
	session := &agentSession{dto: session.AgentSessionDto{ID: "same"}, execute: func(context.Context) error {
		mu.Lock()
		calls++
		if calls == 1 {
			close(entered)
		}
		mu.Unlock()
		<-release
		return nil
	}}
	const waiters = 32
	results := make(chan error, waiters)
	go func() { results <- session.Resume(context.Background()) }()
	<-entered
	for range waiters - 1 {
		go func() { results <- session.Resume(context.Background()) }()
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
