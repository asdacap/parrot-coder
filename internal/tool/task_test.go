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
}

func (c *recordingTaskController) Interrupt(context.Context, string, string) (managedtask.Active, error) {
	return c.interrupted, nil
}

func (c *recordingTaskController) ListActive(string) []managedtask.Active { return c.items }

func TestTaskToolsReturnStableJSONShapes(t *testing.T) {
	started := time.Date(2026, time.July, 19, 1, 2, 3, 0, time.UTC)
	controller := &recordingTaskController{interrupted: managedtask.Active{
		ID: "proc_test", Kind: managedtask.KindShell, Status: "canceled", StartedAt: started,
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
	if result.Text != `{"task_id":"proc_test","kind":"shell","status":"canceled","started_at":"2026-07-19T01:02:03Z"}` {
		t.Fatalf("interrupt = %s", result.Text)
	}
}
