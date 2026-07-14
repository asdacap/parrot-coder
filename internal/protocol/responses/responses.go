// Package responses adapts the OpenAI Responses API wire protocol to the
// provider-neutral protocol package.
package responses

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/amirulashraf/parrot-coder/internal/protocol"
	"github.com/amirulashraf/parrot-coder/internal/protocol/sse"
)

// EncodeRequest encodes a streaming Responses API request.
func EncodeRequest(request protocol.Request) ([]byte, error) {
	input := make([]any, 0, len(request.Messages))
	for _, message := range request.Messages {
		content := make([]any, 0, len(message.Content))
		afterMessage := make([]any, 0)
		for _, part := range message.Content {
			switch part.Type {
			case protocol.ContentText, protocol.ContentReasoning:
				partType := "input_text"
				if message.Role == "assistant" {
					partType = "output_text"
				}
				content = append(content, map[string]any{"type": partType, "text": part.Text})
			case protocol.ContentToolCall:
				if part.ToolCall == nil || !json.Valid(part.ToolCall.Input) {
					return nil, fmt.Errorf("responses: message contains invalid tool call")
				}
				afterMessage = append(afterMessage, map[string]any{
					"type": "function_call", "call_id": part.ToolCall.ID,
					"name": part.ToolCall.Name, "arguments": string(part.ToolCall.Input),
				})
			case protocol.ContentToolResult:
				afterMessage = append(afterMessage, map[string]any{
					"type": "function_call_output", "call_id": part.ToolCallID, "output": part.Text,
				})
			}
		}
		if len(content) > 0 || len(message.Content) == 0 {
			input = append(input, map[string]any{"type": "message", "role": message.Role, "content": content})
		}
		input = append(input, afterMessage...)
	}

	tools := make([]any, 0, len(request.Tools))
	for _, definition := range request.Tools {
		schema := definition.InputSchema
		if len(schema) == 0 {
			schema = json.RawMessage(`{}`)
		}
		if !json.Valid(schema) {
			return nil, fmt.Errorf("responses: tool %q has invalid input schema", definition.Name)
		}
		tools = append(tools, struct {
			Type        string          `json:"type"`
			Name        string          `json:"name"`
			Description string          `json:"description,omitempty"`
			Parameters  json.RawMessage `json:"parameters"`
			Strict      bool            `json:"strict"`
		}{"function", definition.Name, definition.Description, schema, false})
	}
	body := struct {
		Model        string `json:"model"`
		Instructions string `json:"instructions,omitempty"`
		Input        []any  `json:"input"`
		Tools        []any  `json:"tools,omitempty"`
		Stream       bool   `json:"stream"`
		Store        bool   `json:"store"`
		Reasoning    *reasoningOptions `json:"reasoning,omitempty"`
	}{Model: request.Model, Instructions: request.Instructions, Input: input, Tools: tools, Stream: true, Store: false}
	if request.Reasoning != nil && request.Reasoning.Effort != "" {
		body.Reasoning = &reasoningOptions{Effort: request.Reasoning.Effort}
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("responses: encode request: %w", err)
	}
	return encoded, nil
}

type reasoningOptions struct {
	Effort string `json:"effort"`
}

type toolAccumulator struct {
	key, itemID, callID, name, input string
	finalized                        bool
}

// Parser converts a Responses API SSE stream into canonical events.
type Parser struct {
	decoder *sse.Decoder
	tools   map[string]*toolAccumulator
	aliases map[string]string
	pending []protocol.Event
	done    bool
}

// NewParser creates a streaming parser.
func NewParser(reader io.Reader, maxEventBytes int) *Parser {
	return &Parser{
		decoder: sse.NewDecoder(reader, maxEventBytes),
		tools:   make(map[string]*toolAccumulator), aliases: make(map[string]string),
	}
}

// Close stops the underlying SSE decoder.
func (p *Parser) Close() error { return p.decoder.Close() }

