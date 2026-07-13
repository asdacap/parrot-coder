package chatcompletions

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/amirulashraf/parrot-coder/internal/protocol"
)

type chunkReader struct {
	value string
	size  int
}

func (r *chunkReader) Read(buffer []byte) (int, error) {
	if r.value == "" {
		return 0, io.EOF
	}
	count := min(len(r.value), r.size, len(buffer))
	copy(buffer, r.value[:count])
	r.value = r.value[count:]
	return count, nil
}

func TestEncodeRequest(t *testing.T) {
	encoded, err := EncodeRequest(protocol.Request{
		Model: "model", Instructions: "be concise",
		Messages: []protocol.Message{{Role: protocol.RoleUser, Content: []protocol.ContentPart{{Type: protocol.ContentText, Text: "hello"}}}},
		Tools:    []protocol.ToolDefinition{{Name: "read", Description: "read a file", InputSchema: json.RawMessage(`{"type":"object"}`)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(encoded, &body); err != nil {
		t.Fatal(err)
	}
	if body["model"] != "model" || body["stream"] != true {
		t.Fatalf("unexpected body: %s", encoded)
	}
	messages := body["messages"].([]any)
	if len(messages) != 2 || messages[0].(map[string]any)["role"] != "system" {
		t.Fatalf("messages = %#v", messages)
	}
	options := body["stream_options"].(map[string]any)
	if options["include_usage"] != true {
		t.Fatalf("stream_options = %#v", options)
	}
}

func TestParserInterleavedToolCallsReasoningUsageAndFinish(t *testing.T) {
	fixture := strings.Join([]string{
		`data: {"choices":[{"delta":{"reasoning_content":"think "}}]}`,
		`data: {"choices":[{"delta":{"content":"answer","tool_calls":[{"index":0,"id":"call_a","function":{"name":"alpha","arguments":"{\"a\":"}},{"index":1,"id":"call_b","function":{"name":"beta","arguments":"{\"b\":"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"function":{"arguments":"2}"}},{"index":0,"function":{"arguments":"1}"}}]}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":4,"total_tokens":14,"prompt_tokens_details":{"cached_tokens":3},"completion_tokens_details":{"reasoning_tokens":2}}}`,
		`data: [DONE]`,
	}, "\r\n\r\n") + "\r\n\r\n"
	parser := NewParser(&chunkReader{value: fixture, size: 3}, 4096)
	events := collect(t, parser)
	wantTypes := []protocol.EventType{
		protocol.EventReasoningDelta, protocol.EventTextDelta,
		protocol.EventToolInputDelta, protocol.EventToolInputDelta,
		protocol.EventToolInputDelta, protocol.EventToolInputDelta,
		protocol.EventToolCallComplete, protocol.EventToolCallComplete,
		protocol.EventUsage, protocol.EventFinish,
	}
	if len(events) != len(wantTypes) {
		t.Fatalf("event count = %d, want %d: %#v", len(events), len(wantTypes), events)
	}
	for index, want := range wantTypes {
		if events[index].Type != want {
			t.Fatalf("event %d type = %q, want %q", index, events[index].Type, want)
		}
	}
	if string(events[6].ToolCall.Input) != `{"a":1}` || events[6].ToolCall.ID != "call_a" {
		t.Fatalf("first tool = %#v", events[6].ToolCall)
	}
	if string(events[7].ToolCall.Input) != `{"b":2}` || events[7].ToolCall.Name != "beta" {
		t.Fatalf("second tool = %#v", events[7].ToolCall)
	}
	if events[8].Usage.CachedInputTokens != 3 || events[8].Usage.ReasoningTokens != 2 {
		t.Fatalf("usage = %#v", events[8].Usage)
	}
	if events[9].FinishReason != protocol.FinishToolCalls {
		t.Fatalf("finish = %q", events[9].FinishReason)
	}
}

func TestParserRejectsMalformedEventAndToolJSON(t *testing.T) {
	parser := NewParser(strings.NewReader("data: {oops}\n\n"), 1024)
	if _, err := parser.Next(context.Background()); err == nil || !strings.Contains(err.Error(), "decode stream event") {
		t.Fatalf("malformed event error = %v", err)
	}

	fixture := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c\",\"function\":{\"name\":\"f\",\"arguments\":\"{\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n"
	parser = NewParser(strings.NewReader(fixture), 2048)
	if _, err := parser.Next(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := parser.Next(context.Background()); err == nil || !strings.Contains(err.Error(), "invalid JSON input") {
		t.Fatalf("invalid tool error = %v", err)
	}
}

func TestParserEmitsFinishAtEOFAndRejectsMissingTerminal(t *testing.T) {
	fixture := "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"
	parser := NewParser(strings.NewReader(fixture), 1024)
	event, err := parser.Next(context.Background())
	if err != nil || event.Type != protocol.EventFinish || event.FinishReason != protocol.FinishStop {
		t.Fatalf("finish = %#v, %v", event, err)
	}
	parser = NewParser(strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"), 1024)
	if _, err := parser.Next(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := parser.Next(context.Background()); err == nil || !strings.Contains(err.Error(), "terminal event") {
		t.Fatalf("missing terminal error = %v", err)
	}
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
