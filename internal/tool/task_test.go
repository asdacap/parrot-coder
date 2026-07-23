package tool

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	managedtask "github.com/amirulashraf/parrot-coder/internal/task"
)

type recordingTaskController struct {
	items       []managedtask.Active
	interrupted managedtask.Active
	wait        func(context.Context, string, string) (managedtask.Result, error)
}

func (c *recordingTaskController) Interrupt(context.Context, string, string) (managedtask.Active, error) {
	return c.interrupted, nil
}

func (c *recordingTaskController) ListActive(string) []managedtask.Active { return c.items }
func (c *recordingTaskController) Wait(ctx context.Context, sessionID, taskID string) (managedtask.Result, error) {
	return c.wait(ctx, sessionID, taskID)
}

func TestWaitTaskReturnsCompletionAndYieldsWithoutFailure(t *testing.T) {
	controller := &recordingTaskController{wait: func(_ context.Context, sessionID, taskID string) (managedtask.Result, error) {
		if sessionID != "session" || taskID != "task_agent" {
			t.Fatalf("wait = %q, %q", sessionID, taskID)
		}
		return managedtask.Result{ID: taskID, Kind: managedtask.KindAgent, Status: "succeeded", Output: "done"}, nil
	}}
	item := &WaitTaskTool{Controller: controller}
	plan, err := item.Plan(context.Background(), json.RawMessage(`{"task_id":"task_agent"}`), CallContext{SessionID: "session"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := item.Execute(context.Background(), plan, CallContext{SessionID: "session"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != `{"task_id":"task_agent","kind":"agent","status":"succeeded","output":"done"}` {
		t.Fatalf("completion = %s", result.Text)
	}

	controller.wait = func(ctx context.Context, _, taskID string) (managedtask.Result, error) {
		<-ctx.Done()
		return managedtask.Result{ID: taskID, Kind: managedtask.KindAgent, Status: "running"}, ctx.Err()
	}
	plan, err = item.Plan(context.Background(), json.RawMessage(`{"task_id":"task_agent","yield_after_ms":1}`), CallContext{SessionID: "session"})
	if err != nil {
		t.Fatal(err)
	}
	result, err = item.Execute(context.Background(), plan, CallContext{SessionID: "session"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != `{"task_id":"task_agent","kind":"agent","status":"running"}` {
		t.Fatalf("yield = %s", result.Text)
	}
	if described, err := item.DescribeRequest(plan.CanonicalInput); err != nil || described != "Wait for task task_agent" {
		t.Fatalf("description = %q, %v", described, err)
	}
}

func TestWaitTaskRejectsInvalidRequestsAndPropagatesCancellation(t *testing.T) {
	controller := &recordingTaskController{wait: func(ctx context.Context, _, _ string) (managedtask.Result, error) {
		<-ctx.Done()
		return managedtask.Result{}, ctx.Err()
	}}
	item := &WaitTaskTool{Controller: controller}
	for _, test := range []struct {
		name string
		raw  string
		call CallContext
	}{
		{name: "missing caller", raw: `{"task_id":"task_agent"}`},
		{name: "missing task", raw: `{}`, call: CallContext{SessionID: "session"}},
		{name: "negative yield", raw: `{"task_id":"task_agent","yield_after_ms":-1}`, call: CallContext{SessionID: "session"}},
		{name: "overflowing yield", raw: `{"task_id":"task_agent","yield_after_ms":9223372036854775807}`, call: CallContext{SessionID: "session"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := item.Plan(context.Background(), json.RawMessage(test.raw), test.call); err == nil {
				t.Fatal("Plan succeeded")
			}
		})
	}

	plan, err := item.Plan(context.Background(), json.RawMessage(`{"task_id":"task_agent"}`), CallContext{SessionID: "session"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := item.Execute(ctx, plan, CallContext{SessionID: "session"}); err != context.Canceled {
		t.Fatalf("cancellation error = %v", err)
	}
}

func TestTaskToolsReturnStableJSONShapes(t *testing.T) {
	started := time.Date(2026, time.July, 19, 1, 2, 3, 0, time.UTC)
	controller := &recordingTaskController{interrupted: managedtask.Active{
		ID: "proc_test", SessionID: "session", Kind: managedtask.KindShell, Status: "canceled", StartedAt: started,
	}}
	list := &TaskTool{Kind: "task_list_active", Controller: controller}
	plan, err := list.Plan(context.Background(), json.RawMessage(`{}`), CallContext{SessionID: "session"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := list.Execute(context.Background(), plan, CallContext{SessionID: "session"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != `{"tasks":[]}` {
		t.Fatalf("empty list = %s", result.Text)
	}

	interrupt := &TaskTool{Kind: "task_interrupt", Controller: controller}
	plan, err = interrupt.Plan(context.Background(), json.RawMessage(`{"task_id":"proc_test"}`), CallContext{SessionID: "session"})
	if err != nil {
		t.Fatal(err)
	}
	result, err = interrupt.Execute(context.Background(), plan, CallContext{SessionID: "session"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != `{"task_id":"proc_test","session_id":"session","kind":"shell","status":"canceled","started_at":"2026-07-19T01:02:03Z"}` {
		t.Fatalf("interrupt = %s", result.Text)
	}
}
