package compaction

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/amirulashraf/parrot-coder/internal/diagnostics"
	"github.com/amirulashraf/parrot-coder/internal/protocol"
)

var ErrNoSafeCut = errors.New("compaction: no safe history cutoff")

type Store interface {
	Load(context.Context, string) (State, error)
	Completed(context.Context, string, string, int64, int64) (Record, bool, error)
	Begin(context.Context, Attempt) (Attempt, error)
	Complete(context.Context, Attempt, SummaryResult, FullContext) (Record, error)
	Fail(ctx context.Context, sessionID, attemptID, status, reason string) error
}

type Summarizer interface {
	Summarize(context.Context, SummaryRequest) (SummaryResult, error)
}

type ContextObserver interface {
	ObserveFull(context.Context) (FullContext, error)
}

type Config struct {
	ReserveTokens        int
	TriggerFraction      float64
	RecentMessages       int
	MaxSummaryInputBytes int
}

type Service struct {
	store      Store
	summarizer Summarizer
	contexts   ContextObserver
	config     Config
}

func NewService(store Store, summarizer Summarizer, contexts ContextObserver, config Config) (*Service, error) {
	if store == nil || summarizer == nil || contexts == nil {
		return nil, errors.New("compaction: store, summarizer, and context observer are required")
	}
	if config.TriggerFraction == 0 {
		config.TriggerFraction = .8
	}
	if config.TriggerFraction <= 0 || config.TriggerFraction > 1 || config.ReserveTokens < 0 {
		return nil, errors.New("compaction: invalid budget policy")
	}
	if config.RecentMessages <= 0 {
		config.RecentMessages = 8
	}
	if config.MaxSummaryInputBytes <= 0 {
		config.MaxSummaryInputBytes = 2 << 20
	}
	return &Service{store: store, summarizer: summarizer, contexts: contexts, config: config}, nil
}

func (s *Service) Compact(ctx context.Context, request Request) (result Result, err error) {
	started := time.Now()
	diagnostics.Event("compaction_started",
		"session_id", request.SessionID, "provider", request.ProviderID, "model", request.Model.ID, "forced", request.Force,
	)
	defer func() {
		status := result.Status
		if status == "" {
			status = "error"
		}
		attributes := []any{
			"session_id", request.SessionID, "provider", request.ProviderID, "model", request.Model.ID,
			"forced", request.Force, "status", status, "duration_ms", time.Since(started).Milliseconds(),
			"attempt_id", result.AttemptID, "source_epoch_id", result.SourceEpochID, "target_epoch_id", result.TargetEpochID,
		}
		if result.Reason != "" {
			attributes = append(attributes, "reason", result.Reason)
		}
		if err != nil {
			diagnostics.Error("compaction_finished", append(attributes, "error_type", diagnostics.ErrorType(err))...)
		} else {
			diagnostics.Event("compaction_finished", attributes...)
		}
	}()
	if request.SessionID == "" || request.ProviderID == "" || request.Model.ID == "" {
		return Result{}, errors.New("compaction: session, provider, and model are required")
	}
	state, err := s.store.Load(ctx, request.SessionID)
	if err != nil {
		return Result{}, err
	}
	plan, reason := s.plan(state, request)
	if reason != "" {
		return Result{Status: "skipped", SourceEpochID: state.Epoch.ID, Reason: reason}, nil
	}
	if existing, ok, err := s.store.Completed(ctx, request.SessionID, plan.SourceEpochID, plan.CoveredFrom, plan.CoveredTo); err != nil {
		return Result{}, err
	} else if ok {
		return completedResult(existing), nil
	}
	attempt := Attempt{SessionID: request.SessionID, SourceEpochID: plan.SourceEpochID, CoveredFrom: plan.CoveredFrom, CoveredTo: plan.CoveredTo, HistoryCutoff: plan.HistoryCutoff, ProviderID: request.ProviderID, ModelID: request.Model.ID, Forced: request.Force, Status: "active"}
	attempt, err = s.store.Begin(ctx, attempt)
	if err != nil {
		return Result{}, err
	}
	if attempt.ID == "" {
		return Result{}, errors.New("compaction: store did not assign attempt ID")
	}
	fail := func(cause error) (Result, error) {
		status := "failed"
		if errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) || ctx.Err() != nil {
			status = "interrupted"
		}
		cleanupCtx := ctx
		if cleanupCtx.Err() != nil {
			cleanupCtx = context.Background()
		}
		markErr := s.store.Fail(cleanupCtx, attempt.SessionID, attempt.ID, status, cause.Error())
		return Result{Status: status, AttemptID: attempt.ID, SourceEpochID: attempt.SourceEpochID}, errors.Join(cause, markErr)
	}

	prompt, messages, err := summaryInput(state.Epoch, plan, s.config.MaxSummaryInputBytes)
	if err != nil {
		return fail(err)
	}
	summary, err := s.summarizer.Summarize(ctx, SummaryRequest{ProviderID: request.ProviderID, ModelID: request.Model.ID, Prompt: prompt, Messages: messages})
	if err != nil {
		return fail(err)
	}
	if strings.TrimSpace(summary.Summary) == "" {
		return fail(errors.New("compaction: summarizer returned an empty summary"))
	}
	fresh, err := s.contexts.ObserveFull(ctx)
	if err != nil {
		return fail(fmt.Errorf("compaction: observe full context: %w", err))
	}
	if !json.Valid(fresh.Sources) {
		return fail(errors.New("compaction: observer returned invalid sources"))
	}
	fresh.Baseline = composeBaseline(fresh.Baseline, summary.Summary, attempt)
	record, err := s.store.Complete(ctx, attempt, summary, fresh)
	if err != nil {
		if existing, ok, lookupErr := s.store.Completed(context.Background(), request.SessionID, plan.SourceEpochID, plan.CoveredFrom, plan.CoveredTo); lookupErr == nil && ok {
			_ = s.store.Fail(context.Background(), attempt.SessionID, attempt.ID, "failed", "superseded by an idempotent completed compaction")
			return completedResult(existing), nil
		}
		return fail(err)
	}
	return completedResult(record), nil
}

