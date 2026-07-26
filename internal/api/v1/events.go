package v1

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/amirulashraf/parrot-coder/internal/protocol"
)

const (
	EventServerConnected        = "server.connected"
	EventMessagePartDelta       = "message.part.delta"
	EventSessionStatus          = "session.status"
	EventPermission             = "permission.pending"
	EventPermissionReply        = "permission.resolved"
	EventQuestion               = "question.pending"
	EventQuestionReply          = "question.resolved"
	EventSessionInputAdmitted   = "session.input.admitted"
	EventSessionInputPromoted   = "session.input.promoted"
	EventTodoUpdated            = "todo.updated"
	EventGoalUpdated            = "goal.updated"
	EventGoalCleared            = "goal.cleared"
	EventAgentSessionProgress   = "agent_session.progress"
	EventUserSessionStart       = "user_session.start"
	EventUserSessionWorking     = "user_session.working"
	EventUserSessionIdle        = "user_session.idle"
	EventAgentSessionStart      = "agent_session.start"
	EventAgentSessionWorking    = "agent_session.working"
	EventAgentSessionIdle       = "agent_session.idle"
	EventAgentSessionFinished   = "agent_session.finished"
	EventProcessStart           = "process.start"
	EventProcessFinished        = "process.finished"
	EventSessionToolPending     = "session.tool.pending"
	EventSessionToolRunning     = "session.tool.running"
	EventSessionToolSuccess     = "session.tool.success"
	EventSessionToolFailure     = "session.tool.failure"
	EventSessionToolInterrupted = "session.tool.interrupted"
	EventToolOutputDelta        = "tool.output.delta"
	EventCodeDisplay            = "tool.code.display"
)

// Event is used for both durable and disposable live events. SessionID always
// identifies the session which produced the event, including when the broker
// routes descendant activity to an ancestor subscription. Sequence and
// CreatedAt are present only for durable session events.
type Event struct {
	ID        string          `json:"id"`
	Type      string          `json:"type"`
	SessionID string          `json:"session_id,omitempty"`
	Sequence  *int64          `json:"sequence,omitempty"`
	Data      json.RawMessage `json:"data"`
	CreatedAt *time.Time      `json:"created_at,omitempty"`
}

type MessagePartDelta struct {
	MessageID  string `json:"message_id,omitempty"`
	PartID     string `json:"part_id,omitempty"`
	Kind       string `json:"kind"`
	Delta      string `json:"delta"`
	Done       bool   `json:"done,omitempty"`
	ToolCallID string `json:"tool_call_id,omitempty"`
	ToolName   string `json:"tool_name,omitempty"`
}

type Usage struct {
	InputTokens       int     `json:"input_tokens"`
	OutputTokens      int     `json:"output_tokens"`
	TotalTokens       int     `json:"total_tokens"`
	ReasoningTokens   int     `json:"reasoning_tokens"`
	CachedInputTokens int     `json:"cached_input_tokens"`
	InputCost         float64 `json:"input_cost,omitempty"`
	OutputCost        float64 `json:"output_cost,omitempty"`
}

type AgentSessionProgress struct {
	SessionID string `json:"session_id,omitempty"`
	Agent     string `json:"agent"`
	Status    string `json:"status"`
	Usage     Usage  `json:"usage"`
	ToolUses  int    `json:"tool_uses"`
}

type ToolOutputDelta struct {
	ToolCallID string `json:"tool_call_id"`
	Delta      string `json:"delta"`
}

// CodeDisplay is an atomic source block emitted by a tool for presentation.
type CodeDisplay struct {
	ToolCallID string `json:"tool_call_id"`
	Source     string `json:"source"`
	Path       string `json:"path,omitempty"`
	Language   string `json:"language,omitempty"`
	StartLine  int    `json:"start_line,omitempty"`
}

// UserSessionEvent describes the lifecycle of the session directly owned by a
// user. A user session becomes idle between turns and does not finish while it
// remains available for more input.
type UserSessionEvent struct {
	SessionID string `json:"session_id"`
	Status    string `json:"status,omitempty"`
	Error     string `json:"error,omitempty"`
}

// AgentSessionEvent describes the lifecycle of a child agent session.
type AgentSessionEvent struct {
	SessionID       string `json:"session_id"`
	ParentSessionID string `json:"parent_session_id,omitempty"`
	Agent           string `json:"agent,omitempty"`
	Name            string `json:"name,omitempty"`
	Status          string `json:"status,omitempty"`
	Error           string `json:"error,omitempty"`
}

