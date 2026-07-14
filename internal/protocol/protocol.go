// Package protocol defines the provider-neutral language model protocol used by
// the runtime and its provider wire adapters.
package protocol

import "encoding/json"

// Request is one provider turn.
type Request struct {
	Model        string
	Instructions string
	Messages     []Message
	Tools        []ToolDefinition
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

// Usage reports token accounting when supplied by the provider.
type Usage struct {
	InputTokens       int
	OutputTokens      int
	TotalTokens       int
	ReasoningTokens   int
	CachedInputTokens int
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
	EventTextDelta        EventType = "text_delta"
	EventReasoningDelta   EventType = "reasoning_delta"
	EventToolInputDelta   EventType = "tool_input_delta"
	EventToolCallComplete EventType = "tool_call_complete"
	EventUsage            EventType = "usage"
	EventFinish           EventType = "finish"
	EventProviderError    EventType = "provider_error"
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
	Text          string
	ToolInput     *ToolInputDelta
	ToolCall      *ToolCall
	Usage         *Usage
	FinishReason  FinishReason
	ProviderError *ProviderError
}
