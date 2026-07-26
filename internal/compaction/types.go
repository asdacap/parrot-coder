// Package compaction plans and durably records bounded conversation compaction.
package compaction

import (
	"time"

	"github.com/amirulashraf/parrot-coder/internal/protocol"
	"github.com/amirulashraf/parrot-coder/internal/provider"
)

type CompactionEpoch struct {
	ID            string
	Ordinal       int
	SummaryPrompt string
	HistoryCutoff int64
}

type Message struct {
	ID       string
	Role     protocol.Role
	Content  string
	Parts    []protocol.ContentPart
	Status   string
	Usage    protocol.Usage
	Sequence int64
}

type State struct {
	Checkpoint CompactionEpoch
	Messages   []Message
}

type Plan struct {
	SourceEpochID string
	CoveredFrom   int64
	CoveredTo     int64
	HistoryCutoff int64
	Messages      []Message
	Estimate      Estimate
}

type Estimate struct {
	MeasuredTokens        int `json:"measured_tokens"`
	EstimatedTokens       int `json:"estimated_tokens"`
	ProviderContextTokens int `json:"provider_context_tokens"`
}

func (e Estimate) Total() int {
	return max(e.MeasuredTokens+e.EstimatedTokens, e.ProviderContextTokens)
}

type Request struct {
	SessionID    string
	ProviderID   string
	Model        provider.Model
	Instructions string
	Tools        []protocol.ToolDefinition
	Force        bool
}

type Result struct {
	Status        string `json:"status"`
	AttemptID     string `json:"attempt_id,omitempty"`
	RecordID      string `json:"record_id,omitempty"`
	SourceEpochID string `json:"source_epoch_id,omitempty"`
	TargetEpochID string `json:"target_epoch_id,omitempty"`
	HistoryCutoff int64  `json:"history_cutoff,omitempty"`
	Reason        string `json:"reason,omitempty"`
}

type Attempt struct {
	ID            string
	SessionID     string
	SourceEpochID string
	CoveredFrom   int64
	CoveredTo     int64
	HistoryCutoff int64
	ProviderID    string
	ModelID       string
	Forced        bool
	Status        string
	Error         string
	CreatedAt     time.Time
	FinishedAt    *time.Time
}

type Record struct {
	ID            string
	AttemptID     string
	SessionID     string
	SourceEpochID string
	TargetEpochID string
	CoveredFrom   int64
	CoveredTo     int64
	HistoryCutoff int64
	Summary       string
	Usage         protocol.Usage
	ProviderID    string
	ModelID       string
	CreatedAt     time.Time
}

type SummaryRequest struct {
	ProviderID string
	ModelID    string
	Prompt     string
	Messages   []protocol.Message
}

type SummaryResult struct {
	Summary string
	Usage   protocol.Usage
}