// ProcessEvent describes a shell process owned by a session.
type ProcessEvent struct {
	SessionID string `json:"session_id"`
	ProcessID string `json:"process_id"`
	Name      string `json:"name,omitempty"`
	Status    string `json:"status,omitempty"`
	Error     string `json:"error,omitempty"`
}

// ToolEvent is the stable payload shared by the durable tool lifecycle.
type ToolEvent struct {
	CallID     string         `json:"call_id"`
	ToolName   string         `json:"tool_name,omitempty"`
	Input      map[string]any `json:"input,omitempty"`
	Status     string         `json:"status,omitempty"`
	Result     any            `json:"result,omitempty"`
	Error      string         `json:"error,omitempty"`
	OutputTail string         `json:"output_tail,omitempty"`
}

type SessionStatus struct {
	MessageID    string `json:"message_id,omitempty"`
	Kind         string `json:"kind"`
	FinishReason string `json:"finish_reason,omitempty"`
	ErrorCode    string `json:"error_code,omitempty"`
	Message      string `json:"message,omitempty"`
	Usage        *Usage `json:"usage,omitempty"`
}

type PermissionResolved struct {
	RequestID string `json:"request_id"`
	Decision  string `json:"decision"`
}

type QuestionResolved struct {
	RequestID string `json:"request_id"`
	Rejected  bool   `json:"rejected"`
}

type SessionInputAdmitted struct {
	InputID   string `json:"input_id"`
	MessageID string `json:"message_id"`
	Content   string `json:"content"`
	Delivery  string `json:"delivery"`
}

type SessionInputPromoted struct {
	InputID   string `json:"input_id"`
	MessageID string `json:"message_id"`
}

type TodoUpdated struct {
	Todos []Todo `json:"todos"`
}

type EventDefinition struct {
	Name    string `json:"name"`
	Durable bool   `json:"durable"`
	Payload string `json:"payload"`
}

// EventManifest is the closed set understood by the v1 API and client. The
// assistant and tool terminal names are enumerated rather than pattern based.
var EventManifest = []EventDefinition{
	{Name: EventServerConnected, Payload: "Empty"},
	{Name: EventMessagePartDelta, Payload: "MessagePartDelta"},
	{Name: EventSessionStatus, Payload: "SessionStatus"},
	{Name: EventPermission, Payload: "Permission"},
	{Name: EventPermissionReply, Payload: "PermissionResolved"},
	{Name: EventQuestion, Payload: "QuestionRequest"},
	{Name: EventQuestionReply, Payload: "QuestionResolved"},
	{Name: EventSessionInputAdmitted, Durable: true, Payload: "SessionInputAdmitted"},
	{Name: EventSessionInputPromoted, Durable: true, Payload: "SessionInputPromoted"},
	{Name: EventTodoUpdated, Durable: true, Payload: "TodoUpdated"},
	{Name: EventGoalUpdated, Durable: true, Payload: "Goal"},
	{Name: EventGoalCleared, Durable: true, Payload: "Goal"},
	{Name: EventAgentSessionProgress, Payload: "AgentSessionProgress"},
	{Name: EventUserSessionStart, Payload: "UserSessionEvent"},
	{Name: EventUserSessionWorking, Payload: "UserSessionEvent"},
	{Name: EventUserSessionIdle, Payload: "UserSessionEvent"},
	{Name: EventAgentSessionStart, Payload: "AgentSessionEvent"},
	{Name: EventAgentSessionWorking, Payload: "AgentSessionEvent"},
	{Name: EventAgentSessionIdle, Payload: "AgentSessionEvent"},
	{Name: EventAgentSessionFinished, Payload: "AgentSessionEvent"},
	{Name: EventProcessStart, Payload: "ProcessEvent"},
	{Name: EventProcessFinished, Payload: "ProcessEvent"},
	{Name: EventToolOutputDelta, Payload: "ToolOutputDelta"},
	{Name: EventCodeDisplay, Payload: "CodeDisplay"},
	{Name: "session.selection.changed", Durable: true, Payload: "object"},
	{Name: "session.context.initialized", Durable: true, Payload: "object"},
	{Name: "session.context.observed", Durable: true, Payload: "object"},
	{Name: "session.context.changed", Durable: true, Payload: "object"},
	{Name: "session.context.replaced", Durable: true, Payload: "object"},
	{Name: "session.message.appended", Durable: true, Payload: "object"},
	{Name: "session.status_prompt.appended", Durable: true, Payload: "object"},
	{Name: "session.assistant.started", Durable: true, Payload: "object"},
	{Name: "session.assistant.complete", Durable: true, Payload: "object"},
	{Name: "session.assistant.error", Durable: true, Payload: "object"},
	{Name: "session.assistant.interrupted", Durable: true, Payload: "object"},
	{Name: EventSessionToolPending, Durable: true, Payload: "ToolEvent"},
	{Name: EventSessionToolRunning, Durable: true, Payload: "ToolEvent"},
	{Name: EventSessionToolSuccess, Durable: true, Payload: "ToolEvent"},
	{Name: EventSessionToolFailure, Durable: true, Payload: "ToolEvent"},
	{Name: EventSessionToolInterrupted, Durable: true, Payload: "ToolEvent"},
	{Name: "session.runtime.repaired", Durable: true, Payload: "object"},
	{Name: "session.compaction.completed", Durable: true, Payload: "object"},
	{Name: "session.compaction.retry", Durable: true, Payload: "object"},
}

