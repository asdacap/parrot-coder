package v1_test

import (
	"encoding/json"
	"reflect"
	"testing"

	v1 "github.com/amirulashraf/parrot-coder/internal/api/v1"
)

func TestDecodeSessionInputEventData(t *testing.T) {
	tests := []struct {
		name  string
		event v1.Event
		want  any
	}{
		{
			name: "admitted",
			event: v1.Event{
				Type: v1.EventSessionInputAdmitted,
				Data: json.RawMessage(`{"input_id":"inp_1","message_id":"msg_1","content":"hello","delivery":"steer"}`),
			},
			want: &v1.SessionInputAdmitted{InputID: "inp_1", MessageID: "msg_1", Content: "hello", Delivery: "steer"},
		},
		{
			name: "promoted",
			event: v1.Event{
				Type: v1.EventSessionInputPromoted,
				Data: json.RawMessage(`{"input_id":"inp_1","message_id":"msg_1"}`),
			},
			want: &v1.SessionInputPromoted{InputID: "inp_1", MessageID: "msg_1"},
		},
		{
			name:  "todos",
			event: v1.Event{Type: v1.EventTodoUpdated, Data: json.RawMessage(`{"todos":[{"id":"todo_1","content":"test","status":"pending","priority":"high","position":0}]}`)},
			want:  &v1.TodoUpdated{Todos: []v1.Todo{{ID: "todo_1", Content: "test", Status: "pending", Priority: "high", Position: 0}}},
		},
		{
			name:  "task progress",
			event: v1.Event{Type: v1.EventTaskProgress, Data: json.RawMessage(`{"task_id":"task_1","tool_call_id":"call_1","agent":"explore","status":"running","usage":{"input_tokens":10,"output_tokens":2,"total_tokens":12,"reasoning_tokens":1,"cached_input_tokens":3},"tool_uses":4}`)},
			want:  &v1.TaskProgress{TaskID: "task_1", ToolCallID: "call_1", Agent: "explore", Status: "running", Usage: v1.Usage{InputTokens: 10, OutputTokens: 2, TotalTokens: 12, ReasoningTokens: 1, CachedInputTokens: 3}, ToolUses: 4},
		},
		{
			name:  "message part delta",
			event: v1.Event{Type: v1.EventMessagePartDelta, Data: json.RawMessage(`{"message_id":"msg_1","part_id":"reasoning_1:2","kind":"reasoning_summary","delta":"Checking tests"}`)},
			want:  &v1.MessagePartDelta{MessageID: "msg_1", PartID: "reasoning_1:2", Kind: "reasoning_summary", Delta: "Checking tests"},
		},
		{
			name:  "message part done",
			event: v1.Event{Type: v1.EventMessagePartDelta, Data: json.RawMessage(`{"message_id":"msg_1","part_id":"reasoning_1:2","kind":"reasoning_summary","delta":"","done":true}`)},
			want:  &v1.MessagePartDelta{MessageID: "msg_1", PartID: "reasoning_1:2", Kind: "reasoning_summary", Done: true},
		},
		{
			name:  "task start",
			event: v1.Event{Type: v1.EventTaskStart, TaskID: "task_1", Data: json.RawMessage(`{"task_id":"task_1","session_id":"ses_child","parent_session_id":"ses_parent","kind":"agent","agent":"explore","name":"explore-happy-otter"}`)},
			want:  &v1.TaskEvent{TaskID: "task_1", SessionID: "ses_child", ParentSessionID: "ses_parent", Kind: "agent", Agent: "explore", Name: "explore-happy-otter"},
		},
		{
			name:  "task finished",
			event: v1.Event{Type: v1.EventTaskFinished, TaskID: "task_1", Data: json.RawMessage(`{"task_id":"task_1","session_id":"ses_child","kind":"agent","status":"failed","error":"boom"}`)},
			want:  &v1.TaskEvent{TaskID: "task_1", SessionID: "ses_child", Kind: "agent", Status: "failed", Error: "boom"},
		},
		{
			name:  "monitor started",
			event: v1.Event{Type: v1.EventMonitorStarted, TaskID: "task_monitor", Data: json.RawMessage(`{"tool_call_id":"call_1","task_id":"task_1","timeout_ms":1500}`)},
			want:  &v1.MonitorEvent{ToolCallID: "call_1", TaskID: "task_1", TimeoutMS: 1500},
		},
		{
			name:  "monitor finished",
			event: v1.Event{Type: v1.EventMonitorFinished, TaskID: "task_monitor", Data: json.RawMessage(`{"tool_call_id":"call_1","task_id":"task_1","timeout_ms":1500,"status":"failed","error":"boom"}`)},
			want:  &v1.MonitorEvent{ToolCallID: "call_1", TaskID: "task_1", TimeoutMS: 1500, Status: "failed", Error: "boom"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := v1.DecodeEventData(test.event)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("DecodeEventData() = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestDecodeSessionInputEventDataRejectsUnknownFields(t *testing.T) {
	for _, event := range []v1.Event{
		{Type: v1.EventSessionInputAdmitted, Data: json.RawMessage(`{"input_id":"inp_1","message_id":"msg_1","content":"hello","delivery":"steer","extra":true}`)},
		{Type: v1.EventSessionInputPromoted, Data: json.RawMessage(`{"input_id":"inp_1","message_id":"msg_1","extra":true}`)},
		{Type: v1.EventTaskProgress, Data: json.RawMessage(`{"task_id":"task_1","agent":"explore","status":"running","usage":{},"tool_uses":0,"extra":true}`)},
		{Type: v1.EventTaskStart, Data: json.RawMessage(`{"task_id":"task_1","kind":"agent","extra":true}`)},
		{Type: v1.EventMonitorStarted, Data: json.RawMessage(`{"tool_call_id":"call_1","task_id":"task_1","timeout_ms":1500,"extra":true}`)},
	} {
		if _, err := v1.DecodeEventData(event); err == nil {
			t.Fatalf("DecodeEventData(%q) accepted an unknown field", event.Type)
		}
	}
}

func TestMonitorEventsAreLiveManifestEntries(t *testing.T) {
	want := map[string]bool{
		v1.EventMonitorStarted:  false,
		v1.EventMonitorFinished: false,
	}
	for _, definition := range v1.EventManifest {
		durable, ok := want[definition.Name]
		if !ok {
			continue
		}
		if definition.Durable != durable || definition.Payload != "MonitorEvent" {
			t.Errorf("EventManifest entry for %q = %#v, want durable=%t payload=MonitorEvent", definition.Name, definition, durable)
		}
		delete(want, definition.Name)
	}
	for name := range want {
		t.Errorf("EventManifest is missing %q", name)
	}
}
