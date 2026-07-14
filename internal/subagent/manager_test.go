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