func KnownEvent(name string) bool {
	for _, item := range EventManifest {
		if item.Name == name {
			return true
		}
	}
	return false
}

// DecodeEventData decodes a manifest event into its stable payload DTO.
// Durable event payloads without a stable DTO remain JSON objects because their
// authoritative state is exposed by resource queries.
func DecodeEventData(event Event) (any, error) {
	var target any
	switch event.Type {
	case EventServerConnected:
		target = &Empty{}
	case EventMessagePartDelta:
		target = &MessagePartDelta{}
	case EventSessionStatus:
		target = &SessionStatus{}
	case EventPermission:
		target = &Permission{}
	case EventPermissionReply:
		target = &PermissionResolved{}
	case EventQuestion:
		target = &QuestionRequest{}
	case EventQuestionReply:
		target = &QuestionResolved{}
	case EventSessionInputAdmitted:
		target = &SessionInputAdmitted{}
	case EventSessionInputPromoted:
		target = &SessionInputPromoted{}
	case EventTodoUpdated:
		target = &TodoUpdated{}
	case EventGoalUpdated, EventGoalCleared:
		target = &Goal{}
	case EventAgentSessionProgress:
		target = &AgentSessionProgress{}
	case EventUserSessionStart, EventUserSessionWorking, EventUserSessionIdle:
		target = &UserSessionEvent{}
	case EventAgentSessionStart, EventAgentSessionWorking, EventAgentSessionIdle, EventAgentSessionFinished:
		target = &AgentSessionEvent{}
	case EventProcessStart, EventProcessFinished:
		target = &ProcessEvent{}
	case EventSessionToolPending:
		return decodePendingToolEvent(event.Data)
	case EventSessionToolRunning, EventSessionToolSuccess, EventSessionToolFailure, EventSessionToolInterrupted:
		return decodeToolEvent(event.Data)
	case EventToolOutputDelta:
		target = &ToolOutputDelta{}
	case EventCodeDisplay:
		target = &CodeDisplay{}
	default:
		if !KnownEvent(event.Type) {
			return nil, errors.New("v1: unknown event type")
		}
		target = &map[string]any{}
	}
	if err := decodeStrict(event.Data, target); err != nil {
		return nil, err
	}
	return target, nil
}

func decodePendingToolEvent(data json.RawMessage) (*ToolEvent, error) {
	if canonical, err := decodeToolEvent(data); err == nil {
		return canonical, nil
	}

	var fields map[string]json.RawMessage
	if err := decodeStrict(data, &fields); err != nil {
		return nil, err
	}
	if len(fields) != 3 || fields["ID"] == nil || fields["Name"] == nil || fields["Input"] == nil {
		return nil, errors.New("v1: invalid legacy tool event")
	}
	var legacy protocol.ToolCall
	if err := decodeStrict(data, &legacy); err != nil {
		return nil, err
	}
	var input map[string]any
	if err := decodeStrictWithNumbers(legacy.Input, &input); err != nil {
		return nil, err
	}
	return &ToolEvent{CallID: legacy.ID, ToolName: legacy.Name, Input: input}, nil
}

func decodeToolEvent(data json.RawMessage) (*ToolEvent, error) {
	var event ToolEvent
	if err := decodeStrictWithNumbers(data, &event); err != nil {
		return nil, err
	}
	return &event, nil
}

func decodeStrict(data json.RawMessage, target any) error {
	return decodeJSON(data, target, false)
}

func decodeStrictWithNumbers(data json.RawMessage, target any) error {
	return decodeJSON(data, target, true)
}

func decodeJSON(data json.RawMessage, target any, useNumber bool) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if useNumber {
		decoder.UseNumber()
	}
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errors.New("v1: trailing event data")
	}
	return nil
}
