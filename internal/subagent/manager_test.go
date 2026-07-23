package subagent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

type executorFunc func(context.Context, Execution) (string, error)

func (f executorFunc) Execute(ctx context.Context, execution Execution) (string, error) {
	return f(ctx, execution)
}

type preparedExecutor struct {
	prepare func(context.Context, Execution) (string, error)
	execute func(context.Context, Execution) (string, error)
}

func (e preparedExecutor) Prepare(ctx context.Context, execution Execution) (string, error) {
	return e.prepare(ctx, execution)
}

func (e preparedExecutor) Execute(ctx context.Context, execution Execution) (string, error) {
	return e.execute(ctx, execution)
}

func TestFriendlyNames(t *testing.T) {
	executions := make(chan Execution, 4)
	manager := NewManager(executorFunc(func(_ context.Context, execution Execution) (string, error) {
		executions <- execution
		return "done", nil
	}), Config{MaxConcurrent: 8, MaxConcurrentPerParent: 4, NameGenerator: func() string { return "Happy Otter" }})

	ids := make([]string, 0, 3)
	for _, request := range []Request{
		{Prompt: "explicit", Agent: "worker", Name: "  API Review!! "},
		{Prompt: "generated", Agent: "explorer"},
		{Prompt: "duplicate", Agent: "worker", Name: "API Review"},
	} {
		id, err := manager.Launch("parent", nil, request)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}

	wantNames := []string{"api-review", "explorer-happy-otter", "api-review-2"}
	for i, id := range ids {
		task, err := manager.Await(context.Background(), "parent", id)
		if err != nil || task.Name != wantNames[i] {
			t.Fatalf("task %d = %#v, %v; want name %q", i, task, err, wantNames[i])
		}
	}
	gotNames := make(map[string]string)
	for range ids {
		execution := <-executions
		gotNames[execution.Request.Prompt] = execution.Request.Name
	}
	if gotNames["explicit"] != wantNames[0] || gotNames["generated"] != wantNames[1] || gotNames["duplicate"] != wantNames[2] {
		t.Fatalf("execution names = %#v", gotNames)
	}

	following, err := manager.FollowUp("parent", ids[0], Request{Prompt: "again", Name: "ignored"})
	if err != nil {
		t.Fatal(err)
	}
	if following.Name != "api-review" {
		t.Fatalf("follow-up name = %q", following.Name)
	}
	if task, err := manager.Await(context.Background(), "parent", ids[0]); err != nil || task.Name != "api-review" {
		t.Fatalf("follow-up task = %#v, %v", task, err)
	}
}

