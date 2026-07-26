package tool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	managedtask "github.com/amirulashraf/parrot-coder/internal/task"
)

type recordingTaskController struct {
	items       []managedtask.Active
	interrupted managedtask.Active
	wait        func(context.Context, string, string, managedtask.Kind) (managedtask.Result, error)
}

func (c *recordingTaskController) Interrupt(context.Context, string, string) (managedtask.Active, error) {
	return c.interrupted, nil
}

func (c *recordingTaskController) ListActive(string) []managedtask.Active { return c.items }
func (c *recordingTaskController) WaitKind(ctx context.Context, sessionID, taskID string, kind managedtask.Kind) (managedtask.Result, error) {
	return c.wait(ctx, sessionID, taskID, kind)
}

func TestWaitTaskReturnsCompletionAndYieldsWithoutFailure(t *testing.T) {
	controller := &recordingTaskController{wait: func(_ context.Context, sessionID, taskID string, kind managedtask.Kind) (managedtask.Result, error) {
		if sessionID != "session" || taskID != "task_agent" || kind != managedtask.KindAgent {
			t.Fatalf("wait = %q, %q, %q", sessionID, taskID, kind)
		}
		return managedtask.Result{ID: taskID, Kind: managedtask.KindAgent, Status: "succeeded", Output: "done"}, nil
	}}
	item := &WaitTool{Kind: managedtask.KindAgent, Controller: controller}
	plan, err := item.Plan(context.Background(), json.RawMessage(`{"session_id":"task_agent"}`), CallContext{SessionID: "session"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := item.Execute(context.Background(), plan, CallContext{SessionID: "session"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Metadata["task_id"] != "task_agent" || result.Metadata["kind"] != "agent" || result.Metadata["status"] != "succeeded" || result.Metadata["output"] != "done" {
		t.Fatalf("completion = %s", result.Text)
	}
	if _, ok := result.Metadata["elapsed_ms"].(float64); !ok || result.Metadata["yielded"] != nil {
		t.Fatalf("completion metadata = %#v", result.Metadata)
	}

	controller.wait = func(ctx context.Context, _, taskID string, _ managedtask.Kind) (managedtask.Result, error) {
		<-ctx.Done()
		return managedtask.Result{ID: taskID, Kind: managedtask.KindAgent, Status: "running"}, ctx.Err()
	}
	plan, err = item.Plan(context.Background(), json.RawMessage(`{"session_id":"task_agent","yield_after_ms":1}`), CallContext{SessionID: "session"})
	if err != nil {
		t.Fatal(err)
	}
	result, err = item.Execute(context.Background(), plan, CallContext{SessionID: "session"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Metadata["task_id"] != "task_agent" || result.Metadata["kind"] != "agent" || result.Metadata["status"] != "running" || result.Metadata["yielded"] != true {
		t.Fatalf("yield = %s", result.Text)
	}
	if elapsed, ok := result.Metadata["elapsed_ms"].(float64); !ok || elapsed < 1 {
		t.Fatalf("elapsed_ms = %#v", result.Metadata["elapsed_ms"])
	}
	if described, err := item.DescribeRequest(plan.CanonicalInput); err != nil || described != "Wait for agent task_agent" {
		t.Fatalf("description = %q, %v", described, err)
	}
}

func TestWaitTaskYieldsForNewAndPendingSteer(t *testing.T) {
	for _, test := range []struct {
		name string
		kind managedtask.Kind
		raw  string
	}{
		{name: "agent", kind: managedtask.KindAgent, raw: `{"session_id":"task_agent"}`},
		{name: "process", kind: managedtask.KindShell, raw: `{"process_id":"proc_test"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			started := make(chan struct{}, 1)
			controller := &recordingTaskController{wait: func(ctx context.Context, _, taskID string, kind managedtask.Kind) (managedtask.Result, error) {
				started <- struct{}{}
				<-ctx.Done()
				return managedtask.Result{ID: taskID, Kind: kind, Status: "running"}, ctx.Err()
			}}
			item := &WaitTool{Kind: test.kind, Controller: controller}
			plan, err := item.Plan(context.Background(), json.RawMessage(test.raw), CallContext{SessionID: "session"})
			if err != nil {
				t.Fatal(err)
			}

			steer := make(chan struct{})
			done := make(chan Result, 1)
			go func() {
				result, executeErr := item.Execute(context.Background(), plan, CallContext{SessionID: "session", Steer: steer})
				if executeErr != nil {
					t.Errorf("Execute() error = %v", executeErr)
				}
				done <- result
			}()
			<-started
			close(steer)
			result := <-done
			if result.Metadata["status"] != "running" || result.Metadata["yielded"] != true {
				t.Fatalf("new steer result = %s", result.Text)
			}

			pending := make(chan struct{})
			close(pending)
			result, err = item.Execute(context.Background(), plan, CallContext{SessionID: "session", Steer: pending})
			if err != nil {
				t.Fatal(err)
			}
			<-started
			if result.Metadata["status"] != "running" || result.Metadata["yielded"] != true {
				t.Fatalf("pending steer result = %s", result.Text)
			}
		})
	}
}

func TestWaitTaskRejectsInvalidRequestsAndPropagatesCancellation(t *testing.T) {
	controller := &recordingTaskController{wait: func(ctx context.Context, _, _ string, _ managedtask.Kind) (managedtask.Result, error) {
		<-ctx.Done()
		return managedtask.Result{}, ctx.Err()
	}}
	item := &WaitTool{Kind: managedtask.KindAgent, Controller: controller}
	for _, test := range []struct {
		name string
		raw  string
		call CallContext
	}{
		{name: "missing caller", raw: `{"session_id":"task_agent"}`},
		{name: "missing task", raw: `{}`, call: CallContext{SessionID: "session"}},
		{name: "negative yield", raw: `{"session_id":"task_agent","yield_after_ms":-1}`, call: CallContext{SessionID: "session"}},
		{name: "overflowing yield", raw: `{"session_id":"task_agent","yield_after_ms":9223372036854775807}`, call: CallContext{SessionID: "session"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := item.Plan(context.Background(), json.RawMessage(test.raw), test.call); err == nil {
				t.Fatal("Plan succeeded")
			}
		})
	}

	plan, err := item.Plan(context.Background(), json.RawMessage(`{"session_id":"task_agent"}`), CallContext{SessionID: "session"})
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

	for name, description := range map[string]string{
		"task_interrupt":   interrupt.Descriptor().Description,
		"task_list_active": list.Descriptor().Description,
	} {
		contract := strings.ToLower(description)
		if !strings.Contains(contract, "process") || !strings.Contains(contract, "session") {
			t.Errorf("%s description = %q", name, description)
		}
	}
	for _, test := range []struct {
		kind       managedtask.Kind
		identifier string
	}{
		{kind: managedtask.KindAgent, identifier: "session"},
		{kind: managedtask.KindShell, identifier: "process"},
	} {
		item := &WaitTool{Kind: test.kind}
		if description := strings.ToLower(item.Descriptor().Description); !strings.Contains(description, test.identifier) {
			t.Errorf("%s description = %q", item.ID(), description)
		}
		if schema := strings.ToLower(string(item.JSONSchema())); !strings.Contains(schema, test.identifier) {
			t.Errorf("%s schema = %s", item.ID(), schema)
		}
		if presentation := item.Presentation(); !presentation.Modeline || !presentation.LiveOnly {
			t.Errorf("%s presentation = %#v", item.ID(), presentation)
		}
	}
}
