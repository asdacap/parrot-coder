package tool

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/amirulashraf/parrot-coder/internal/queue"
)

func TestQueueDescriptors(t *testing.T) {
	create := (&QueueTool{Kind: "queue_create"}).Descriptor()
	if create.SystemPromptGuidance != "When a task have many work item, use queue and multiple worker subagent. Publisher and consumer can be spawned at the same time." {
		t.Fatalf("SystemPromptGuidance = %q", create.SystemPromptGuidance)
	}
	if got := string(create.Schema); !strings.Contains(got, `"pattern":"^[a-z0-9]+(-[a-z0-9]+)*$"`) {
		t.Fatalf("Schema does not allow arbitrary queue name word counts: %s", got)
	}

	push := (&QueueTool{Kind: "queue_push"}).Descriptor()
	got := string(push.Schema)
	if !strings.Contains(got, `"items":{"type":"array","items":{"type":"string"}`) ||
		!strings.Contains(got, `"required":["name","items"]`) || strings.Contains(got, `"item":`) {
		t.Fatalf("queue_push schema does not require a string array or exposes singular item: %s", got)
	}

	take := (&QueueTool{Kind: "queue_take"}).Descriptor()
	got = string(take.Schema)
	if !strings.Contains(got, `"count":{"type":"integer","minimum":1`) ||
		!strings.Contains(got, `"required":["name"]`) {
		t.Fatalf("queue_take schema does not expose an optional positive count: %s", got)
	}
}

func TestQueueToolsLifecycle(t *testing.T) {
	store, err := queue.New(filepath.Join(t.TempDir(), "bound-session"))
	if err != nil {
		t.Fatal(err)
	}
	redirected, err := queue.New(filepath.Join(t.TempDir(), "redirected-session"))
	if err != nil {
		t.Fatal(err)
	}
	call := CallContext{}
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
		{kind: "queue_push", input: `{"name":"build-work-now","items":["first","second","third"]}`, check: func(got map[string]any) {
			if got["size"] != float64(3) {
				t.Fatalf("push output = %#v", got)
			}
		}},
		{kind: "queue_listen", input: `{"name":"build-work-now"}`, check: func(got map[string]any) {
			if got["size"] != float64(3) || got["monitored"] != true {
				t.Fatalf("listen output = %#v", got)
			}
		}},
		{kind: "queue_info", input: `{"name":"build-work-now"}`, check: func(got map[string]any) {
			if got["size"] != float64(3) || got["monitored"] != true {
				t.Fatalf("info output = %#v", got)
			}
		}},
		{kind: "queue_take", input: `{"name":"build-work-now"}`, check: func(got map[string]any) {
			items, ok := got["items"].([]any)
			if !ok || len(items) != 1 || items[0] != "first" || got["size"] != float64(2) {
				t.Fatalf("default take output = %#v", got)
			}
		}},
		{kind: "queue_take", input: `{"name":"build-work-now","count":5}`, check: func(got map[string]any) {
			items, ok := got["items"].([]any)
			if !ok || len(items) != 2 || items[0] != "second" || items[1] != "third" || got["size"] != float64(0) {
				t.Fatalf("take output = %#v", got)
			}
			if _, ok := got["item"]; ok {
				t.Fatalf("take output exposes singular item = %#v", got)
			}
		}},
	}
	for i, test := range tests {
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
		if i == 0 {
			info, err := store.Get("build-work-now")
			if err != nil || filepath.Dir(info.Path) != store.Directory() {
				t.Fatalf("queue was not created in bound manager: %#v, %v", info, err)
			}
			if _, err := redirected.Get("build-work-now"); !errors.Is(err, queue.ErrNotFound) {
				t.Fatalf("CallContext.SessionID redirected queue tool: %v", err)
			}
		}
	}
}