func TestLaunchConcurrencyAwaitAndStatus(t *testing.T) {
	release := make(chan struct{})
	started := make(chan Execution, 2)
	executor := executorFunc(func(ctx context.Context, execution Execution) (string, error) {
		started <- execution
		select {
		case <-release:
			return "result", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	})
	manager := NewManager(executor, Config{MaxConcurrent: 2, MaxConcurrentPerParent: 1, MaxDepth: 3})
	id, err := manager.Launch("parent-a", []string{"root"}, Request{Prompt: "work", Agent: "worker", Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case execution := <-started:
		if execution.Request.Model != "m" || len(execution.Lineage) != 1 {
			t.Fatalf("execution = %#v", execution)
		}
	case <-time.After(time.Second):
		t.Fatal("executor did not start")
	}
	if _, err := manager.Launch("parent-a", nil, Request{Prompt: "more", Agent: "other"}); !errors.Is(err, ErrConcurrency) {
		t.Fatalf("per-parent error = %v", err)
	}
	id2, err := manager.Launch("parent-b", nil, Request{Prompt: "more", Agent: "other"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Launch("parent-c", nil, Request{Prompt: "more", Agent: "third"}); !errors.Is(err, ErrConcurrency) {
		t.Fatalf("global error = %v", err)
	}

	waitCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := manager.Await(waitCtx, "parent-a", id); !errors.Is(err, context.Canceled) {
		t.Fatalf("await error = %v", err)
	}
	status, err := manager.Status("parent-a", id)
	if err != nil || status.Status != StatusRunning {
		t.Fatalf("status = %#v, %v", status, err)
	}
	close(release)
	result, err := manager.Await(context.Background(), "parent-a", id)
	if err != nil || result.Status != StatusSucceeded || result.Output != "result" {
		t.Fatalf("result = %#v, %v", result, err)
	}
	if _, err := manager.Await(context.Background(), "parent-b", id2); err != nil {
		t.Fatal(err)
	}
	listed := manager.List("parent-a")
	if len(listed) != 1 || listed[0].ID != id {
		t.Fatalf("list = %#v", listed)
	}
}

func TestDepthRecursionCancelAndResultBound(t *testing.T) {
	started := make(chan struct{})
	var once sync.Once
	executor := executorFunc(func(ctx context.Context, execution Execution) (string, error) {
		once.Do(func() { close(started) })
		if execution.Request.Prompt == "large" {
			return strings.Repeat("x", 20), nil
		}
		<-ctx.Done()
		return "", ctx.Err()
	})
	manager := NewManager(executor, Config{MaxConcurrent: 8, MaxConcurrentPerParent: 4, MaxDepth: 2, MaxResultBytes: 5, AgentRecursionLimit: func(string) int { return 1 }})
	limited := NewManager(executor, Config{MaxConcurrent: 8, MaxConcurrentPerParent: 4, MaxPromptBytes: 3})
	if _, err := limited.Launch("p", nil, Request{Prompt: "large", Agent: "worker"}); !errors.Is(err, ErrRequestLimit) {
		t.Fatalf("request limit error = %v", err)
	}
	if _, err := manager.Launch("p", []string{"a", "b"}, Request{Prompt: "x", Agent: "c"}); !errors.Is(err, ErrDepth) {
		t.Fatalf("depth error = %v", err)
	}
	if _, err := manager.Launch("p", []string{"a"}, Request{Prompt: "x", Agent: "a"}); !errors.Is(err, ErrRecursion) {
		t.Fatalf("recursion error = %v", err)
	}
	id, err := manager.Launch("p", nil, Request{Prompt: "wait", Agent: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("executor did not start")
	}
	if err := manager.Cancel(context.Background(), "p", id); err != nil {
		t.Fatal(err)
	}
	task, err := manager.Status("p", id)
	if err != nil || task.Status != StatusCanceled {
		t.Fatalf("task = %#v, %v", task, err)
	}
	if _, err := manager.Await(context.Background(), "p", id); !errors.Is(err, ErrCanceled) {
		t.Fatalf("await error = %v", err)
	}

	largeManager := NewManager(executorFunc(func(context.Context, Execution) (string, error) { return strings.Repeat("y", 20), nil }), Config{MaxConcurrent: 8, MaxConcurrentPerParent: 4, MaxResultBytes: 5})
	largeID, err := largeManager.Launch("p", nil, Request{Prompt: "large", Agent: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	large, err := largeManager.Await(context.Background(), "p", largeID)
	if err != nil || large.Output != "yyyyy" || !large.Truncated {
		t.Fatalf("large = %#v, %v", large, err)
	}
}

func TestObserverReturnsTerminalLifecycleAsData(t *testing.T) {
	for _, test := range []struct {
		name      string
		executor  Executor
		config    Config
		interrupt bool
		status    Status
		errorText string
	}{
		{name: "failed", executor: executorFunc(func(context.Context, Execution) (string, error) { return "", errors.New("failed") }), config: Config{MaxConcurrent: 8, MaxConcurrentPerParent: 4}, status: StatusFailed, errorText: "failed"},
		{name: "canceled", executor: executorFunc(func(ctx context.Context, _ Execution) (string, error) { <-ctx.Done(); return "", ctx.Err() }), config: Config{MaxConcurrent: 8, MaxConcurrentPerParent: 4}, interrupt: true, status: StatusCanceled, errorText: ErrCanceled.Error()},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager := NewManager(test.executor, test.config)
			id, err := manager.Launch("parent", nil, Request{Prompt: "work", Agent: "explore"})
			if err != nil {
				t.Fatal(err)
			}
			observer, err := manager.Observe("parent", id)
			if err != nil {
				t.Fatal(err)
			}
			if test.interrupt {
				if _, err := manager.Interrupt(context.Background(), "parent", id); err != nil {
					t.Fatal(err)
				}
			}
			task, err := observer.Wait(context.Background())
			if err != nil || task.Status != test.status || task.Error != test.errorText {
				t.Fatalf("observer result = %#v, %v", task, err)
			}
		})
	}
}

func TestProgressAccumulatesAndReportsSnapshots(t *testing.T) {
	var snapshots []Task
	terminalProgress := make(chan struct{})
	manager := NewManager(executorFunc(func(_ context.Context, execution Execution) (string, error) {
		execution.ReportProgress(Progress{Usage: Usage{InputTokens: 10, OutputTokens: 2, TotalTokens: 12}, ToolUses: 1})
		execution.ReportProgress(Progress{Usage: Usage{InputTokens: 20, OutputTokens: 3, TotalTokens: 23}, ToolUses: 2})
		return "done", nil
	}), Config{MaxConcurrent: 8, MaxConcurrentPerParent: 4, OnProgress: func(task Task) {
		snapshots = append(snapshots, task)
		if task.Status != StatusRunning {
			close(terminalProgress)
		}
	}})
	id, err := manager.Launch("parent", nil, Request{Prompt: "work", Agent: "explore", ToolCallID: "call"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := manager.Await(context.Background(), "parent", id)
	if err != nil {
		t.Fatal(err)
	}
	if task.Usage.TotalTokens != 35 || task.Usage.InputTokens != 30 || task.Usage.OutputTokens != 5 || task.ToolUses != 3 || task.ToolCallID != "call" {
		t.Fatalf("task progress = %#v", task)
	}
	<-terminalProgress
	if len(snapshots) != 3 || snapshots[0].Usage.TotalTokens != 12 || snapshots[1].ToolUses != 3 || snapshots[2].Status != StatusSucceeded {
		t.Fatalf("snapshots = %#v", snapshots)
	}
}

type reusableExecutor struct {
	mu       sync.Mutex
	runs     chan Execution
	releases map[string]chan string
	sends    chan string
}

func newReusableExecutor() *reusableExecutor {
	return &reusableExecutor{runs: make(chan Execution, 8), releases: make(map[string]chan string), sends: make(chan string, 8)}
}

func (e *reusableExecutor) Prepare(_ context.Context, execution Execution) (string, error) {
	return "session-" + execution.TaskID, nil
}

func (e *reusableExecutor) Execute(ctx context.Context, execution Execution) (string, error) {
	e.mu.Lock()
	release := make(chan string, 1)
	e.releases[execution.TaskID] = release
	e.mu.Unlock()
	e.runs <- execution
	select {
	case output := <-release:
		return output, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (e *reusableExecutor) release(id, output string) {
	e.mu.Lock()
	release := e.releases[id]
	e.mu.Unlock()
	release <- output
}

func (e *reusableExecutor) Send(_ context.Context, execution Execution, message string) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.sends <- execution.SessionID + ":" + message
	return "message-1", nil
}

func TestReusableAgentLifecycleOwnershipObservationAndActiveListing(t *testing.T) {
	executor := newReusableExecutor()
	manager := NewManager(executor, Config{MaxConcurrent: 4, MaxConcurrentPerParent: 4, MaxDepth: 4})

	id, err := manager.Spawn(context.Background(), "root", "build", Request{Prompt: "first", Agent: "explore"})
	if err != nil {
		t.Fatal(err)
	}
	first := <-executor.runs
	if first.SessionID != "session-"+first.TaskID || first.Turn != 1 || first.Request.Prompt != "first" {
		t.Fatalf("first execution = %#v", first)
	}
	task, err := manager.Status("root", id)
	if err != nil || task.SessionID == "" || task.Status != StatusRunning || task.Turn != 1 {
		t.Fatalf("running task = %#v, %v", task, err)
	}
	if _, err := manager.Status("unrelated", id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unrelated status error = %v", err)
	}
	if _, err := manager.Observe("unrelated", id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unrelated observe error = %v", err)
	}
	if messageID, err := manager.Send(context.Background(), "root", id, "steer"); err != nil || messageID != "message-1" {
		t.Fatalf("send = %q, %v", messageID, err)
	}
	if sent := <-executor.sends; sent != task.SessionID+":steer" {
		t.Fatalf("sent = %q", sent)
	}

	otherID, err := manager.Spawn(context.Background(), "root", "build", Request{Prompt: "other", Agent: "plan"})
	if err != nil {
		t.Fatal(err)
	}
	<-executor.runs
	firstObserver, err := manager.Observe("root", id)
	if err != nil {
		t.Fatal(err)
	}
	active := manager.ListActive("root")
	if len(active) != 2 || active[0].ID != min(id, otherID) || active[1].ID != max(id, otherID) {
		t.Fatalf("active tasks = %#v", active)
	}
	if _, err := manager.Send(context.Background(), "root", otherID, "unrelated"); err != nil {
		t.Fatal(err)
	}
	waitCtx, cancelWait := context.WithTimeout(context.Background(), 10*time.Millisecond)
	if _, err := firstObserver.Wait(waitCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("observer wait error = %v", err)
	}
	cancelWait()

	executor.release(id, "first output")
	completed, err := firstObserver.Wait(context.Background())
	if err != nil || completed.Output != "first output" || completed.Status != StatusSucceeded {
		t.Fatalf("completed = %#v, %v", completed, err)
	}
	following, err := manager.FollowUp("root", id, Request{Prompt: "second", ToolCallID: "follow-up"})
	if err != nil || following.Turn != 2 || following.SessionID != task.SessionID {
		t.Fatalf("follow-up = %#v, %v", following, err)
	}
	second := <-executor.runs
	if second.SessionID != task.SessionID || second.Turn != 2 || second.Request.Prompt != "second" {
		t.Fatalf("second execution = %#v", second)
	}
	if previous, err := firstObserver.Wait(context.Background()); err != nil || previous.Turn != 1 || previous.Output != "first output" {
		t.Fatalf("captured first turn = %#v, %v", previous, err)
	}
	active = manager.ListActive("root")
	if len(active) != 2 {
		t.Fatalf("active after follow-up = %#v", active)
	}
	executor.release(id, "second output")
	completed, err = manager.Await(context.Background(), "root", id)
	if err != nil || completed.Output != "second output" {
		t.Fatalf("second completion = %#v, %v", completed, err)
	}
	active = manager.ListActive("root")
	if len(active) != 1 || active[0].ID != otherID {
		t.Fatalf("active after completion = %#v", active)
	}

	if _, err := manager.Interrupt(context.Background(), "root", otherID); err != nil {
		t.Fatal(err)
	}
	if err := manager.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Launch("root", nil, Request{Prompt: "closed", Agent: "explore"}); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed launch error = %v", err)
	}
}

func TestObserveAndListActivePreserveDescendantVisibility(t *testing.T) {
	executor := newReusableExecutor()
	manager := NewManager(executor, Config{MaxConcurrent: 4, MaxConcurrentPerParent: 4, MaxDepth: 4})
	parentID, err := manager.Spawn(context.Background(), "root", "build", Request{Prompt: "parent", Agent: "explore"})
	if err != nil {
		t.Fatal(err)
	}
	<-executor.runs
	parent, err := manager.Status("root", parentID)
	if err != nil {
		t.Fatal(err)
	}
	childID, err := manager.Spawn(context.Background(), parent.SessionID, "explore", Request{Prompt: "child", Agent: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	<-executor.runs

	rootActive := manager.ListActive("root")
	parentActive := manager.ListActive(parent.SessionID)
	if len(rootActive) != 2 || len(parentActive) != 1 || parentActive[0].ID != childID || len(manager.ListActive("unrelated")) != 0 {
		t.Fatalf("active tasks: root=%#v parent=%#v", rootActive, parentActive)
	}
	if _, err := manager.Observe(parent.SessionID, childID); err != nil {
		t.Fatalf("observe child: %v", err)
	}
	child, err := manager.Status(parent.SessionID, childID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Observe(child.SessionID, parentID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("observe ancestor error = %v", err)
	}
	if _, err := manager.Interrupt(context.Background(), "root", childID); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Interrupt(context.Background(), "root", parentID); err != nil {
		t.Fatal(err)
	}
}

func TestLaunchEnforcesPerAgentRecursionLimits(t *testing.T) {
	manager := NewManager(executorFunc(func(context.Context, Execution) (string, error) { return "", nil }), Config{MaxConcurrent: 16, MaxConcurrentPerParent: 16, AgentIdentity: func(id string) string {
		if id == "explore" {
			return "explorer"
		}
		return id
	}, AgentRecursionLimit: func(id string) int {
		if id == "plan" {
			return 1
		}
		return 3
	}})
	for _, test := range []struct {
		name    string
		lineage []string
		target  string
		wantErr error
	}{
		{name: "plan first occurrence", target: "plan"},
		{name: "plan second occurrence", lineage: []string{"plan"}, target: "plan", wantErr: ErrRecursion},
		{name: "other role third occurrence", lineage: []string{"worker", "worker"}, target: "worker"},
		{name: "other role fourth occurrence", lineage: []string{"worker", "worker", "worker"}, target: "worker", wantErr: ErrRecursion},
		{name: "different roles do not consume limit", lineage: []string{"worker", "plan", "explorer"}, target: "worker"},
		{name: "aliases share limit", lineage: []string{"explore", "explorer", "explore"}, target: "explorer", wantErr: ErrRecursion},
		{name: "maximum depth remains independent", lineage: []string{"a", "b", "c", "d"}, target: "worker", wantErr: ErrDepth},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := manager.Launch("root-"+test.name, test.lineage, Request{Prompt: "recurse", Agent: test.target})
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("Launch(%v -> %q) error = %v, want %v", test.lineage, test.target, err, test.wantErr)
			}
		})
	}

	derived := NewManager(preparedExecutor{
		prepare: func(_ context.Context, execution Execution) (string, error) {
			return "session-" + execution.TaskID, nil
		},
		execute: func(context.Context, Execution) (string, error) { return "", nil },
	}, Config{MaxConcurrent: 8, MaxConcurrentPerParent: 4})
	parentSession := "root"
	for range 2 {
		id, err := derived.Spawn(context.Background(), parentSession, "build", Request{Prompt: "recurse", Agent: "build"})
		if err != nil {
			t.Fatal(err)
		}
		task, err := derived.Status(parentSession, id)
		if err != nil {
			t.Fatal(err)
		}
		parentSession = task.SessionID
	}
	if _, err := derived.Spawn(context.Background(), parentSession, "build", Request{Prompt: "recurse", Agent: "build"}); !errors.Is(err, ErrRecursion) {
		t.Fatalf("derived recursion error = %v", err)
	}
}

func TestSpawnFailureAndCancellationDoNotRetainUnreachableAgents(t *testing.T) {
	for _, test := range []struct {
		name     string
		executor Executor
		cancel   bool
	}{
		{name: "failure", executor: preparedExecutor{
			prepare: func(context.Context, Execution) (string, error) { return "", errors.New("create failed") },
			execute: func(context.Context, Execution) (string, error) { return "", nil },
		}},
		{name: "cancellation", cancel: true, executor: preparedExecutor{
			prepare: func(ctx context.Context, _ Execution) (string, error) { <-ctx.Done(); return "", ctx.Err() },
			execute: func(context.Context, Execution) (string, error) { return "", nil },
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager := NewManager(test.executor, Config{MaxConcurrent: 8, MaxConcurrentPerParent: 4})
			ctx := context.Background()
			if test.cancel {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			if _, err := manager.Spawn(ctx, "root", "build", Request{Prompt: "work", Agent: "explore"}); err == nil {
				t.Fatal("spawn succeeded")
			}
			if err := manager.Shutdown(context.Background()); err != nil {
				t.Fatal(err)
			}
			if tasks := manager.List("root"); len(tasks) != 0 {
				t.Fatalf("retained tasks = %#v", tasks)
			}
		})
	}
}