func (s *Service) plan(state State, request Request) (Plan, string) {
	estimate := EstimateRequest(state.Epoch.Baseline+request.Instructions, state.Messages, request.Tools)
	usable := request.Model.ContextWindow - request.Model.MaxOutputTokens - s.config.ReserveTokens
	if usable <= 0 {
		if !request.Force {
			return Plan{}, "model has no usable input budget"
		}
	} else {
		trigger := int(float64(usable) * s.config.TriggerFraction)
		if !request.Force && estimate.Total() < trigger {
			return Plan{}, "under budget trigger"
		}
	}
	cut, ok := SafeCut(state.Messages, s.config.RecentMessages)
	// A forced compaction follows a provider-confirmed overflow (or an explicit
	// user request), so it must prioritize making progress over the normal
	// recent-history preference. Do not gate this fallback on our token estimate:
	// the provider has already proved that estimate can be too low. This also
	// applies after an earlier compaction, once enough new history has accumulated.
	// SafeCut still preserves active messages and complete tool-call groups.
	if !ok && request.Force && s.config.RecentMessages > 1 && len(state.Messages) > 2 {
		cut, ok = SafeCut(state.Messages, 1)
	}
	if !ok {
		return Plan{}, ErrNoSafeCut.Error()
	}
	covered := make([]Message, 0, len(state.Messages))
	for _, message := range state.Messages {
		if message.Sequence >= cut {
			break
		}
		covered = append(covered, message)
	}
	if len(covered) == 0 {
		return Plan{}, ErrNoSafeCut.Error()
	}
	return Plan{SourceEpochID: state.Epoch.ID, CoveredFrom: state.Epoch.HistoryCutoff, CoveredTo: covered[len(covered)-1].Sequence, HistoryCutoff: cut, Messages: covered, Estimate: estimate}, ""
}

// HeuristicTextTokens is deterministic and intentionally conservative. It is
// an accounting heuristic based on UTF-8 bytes and runes, not a tokenizer.
func HeuristicTextTokens(text string) int {
	if text == "" {
		return 0
	}
	bytes := len(text)
	runes := utf8.RuneCountInString(text)
	return (bytes+2)/3 + (runes+3)/4
}

