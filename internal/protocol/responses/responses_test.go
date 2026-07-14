package responses

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/amirulashraf/parrot-coder/internal/protocol"
)

func TestEncodeRequest(t *testing.T) {
	encoded, err := EncodeRequest(protocol.Request{
		Model: "model", Instructions: "instructions",
		Messages:  []protocol.Message{{Role: protocol.RoleUser, Content: []protocol.ContentPart{{Type: protocol.ContentText, Text: "hello"}}}},
		Tools:     []protocol.ToolDefinition{{Name: "shell", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		Reasoning: &protocol.ReasoningOptions{Effort: "high", Summary: "auto"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(encoded, &body); err != nil {
		t.Fatal(err)
	}
	if body["instructions"] != "instructions" || body["store"] != false || body["stream"] != true {
		t.Fatalf("unexpected body: %s", encoded)
	}
	reasoning := body["reasoning"].(map[string]any)
	if reasoning["effort"] != "high" || reasoning["summary"] != "auto" {
		t.Fatalf("reasoning = %#v", reasoning)
	}
	tool := body["tools"].([]any)[0].(map[string]any)
	if tool["type"] != "function" || tool["strict"] != false {
		t.Fatalf("tool = %#v", tool)
	}
}

func TestParserInterleavedFunctionsTextReasoningUsageAndCompletion(t *testing.T) {
	fixture := streamFixture(
		`{"type":"response.reasoning_summary_text.delta","item_id":"reasoning_item","summary_index":0,"delta":"thinking"}`,
		`{"type":"response.output_text.delta","delta":"result"}`,
		`{"type":"response.output_item.added","item":{"id":"item_b","type":"function_call","call_id":"call_b","name":"beta","arguments":""}}`,
		`{"type":"response.output_item.added","item":{"id":"item_a","type":"function_call","call_id":"call_a","name":"alpha","arguments":""}}`,
		`{"type":"response.function_call_arguments.delta","item_id":"item_a","delta":"{\"a\":"}`,
		`{"type":"response.function_call_arguments.delta","item_id":"item_b","delta":"{\"b\":"}`,
		`{"type":"response.function_call_arguments.delta","item_id":"item_b","delta":"2}"}`,
		`{"type":"response.function_call_arguments.delta","item_id":"item_a","delta":"1}"}`,
		`{"type":"response.completed","response":{"usage":{"input_tokens":8,"output_tokens":5,"total_tokens":13,"input_tokens_details":{"cached_tokens":2},"output_tokens_details":{"reasoning_tokens":3}}}}`,
	)
	parser := NewParser(strings.NewReader(fixture), 4096)
	events := collect(t, parser)
	wantTypes := []protocol.EventType{
		protocol.EventReasoningSummaryDelta, protocol.EventTextDelta,
		protocol.EventToolInputDelta, protocol.EventToolInputDelta,
		protocol.EventToolInputDelta, protocol.EventToolInputDelta,
		protocol.EventToolCallComplete, protocol.EventToolCallComplete,
		protocol.EventUsage, protocol.EventFinish,
	}
	if len(events) != len(wantTypes) {
		t.Fatalf("event count = %d: %#v", len(events), events)
	}
	for index, want := range wantTypes {
		if events[index].Type != want {
			t.Fatalf("event %d type = %q, want %q", index, events[index].Type, want)
		}
	}
	if events[0].PartID != "reasoning_item:reasoning:0" {
		t.Fatalf("reasoning part ID = %q", events[0].PartID)
	}
	if events[6].ToolCall.ID != "call_a" || string(events[6].ToolCall.Input) != `{"a":1}` {
		t.Fatalf("first tool = %#v", events[6].ToolCall)
	}
	if events[7].ToolCall.ID != "call_b" || string(events[7].ToolCall.Input) != `{"b":2}` {
		t.Fatalf("second tool = %#v", events[7].ToolCall)
	}
	if events[8].Usage.ReasoningTokens != 3 || events[9].FinishReason != protocol.FinishToolCalls {
		t.Fatalf("terminal events = %#v %#v", events[8], events[9])
	}
}

func TestParserCallIDFallbackIncompleteAndErrors(t *testing.T) {
	fixture := streamFixture(
		`{"type":"response.function_call_arguments.delta","call_id":"call_only","name":"run","delta":"{}"}`,
		`{"type":"response.incomplete","response":{"incomplete_details":{"reason":"max_output_tokens"}}}`,
	)
	events := collect(t, NewParser(strings.NewReader(fixture), 2048))
	if events[1].ToolCall.ID != "call_only" || events[2].FinishReason != protocol.FinishLength {
		t.Fatalf("events = %#v", events)
	}

	events = collect(t, NewParser(strings.NewReader(streamFixture(`{"type":"error","code":"rate_limit","message":"slow down"}`)), 1024))
	if len(events) != 1 || events[0].Type != protocol.EventProviderError || events[0].ProviderError.Code != "rate_limit" {
		t.Fatalf("error events = %#v", events)
	}
}

func TestParserPreservesReasoningSummaryPartIdentity(t *testing.T) {
	fixture := streamFixture(
		`{"type":"response.reasoning_summary_text.delta","item_id":"reasoning_a","summary_index":0,"delta":"First"}`,
		`{"type":"response.reasoning_summary_text.delta","item_id":"reasoning_a","summary_index":1,"delta":"Second"}`,
		`{"type":"response.reasoning_summary_text.delta","item_id":"reasoning_a","summary_index":0,"delta":" item"}`,
		`{"type":"response.completed","response":{}}`,
	)
	events := collect(t, NewParser(strings.NewReader(fixture), 2048))
	if len(events) != 4 {
		t.Fatalf("event count = %d: %#v", len(events), events)
	}
	wantPartIDs := []string{"reasoning_a:reasoning:0", "reasoning_a:reasoning:1", "reasoning_a:reasoning:0"}
	for i, want := range wantPartIDs {
		if events[i].Type != protocol.EventReasoningSummaryDelta || events[i].PartID != want {
			t.Errorf("event %d = %#v, want part ID %q", i, events[i], want)
		}
	}
}

func TestParserRejectsMalformedJSON(t *testing.T) {
	parser := NewParser(strings.NewReader("data: nope\n\n"), 1024)
	if _, err := parser.Next(context.Background()); err == nil || !strings.Contains(err.Error(), "decode stream event") {
		t.Fatalf("malformed event error = %v", err)
	}

	fixture := streamFixture(
		`{"type":"response.output_item.added","item":{"id":"item","type":"function_call","call_id":"call","name":"bad"}}`,
		`{"type":"response.completed","response":{}}`,
	)
	parser = NewParser(strings.NewReader(fixture), 2048)
	if _, err := parser.Next(context.Background()); err == nil || !strings.Contains(err.Error(), "invalid JSON input") {
		t.Fatalf("invalid tool error = %v", err)
	}
}

func TestParserRejectsMissingTerminalEventAndToolIdentity(t *testing.T) {
	parser := NewParser(strings.NewReader(streamFixture(`{"type":"response.output_text.delta","delta":"partial"}`)), 1024)
	if _, err := parser.Next(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := parser.Next(context.Background()); err == nil || !strings.Contains(err.Error(), "terminal event") {
		t.Fatalf("missing terminal error = %v", err)
	}
	parser = NewParser(strings.NewReader(streamFixture(
		`{"type":"response.output_item.added","item":{"type":"function_call","arguments":"{}"}}`,
		`{"type":"response.completed","response":{}}`,
	)), 1024)
	if _, err := parser.Next(context.Background()); err == nil || !strings.Contains(err.Error(), "ID and name") {
		t.Fatalf("missing tool identity error = %v", err)
	}
}

func streamFixture(events ...string) string {
	var builder strings.Builder
	for _, event := range events {
		builder.WriteString("data: ")
		builder.WriteString(event)
		builder.WriteString("\n\n")
	}
	return builder.String()
}

func collect(t *testing.T, parser *Parser) []protocol.Event {
	t.Helper()
	var events []protocol.Event
	for {
		event, err := parser.Next(context.Background())
		if errors.Is(err, io.EOF) {
			return events
		}
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
}
