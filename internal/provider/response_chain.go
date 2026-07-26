package provider

import (
	"bytes"
	"context"
	"sync"

	"github.com/amirulashraf/parrot-coder/internal/protocol"
)

const responseChainLimit = 128

type responseChain struct {
	mu      sync.Mutex
	entries []responseChainEntry
}

type responseChainEntry struct {
	responseID string
	model      string
	history    []protocol.Message
}

func (c *responseChain) prepare(request protocol.Request) protocol.Request {
	c.mu.Lock()
	defer c.mu.Unlock()
	best, bestPrefix := -1, 0
	for i := range c.entries {
		entry := &c.entries[i]
		if entry.model != request.Model || len(entry.history) > len(request.Messages) || len(entry.history) < bestPrefix {
			continue
		}
		if messagesEqual(entry.history, request.Messages[:len(entry.history)]) {
			best, bestPrefix = i, len(entry.history)
		}
	}
	if best >= 0 {
		request.PreviousResponseID = c.entries[best].responseID
		request.Messages = cloneMessages(request.Messages[bestPrefix:])
	}
	return request
}

func (c *responseChain) observe(stream Stream, request protocol.Request) Stream {
	return &responseRecordingStream{Stream: stream, chain: c, model: request.Model, requestHistory: cloneMessages(request.Messages)}
}

func (c *responseChain) add(responseID, model string, requestHistory []protocol.Message, assistant protocol.Message) {
	if responseID == "" {
		return
	}
	history := append(cloneMessages(requestHistory), cloneMessage(assistant))
	c.mu.Lock()
	c.entries = append(c.entries, responseChainEntry{responseID: responseID, model: model, history: history})
	if excess := len(c.entries) - responseChainLimit; excess > 0 {
		copy(c.entries, c.entries[excess:])
		c.entries = c.entries[:responseChainLimit]
	}
	c.mu.Unlock()
}

type responseRecordingStream struct {
	Stream
	chain          *responseChain
	model          string
	requestHistory []protocol.Message
	responseID     string
	text           []byte
	reasoning      []byte
	summary        responseSummary
	calls          []protocol.ToolCall
	done           bool
}

func (s *responseRecordingStream) Next(ctx context.Context) (protocol.Event, error) {
	for {
		event, err := s.Stream.Next(ctx)
		if err != nil {
			return protocol.Event{}, err
		}
		switch event.Type {
		case protocol.EventResponseID:
			if event.ResponseID != "" {
				s.responseID = event.ResponseID
			}
			continue
		case protocol.EventTextDelta:
			s.text = append(s.text, event.Text...)
		case protocol.EventReasoningDelta:
			s.reasoning = append(s.reasoning, event.Text...)
		case protocol.EventReasoningSummaryDelta:
			s.summary.write(event.PartID, event.Text)
		case protocol.EventReasoningSummaryDone:
			if event.Text != "" {
				s.summary.set(event.PartID, event.Text)
			}
		case protocol.EventToolCallComplete:
			if event.ToolCall != nil {
				s.calls = append(s.calls, cloneToolCall(*event.ToolCall))
			}
		case protocol.EventProviderError:
			s.done = true
		case protocol.EventFinish:
			if !s.done {
				s.done = true
				s.chain.add(s.responseID, s.model, s.requestHistory, s.assistantMessage())
			}
		}
		return event, nil
	}
}

func (s *responseRecordingStream) assistantMessage() protocol.Message {
	parts := make([]protocol.ContentPart, 0, 2+len(s.calls))
	reasoning := string(s.reasoning)
	if summary := s.summary.String(); summary != "" {
		reasoning = summary
	}
	if reasoning != "" {
		parts = append(parts, protocol.ContentPart{Type: protocol.ContentReasoning, Text: reasoning})
	}
	if len(s.text) > 0 {
		parts = append(parts, protocol.ContentPart{Type: protocol.ContentText, Text: string(s.text)})
	}
	for i := range s.calls {
		call := cloneToolCall(s.calls[i])
		parts = append(parts, protocol.ContentPart{Type: protocol.ContentToolCall, ToolCall: &call})
	}
	return protocol.Message{Role: protocol.RoleAssistant, Content: parts}
}

type responseSummary struct {
	parts map[string][]byte
	order []string
}

func (s *responseSummary) write(id, text string) {
	if text == "" {
		return
	}
	if s.parts == nil {
		s.parts = make(map[string][]byte)
	}
	if _, exists := s.parts[id]; !exists {
		s.order = append(s.order, id)
	}
	s.parts[id] = append(s.parts[id], text...)
}

func (s *responseSummary) set(id, text string) {
	if s.parts == nil {
		s.parts = make(map[string][]byte)
	}
	if _, exists := s.parts[id]; !exists {
		s.order = append(s.order, id)
	}
	s.parts[id] = append(s.parts[id][:0], text...)
}

func (s *responseSummary) String() string {
	var result []byte
	for _, id := range s.order {
		result = append(result, s.parts[id]...)
	}
	return string(result)
}

func messagesEqual(left, right []protocol.Message) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i].Role != right[i].Role || len(left[i].Content) != len(right[i].Content) {
			return false
		}
		for j := range left[i].Content {
			if !contentPartEqual(left[i].Content[j], right[i].Content[j]) {
				return false
			}
		}
	}
	return true
}

func contentPartEqual(left, right protocol.ContentPart) bool {
	if left.Type != right.Type || left.Text != right.Text || left.ToolCallID != right.ToolCallID {
		return false
	}
	if left.ToolCall == nil || right.ToolCall == nil {
		return left.ToolCall == nil && right.ToolCall == nil
	}
	return left.ToolCall.ID == right.ToolCall.ID && left.ToolCall.Name == right.ToolCall.Name && bytes.Equal(left.ToolCall.Input, right.ToolCall.Input)
}

func cloneMessages(messages []protocol.Message) []protocol.Message {
	if messages == nil {
		return nil
	}
	result := make([]protocol.Message, len(messages))
	for i := range messages {
		result[i] = cloneMessage(messages[i])
	}
	return result
}

func cloneMessage(message protocol.Message) protocol.Message {
	result := protocol.Message{Role: message.Role}
	if message.Content != nil {
		result.Content = make([]protocol.ContentPart, len(message.Content))
		for i, part := range message.Content {
			result.Content[i] = part
			if part.ToolCall != nil {
				call := cloneToolCall(*part.ToolCall)
				result.Content[i].ToolCall = &call
			}
		}
	}
	return result
}

func cloneToolCall(call protocol.ToolCall) protocol.ToolCall {
	call.Input = append([]byte(nil), call.Input...)
	return call
}

var _ Stream = (*responseRecordingStream)(nil)