func EstimateRequest(instructions string, messages []Message, tools []protocol.ToolDefinition) Estimate {
	estimate := Estimate{HeuristicTokens: HeuristicTextTokens(instructions) + 4}
	providerContext, suffixTokens := 0, 0
	for _, message := range messages {
		raw, _ := json.Marshal(message.Parts)
		heuristic := HeuristicTextTokens(message.Content) + HeuristicTextTokens(string(raw)) + 4
		measured := 0
		if message.Role == protocol.RoleAssistant {
			measured = message.Usage.OutputTokens
			if measured == 0 {
				measured = message.Usage.TotalTokens
			}
			contextTokens := message.Usage.InputTokens
			if contextTokens == 0 {
				contextTokens = message.Usage.TotalTokens
			}
			if contextTokens > 0 {
				providerContext, suffixTokens = contextTokens, 0
			}
		}
		if measured > 0 {
			estimate.MeasuredTokens += measured
		} else {
			estimate.HeuristicTokens += heuristic
		}
		if providerContext > 0 {
			suffixTokens += heuristic
		}
	}
	estimate.ProviderContextTokens = providerContext + suffixTokens
	for _, definition := range tools {
		estimate.HeuristicTokens += HeuristicTextTokens(definition.Name) + HeuristicTextTokens(definition.Description) + HeuristicTextTokens(string(definition.InputSchema)) + 8
	}
	return estimate
}

// SafeCut returns the sequence of the first retained message. It retains at
// least recent complete messages, backs up over tool-call groups, and never
// places uncertain terminal or active work in the compacted prefix.
func SafeCut(messages []Message, recent int) (int64, bool) {
	if recent < 1 {
		return 0, false
	}
	index, complete := len(messages), 0
	for index > 0 && complete < recent {
		index--
		if messages[index].Status == "complete" {
			complete++
		}
	}
	if complete < recent || index == 0 {
		return 0, false
	}
	for i := 0; i < index; i++ {
		if messages[i].Status != "complete" {
			index = i
			break
		}
	}
	callStart := make(map[string]int)
	callEnd := make(map[string]int)
	for i, message := range messages {
		for _, part := range message.Parts {
			if part.Type == protocol.ContentToolCall && part.ToolCall != nil {
				callStart[part.ToolCall.ID] = i
				callEnd[part.ToolCall.ID] = -1
			}
			if part.Type == protocol.ContentToolResult && part.ToolCallID != "" {
				if _, exists := callStart[part.ToolCallID]; exists {
					callEnd[part.ToolCallID] = i
				}
			}
		}
	}
	for id, start := range callStart {
		end := callEnd[id]
		if end < 0 {
			if start < index {
				index = start
			}
			continue
		}
		if start < index && index <= end {
			index = start
		}
	}
	if index <= 0 || index >= len(messages) {
		return 0, false
	}
	return messages[index].Sequence, true
}

func summaryInput(epoch Epoch, plan Plan, maxBytes int) (string, []protocol.Message, error) {
	const instructions = "Summarize the covered session history for continuation. Preserve user intent, decisions and rationale, modified files, commands and tests run, unresolved errors, current todo state, and current permission state. Do not include credentials, tokens, secrets, or secret values. Be concise and factual."
	prompt := instructions + fmt.Sprintf("\n\nSource epoch: %s. Covered event sequences: %d-%d. Prior epoch baseline and any prior compacted summary follow:\n\n%s", epoch.ID, plan.CoveredFrom, plan.CoveredTo, epoch.Baseline)
	messages := make([]protocol.Message, 0, len(plan.Messages))
	total := len(prompt)
	for _, item := range plan.Messages {
		parts := append([]protocol.ContentPart(nil), item.Parts...)
		if len(parts) == 0 && item.Content != "" {
			parts = []protocol.ContentPart{{Type: protocol.ContentText, Text: item.Content}}
		}
		raw, _ := json.Marshal(parts)
		total += len(raw) + len(item.Role) + 16
		if total > maxBytes {
			return "", nil, errors.New("compaction: summary input exceeds configured bound")
		}
		messages = append(messages, protocol.Message{Role: item.Role, Content: parts})
	}
	return prompt, messages, nil
}

func composeBaseline(observed, summary string, attempt Attempt) string {
	const begin = "----- BEGIN COMPACTED SESSION HISTORY -----"
	const end = "----- END COMPACTED SESSION HISTORY -----"
	section := fmt.Sprintf("%s\nSource epoch: %s\nCovered sequences: %d-%d\nHistory cutoff: %d\n\n%s\n%s", begin, attempt.SourceEpochID, attempt.CoveredFrom, attempt.CoveredTo, attempt.HistoryCutoff, strings.TrimSpace(summary), end)
	if strings.TrimSpace(observed) == "" {
		return section
	}
	return strings.TrimSpace(observed) + "\n\n" + section
}

func completedResult(record Record) Result {
	return Result{Status: "complete", AttemptID: record.AttemptID, RecordID: record.ID, SourceEpochID: record.SourceEpochID, TargetEpochID: record.TargetEpochID, HistoryCutoff: record.HistoryCutoff}
}
