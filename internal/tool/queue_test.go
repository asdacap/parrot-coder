package tool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/amirulashraf/parrot-coder/internal/queue"
)

func TestQueueToolsLifecycle(t *testing.T) {
	state := t.TempDir()
	sessionID := "ses_test"
	if err := os.MkdirAll(filepath.Join(state, "session", sessionID), 0o700); err != nil {
		t.Fatal(err)
	}
	store := queue.New(state)
	call := CallContext{SessionID: sessionID}
	tests := []struct {
		kind  string
		input string
		check func(map[string]any)
	}{
		{kind: "queue_create", input: `{"name":"build-work-now","description":"release tasks"}`, check: func(got map[string]any) {
			if got["name"] != "build-work-now" || got["description"] != "release tasks" || got["size"] != float64(0) {
				t.Fatalf("create output = %#v", got)
			}
		}},
		{kind: "queue_push", input: `{"name":"build-work-now","item":"first"}`, check: func(got map[string]any) {
			if got["size"] != float64(1) {
				t.Fatalf("push output = %#v", got)
			}
		}},
		{kind: "queue_monitor", input: `{"name":"build-work-now"}`, check: func(got map[string]any) {
			if got["size"] != float64(1) || got["monitored"] != true {
				t.Fatalf("monitor output = %#v", got)
			}
		}},
		{kind: "queue_info", input: `{"name":"build-work-now"}`, check: func(got map[string]any) {
			if got["size"] != float64(1) || got["monitored"] != true {
				t.Fatalf("info output = %#v", got)
			}
		}},
		{kind: "queue_take", input: `{"name":"build-work-now"}`, check: func(got map[string]any) {
			if got["item"] != "first" || got["size"] != float64(0) {
				t.Fatalf("take output = %#v", got)
			}
		}},
	}
	for _, test := range tests {
		tool := &QueueTool{Kind: test.kind, Store: store}
		if _, err := parseSchema(tool.JSONSchema()); err != nil {
			t.Fatalf("%s schema: %v", test.kind, err)
		}
		plan, err := tool.Plan(context.Background(), json.RawMessage(test.input), call)
		if err != nil {
			t.Fatalf("%s Plan: %v", test.kind, err)
		}
		result, err := tool.Execute(context.Background(), plan, call)
		if err != nil {
			t.Fatalf("%s Execute: %v", test.kind, err)
		}
		var got map[string]any
		if err := json.Unmarshal([]byte(result.Text), &got); err != nil {
			t.Fatal(err)
		}
		if result.ModelText != result.Text || result.Metadata["queue"] != "build-work-now" {
			t.Fatalf("%s result = %#v", test.kind, result)
		}
		test.check(got)
	}
}
