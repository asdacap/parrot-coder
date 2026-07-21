// Package protocol defines the provider-neutral language model protocol used by
// the runtime and its provider wire adapters.
package protocol

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// Request is one provider turn.
type Request struct {
	Model        string
	Instructions string
	Messages     []Message
	Tools        []ToolDefinition
	Reasoning    *ReasoningOptions
	// ProviderPreferences carries provider-specific request body extensions
	// as opaque JSON. OpenAI-compatible routers (such as OpenRouter) read it
	// as the top-level "provider" object to influence routing, fallback, and
	// data-collection behavior. Encoders pass it through verbatim when it is
	// valid JSON, so the protocol package stays neutral to any one vendor's
	// schema.
	ProviderPreferences json.RawMessage
}

// NormalizeProviderPreferences returns preferences as a JSON object suitable
// for a request body, or nil when unset. A non-object JSON value is rejected
// because the "provider" field OpenAI-compatible routers expect is always an
// object. Wire adapters call this to validate and forward the opaque blob
// without coupling the protocol package to any one vendor's schema.
func NormalizeProviderPreferences(preferences json.RawMessage) (json.RawMessage, error) {
	if len(preferences) == 0 {
		return nil, nil
	}
	if !json.Valid(preferences) {
		return nil, fmt.Errorf("provider preferences are not valid JSON")
	}
	trimmed := bytes.TrimSpace(preferences)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, fmt.Errorf("provider preferences must be a JSON object")
	}
	return preferences, nil
}

// ReasoningOptions controls optional provider reasoning behavior.
type ReasoningOptions struct {
	Effort  string
	Summary string
}

// Message is a conversation item. Content parts retain tool calls and results
// without coupling callers to a provider's message representation.
type Message struct {
	Role    Role
	Content []ContentPart
}

// Role identifies the author of a message.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// ContentPartType identifies a message content part.
type ContentPartType string

const (
	ContentText       ContentPartType = "text"
	ContentReasoning  ContentPartType = "reasoning"
	ContentToolCall   ContentPartType = "tool_call"
	ContentToolResult ContentPartType = "tool_result"
)

// ContentPart contains exactly the fields relevant to Type.
type ContentPart struct {
	Type       ContentPartType
	Text       string
	ToolCall   *ToolCall
	ToolCallID string
}

// ToolDefinition describes a callable function. InputSchema must be a JSON
// Schema object.
type ToolDefinition struct {
	Name        string
	Description string
	InputSchema json.RawMessage
}

// ToolCall is a finalized function call. Input is always valid JSON when a
// ToolCallComplete event is emitted by an adapter.
type ToolCall struct {
	ID    string
	Name  string
	Input json.RawMessage
}

// Usage reports token accounting when supplied by the provider. InputCost and
// OutputCost are set by the runner from the model's per-token prices.
type Usage struct {
	InputTokens       int
	OutputTokens      int
	TotalTokens       int
	ReasoningTokens   int
	CachedInputTokens int
	InputCost         float64
	OutputCost        float64
}

// FinishReason is the provider-neutral reason a turn ended.
type FinishReason string

const (
	FinishStop          FinishReason = "stop"
	FinishToolCalls     FinishReason = "tool_calls"
	FinishLength        FinishReason = "length"
	FinishContentFilter FinishReason = "content_filter"
	FinishIncomplete    FinishReason = "incomplete"
	FinishError         FinishReason = "error"
)

// EventType identifies an item in the canonical streaming lifecycle.
type EventType string

const (
	EventTextDelta             EventType = "text_delta"
	EventReasoningDelta        EventType = "reasoning_delta"
	EventReasoningSummaryDelta EventType = "reasoning_summary_delta"
	EventReasoningSummaryDone  EventType = "reasoning_summary_done"
	EventToolInputDelta        EventType = "tool_input_delta"
	EventToolCallComplete      EventType = "tool_call_complete"
	EventUsage                 EventType = "usage"
	EventFinish                EventType = "finish"
	EventProviderError         EventType = "provider_error"
	EventProviderRetry         EventType = "provider_retry"
	EventToolOutputDelta       EventType = "tool_output_delta"
)

// ToolInputDelta is an incremental fragment of a function's JSON arguments.
type ToolInputDelta struct {
	ID    string
	Name  string
	Delta string
}

// ProviderError is a safe, structured provider failure.
type ProviderError struct {
	Type    string
	Code    string
	Message string
}

// Event is one canonical stream event. Only the payload corresponding to Type
// is populated.
type Event struct {
	Type          EventType
	MessageID     string
	PartID        string
	Text          string
	ToolInput     *ToolInputDelta
	ToolCall      *ToolCall
	Usage         *Usage
	FinishReason  FinishReason
	ProviderError *ProviderError
	ToolCallID    string
}
