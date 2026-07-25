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
	ID              string    `json:"id"`
	ParentSessionID string    `json:"parent_session_id,omitempty"`
	ProjectID       string    `json:"project_id,omitempty"`
	Title           string    `json:"title"`
	Agent           string    `json:"agent,omitempty"`
	Mode            string    `json:"mode,omitempty"`
	Provider        string    `json:"provider,omitempty"`
	Model           string    `json:"model,omitempty"`
	Variant         string    `json:"variant,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

type SessionList struct {
	Items      []Session `json:"items"`
	NextCursor string    `json:"next_cursor,omitempty"`
}

type CreateSessionRequest struct {
	ParentSessionID string  `json:"parent_session_id,omitempty"`
	ProjectID       string  `json:"project_id,omitempty"`
	Title           string  `json:"title,omitempty"`
	Agent           string  `json:"agent,omitempty"`
	Mode            string  `json:"mode,omitempty"`
	Model           string  `json:"model,omitempty"`
	Variant         *string `json:"variant,omitempty"`
}

type ClaimSessionRequest struct {
	WorkingDirectory string  `json:"working_directory"`
	HostKey          string  `json:"host_key"`
	PID              int     `json:"pid"`
	ProjectID        string  `json:"project_id,omitempty"`
	Title            string  `json:"title,omitempty"`
	Agent            string  `json:"agent,omitempty"`
	Mode             string  `json:"mode,omitempty"`
	Model            string  `json:"model,omitempty"`
	Variant          *string `json:"variant,omitempty"`
	ForceNew         bool    `json:"force_new,omitempty"`
}

type ClaimSessionResponse struct {
	Session     Session `json:"session"`
	Disposition string  `json:"disposition"`
}

// SessionSelection is the resolved execution selection persisted on a session.
type SessionSelection struct {
	Agent    string `json:"agent"`
	Mode     string `json:"mode"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Variant  string `json:"variant,omitempty"`
}

