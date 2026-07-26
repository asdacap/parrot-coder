package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/amirulashraf/parrot-coder/internal/auth"
	"github.com/amirulashraf/parrot-coder/internal/protocol"
)

func TestOpenAICompatibleResponseChain(t *testing.T) {
	var bodies []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		bodies = append(bodies, body)
		response.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(response, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\"}}\n\n"+
			"data: {\"type\":\"response.output_text.delta\",\"delta\":\"answer\"}\n\n"+
			"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\"}}\n\n")
	}))
	defer server.Close()
	value, err := NewOpenAICompatible(OpenAICompatibleOptions{
		ID: "local", BaseURL: server.URL, Protocol: ProtocolResponses, APIKey: auth.Secret("key"),
		AllowInsecureLocalhost: true, HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	user := protocol.Message{Role: protocol.RoleUser, Content: []protocol.ContentPart{{Type: protocol.ContentText, Text: "hello"}}}
	assistant := protocol.Message{Role: protocol.RoleAssistant, Content: []protocol.ContentPart{{Type: protocol.ContentText, Text: "answer"}}}
	for _, messages := range [][]protocol.Message{{user}, {user, assistant, {Role: protocol.RoleUser, Content: []protocol.ContentPart{{Type: protocol.ContentText, Text: "next"}}}}} {
		stream, streamErr := value.Stream(context.Background(), protocol.Request{Model: "model", Messages: messages})
		if streamErr != nil {
			t.Fatal(streamErr)
		}
		for {
			event, nextErr := stream.Next(context.Background())
			if nextErr == io.EOF {
				break
			}
			if nextErr != nil {
				t.Fatal(nextErr)
			}
			if event.Type == protocol.EventResponseID {
				t.Fatal("response identity leaked past provider")
			}
		}
		_ = stream.Close()
	}
	if len(bodies) != 2 || bodies[1]["previous_response_id"] != "resp_1" {
		t.Fatalf("request bodies = %#v", bodies)
	}
	input := bodies[1]["input"].([]any)
	if len(input) != 1 || input[0].(map[string]any)["role"] != "user" {
		t.Fatalf("continued input = %#v", input)
	}
}

func TestResponseChainContinuationAndSafety(t *testing.T) {
	user := protocol.Message{Role: protocol.RoleUser, Content: []protocol.ContentPart{{Type: protocol.ContentText, Text: "hello"}}}
	assistant := protocol.Message{Role: protocol.RoleAssistant, Content: []protocol.ContentPart{{Type: protocol.ContentReasoning, Text: "thought"}, {Type: protocol.ContentText, Text: "answer"}}}
	next := protocol.Message{Role: protocol.RoleUser, Content: []protocol.ContentPart{{Type: protocol.ContentText, Text: "next"}}}

	chain := &responseChain{}
	stream := chain.observe(&eventStream{events: []protocol.Event{
		{Type: protocol.EventResponseID, ResponseID: "resp_1"},
		{Type: protocol.EventReasoningSummaryDelta, PartID: "summary", Text: "thought"},
		{Type: protocol.EventTextDelta, Text: "answer"},
		{Type: protocol.EventFinish, FinishReason: protocol.FinishStop},
	}}, protocol.Request{Model: "model", Messages: []protocol.Message{user}})
	if event, err := stream.Next(context.Background()); err != nil || event.Type != protocol.EventReasoningSummaryDelta {
		t.Fatalf("first visible event = %#v, err = %v", event, err)
	}
	for i := 0; i < 2; i++ {
		if _, err := stream.Next(context.Background()); err != nil {
			t.Fatal(err)
		}
	}

	continued := chain.prepare(protocol.Request{Model: "model", Messages: []protocol.Message{user, assistant, next}})
	if continued.PreviousResponseID != "resp_1" || len(continued.Messages) != 1 || !messagesEqual(continued.Messages, []protocol.Message{next}) {
		t.Fatalf("continued request = %#v", continued)
	}
	for _, test := range []struct {
		name    string
		request protocol.Request
	}{
		{name: "different model", request: protocol.Request{Model: "other", Messages: []protocol.Message{user, assistant, next}}},
		{name: "changed history", request: protocol.Request{Model: "model", Messages: []protocol.Message{user, {Role: protocol.RoleAssistant, Content: []protocol.ContentPart{{Type: protocol.ContentText, Text: "changed"}}}, next}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			prepared := chain.prepare(test.request)
			if prepared.PreviousResponseID != "" || !messagesEqual(prepared.Messages, test.request.Messages) {
				t.Fatalf("unsafe continuation = %#v", prepared)
			}
		})
	}

	failed := &responseChain{}
	failedStream := failed.observe(&eventStream{events: []protocol.Event{
		{Type: protocol.EventResponseID, ResponseID: "resp_failed"},
		{Type: protocol.EventProviderError, ProviderError: &protocol.ProviderError{Message: "failed"}},
		{Type: protocol.EventFinish, FinishReason: protocol.FinishError},
	}}, protocol.Request{Model: "model", Messages: []protocol.Message{user}})
	for i := 0; i < 2; i++ {
		if _, err := failedStream.Next(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if prepared := failed.prepare(protocol.Request{Model: "model", Messages: []protocol.Message{user, assistant}}); prepared.PreviousResponseID != "" {
		t.Fatalf("failed response became chain anchor: %#v", prepared)
	}
}

type eventStream struct {
	events []protocol.Event
	index  int
}

func (s *eventStream) Next(context.Context) (protocol.Event, error) {
	if s.index == len(s.events) {
		return protocol.Event{}, io.EOF
	}
	event := s.events[s.index]
	s.index++
	return event, nil
}

func (s *eventStream) Close() error { return nil }
