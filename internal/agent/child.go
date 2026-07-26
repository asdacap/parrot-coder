package agent

import (
	"context"
	"errors"
	"time"
)

var (
	ErrInvalidChildRequest = errors.New("agent: invalid child request")
	ErrChildConcurrency    = errors.New("agent: child concurrency limit reached")
	ErrChildDepth          = errors.New("agent: maximum child depth reached")
	ErrChildRecursion      = errors.New("agent: child agent recursion limit reached")
	ErrChildNotFound       = errors.New("agent: child task not found")
	ErrChildCanceled       = errors.New("agent: child task canceled")
	ErrChildTaskLimit      = errors.New("agent: retained child task limit reached")
	ErrChildRequestLimit   = errors.New("agent: child request limit reached")
	ErrChildRunning        = errors.New("agent: child agent is already running")
	ErrChildNotRunning     = errors.New("agent: child agent is not running")
	ErrUserSessionClosed   = errors.New("agent: user session is closed")
)

// ChildRequest describes one turn of a reusable child agent. It intentionally
// contains no parent permission grants; authorization is inherited from the
// parent AgentSession at execution time.
type ChildRequest struct {
	Prompt string
	Agent  string
	// Model is a canonical provider/model[/variant] selector. Empty inherits the
	// parent's complete selector.
	Model string
	Name  string
}

type ChildUsage struct {
	InputTokens       int `json:"input_tokens"`
	OutputTokens      int `json:"output_tokens"`
	TotalTokens       int `json:"total_tokens"`
	ReasoningTokens   int `json:"reasoning_tokens"`
	CachedInputTokens int `json:"cached_input_tokens"`
}

type ChildProgress struct {
	Usage    ChildUsage
	ToolUses int
}

type Status struct {
	SessionID      string
	ParentSession  string
	RootSession    string
	Agent          string
	Model          string
	Name           string
	Lineage        []string
	Depth          int
	Turn           int
	State          AgentStatus
	StartedAt      time.Time
	FinishedAt     time.Time
	Output         string
	Error          string
	NoFinalMessage bool
	Truncated      bool
	Usage          ChildUsage
	ToolUses       int
}

func turnActive(status AgentStatus) bool {
	return status == StatusBlocked || status == StatusPending || status == StatusRunning || status == StatusInterrupting
}

const (
	TurnLifecycleStart    = "start"
	TurnLifecycleWorking  = "working"
	TurnLifecycleIdle     = "idle"
	TurnLifecycleFinished = "finished"
)

type TurnLifecycleEvent struct {
	Kind   string
	Status Status
}

// ChildTurnObserver remains bound to the turn current when it was created.
type ChildTurnObserver interface {
	Wait(context.Context) (Status, error)
}
