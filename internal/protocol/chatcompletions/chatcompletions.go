// Package chatcompletions adapts the OpenAI-compatible Chat Completions wire
// protocol to the provider-neutral protocol package.
package chatcompletions

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

// EncodeRequest encodes a streaming Chat Completions request.
func EncodeRequest(request protocol.Request) ([]byte, error) {
	type function struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters"`
	}
	type tool struct {
		Type     string   `json:"type"`
		Function function `json:"function"`
	}

	messages := make([]any, 0, len(request.Messages)+1)
	if request.Instructions != "" {
		messages = append(messages, map[string]any{"role": "system", "content": request.Instructions})
	}
	for _, message := range request.Messages {
		text := ""
		toolCalls := make([]any, 0)
		for _, part := range message.Content {
			switch part.Type {
			case protocol.ContentText, protocol.ContentReasoning:
				text += part.Text
			case protocol.ContentToolCall:
				if part.ToolCall == nil || !json.Valid(part.ToolCall.Input) {
					return nil, fmt.Errorf("chatcompletions: message contains invalid tool call")
				}
				toolCalls = append(toolCalls, map[string]any{
					"id": part.ToolCall.ID, "type": "function",
					"function": map[string]any{"name": part.ToolCall.Name, "arguments": string(part.ToolCall.Input)},
				})
			case protocol.ContentToolResult:
				messages = append(messages, map[string]any{"role": "tool", "tool_call_id": part.ToolCallID, "content": part.Text})
			}
		}
		if text != "" || len(toolCalls) > 0 || len(message.Content) == 0 {
			item := map[string]any{"role": message.Role, "content": text}
			if len(toolCalls) > 0 {
				item["tool_calls"] = toolCalls
			}
			messages = append(messages, item)
		}
	}

	tools := make([]tool, 0, len(request.Tools))
	for _, definition := range request.Tools {
		schema := definition.InputSchema
		if len(schema) == 0 {
			schema = json.RawMessage(`{}`)
		}
		if !json.Valid(schema) {
			return nil, fmt.Errorf("chatcompletions: tool %q has invalid input schema", definition.Name)
		}
		tools = append(tools, tool{Type: "function", Function: function{
			Name: definition.Name, Description: definition.Description, Parameters: schema,
		}})
	}

	body := struct {
		Model                  string          `json:"model"`
		Messages               []any           `json:"messages"`
		Tools                  []tool          `json:"tools,omitempty"`
		Stream                 bool            `json:"stream"`
		Options                any             `json:"stream_options"`
		ReasoningEffort        string          `json:"reasoning_effort,omitempty"`
		Provider               json.RawMessage `json:"provider,omitempty"`
		IncludeRouterMetadata  bool            `json:"include_router_metadata,omitempty"`
	}{Model: request.Model, Messages: messages, Tools: tools, Stream: true, Options: map[string]bool{"include_usage": true}}
	if request.Reasoning != nil {
		body.ReasoningEffort = request.Reasoning.Effort
	}
	preferences, err := protocol.NormalizeProviderPreferences(request.ProviderPreferences)
	body.IncludeRouterMetadata = request.IncludeRouterMetadata
	if err != nil {
		return nil, fmt.Errorf("chatcompletions: %w", err)
	}
	body.Provider = preferences
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("chatcompletions: encode request: %w", err)
	}
	return encoded, nil
}

type toolAccumulator struct {
	id, name string
	input    string
}

// Parser converts a Chat Completions SSE stream into canonical events.
type Parser struct {
	decoder  *sse.Decoder
	tools    map[int]*toolAccumulator
	pending  []protocol.Event
	finish   *protocol.Event
	terminal bool
	done     bool
}

