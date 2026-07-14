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
	} {
		if _, err := v1.DecodeEventData(event); err == nil {
			t.Fatalf("DecodeEventData(%q) accepted an unknown field", event.Type)
		}
	}
}
