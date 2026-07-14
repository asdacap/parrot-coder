// Package v1 defines the stable version 1 HTTP and event wire contract.
package v1

import (
	"encoding/json"
	"time"
)

const (
	MediaTypeJSON    = "application/json"
	MediaTypeProblem = "application/problem+json"
	MediaTypeSSE     = "text/event-stream"
)

type Problem struct {
	Type      string `json:"type"`
	Title     string `json:"title"`
	Status    int    `json:"status"`
	Detail    string `json:"detail"`
	Code      string `json:"code"`
	RequestID string `json:"request_id"`
	ErrorRef  string `json:"error_ref,omitempty"`
}

type Health struct {
	Status string `json:"status"`
}

type Runtime struct {
	Version string           `json:"version"`
	Active  []RuntimeSession `json:"active"`
}

type RuntimeSession struct {
	SessionID string `json:"session_id"`
	Status    string `json:"status"`
}

type Session struct {
	ID        string    `json:"id"`
	ProjectID string    `json:"project_id,omitempty"`
	Title     string    `json:"title"`
	Agent     string    `json:"agent,omitempty"`
	Provider  string    `json:"provider,omitempty"`
	Model     string    `json:"model,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type SessionList struct {
	Items      []Session `json:"items"`
	NextCursor string    `json:"next_cursor,omitempty"`
}

type CreateSessionRequest struct {
	ProjectID string `json:"project_id,omitempty"`
	Title     string `json:"title,omitempty"`
	Agent     string `json:"agent,omitempty"`
	Model     string `json:"model,omitempty"`
}

// SessionSelection is the resolved execution selection persisted on a session.
type SessionSelection struct {
	Agent    string `json:"agent"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// UpdateSessionSelectionRequest changes either or both selection dimensions.
// Model accepts provider/model or a model ID under the current provider.
type UpdateSessionSelectionRequest struct {
	Agent string `json:"agent,omitempty"`
	Model string `json:"model,omitempty"`
}

type Message struct {
	ID           string          `json:"id"`
	SessionID    string          `json:"session_id"`
	Role         string          `json:"role"`
	Content      string          `json:"content"`
	Parts        json.RawMessage `json:"parts"`
	Status       string          `json:"status"`
	FinishReason string          `json:"finish_reason,omitempty"`
	Error        string          `json:"error,omitempty"`
	Usage        json.RawMessage `json:"usage"`
	InputID      string          `json:"input_id,omitempty"`
	Sequence     int64           `json:"sequence"`
	CreatedAt    time.Time       `json:"created_at"`
}

type MessageList struct {
	Items      []Message `json:"items"`
	NextCursor string    `json:"next_cursor,omitempty"`
}

type Todo struct {
	ID       string `json:"id"`
	Content  string `json:"content"`
	Status   string `json:"status"`
	Priority string `json:"priority"`
	Position int    `json:"position"`
}

type TodoList struct {
	Items []Todo `json:"items"`
}

type PromptRequest struct {
	MessageID string `json:"message_id"`
	Content   string `json:"content"`
	Delivery  string `json:"delivery"`
}

type PromptAccepted struct {
	InputID   string `json:"input_id"`
	MessageID string `json:"message_id"`
	Delivery  string `json:"delivery"`
	Status    string `json:"status"`
	Created   bool   `json:"created"`
}

type Compaction struct {
	Status        string `json:"status"`
	AttemptID     string `json:"attempt_id,omitempty"`
	RecordID      string `json:"record_id,omitempty"`
	SourceEpochID string `json:"source_epoch_id,omitempty"`
	TargetEpochID string `json:"target_epoch_id,omitempty"`
	HistoryCutoff int64  `json:"history_cutoff,omitempty"`
	Reason        string `json:"reason,omitempty"`
}

type PermissionResource struct {
	Kind       string            `json:"kind"`
	Identifier string            `json:"identifier"`
	Operation  string            `json:"operation"`
	Attributes map[string]string `json:"attributes,omitempty"`
}

type Permission struct {
	ID             string               `json:"id"`
	ToolID         string               `json:"tool_id"`
	CanonicalInput json.RawMessage      `json:"canonical_input"`
	Resources      []PermissionResource `json:"resources"`
	Review         json.RawMessage      `json:"review,omitempty"`
	OperationHash  string               `json:"operation_hash"`
	Reason         string               `json:"reason"`
}

type PermissionList struct {
	Items []Permission `json:"items"`
}

type PermissionReply struct {
	Decision string `json:"decision"`
	Scope    string `json:"scope,omitempty"`
}

type Option struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

type Question struct {
	ID       string   `json:"id"`
	Header   string   `json:"header,omitempty"`
	Prompt   string   `json:"prompt"`
	Options  []Option `json:"options,omitempty"`
	Multiple bool     `json:"multiple,omitempty"`
	Custom   bool     `json:"custom,omitempty"`
}

type QuestionRequest struct {
	ID        string     `json:"id"`
	Questions []Question `json:"questions"`
}

type QuestionList struct {
	Items []QuestionRequest `json:"items"`
}

type Answer struct {
	QuestionID string   `json:"question_id"`
	OptionIDs  []string `json:"option_ids,omitempty"`
	Custom     string   `json:"custom,omitempty"`
}

type QuestionReply struct {
	Answers []Answer `json:"answers,omitempty"`
	Reject  bool     `json:"reject,omitempty"`
}

type SnapshotTransaction struct {
	ID        string    `json:"id"`
	SessionID string    `json:"session_id"`
	Position  int       `json:"position"`
	CreatedAt time.Time `json:"created_at"`
	Paths     []string  `json:"paths"`
}

type Model struct {
	Provider        string   `json:"provider"`
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	ContextWindow   int      `json:"context_window"`
	MaxOutputTokens int      `json:"max_output_tokens"`
	Tools           bool     `json:"tools"`
	Reasoning       bool     `json:"reasoning"`
	Output          []string `json:"output"`
}

type ModelList struct {
	Items []Model `json:"items"`
}

type Agent struct {
	ID       string `json:"id"`
	ReadOnly bool   `json:"read_only"`
	MaxTurns int    `json:"max_turns"`
}

type AgentList struct {
	Items []Agent `json:"items"`
}

type Empty struct{}