// Next returns the next canonical event.
func (p *Parser) Next(ctx context.Context) (protocol.Event, error) {
	for len(p.pending) == 0 {
		if p.done {
			return protocol.Event{}, io.EOF
		}
		record, err := p.decoder.Next(ctx)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return protocol.Event{}, errors.New("responses: stream ended without a terminal event")
			}
			return protocol.Event{}, err
		}
		if record.Data == "[DONE]" {
			return protocol.Event{}, errors.New("responses: stream ended without a terminal event")
		}
		if err := p.consume([]byte(record.Data)); err != nil {
			return protocol.Event{}, err
		}
	}
	event := p.pending[0]
	p.pending = p.pending[1:]
	return event, nil
}

func (p *Parser) consume(data []byte) error {
	var event struct {
		Type      string `json:"type"`
		Delta     string `json:"delta"`
		ItemID    string `json:"item_id"`
		CallID    string `json:"call_id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
		Code      string `json:"code"`
		Message   string `json:"message"`
		Item      struct {
			ID        string `json:"id"`
			Type      string `json:"type"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"item"`
		Error    *wireError `json:"error"`
		Response struct {
			Status            string     `json:"status"`
			Error             *wireError `json:"error"`
			IncompleteDetails struct {
				Reason string `json:"reason"`
			} `json:"incomplete_details"`
			Usage *wireUsage `json:"usage"`
		} `json:"response"`
	}
	if err := json.Unmarshal(data, &event); err != nil {
		return fmt.Errorf("responses: decode stream event: %w", err)
	}

	switch event.Type {
	case "response.output_text.delta":
		if event.Delta != "" {
			p.pending = append(p.pending, protocol.Event{Type: protocol.EventTextDelta, Text: event.Delta})
		}
	case "response.reasoning_text.delta", "response.reasoning_summary_text.delta", "response.output_text.annotation.added":
		if event.Delta != "" {
			p.pending = append(p.pending, protocol.Event{Type: protocol.EventReasoningDelta, Text: event.Delta})
		}
	case "response.output_item.added":
		if event.Item.Type == "function_call" {
			p.addTool(event.Item.ID, event.Item.CallID, event.Item.Name, event.Item.Arguments)
		}
	case "response.function_call_arguments.delta":
		item := p.findTool(event.ItemID, event.CallID)
		if item == nil {
			item = p.addTool(event.ItemID, event.CallID, event.Name, "")
		}
		item.input += event.Delta
		if event.Delta != "" {
			p.pending = append(p.pending, protocol.Event{Type: protocol.EventToolInputDelta, ToolInput: &protocol.ToolInputDelta{
				ID: toolID(item), Name: item.name, Delta: event.Delta,
			}})
		}
	case "response.function_call_arguments.done":
		item := p.findTool(event.ItemID, event.CallID)
		if item == nil {
			item = p.addTool(event.ItemID, event.CallID, event.Name, event.Arguments)
		} else if event.Arguments != "" {
			item.input = event.Arguments
		}
	case "response.output_item.done":
		if event.Item.Type == "function_call" {
			item := p.findTool(event.Item.ID, event.Item.CallID)
			if item == nil {
				item = p.addTool(event.Item.ID, event.Item.CallID, event.Item.Name, event.Item.Arguments)
			} else if event.Item.Arguments != "" {
				item.input = event.Item.Arguments
			}
			if err := p.finalize(item); err != nil {
				return err
			}
		}
	case "response.completed":
		if err := p.finalizeAll(); err != nil {
			return err
		}
		p.appendUsage(event.Response.Usage)
		reason := protocol.FinishStop
		if len(p.tools) > 0 {
			reason = protocol.FinishToolCalls
		}
		p.pending = append(p.pending, protocol.Event{Type: protocol.EventFinish, FinishReason: reason})
		p.done = true
	case "response.incomplete":
		if err := p.finalizeAll(); err != nil {
			return err
		}
		p.appendUsage(event.Response.Usage)
		p.pending = append(p.pending, protocol.Event{Type: protocol.EventFinish, FinishReason: incompleteReason(event.Response.IncompleteDetails.Reason)})
		p.done = true
	case "response.failed":
		p.appendUsage(event.Response.Usage)
		failure := event.Response.Error
		if failure == nil {
			failure = &wireError{Type: "provider_error", Message: "response failed"}
		}
		p.appendError(failure)
	case "error":
		failure := event.Error
		if failure == nil {
			failure = &wireError{Type: "provider_error", Code: event.Code, Message: event.Message}
		}
		p.appendError(failure)
	}
	return nil
}

type wireError struct {
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

type wireUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
	InputDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	OutputDetails struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

func (p *Parser) addTool(itemID, callID, name, input string) *toolAccumulator {
	key := itemID
	if key == "" {
		key = callID
	}
	item := p.findTool(itemID, callID)
	if item == nil {
		item = &toolAccumulator{key: key, itemID: itemID, callID: callID, name: name, input: input}
		p.tools[key] = item
	} else {
		if item.itemID == "" {
			item.itemID = itemID
		}
		if item.callID == "" {
			item.callID = callID
		}
		if item.name == "" {
			item.name = name
		}
		if item.input == "" {
			item.input = input
		}
		key = item.key
	}
	if itemID != "" {
		p.aliases[itemID] = key
	}
	if callID != "" {
		p.aliases[callID] = key
	}
	return item
}

func (p *Parser) findTool(itemID, callID string) *toolAccumulator {
	for _, candidate := range []string{itemID, callID} {
		if key, ok := p.aliases[candidate]; ok {
			return p.tools[key]
		}
		if item := p.tools[candidate]; item != nil {
			return item
		}
	}
	return nil
}

func (p *Parser) finalizeAll() error {
	keys := make([]string, 0, len(p.tools))
	for key := range p.tools {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if err := p.finalize(p.tools[key]); err != nil {
			return err
		}
	}
	return nil
}

func (p *Parser) finalize(item *toolAccumulator) error {
	if item.finalized {
		return nil
	}
	if toolID(item) == "" || item.name == "" {
		return errors.New("responses: tool call requires an ID and name")
	}
	input := json.RawMessage(item.input)
	if !json.Valid(input) {
		return fmt.Errorf("responses: tool call %q has invalid JSON input", toolID(item))
	}
	item.finalized = true
	p.pending = append(p.pending, protocol.Event{Type: protocol.EventToolCallComplete, ToolCall: &protocol.ToolCall{
		ID: toolID(item), Name: item.name, Input: append(json.RawMessage(nil), input...),
	}})
	return nil
}

func toolID(item *toolAccumulator) string {
	if item.callID != "" {
		return item.callID
	}
	return item.itemID
}

func (p *Parser) appendUsage(usage *wireUsage) {
	if usage == nil {
		return
	}
	p.pending = append(p.pending, protocol.Event{Type: protocol.EventUsage, Usage: &protocol.Usage{
		InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens, TotalTokens: usage.TotalTokens,
		ReasoningTokens: usage.OutputDetails.ReasoningTokens, CachedInputTokens: usage.InputDetails.CachedTokens,
	}})
}

func (p *Parser) appendError(failure *wireError) {
	if failure == nil {
		failure = &wireError{Type: "provider_error", Message: "provider returned an error"}
	}
	p.pending = append(p.pending, protocol.Event{Type: protocol.EventProviderError, ProviderError: &protocol.ProviderError{
		Type: failure.Type, Code: failure.Code, Message: failure.Message,
	}})
	p.done = true
}

func incompleteReason(reason string) protocol.FinishReason {
	switch reason {
	case "max_output_tokens", "max_tokens":
		return protocol.FinishLength
	case "content_filter":
		return protocol.FinishContentFilter
	default:
		return protocol.FinishIncomplete
	}
}
