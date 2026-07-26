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
			name:  "agent session progress",
			event: v1.Event{Type: v1.EventAgentSessionProgress, Data: json.RawMessage(`{"session_id":"ses_child","agent":"explore","status":"running","usage":{"input_tokens":10,"output_tokens":2,"total_tokens":12,"reasoning_tokens":1,"cached_input_tokens":3},"tool_uses":4}`)},
			want:  &v1.AgentSessionProgress{SessionID: "ses_child", Agent: "explore", Status: "running", Usage: v1.Usage{InputTokens: 10, OutputTokens: 2, TotalTokens: 12, ReasoningTokens: 1, CachedInputTokens: 3}, ToolUses: 4},
		},
		{
			name:  "message part delta",
			event: v1.Event{Type: v1.EventMessagePartDelta, Data: json.RawMessage(`{"message_id":"msg_1","part_id":"reasoning_1:2","kind":"reasoning_summary","delta":"Checking tests"}`)},
			want:  &v1.MessagePartDelta{MessageID: "msg_1", PartID: "reasoning_1:2", Kind: "reasoning_summary", Delta: "Checking tests"},
		},
		{
			name:  "code display",
			event: v1.Event{Type: v1.EventCodeDisplay, Data: json.RawMessage(`{"tool_call_id":"call_1","source":"package main\n","path":"main.go","language":"go","start_line":4}`)},
			want:  &v1.CodeDisplay{ToolCallID: "call_1", Source: "package main\n", Path: "main.go", Language: "go", StartLine: 4},
		},
		{
			name:  "message part done",
			event: v1.Event{Type: v1.EventMessagePartDelta, Data: json.RawMessage(`{"message_id":"msg_1","part_id":"reasoning_1:2","kind":"reasoning_summary","delta":"","done":true}`)},
			want:  &v1.MessagePartDelta{MessageID: "msg_1", PartID: "reasoning_1:2", Kind: "reasoning_summary", Done: true},
		},
		{
			name:  "user session idle",
			event: v1.Event{Type: v1.EventUserSessionIdle, Data: json.RawMessage(`{"session_id":"ses_main","status":"error","error":"boom"}`)},
			want:  &v1.UserSessionEvent{SessionID: "ses_main", Status: "error", Error: "boom"},
		},
		{
			name:  "agent session start",
			event: v1.Event{Type: v1.EventAgentSessionStart, Data: json.RawMessage(`{"session_id":"ses_child","parent_session_id":"ses_parent","agent":"explore","name":"explore-happy-otter"}`)},
			want:  &v1.AgentSessionEvent{SessionID: "ses_child", ParentSessionID: "ses_parent", Agent: "explore", Name: "explore-happy-otter"},
		},
		{
			name:  "agent session finished",
			event: v1.Event{Type: v1.EventAgentSessionFinished, Data: json.RawMessage(`{"session_id":"ses_child","status":"failed","error":"boom"}`)},
			want:  &v1.AgentSessionEvent{SessionID: "ses_child", Status: "failed", Error: "boom"},
		},
		{
			name:  "process finished",
			event: v1.Event{Type: v1.EventProcessFinished, Data: json.RawMessage(`{"session_id":"ses_parent","process_id":"proc_1","name":"tests","status":"failed","error":"boom"}`)},
			want:  &v1.ProcessEvent{SessionID: "ses_parent", ProcessID: "proc_1", Name: "tests", Status: "failed", Error: "boom"},
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
		{Type: v1.EventAgentSessionProgress, Data: json.RawMessage(`{"session_id":"ses_child","agent":"explore","status":"running","usage":{},"tool_uses":0,"extra":true}`)},
		{Type: v1.EventUserSessionStart, Data: json.RawMessage(`{"session_id":"ses_main","extra":true}`)},
		{Type: v1.EventAgentSessionStart, Data: json.RawMessage(`{"session_id":"ses_child","parent_session_id":"ses_main","extra":true}`)},
		{Type: v1.EventProcessStart, Data: json.RawMessage(`{"session_id":"ses_main","process_id":"proc_1","extra":true}`)},
		{Type: v1.EventAgentSessionProgress, Data: json.RawMessage(`{"task` + `_id":"task_1","session_id":"ses_child","agent":"explore","status":"running","usage":{},"tool_uses":0}`)},
		{Type: v1.EventCodeDisplay, Data: json.RawMessage(`{"tool_call_id":"call_1","source":"x","extra":true}`)},
		{Type: v1.EventSessionToolSuccess, Data: json.RawMessage(`{"call_id":"call_1","result":"ok","extra":true}`)},
	} {
		if _, err := v1.DecodeEventData(event); err == nil {
			t.Fatalf("DecodeEventData(%q) accepted an unknown field", event.Type)
		}
	}
}

