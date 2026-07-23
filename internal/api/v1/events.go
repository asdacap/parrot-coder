package v1

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"time"
)

const (
	EventServerConnected      = "server.connected"
	EventMessagePartDelta     = "message.part.delta"
	EventSessionStatus        = "session.status"
	EventPermission           = "permission.pending"
	EventPermissionReply      = "permission.resolved"
	EventQuestion             = "question.pending"
	EventQuestionReply        = "question.resolved"
	EventSessionInputAdmitted = "session.input.admitted"
	EventSessionInputPromoted = "session.input.promoted"
	EventTodoUpdated          = "todo.updated"
	EventGoalUpdated          = "goal.updated"
	EventGoalCleared          = "goal.cleared"
	EventTaskProgress         = "task.progress"
	EventTaskStart            = "task.start"
	EventTaskWorking          = "task.working"
	EventTaskIdle             = "task.idle"
	EventTaskFinished         = "task.finished"
	EventToolOutputDelta      = "tool.output.delta"
)

// Event is used for both durable and disposable live events. Sequence and
// CreatedAt are present only for durable session events. TaskID identifies the
// task which produced the event; every event belongs to exactly one task. Task
// events are flat: a subtask's events are never nested inside a parent task's
// event, they carry their own task_id and session_id instead.
type Event struct {
	ID        string          `json:"id"`
	Type      string          `json:"type"`
	SessionID string          `json:"session_id,omitempty"`
	TaskID    string          `json:"task_id,omitempty"`
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

type TaskProgress struct {
	TaskID     string `json:"task_id"`
	SessionID  string `json:"session_id,omitempty"`
	ToolCallID string `json:"tool_call_id,omitempty"`
	Agent      string `json:"agent"`
	Status     string `json:"status"`
	Usage      Usage  `json:"usage"`
	ToolUses   int    `json:"tool_uses"`
}

type ToolOutputDelta struct {
	ToolCallID string `json:"tool_call_id"`
	Delta      string `json:"delta"`
}

// TaskEvent is the flat lifecycle record every task emits. Every task belongs
// directly to SessionID; ParentSessionID links that session into the hierarchy.
// The event envelope may name an ancestor stream when descendant activity is
// forwarded, while these fields retain the task's actual ownership. Status and
// Error are set on task.finished.
type TaskEvent struct {
	TaskID          string `json:"task_id"`
	SessionID       string `json:"session_id"`
	ParentSessionID string `json:"parent_session_id,omitempty"`
	Kind            string `json:"kind"`
	Agent           string `json:"agent,omitempty"`
	Name            string `json:"name,omitempty"`
	Status          string `json:"status,omitempty"`
	Error           string `json:"error,omitempty"`
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
	{Name: EventTaskProgress, Payload: "TaskProgress"},
	{Name: EventTaskStart, Payload: "TaskEvent"},
	{Name: EventTaskWorking, Payload: "TaskEvent"},
	{Name: EventTaskIdle, Payload: "TaskEvent"},
	{Name: EventTaskFinished, Payload: "TaskEvent"},
	{Name: EventToolOutputDelta, Payload: "ToolOutputDelta"},
	{Name: "session.selection.changed", Durable: true, Payload: "object"},
	{Name: "session.context.initialized", Durable: true, Payload: "object"},
	{Name: "session.context.observed", Durable: true, Payload: "object"},
	{Name: "session.context.changed", Durable: true, Payload: "object"},
	{Name: "session.context.replaced", Durable: true, Payload: "object"},
	{Name: "session.message.appended", Durable: true, Payload: "object"},
	{Name: "session.assistant.started", Durable: true, Payload: "object"},
	{Name: "session.assistant.complete", Durable: true, Payload: "object"},
	{Name: "session.assistant.error", Durable: true, Payload: "object"},
	{Name: "session.assistant.interrupted", Durable: true, Payload: "object"},
	{Name: "session.tool.pending", Durable: true, Payload: "object"},
	{Name: "session.tool.running", Durable: true, Payload: "object"},
	{Name: "session.tool.success", Durable: true, Payload: "object"},
	{Name: "session.tool.failure", Durable: true, Payload: "object"},
	{Name: "session.tool.interrupted", Durable: true, Payload: "object"},
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
	case EventTaskProgress:
		target = &TaskProgress{}
	case EventTaskStart, EventTaskWorking, EventTaskIdle, EventTaskFinished:
		target = &TaskEvent{}
	case EventToolOutputDelta:
		target = &ToolOutputDelta{}
	default:
		if !KnownEvent(event.Type) {
			return nil, errors.New("v1: unknown event type")
		}
		target = &map[string]any{}
	}
	decoder := json.NewDecoder(bytes.NewReader(event.Data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return nil, err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, errors.New("v1: trailing event data")
	}
	return target, nil
}