// UpdateSessionSelectionRequest changes either or both selection dimensions.
// Model accepts provider/model or a model ID under the current provider.
type UpdateSessionSelectionRequest struct {
	Agent   string  `json:"agent,omitempty"`
	Mode    string  `json:"mode,omitempty"`
	Model   string  `json:"model,omitempty"`
	Variant *string `json:"variant,omitempty"`
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

type Goal struct {
	ID              string    `json:"id"`
	SessionID       string    `json:"session_id"`
	Objective       string    `json:"objective"`
	Status          string    `json:"status"`
	TokenBudget     *int64    `json:"token_budget,omitempty"`
	TokensUsed      int64     `json:"tokens_used"`
	RemainingTokens *int64    `json:"remaining_tokens,omitempty"`
	ElapsedSeconds  int64     `json:"elapsed_seconds"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

type PutGoalRequest struct {
	Objective        *string `json:"objective,omitempty"`
	Status           *string `json:"status,omitempty"`
	TokenBudget      *int64  `json:"token_budget,omitempty"`
	ClearTokenBudget bool    `json:"clear_token_budget,omitempty"`
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

type Permission struct {
	ID             string          `json:"id"`
	ToolID         string          `json:"tool_id"`
	Description    string          `json:"description"`
	CanonicalInput json.RawMessage `json:"canonical_input"`
	Review         json.RawMessage `json:"review,omitempty"`
	// Choices are the answers the requesting tool offers. A client must reply
	// with one of them; the server rejects anything else.
	Choices []PermissionChoice `json:"choices,omitempty"`
}

type PermissionChoice struct {
	Value          string `json:"value"`
	Decision       string `json:"decision"`
	Label          string `json:"label"`
	Description    string `json:"description,omitempty"`
	RequiresReason bool   `json:"requires_reason,omitempty"`
}

type PermissionList struct {
	Items []Permission `json:"items"`
}

type PermissionReply struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason,omitempty"`
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

type Model struct {
	Provider        string         `json:"provider"`
	ID              string         `json:"id"`
	Name            string         `json:"name"`
	ContextWindow   int            `json:"context_window"`
	MaxOutputTokens int            `json:"max_output_tokens"`
	Tools           bool           `json:"tools"`
	Reasoning       bool           `json:"reasoning"`
	Output          []string       `json:"output"`
	Variants        []ModelVariant `json:"variants"`
}

type ModelVariant struct {
	Name            string `json:"name"`
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
}

type ModelList struct {
	Items []Model `json:"items"`
}

// SubscriptionUsage reports the subscription limits of the provider named in
// Provider. RemainingPercent is derived from upstream's used percentage and
// clamped to 0 through 100. A provider may report windows, credits, or both.
type SubscriptionUsage struct {
	Provider        string        `json:"provider"`
	PlanType        string        `json:"plan_type,omitempty"`
	PrimaryWindow   *UsageWindow  `json:"primary_window,omitempty"`
	SecondaryWindow *UsageWindow  `json:"secondary_window,omitempty"`
	Credits         *UsageCredits `json:"credits,omitempty"`
}

type UsageWindow struct {
	UsedPercent        float64   `json:"used_percent"`
	RemainingPercent   float64   `json:"remaining_percent"`
	ResetAt            time.Time `json:"reset_at"`
	LimitWindowSeconds int64     `json:"limit_window_seconds"`
}

type UsageCredits struct {
	HasCredits bool   `json:"has_credits"`
	Balance    string `json:"balance,omitempty"`
}

type Agent struct {
	ID       string `json:"id"`
	ReadOnly bool   `json:"read_only"`
	MaxTurns int    `json:"max_turns"`
}

type AgentList struct {
	Items []Agent `json:"items"`
}

type Mode struct {
	ID       string `json:"id"`
	ReadOnly bool   `json:"read_only"`
	MaxTurns int    `json:"max_turns"`
	// TurnComplete is the JSON-serialized mode.TurnCompleteResult that
	// declares the mode's turn-complete behavior. nil/empty means no special
	// action. It is opaque to the wire layer; the producer and consumer both
	// bind it to mode.TurnCompleteResult.
	TurnComplete json.RawMessage `json:"turn_complete,omitempty"`
}

type ModeList struct {
	Items []Mode `json:"items"`
}

type TurnCompletion struct {
	TurnComplete json.RawMessage `json:"turn_complete,omitempty"`
}

// ToolPresentation is display-only metadata a tool declares about itself, so
// that a renderer can branch on what a tool does rather than on its identity.
// It mirrors tool.Presentation; a client which does not recognise a field
// simply falls back to generic rendering.
type ToolPresentation struct {
	Label             ToolLabel          `json:"label,omitempty"`
	Redact            []string           `json:"redact,omitempty"`
	Muted             bool               `json:"muted,omitempty"`
	Result            string             `json:"result,omitempty"`
	Output            string             `json:"output,omitempty"`
	Failure           string             `json:"failure,omitempty"`
	Subagent          bool               `json:"subagent,omitempty"`
	Modeline          bool               `json:"modeline,omitempty"`
	LabelInPermission bool               `json:"label_in_permission,omitempty"`
	CompletedInput    ToolCompletedInput `json:"completed_input,omitempty"`
}

type ToolCompletedInput struct {
	Fields       []string `json:"fields,omitempty"`
	TerminalOnly bool     `json:"terminal_only,omitempty"`
}

type ToolLabel struct {
	Kind   string          `json:"kind,omitempty"`
	Fields []ToolLabelPart `json:"fields,omitempty"`
	Source []string        `json:"source,omitempty"`
	Prefix string          `json:"prefix,omitempty"`
	Noun   string          `json:"noun,omitempty"`
}

type ToolLabelPart struct {
	Names    []string `json:"names,omitempty"`
	Quote    bool     `json:"quote,omitempty"`
	Default  string   `json:"default,omitempty"`
	Array    bool     `json:"array,omitempty"`
	Item     []string `json:"item,omitempty"`
	Overflow bool     `json:"overflow,omitempty"`
	TaskName bool     `json:"task_name,omitempty"`
}

type Tool struct {
	ID           string           `json:"id"`
	Presentation ToolPresentation `json:"presentation"`
}

type ToolList struct {
	Items []Tool `json:"items"`
}

type Empty struct{}
