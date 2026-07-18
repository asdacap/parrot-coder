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

func TestDepthCycleCancelAndResultBound(t *testing.T) {
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
	manager := NewManager(executor, Config{MaxDepth: 2, MaxResultBytes: 5, Timeout: time.Minute})
	limited := NewManager(executor, Config{MaxPromptBytes: 3})
	if _, err := limited.Launch("p", nil, Request{Prompt: "large", Agent: "worker"}); !errors.Is(err, ErrRequestLimit) {
		t.Fatalf("request limit error = %v", err)
	}
	if _, err := manager.Launch("p", []string{"a", "b"}, Request{Prompt: "x", Agent: "c"}); !errors.Is(err, ErrDepth) {
		t.Fatalf("depth error = %v", err)
	}
	if _, err := manager.Launch("p", []string{"a"}, Request{Prompt: "x", Agent: "a"}); !errors.Is(err, ErrCycle) {
		t.Fatalf("cycle error = %v", err)
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

	largeManager := NewManager(executorFunc(func(context.Context, Execution) (string, error) { return strings.Repeat("y", 20), nil }), Config{MaxResultBytes: 5})
	largeID, err := largeManager.Launch("p", nil, Request{Prompt: "large", Agent: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	large, err := largeManager.Await(context.Background(), "p", largeID)
	if err != nil || large.Output != "yyyyy" || !large.Truncated {
		t.Fatalf("large = %#v, %v", large, err)
	}
}

func TestTimeout(t *testing.T) {
	executor := executorFunc(func(ctx context.Context, _ Execution) (string, error) { <-ctx.Done(); return "", ctx.Err() })
	manager := NewManager(executor, Config{Timeout: 10 * time.Millisecond})
	id, err := manager.Launch("p", nil, Request{Prompt: "wait", Agent: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := manager.Await(context.Background(), "p", id)
	if !errors.Is(err, ErrTimeout) || task.Status != StatusTimedOut {
		t.Fatalf("task = %#v, error = %v", task, err)
	}
}

func TestProgressAccumulatesAndReportsSnapshots(t *testing.T) {
	var snapshots []Task
	manager := NewManager(executorFunc(func(_ context.Context, execution Execution) (string, error) {
		execution.ReportProgress(Progress{Usage: Usage{InputTokens: 10, OutputTokens: 2, TotalTokens: 12}, ToolUses: 1})
		execution.ReportProgress(Progress{Usage: Usage{InputTokens: 20, OutputTokens: 3, TotalTokens: 23}, ToolUses: 2})
		return "done", nil
	}), Config{OnProgress: func(task Task) { snapshots = append(snapshots, task) }})
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
	if len(snapshots) != 2 || snapshots[0].Usage.TotalTokens != 12 || snapshots[1].ToolUses != 3 {
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

func (e *reusableExecutor) Execute(ctx context.Context, execution Execution) (string, error) {
	if execution.SessionID == "" {
		execution.RegisterSession("session-" + execution.TaskID)
	}
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

func TestReusableAgentLifecycleOwnershipAndSelectiveWait(t *testing.T) {
	executor := newReusableExecutor()
	manager := NewManager(executor, Config{MaxConcurrent: 4, MaxConcurrentPerParent: 4, MaxDepth: 4})

	id, err := manager.Spawn(context.Background(), "root", "build", Request{Prompt: "first", Agent: "explore"})
	if err != nil {
		t.Fatal(err)
	}
	first := <-executor.runs
	if first.SessionID != "" || first.Turn != 1 || first.Request.Prompt != "first" {
		t.Fatalf("first execution = %#v", first)
	}
	task, err := manager.Status("root", id)
	if err != nil || task.SessionID == "" || task.Status != StatusRunning || task.Turn != 1 {
		t.Fatalf("running task = %#v, %v", task, err)
	}
	if _, err := manager.Status("unrelated", id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unrelated status error = %v", err)
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
	waitCtx, cancelWait := context.WithTimeout(context.Background(), 30*time.Millisecond)
	waited := make(chan error, 1)
	go func() {
		_, waitErr := manager.Wait(waitCtx, "root", []string{id})
		waited <- waitErr
	}()
	if _, err := manager.Send(context.Background(), "root", otherID, "unrelated"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-waited:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("selective wait error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("selective wait did not time out")
	}
	cancelWait()

	executor.release(id, "first output")
	completed, err := manager.Await(context.Background(), "root", id)
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
	executor.release(id, "second output")
	completed, err = manager.Await(context.Background(), "root", id)
	if err != nil || completed.Output != "second output" {
		t.Fatalf("second completion = %#v, %v", completed, err)
	}

	if _, err := manager.Spawn(context.Background(), task.SessionID, "explore", Request{Prompt: "cycle", Agent: "build"}); !errors.Is(err, ErrCycle) {
		t.Fatalf("derived cycle error = %v", err)
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

func TestSpawnFailureAndCancellationDoNotRetainUnreachableAgents(t *testing.T) {
	for _, test := range []struct {
		name     string
		executor Executor
		cancel   bool
	}{
		{name: "failure", executor: executorFunc(func(context.Context, Execution) (string, error) { return "", errors.New("create failed") })},
		{name: "cancellation", cancel: true, executor: executorFunc(func(ctx context.Context, _ Execution) (string, error) { <-ctx.Done(); return "", ctx.Err() })},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager := NewManager(test.executor, Config{})
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