// NewParser creates a streaming parser.
func NewParser(reader io.Reader, maxEventBytes int) *Parser {
	return &Parser{decoder: sse.NewDecoder(reader, maxEventBytes), tools: make(map[int]*toolAccumulator)}
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
			if errors.Is(err, io.EOF) && p.finish != nil {
				p.pending = append(p.pending, *p.finish)
				p.finish = nil
				p.terminal = true
				p.done = true
				continue
			}
			if errors.Is(err, io.EOF) && p.terminal {
				p.done = true
				continue
			}
			if errors.Is(err, io.EOF) {
				return protocol.Event{}, errors.New("chatcompletions: stream ended without a terminal event")
			}
			return protocol.Event{}, err
		}
		if record.Data == "[DONE]" {
			if p.finish == nil && !p.terminal {
				return protocol.Event{}, errors.New("chatcompletions: stream ended without a terminal event")
			}
			if p.finish != nil {
				p.pending = append(p.pending, *p.finish)
				p.finish = nil
				p.terminal = true
			}
			p.done = true
			continue
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
	var chunk struct {
		Choices []struct {
			Delta struct {
				Content          *string `json:"content"`
				ReasoningContent *string `json:"reasoning_content"`
				Reasoning        *string `json:"reasoning"`
				ToolCalls        []struct {
					Index    int    `json:"index"`
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
			PromptDetails    struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
			CompletionDetails struct {
				ReasoningTokens int `json:"reasoning_tokens"`
			} `json:"completion_tokens_details"`
		} `json:"usage"`
		Error *struct {
			Type    string `json:"type"`
			Code    any    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Provider *struct {
			ProviderName string `json:"provider_name"`
			Model        string `json:"model,omitempty"`
		} `json:"provider,omitempty"`
	}
	if err := json.Unmarshal(data, &chunk); err != nil {
		return fmt.Errorf("chatcompletions: decode stream event: %w", err)
	}
	if chunk.Error != nil {
		code := ""
		if chunk.Error.Code != nil {
			code = fmt.Sprint(chunk.Error.Code)
		}
		p.pending = append(p.pending, protocol.Event{Type: protocol.EventProviderError, ProviderError: &protocol.ProviderError{
			Type: chunk.Error.Type, Code: code, Message: chunk.Error.Message,
		}})
		p.done = true
		return nil
	}
	for _, choice := range chunk.Choices {
		if choice.Delta.Content != nil && *choice.Delta.Content != "" {
			p.pending = append(p.pending, protocol.Event{Type: protocol.EventTextDelta, Text: *choice.Delta.Content})
		}
		reasoning := choice.Delta.ReasoningContent
		if reasoning == nil {
			reasoning = choice.Delta.Reasoning
		}
		if reasoning != nil && *reasoning != "" {
			p.pending = append(p.pending, protocol.Event{Type: protocol.EventReasoningDelta, Text: *reasoning})
		}
		for _, delta := range choice.Delta.ToolCalls {
			accumulator := p.tools[delta.Index]
			if accumulator == nil {
				accumulator = &toolAccumulator{}
				p.tools[delta.Index] = accumulator
			}
			if delta.ID != "" {
				accumulator.id = delta.ID
			}
			if delta.Function.Name != "" {
				accumulator.name = delta.Function.Name
			}
			accumulator.input += delta.Function.Arguments
			if delta.Function.Arguments != "" {
				p.pending = append(p.pending, protocol.Event{Type: protocol.EventToolInputDelta, ToolInput: &protocol.ToolInputDelta{
					ID: accumulator.id, Name: accumulator.name, Delta: delta.Function.Arguments,
				}})
			}
		}
		if choice.FinishReason != nil {
			if err := p.finalizeTools(); err != nil {
				return err
			}
			finished := protocol.Event{Type: protocol.EventFinish, FinishReason: finishReason(*choice.FinishReason)}
			p.finish = &finished
		}
	}
	if chunk.Provider != nil && chunk.Provider.ProviderName != "" {
		p.pending = append(p.pending, protocol.Event{
			Type:           protocol.EventRouterMetadata,
			RouterMetadata: &protocol.RouterMetadata{ProviderName: chunk.Provider.ProviderName, Model: chunk.Provider.Model},
		})
	}
	if chunk.Usage != nil {
		p.pending = append(p.pending, protocol.Event{Type: protocol.EventUsage, Usage: &protocol.Usage{
			InputTokens: chunk.Usage.PromptTokens, OutputTokens: chunk.Usage.CompletionTokens,
			TotalTokens: chunk.Usage.TotalTokens, ReasoningTokens: chunk.Usage.CompletionDetails.ReasoningTokens,
			CachedInputTokens: chunk.Usage.PromptDetails.CachedTokens,
		}})
		if p.finish != nil {
			p.pending = append(p.pending, *p.finish)
			p.finish = nil
			p.terminal = true
		}
	}
	return nil
}

func (p *Parser) finalizeTools() error {
	indexes := make([]int, 0, len(p.tools))
	for index := range p.tools {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	for _, index := range indexes {
		item := p.tools[index]
		if index < 0 || item.id == "" || item.name == "" {
			return fmt.Errorf("chatcompletions: tool call %d requires an ID and name", index)
		}
		input := json.RawMessage(item.input)
		if !json.Valid(input) {
			return fmt.Errorf("chatcompletions: tool call %d (%q) has invalid JSON input", index, item.name)
		}
		p.pending = append(p.pending, protocol.Event{Type: protocol.EventToolCallComplete, ToolCall: &protocol.ToolCall{
			ID: item.id, Name: item.name, Input: append(json.RawMessage(nil), input...),
		}})
	}
	clear(p.tools)
	return nil
}

func finishReason(reason string) protocol.FinishReason {
	switch reason {
	case "stop":
		return protocol.FinishStop
	case "tool_calls", "function_call":
		return protocol.FinishToolCalls
	case "length", "max_tokens":
		return protocol.FinishLength
	case "content_filter":
		return protocol.FinishContentFilter
	default:
		return protocol.FinishIncomplete
	}
}
