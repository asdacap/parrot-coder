package agent

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"
)

func TestAgentSessionRepositoryIdentityLifecycleAndRemoval(t *testing.T) {
	repository := &AgentSessionRepository{sessions: make(map[string]*agentSession)}
	first := repository.Get("a")
	if first != repository.Get("a") || first == repository.Get("b") {
		t.Fatal("repository did not preserve one runtime object per session ID")
	}
	if _, ok := repository.Lookup("missing"); ok {
		t.Fatal("Lookup created a missing session")
	}

	started := make(chan string, 2)
	release := make(chan struct{})
	for _, id := range []string{"a", "b"} {
		session := repository.Get(id).(*agentSession)
		session.execute = func(context.Context) error {
			started <- session.id
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
	if err := repository.Remove("a"); err != nil {
		t.Fatal(err)
	}
	if recreated := repository.Get("a"); recreated == first {
		t.Fatal("removed session was not recreated")
	}
}

func TestAgentSessionRepositoryChildHierarchy(t *testing.T) {
	repository := &AgentSessionRepository{children: map[string]ChildSession{
		"child-b": {SessionID: "child-b", ParentSessionID: "parent", TaskID: "task-b"},
		"nested":  {SessionID: "nested", ParentSessionID: "child-a", TaskID: "task-nested"},
		"child-a": {SessionID: "child-a", ParentSessionID: "parent", TaskID: "task-a"},
	}}

	parent, task, ok := repository.ChildRelation("child-a")
	if !ok || parent != "parent" || task != "task-a" {
		t.Fatalf("ChildRelation = %q, %q, %v", parent, task, ok)
	}
	if _, _, ok := repository.ChildRelation("missing"); ok {
		t.Fatal("missing child relation was found")
	}
	children := repository.ChildSessions("parent")
	want := []ChildSession{
		{SessionID: "child-a", ParentSessionID: "parent", TaskID: "task-a"},
		{SessionID: "child-b", ParentSessionID: "parent", TaskID: "task-b"},
	}
	if !slices.Equal(children, want) {
		t.Fatalf("ChildSessions = %#v, want %#v", children, want)
	}
	if err := repository.ForgetChild("child-a", "other-task"); !errors.Is(err, ErrChildTaskMismatch) {
		t.Fatalf("ForgetChild wrong task = %v", err)
	}
	if _, _, ok := repository.ChildRelation("child-a"); !ok {
		t.Fatal("wrong task forgot child relation")
	}
	if err := repository.ForgetChild("child-a", "task-a"); err != nil {
		t.Fatal(err)
	}
	if err := repository.ForgetChild("missing", "task"); err != nil {
		t.Fatalf("ForgetChild missing = %v", err)
	}
	if _, _, ok := repository.ChildRelation("child-a"); ok {
		t.Fatal("child relation was not forgotten")
	}
}

func TestAgentSessionConcurrentResumeJoinsLifecycle(t *testing.T) {
	var calls int
	var mu sync.Mutex
	entered := make(chan struct{})
	release := make(chan struct{})
	session := &agentSession{id: "same", execute: func(context.Context) error {
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