func TestDecodeToolEventData(t *testing.T) {
	tests := []struct {
		name  string
		event v1.Event
		want  *v1.ToolEvent
	}{
		{
			name:  "canonical pending",
			event: v1.Event{Type: v1.EventSessionToolPending, Data: json.RawMessage(`{"call_id":"call_1","tool_name":"exec_command","input":{"cmd":"go test ./...","limit":9007199254740993},"status":"pending"}`)},
			want:  &v1.ToolEvent{CallID: "call_1", ToolName: "exec_command", Input: map[string]any{"cmd": "go test ./...", "limit": json.Number("9007199254740993")}, Status: "pending"},
		},
		{
			name:  "legacy pending protocol tool call",
			event: v1.Event{Type: v1.EventSessionToolPending, Data: json.RawMessage(`{"ID":"call_2","Name":"apply_patch","Input":{"patch":"change","line":9007199254740993}}`)},
			want:  &v1.ToolEvent{CallID: "call_2", ToolName: "apply_patch", Input: map[string]any{"patch": "change", "line": json.Number("9007199254740993")}},
		},
		{
			name:  "running",
			event: v1.Event{Type: v1.EventSessionToolRunning, Data: json.RawMessage(`{"call_id":"call_1","tool_name":"exec_command","status":"running"}`)},
			want:  &v1.ToolEvent{CallID: "call_1", ToolName: "exec_command", Status: "running"},
		},
		{
			name:  "success structured result",
			event: v1.Event{Type: v1.EventSessionToolSuccess, Data: json.RawMessage(`{"call_id":"call_1","status":"success","result":{"exit_code":0},"output_tail":"ok"}`)},
			want:  &v1.ToolEvent{CallID: "call_1", Status: "success", Result: map[string]any{"exit_code": json.Number("0")}, OutputTail: "ok"},
		},
		{
			name:  "failure",
			event: v1.Event{Type: v1.EventSessionToolFailure, Data: json.RawMessage(`{"call_id":"call_1","status":"failure","error":"boom"}`)},
			want:  &v1.ToolEvent{CallID: "call_1", Status: "failure", Error: "boom"},
		},
		{
			name:  "interrupted",
			event: v1.Event{Type: v1.EventSessionToolInterrupted, Data: json.RawMessage(`{"call_id":"call_1","status":"interrupted","error":"stopped"}`)},
			want:  &v1.ToolEvent{CallID: "call_1", Status: "interrupted", Error: "stopped"},
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

func TestDecodePendingToolEventRejectsNonContractShapes(t *testing.T) {
	for _, data := range []string{
		`{"call_id":"call_1","tool_name":"exec_command","input":{},"extra":true}`,
		`{"ID":"call_1","Name":"exec_command","Input":{},"Extra":true}`,
		`{"id":"call_1","name":"exec_command","input":{}}`,
		`{"ID":"call_1","Name":"exec_command","Input":[]} trailing`,
	} {
		if _, err := v1.DecodeEventData(v1.Event{Type: v1.EventSessionToolPending, Data: json.RawMessage(data)}); err == nil {
			t.Fatalf("DecodeEventData() accepted %s", data)
		}
	}
}

func TestEventManifestUsesCanonicalNamesAndTypedPayloads(t *testing.T) {
	canonical := []string{
		v1.EventServerConnected, v1.EventMessagePartDelta, v1.EventSessionStatus,
		v1.EventPermission, v1.EventPermissionReply, v1.EventQuestion, v1.EventQuestionReply,
		v1.EventSessionInputAdmitted, v1.EventSessionInputPromoted, v1.EventTodoUpdated,
		v1.EventGoalUpdated, v1.EventGoalCleared, v1.EventAgentSessionProgress,
		v1.EventUserSessionStart, v1.EventUserSessionWorking, v1.EventUserSessionIdle,
		v1.EventAgentSessionStart, v1.EventAgentSessionWorking, v1.EventAgentSessionIdle, v1.EventAgentSessionFinished,
		v1.EventProcessStart, v1.EventProcessFinished, v1.EventToolOutputDelta, v1.EventCodeDisplay,
		v1.EventSessionSelectionChanged, v1.EventSessionContextInitialized, v1.EventSessionContextObserved,
		v1.EventSessionContextChanged, v1.EventSessionContextReplaced, v1.EventSessionMessageAppended,
		v1.EventSessionStatusPromptAppended, v1.EventSessionAssistantStarted, v1.EventSessionAssistantComplete,
		v1.EventSessionAssistantError, v1.EventSessionAssistantInterrupted, v1.EventSessionToolPending,
		v1.EventSessionToolRunning, v1.EventSessionToolSuccess, v1.EventSessionToolFailure,
		v1.EventSessionToolInterrupted, v1.EventSessionRuntimeRepaired, v1.EventSessionCompactionCompleted,
		v1.EventSessionCompactionRetry,
	}
	if len(v1.EventManifest) != len(canonical) {
		t.Fatalf("manifest size = %d, canonical event count = %d", len(v1.EventManifest), len(canonical))
	}
	seen := make(map[string]int, len(v1.EventManifest))
	for _, definition := range v1.EventManifest {
		seen[definition.Name]++
	}
	for _, name := range canonical {
		if seen[name] != 1 {
			t.Fatalf("canonical event %q occurs %d times in manifest", name, seen[name])
		}
		delete(seen, name)
	}
	if len(seen) != 0 {
		t.Fatalf("manifest has events without canonical constants: %#v", seen)
	}

	want := map[string]string{
		v1.EventUserSessionStart:       "UserSessionEvent",
		v1.EventUserSessionWorking:     "UserSessionEvent",
		v1.EventUserSessionIdle:        "UserSessionEvent",
		v1.EventAgentSessionStart:      "AgentSessionEvent",
		v1.EventAgentSessionWorking:    "AgentSessionEvent",
		v1.EventAgentSessionIdle:       "AgentSessionEvent",
		v1.EventAgentSessionFinished:   "AgentSessionEvent",
		v1.EventProcessStart:           "ProcessEvent",
		v1.EventProcessFinished:        "ProcessEvent",
		v1.EventAgentSessionProgress:   "AgentSessionProgress",
		v1.EventSessionToolPending:     "ToolEvent",
		v1.EventSessionToolRunning:     "ToolEvent",
		v1.EventSessionToolSuccess:     "ToolEvent",
		v1.EventSessionToolFailure:     "ToolEvent",
		v1.EventSessionToolInterrupted: "ToolEvent",
	}
	for _, definition := range v1.EventManifest {
		if payload, ok := want[definition.Name]; ok {
			if definition.Payload != payload {
				t.Fatalf("event %q payload = %q, want %q", definition.Name, definition.Payload, payload)
			}
			delete(want, definition.Name)
		}
	}
	if len(want) != 0 {
		t.Fatalf("event manifest is missing %#v", want)
	}
	for _, legacy := range []string{"task.start", "task.working", "task.idle", "task.finished", "task.progress"} {
		if v1.KnownEvent(legacy) {
			t.Fatalf("legacy event %q remains in event manifest", legacy)
		}
	}
}
