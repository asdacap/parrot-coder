package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"runtime/debug"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/amirulashraf/parrot-coder/internal/compaction"
	"github.com/amirulashraf/parrot-coder/internal/diagnostics"
	"github.com/amirulashraf/parrot-coder/internal/process"
	"github.com/amirulashraf/parrot-coder/internal/protocol"
	"github.com/amirulashraf/parrot-coder/internal/provider"
	"github.com/amirulashraf/parrot-coder/internal/security"
	"github.com/amirulashraf/parrot-coder/internal/session"
	statusinfo "github.com/amirulashraf/parrot-coder/internal/status"
	"github.com/amirulashraf/parrot-coder/internal/tool"
	"github.com/amirulashraf/parrot-coder/internal/workspace"
)

type SessionRuntime interface {
	Get(context.Context, string) (session.AgentSessionDto, error)
	List(context.Context) ([]session.AgentSessionDto, error)
	CreateSelected(context.Context, session.CreateParams, session.Selection) (session.AgentSessionDto, error)
	Delete(context.Context, string) error
	Admit(context.Context, string, session.AdmitParams) (session.Admission, error)
	ListMessages(context.Context, string) ([]session.Message, error)
	LatestSequence(context.Context, string) (int64, error)
	CurrentContextEpoch(context.Context, string) (session.ContextEpoch, error)
	PromoteSteers(context.Context, string, int64) ([]session.Message, error)
	PromoteNextQueue(context.Context, string) ([]session.Message, error)
	ListModelHistory(context.Context, string, int64) ([]protocol.Message, error)
	StartAssistant(context.Context, string) (session.Message, error)
	FinishAssistant(context.Context, string, string, session.AssistantFinal) error
	AddToolCall(context.Context, string, string, protocol.ToolCall) (session.ToolCall, error)
	StartTool(context.Context, string, string) error
	SettleTool(context.Context, string, string, string, string, string) error
	AppendMessage(context.Context, string, protocol.Message) (session.Message, error)
	AppendStatusPrompt(context.Context, string, string) (session.Message, error)
	RepairActive(context.Context, string) error
	StatusPromptPending(context.Context, string) (bool, error)
}

type StatusObserver interface {
	Observe(context.Context, statusinfo.Query, statusinfo.Provider) (string, error)
}

type ContextRuntime interface {
	Initialize(context.Context, string, int64) (session.ContextEpoch, error)
	Reconcile(context.Context, string) (session.ContextEpoch, error)
}

type LivePublisher interface {
	Publish(string, protocol.Event)
}

type toolOutputWriter struct {
	live      LivePublisher
	sessionID string
	callID    string
	mu        sync.Mutex
	pending   []byte
}

func (w *toolOutputWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pending = append(w.pending, p...)
	complete := len(w.pending)
	if complete > 0 {
		start := complete - 1
		for start > 0 && !utf8.RuneStart(w.pending[start]) {
			start--
		}
		if !utf8.FullRune(w.pending[start:]) {
			complete = start
		}
	}
	if complete > 0 && w.live != nil {
		w.live.Publish(w.sessionID, protocol.Event{Type: protocol.EventToolOutputDelta, ToolCallID: w.callID, Text: string(w.pending[:complete])})
	}
	w.pending = append(w.pending[:0], w.pending[complete:]...)
	return len(p), nil
}

type Compactor interface {
	Compact(context.Context, compaction.Request) (compaction.Result, error)
}

type ProfileResolver interface {
	GetProfile(string) (Profile, error)
}

type AgentSessionConfig struct {
	Sessions           SessionRuntime
	Contexts           ContextRuntime
	StateDirectories   UserSessionStateDirectories
	Agents             *Registry
	Profiles           ProfileResolver
	Providers          ProviderResolver
	ToolSnapshot       func() tool.Snapshot
	ToolExecutor       func(tool.Snapshot) tool.Executor
	Workspace          *workspace.Workspace
	Outputs            *tool.OutputStore
	Processes          *process.Runner
	TaskIDFor          func(string) string
	Live               LivePublisher
	Compactor          Compactor
	Goals              *session.GoalService
	Status             StatusObserver
	MaxConcurrentTools int
	CleanupTimeout     time.Duration
	// ToolPanicLogger, when set, receives diagnostics for a tool call whose
	// Plan or Execute panicked and was recovered into a failure. It is purely
	// an observability seam: the panic is always reported to the model as a
	// tool failure whether or not a logger is configured.
	ToolPanicLogger func(ctx context.Context, sessionID, toolName string, recovered any, stack []byte)
}

type agentSession struct {
	dto             session.AgentSessionDto
	parent          AgentSession
	user            *userSession
	config          AgentSessionConfig
	securityProfile *agentSessionSecurityProfile
	mu              sync.Mutex
	childOp         sync.Mutex
	child           *childState
	drain           *drainState
	childCreations  int
	removed         bool
	childTurns      childTurnSemaphore
	observers       []LifecycleObserver
	execute         func(context.Context) error
}

type agentSessionSecurityProfile struct {
	readOnly     bool
	baseRules    []security.Rule
	capabilities []security.Rule
}

func newAgentSessionSecurityProfile(profile security.SecurityProfile) *agentSessionSecurityProfile {
	session := &agentSessionSecurityProfile{readOnly: profile.IsReadOnly(), baseRules: append([]security.Rule(nil), profile.Rules()...)}
	if layered, ok := profile.(security.LayeredSecurityProfile); ok {
		session.baseRules = layered.BaseRules()
		session.capabilities = layered.CapabilityRules()
	}
	return session
}

func (p *agentSessionSecurityProfile) IsReadOnly() bool { return p.readOnly }
func (p *agentSessionSecurityProfile) Rules() []security.Rule {
	return append(p.BaseRules(), p.capabilities...)
}
func (p *agentSessionSecurityProfile) BaseRules() []security.Rule {
	return append([]security.Rule(nil), p.baseRules...)
}
func (p *agentSessionSecurityProfile) CapabilityRules() []security.Rule {
	return append([]security.Rule(nil), p.capabilities...)
}
func (p *agentSessionSecurityProfile) AddCapability(rule security.Rule) {
	p.capabilities = append(p.capabilities, rule)
}

func validateAgentSessionConfig(config *AgentSessionConfig) error {
	if config.Profiles == nil {
		config.Profiles = config.Agents
	}
	if config.Sessions == nil || config.StateDirectories == nil || config.Profiles == nil || config.Providers == nil || config.ToolSnapshot == nil || config.ToolExecutor == nil {
		return errors.New("agent: session dependencies are required")
	}
	if config.MaxConcurrentTools <= 0 {
		config.MaxConcurrentTools = 4
	}
	if config.CleanupTimeout <= 0 {
		config.CleanupTimeout = 5 * time.Second
	}
	return nil
}

func (r *agentSession) drainOnce(ctx context.Context) (runErr error) {
	stateDirectory, err := r.config.StateDirectories.Prepare(r.dto.ID)
	if err != nil {
		return err
	}
	scratchPath := stateDirectory.ScratchPath()
	defer func() {
		if runErr == nil || ctx.Err() != nil || r.config.Goals == nil || !provider.IsUsageLimitError(runErr) {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), r.config.CleanupTimeout)
		defer cancel()
		if _, _, err := r.config.Goals.MarkUsageLimited(cleanupCtx, r.dto.ID); err != nil && !errors.Is(err, session.ErrGoalNotFound) {
			runErr = errors.Join(runErr, err)
		}
	}()
	if err := r.config.Sessions.RepairActive(ctx, r.dto.ID); err != nil {
		return err
	}
	turn := 0
	var profile Profile
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		cutoff, err := r.config.Sessions.LatestSequence(ctx, r.dto.ID)
		if err != nil {
			return err
		}
		epoch, epochErr := r.config.Sessions.CurrentContextEpoch(ctx, r.dto.ID)
		initial := errors.Is(epochErr, session.ErrNotFound)
		if epochErr != nil && !initial {
			return epochErr
		}
		if initial {
			if r.config.Contexts == nil {
				return errors.New("agent: context runtime is required for initialization")
			}
			epoch, err = r.config.Contexts.Initialize(ctx, r.dto.ID, cutoff)
			if err != nil {
				return err
			}
		}
		if !initial && r.config.Contexts != nil {
			reconciled, reconcileErr := r.config.Contexts.Reconcile(ctx, r.dto.ID)
			if reconcileErr != nil && reconciled.ID == "" {
				return reconcileErr
			}
			if reconciled.ID != "" {
				epoch = reconciled
			}
		}
		promoted, err := r.config.Sessions.PromoteSteers(ctx, r.dto.ID, cutoff)
		if err != nil {
			return err
		}

		selected, err := r.config.Sessions.Get(ctx, r.dto.ID)
		if err != nil {
			return err
		}
		if turn == 0 {
			var turnProfile TurnProfile
			if preparer, ok := r.config.Profiles.(interface {
				PrepareTurn(string, string) (TurnProfile, error)
			}); ok {
				turnProfile, err = preparer.PrepareTurn(selected.Agent, r.dto.ID)
			} else {
				profile, err = r.config.Profiles.GetProfile(selected.Agent)
				turnProfile = NewTurnProfile(profile)
			}
			if err != nil {
				return err
			}
			profile = turnProfile.Profile()
			r.securityProfile = newAgentSessionSecurityProfile(turnProfile)
			r.securityProfile.AddCapability(security.Rule{Path: scratchPath, Action: security.ActionAllowWrite})
		}
		providerClient, model, err := r.config.Providers.Resolve(selected.Provider, selected.Model)
		if err != nil {
			return err
		}
		history, err := r.config.Sessions.ListModelHistory(ctx, r.dto.ID, epoch.HistoryCutoff)
		if err != nil {
			return err
		}
		ready := len(promoted) > 0
		// ReconcileContext may append a "system" message after the last user or
		// tool message. A system message is context metadata, not a turn that
		// waits for a response, so it must not gate the continuation: scan back
		// over trailing system messages to find the last meaningful role.
		for i := len(history) - 1; i >= 0 && !ready; i-- {
			role := history[i].Role
			if role == protocol.RoleSystem {
				continue
			}
			ready = role == protocol.RoleUser || role == protocol.RoleTool
			break
		}
		if !ready {
			queued, err := r.config.Sessions.PromoteNextQueue(ctx, r.dto.ID)
			if err != nil || len(queued) == 0 {
				return err
			}
			history, err = r.config.Sessions.ListModelHistory(ctx, r.dto.ID, epoch.HistoryCutoff)
			if err != nil {
				return err
			}
		}
		if turn == 0 && r.config.Status != nil {
			pending, err := r.config.Sessions.StatusPromptPending(ctx, r.dto.ID)
			if err != nil {
				return err
			}
			if pending {
				statusPrompt, err := r.config.Status.Observe(ctx, r.statusQuery(ctx, selected, profile), newProfileStatus(profile))
				if err != nil {
					return err
				}
				if strings.TrimSpace(statusPrompt) != "" {
					if _, err := r.config.Sessions.AppendStatusPrompt(ctx, r.dto.ID, statusPrompt); err != nil {
						return err
					}
					r.publishStatusPromptInjected()
					history, err = r.config.Sessions.ListModelHistory(ctx, r.dto.ID, epoch.HistoryCutoff)
					if err != nil {
						return err
					}
				}
			}
		}

		snapshot := r.config.ToolSnapshot()
		definitions := toolDefinitions(snapshot)
		turn++
		instructions := runnerInstructions(epoch.Baseline, scratchPath, turn >= profile.MaxTurns)
		if turn >= profile.MaxTurns {
			definitions = nil
		}
		if r.config.Compactor != nil {
			result, compactErr := r.config.Compactor.Compact(ctx, compaction.Request{
				SessionID: r.dto.ID, ProviderID: selected.Provider, Model: model,
				Instructions: finalTurnInstructions(turn >= profile.MaxTurns), Tools: definitions,
			})
			if compactErr != nil {
				return compactErr
			}
			if result.Status == "complete" {
				epoch, err = r.config.Sessions.CurrentContextEpoch(ctx, r.dto.ID)
				if err != nil {
					return err
				}
				history, err = r.config.Sessions.ListModelHistory(ctx, r.dto.ID, epoch.HistoryCutoff)
				if err != nil {
					return err
				}
				instructions = runnerInstructions(epoch.Baseline, scratchPath, turn >= profile.MaxTurns)
			}
		}
		request := protocol.Request{Model: model.ID, Instructions: instructions, Messages: history, Tools: definitions}
		if selected.Variant != "" {
			variant, ok := model.Capabilities.Variant(selected.Variant)
			if !ok {
				return fmt.Errorf("agent: unknown model variant %q", selected.Variant)
			}
			request.Reasoning = &protocol.ReasoningOptions{Effort: variant.ReasoningEffort, Summary: "auto"}
		}
		calls, finish, err := r.loggedProviderTurn(ctx, selected.Provider, turn, providerClient, model, request)
		if err != nil {
			var failure *providerTurnFailure
			if !errors.As(err, &failure) || !failure.overflow || !failure.retrySafe || r.config.Compactor == nil {
				return err
			}
			result, compactErr := r.config.Compactor.Compact(ctx, compaction.Request{
				SessionID: r.dto.ID, ProviderID: selected.Provider, Model: model,
				Instructions: finalTurnInstructions(turn >= profile.MaxTurns), Tools: definitions, Force: true,
			})
			if compactErr != nil || result.Status != "complete" {
				if compactErr != nil {
					return errors.Join(err, compactErr)
				}
				return err
			}
			recorder, ok := r.config.Sessions.(interface {
				RecordCompactionRetry(context.Context, string, string, string) error
			})
			if !ok {
				return errors.New("agent: session runtime cannot record compaction retry")
			}
			if recordErr := recorder.RecordCompactionRetry(ctx, r.dto.ID, failure.code, result.RecordID); recordErr != nil {
				return recordErr
			}
			epoch, err = r.config.Sessions.CurrentContextEpoch(ctx, r.dto.ID)
			if err != nil {
				return err
			}
			history, err = r.config.Sessions.ListModelHistory(ctx, r.dto.ID, epoch.HistoryCutoff)
			if err != nil {
				return err
			}
			request.Instructions = runnerInstructions(epoch.Baseline, scratchPath, turn >= profile.MaxTurns)
			request.Messages = history
			calls, finish, err = r.loggedProviderTurn(ctx, selected.Provider, turn, providerClient, model, request)
			if err != nil {
				return err
			}
		}
		if len(calls) > 0 {
			if turn >= profile.MaxTurns {
				return errors.New("agent: provider returned tools after max-turn tool omission")
			}
			if err := r.executeTools(ctx, selected, profile, snapshot, calls); err != nil {
				return err
			}
			if r.config.Goals != nil {
				goal, err := r.config.Goals.Get(ctx, r.dto.ID)
				if errors.Is(err, session.ErrGoalNotFound) {
					continue
				}
				if err != nil {
					return err
				}
				if goal.Status != session.GoalActive {
					return nil
				}
			}
			continue
		}
		if finish == protocol.FinishStop || finish == protocol.FinishLength || finish == protocol.FinishContentFilter || finish == protocol.FinishIncomplete {
			queued, err := r.config.Sessions.PromoteNextQueue(ctx, r.dto.ID)
			if err != nil {
				return err
			}
			if len(queued) == 0 {
				return nil
			}
			continue
		}
		return nil
	}
}

func (r *agentSession) statusQuery(ctx context.Context, selected session.AgentSessionDto, profile Profile) statusinfo.Query {
	query := statusinfo.Query{
		SessionID:       r.dto.ID,
		ParentSessionID: selected.ParentSessionID,
		Agent:           profile.ID,
		Provider:        selected.Provider,
		Model:           selected.Model,
		Variant:         selected.Variant,
	}
	if selected.ParentSessionID != "" {
		if parent, err := r.config.Sessions.Get(ctx, selected.ParentSessionID); err == nil {
			query.ParentSessionName = parent.Name
			query.ParentAgent = parent.Agent
		}
	}
	return query
}

func (r *agentSession) publishStatusPromptInjected() {
	if r.config.Live != nil {
		r.config.Live.Publish(r.dto.ID, protocol.Event{Type: protocol.EventStatusPromptInjected, Text: "Status prompt injected"})
	}
}

// PrepareContinuation persists a new synthetic user turn when an active goal
// remains after a successful drain. The coordinator invokes this only while
// the session is otherwise idle, so each continuation is a normal new turn.
func (r *agentSession) prepareContinuation(ctx context.Context) (bool, error) {
	if r.config.Goals == nil {
		return false, nil
	}
	goal, active, err := r.config.Goals.Active(ctx, r.dto.ID)
	if err != nil || !active {
		return false, err
	}
	selected, err := r.config.Sessions.Get(ctx, r.dto.ID)
	if err != nil {
		return false, err
	}
	profile, err := r.config.Profiles.GetProfile(selected.Agent)
	if err != nil {
		return false, err
	}
	// A read-only profile cannot update goals, so it also
	// cannot participate in goal continuation.
	if profile.IsReadOnly() {
		return false, nil
	}
	remaining := "unlimited"
	if value := goal.RemainingTokens(); value != nil {
		remaining = fmt.Sprintf("%d", *value)
	}
	message := fmt.Sprintf("Continue working autonomously toward the active goal: %s\n\nThis is an automatic goal continuation turn. Remaining token budget: %s. Use get_goal to inspect current state. Mark the goal complete only when achieved; mark it blocked only for a genuine recurring blocker.", goal.Objective, remaining)
	_, err = r.config.Sessions.AppendMessage(ctx, r.dto.ID, protocol.Message{Role: protocol.RoleUser, Content: []protocol.ContentPart{{Type: protocol.ContentText, Text: message}}})
	return err == nil, err
}

type completedCall struct {
	messageID string
	call      protocol.ToolCall
}

type providerTurnFailure struct {
	err       error
	code      string
	overflow  bool
	retrySafe bool
}

func (e *providerTurnFailure) Error() string { return e.err.Error() }
func (e *providerTurnFailure) Unwrap() error { return e.err }

func (r *agentSession) loggedProviderTurn(ctx context.Context, providerID string, turn int, client provider.Provider, model provider.Model, request protocol.Request) ([]completedCall, protocol.FinishReason, error) {
	started := time.Now()
	diagnostics.Event("provider_turn_started",
		"session_id", r.dto.ID, "provider", providerID, "model", request.Model, "turn", turn,
		"message_count", len(request.Messages), "tool_count", len(request.Tools),
	)
	calls, finish, err := r.providerTurn(ctx, client, model, request)
	attributes := []any{
		"session_id", r.dto.ID, "provider", providerID, "model", request.Model, "turn", turn,
		"finish_reason", finish, "tool_call_count", len(calls), "duration_ms", time.Since(started).Milliseconds(),
	}
	if err != nil {
		diagnostics.Error("provider_turn_finished", append(attributes, "status", "error", "error_type", diagnostics.ErrorType(err))...)
	} else {
		diagnostics.Event("provider_turn_finished", append(attributes, "status", "success")...)
	}
	return calls, finish, err
}

func (r *agentSession) providerTurn(ctx context.Context, client provider.Provider, model provider.Model, request protocol.Request) ([]completedCall, protocol.FinishReason, error) {
	assistant, err := r.config.Sessions.StartAssistant(ctx, r.dto.ID)
	if err != nil {
		return nil, "", err
	}
	stream, err := provider.StreamWithRetry(ctx, client, request, func(notice provider.RetryNotice) {
		if r.config.Live != nil {
			r.config.Live.Publish(r.dto.ID, protocol.Event{Type: protocol.EventProviderRetry, Text: notice.String()})
		}
	})
	if err != nil {
		finishErr := r.finishOnCleanup(assistant.ID, nil, protocol.FinishError, err.Error(), "error")
		code, overflow := contextOverflowError(err)
		return nil, "", &providerTurnFailure{err: errors.Join(err, finishErr), code: code, overflow: overflow, retrySafe: finishErr == nil}
	}
	defer stream.Close()
	var text, reasoning strings.Builder
	var reasoningSummary reasoningSummaryAccumulator
	var usage protocol.Usage
	var calls []completedCall
	finish := protocol.FinishIncomplete
	for {
		item, nextErr := stream.Next(ctx)
		if nextErr != nil {
			if errors.Is(nextErr, io.EOF) {
				break
			}
			status := "error"
			if ctx.Err() != nil {
				status = "interrupted"
			}
			// A call is only part of valid model history once the provider turn
			// completes and AddToolCall has made it executable. Keeping calls from
			// an interrupted or failed stream would replay a function_call without
			// a matching function_call_output on the next request.
			parts := finalParts(text.String(), preferredReasoning(reasoning.String(), reasoningSummary.String()), nil)
			_ = r.finishOnCleanup(assistant.ID, parts, protocol.FinishError, nextErr.Error(), status)
			return nil, "", nextErr
		}
		// Usage must be priced before it is published: the live event is the only
		// carrier of cost to clients, so pricing a copy after the fact leaves every
		// subscriber with a zero cost.
		if item.Type == protocol.EventUsage && item.Usage != nil {
			item.Usage.InputCost = float64(item.Usage.InputTokens) * model.InputPrice
			item.Usage.OutputCost = float64(item.Usage.OutputTokens) * model.OutputPrice
		}
		if r.config.Live != nil {
			item.MessageID = assistant.ID
			r.config.Live.Publish(r.dto.ID, item)
		}
		switch item.Type {
		case protocol.EventTextDelta:
			text.WriteString(item.Text)
		case protocol.EventReasoningDelta:
			reasoning.WriteString(item.Text)
		case protocol.EventReasoningSummaryDelta:
			reasoningSummary.Write(item.PartID, item.Text)
		case protocol.EventReasoningSummaryDone:
			if item.Text != "" {
				reasoningSummary.Set(item.PartID, item.Text)
			}
		case protocol.EventToolCallComplete:
			if item.ToolCall == nil {
				continue
			}
			calls = append(calls, completedCall{assistant.ID, *item.ToolCall})
		case protocol.EventUsage:
			if item.Usage != nil {
				usage = *item.Usage
			}
		case protocol.EventFinish:
			finish = item.FinishReason
		case protocol.EventProviderError:
			message := "provider error"
			kind := ""
			code := ""
			if item.ProviderError != nil {
				message = item.ProviderError.Message
				kind = item.ProviderError.Type
				code = item.ProviderError.Code
			}
			// Tool calls from a failed provider turn are not executed, so they must
			// not be retained without corresponding tool results.
			parts := finalParts(text.String(), preferredReasoning(reasoning.String(), reasoningSummary.String()), nil)
			finishErr := r.finishAssistantOnCleanup(assistant.ID, session.AssistantFinal{Parts: parts, Usage: usage, FinishReason: protocol.FinishError, Error: message, Status: "error"})
			overflow := item.ProviderError != nil && (canonicalOverflow(item.ProviderError.Type, item.ProviderError.Code) || overflowMessage(item.ProviderError.Message))
			responseErr := &provider.ResponseError{Type: kind, Code: code, Message: message}
			return nil, protocol.FinishError, &providerTurnFailure{err: errors.Join(responseErr, finishErr), code: code, overflow: overflow, retrySafe: finishErr == nil && text.Len() == 0 && reasoning.Len() == 0 && reasoningSummary.Len() == 0 && len(calls) == 0}
		}
	}
	parts := finalParts(text.String(), preferredReasoning(reasoning.String(), reasoningSummary.String()), calls)
	final := session.AssistantFinal{Parts: parts, Usage: usage, FinishReason: finish, Status: "complete"}
	if err := r.config.Sessions.FinishAssistant(ctx, r.dto.ID, assistant.ID, final); err != nil {
		if ctx.Err() != nil {
			final.Status = "interrupted"
			final.FinishReason = protocol.FinishError
			final.Error = ctx.Err().Error()
			if cleanupErr := r.finishAssistantOnCleanup(assistant.ID, final); cleanupErr != nil {
				return nil, finish, errors.Join(err, cleanupErr)
			}
		}
		return nil, finish, err
	}
	if r.config.Goals != nil {
		if _, err := r.config.Goals.AccountUsage(ctx, r.dto.ID, usage); err != nil && !errors.Is(err, session.ErrGoalNotFound) {
			return nil, finish, err
		}
	}
	for _, call := range calls {
		if _, err := r.config.Sessions.AddToolCall(ctx, r.dto.ID, assistant.ID, call.call); err != nil {
			return nil, finish, err
		}
	}
	return calls, finish, nil
}

func contextOverflowError(err error) (string, bool) {
	var httpErr *provider.HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != 400 && httpErr.StatusCode != 413 {
		return "", false
	}
	return httpErr.Code, canonicalOverflow(httpErr.Type, httpErr.Code) || overflowMessage(httpErr.Message)
}

func canonicalOverflow(kind, code string) bool {
	for _, value := range []string{strings.ToLower(kind), strings.ToLower(code)} {
		switch value {
		case "context_length_exceeded", "context_window_exceeded", "prompt_too_long", "input_too_long", "max_context_length_exceeded":
			return true
		}
	}
	return false
}

// overflowMessage recognizes providers that report a context overflow only in
// the human-readable error message rather than a canonical type or code (for
// example Kimi: "Your request exceeded model token limit"). Phrases are kept
// specific to input size so unrelated invalid-request errors do not trigger a
// wasted compaction and retry.
func overflowMessage(message string) bool {
	lower := strings.ToLower(message)
	for _, phrase := range []string{
		"exceeded model token limit", "token limit", "context length", "context window",
		"maximum context", "prompt is too long", "too many tokens", "reduce the length of the messages",
	} {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	return false
}

func runnerInstructions(baseline, scratchPath string, final bool) string {
	sections := make([]string, 0, 3)
	for _, section := range []string{baseline, "Scratch directory: " + scratchPath, finalTurnInstructions(final)} {
		if section != "" {
			sections = append(sections, section)
		}
	}
	return strings.Join(sections, "\n\n")
}

func finalTurnInstructions(final bool) string {
	if final {
		return "This is the final turn. Do not call tools; provide the best final answer now."
	}
	return ""
}

type profileStatus struct {
	prompt    string
	hardRules []string
	provider  statusinfo.Provider
}

func newProfileStatus(profile Profile) statusinfo.Provider {
	return profileStatus{prompt: profile.Prompt, hardRules: profile.HardRules, provider: profile.Status}
}

func (s profileStatus) Key() string {
	if s.provider != nil {
		return s.provider.Key()
	}
	return "profile:instructions"
}

func (s profileStatus) Observe(ctx context.Context, query statusinfo.Query) (statusinfo.Observation, error) {
	sections := make([]string, 0, 2)
	instructions := s.prompt
	if len(s.hardRules) > 0 {
		instructions += "\n\nHard rules:\n- " + strings.Join(s.hardRules, "\n- ")
	}
	if instructions != "" {
		sections = append(sections, instructions)
	}
	if s.provider != nil {
		observation, err := s.provider.Observe(ctx, query)
		if err != nil {
			return statusinfo.Observation{}, err
		}
		if observation.Available && strings.TrimSpace(observation.Text) != "" {
			sections = append(sections, observation.Text)
		}
	}
	return statusinfo.Observation{Available: len(sections) > 0, Text: strings.Join(sections, "\n\n")}, nil
}

func finalParts(text, reasoning string, calls []completedCall) []protocol.ContentPart {
	var parts []protocol.ContentPart
	if reasoning != "" {
		parts = append(parts, protocol.ContentPart{Type: protocol.ContentReasoning, Text: reasoning})
	}
	if text != "" {
		parts = append(parts, protocol.ContentPart{Type: protocol.ContentText, Text: text})
	}
	for _, call := range calls {
		copy := call.call
		parts = append(parts, protocol.ContentPart{Type: protocol.ContentToolCall, ToolCall: &copy})
	}
	return parts
}

func preferredReasoning(reasoning, summary string) string {
	if summary != "" {
		return summary
	}
	return reasoning
}

type reasoningSummaryAccumulator struct {
	parts map[string]*strings.Builder
	order []string
	bytes int
}

func (a *reasoningSummaryAccumulator) Write(partID, text string) {
	if text == "" {
		return
	}
	if a.parts == nil {
		a.parts = make(map[string]*strings.Builder)
	}
	part := a.parts[partID]
	if part == nil {
		part = &strings.Builder{}
		a.parts[partID] = part
		a.order = append(a.order, partID)
	}
	part.WriteString(text)
	a.bytes += len(text)
}

func (a *reasoningSummaryAccumulator) Set(partID, text string) {
	if a.parts == nil {
		a.parts = make(map[string]*strings.Builder)
	}
	part := a.parts[partID]
	if part == nil {
		part = &strings.Builder{}
		a.parts[partID] = part
		a.order = append(a.order, partID)
	} else {
		a.bytes -= part.Len()
		part.Reset()
	}
	part.WriteString(text)
	a.bytes += len(text)
}

func (a *reasoningSummaryAccumulator) String() string {
	var summary strings.Builder
	summary.Grow(a.bytes)
	for _, partID := range a.order {
		summary.WriteString(a.parts[partID].String())
	}
	return summary.String()
}

func (a *reasoningSummaryAccumulator) Len() int { return a.bytes }

type toolOutcome struct {
	call        completedCall
	text        string
	modelText   string
	err         error
	interrupted bool
	settled     bool
	persistErr  error
}

func settleTool(ctx context.Context, sessions SessionRuntime, sessionID, callID, status string, result tool.Result, errorText string) error {
	if settler, ok := sessions.(interface {
		SettleToolWithOutput(context.Context, string, string, string, string, string, string) error
	}); ok {
		tail, _ := result.Metadata["output_tail"].(string)
		return settler.SettleToolWithOutput(ctx, sessionID, callID, status, result.Text, errorText, tail)
	}
	return sessions.SettleTool(ctx, sessionID, callID, status, result.Text, errorText)
}

// executeToolCall runs one tool and converts any panic in its Plan or Execute
// into a normal error. A tool call executes in its own goroutine, so without
// this recovery a single panicking tool would terminate the entire process
// rather than failing just that call. The recovered stack is captured in the
// error so the underlying defect remains diagnosable.
func executeToolCall(ctx context.Context, executor tool.Executor, call completedCall, callContext tool.CallContext, onPanic func(recovered any, stack []byte)) (result tool.Result, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			stack := debug.Stack()
			if onPanic != nil {
				onPanic(recovered, stack)
			}
			result = tool.Result{}
			err = fmt.Errorf("tool %q panicked: %v\n%s", call.call.Name, recovered, stack)
		}
	}()
	return executor.Execute(ctx, call.call.Name, json.RawMessage(call.call.Input), callContext)
}

func (r *agentSession) executeTools(ctx context.Context, selected session.AgentSessionDto, profile Profile, snapshot tool.Snapshot, calls []completedCall) error {
	executor := r.config.ToolExecutor(snapshot)
	statusQuery := r.statusQuery(ctx, selected, profile)
	sem := make(chan struct{}, r.config.MaxConcurrentTools)
	outcomes := make([]toolOutcome, len(calls))
	var wg sync.WaitGroup
	for i, call := range calls {
		wg.Add(1)
		go func(i int, call completedCall) {
			defer wg.Done()
			// Record the call before any cancellable operation. This lets cleanup
			// settle and answer calls that were still waiting for the semaphore.
			outcomes[i].call = call
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				outcomes[i] = toolOutcome{call: call, err: ctx.Err(), interrupted: true}
				return
			}
			defer func() { <-sem }()
			if err := r.config.Sessions.StartTool(ctx, r.dto.ID, call.call.ID); err != nil {
				outcomes[i] = toolOutcome{call: call, persistErr: err}
				return
			}
			var onPanic func(recovered any, stack []byte)
			if logger := r.config.ToolPanicLogger; logger != nil {
				onPanic = func(recovered any, stack []byte) {
					logger(ctx, r.dto.ID, call.call.Name, recovered, stack)
				}
			}
			taskID := ""
			if r.config.TaskIDFor != nil {
				taskID = r.config.TaskIDFor(r.dto.ID)
			}
			result, err := executeToolCall(ctx, executor, call, tool.CallContext{Workspace: r.config.Workspace, Outputs: r.config.Outputs, SessionID: r.dto.ID, TaskID: taskID, Processes: r.config.Processes, Agent: profile.ID, ToolCallID: call.call.ID, Output: &toolOutputWriter{live: r.config.Live, sessionID: r.dto.ID, callID: call.call.ID}, SecurityProfile: r.securityProfile, StatusQuery: statusQuery, StatusProvider: newProfileStatus(profile)}, onPanic)
			outcome := toolOutcome{call: call, text: result.Text, modelText: result.ModelText, err: err, interrupted: ctx.Err() != nil}
			status, errorText := "success", ""
			if outcome.interrupted {
				status, errorText = "interrupted", ctx.Err().Error()
			} else if err != nil {
				status, errorText = "failure", err.Error()
			}
			settleCtx := ctx
			var cancel context.CancelFunc
			if ctx.Err() != nil {
				settleCtx, cancel = context.WithTimeout(context.Background(), r.config.CleanupTimeout)
				defer cancel()
			}
			settleErr := settleTool(settleCtx, r.config.Sessions, r.dto.ID, call.call.ID, status, result, errorText)
			outcome.settled = settleErr == nil
			outcome.persistErr = settleErr
			outcomes[i] = outcome
		}(i, call)
	}
	wg.Wait()
	if ctx.Err() != nil {
		cleanup, cancel := context.WithTimeout(context.Background(), r.config.CleanupTimeout)
		defer cancel()
		for i := range outcomes {
			if !outcomes[i].settled {
				outcomes[i].interrupted = true
				if outcomes[i].err == nil {
					outcomes[i].err = ctx.Err()
				}
				if err := r.config.Sessions.SettleTool(cleanup, r.dto.ID, outcomes[i].call.call.ID, "interrupted", "", ctx.Err().Error()); err != nil {
					outcomes[i].persistErr = err
				} else {
					outcomes[i].settled = true
					outcomes[i].persistErr = nil
				}
			}
		}
		resultErr := error(ctx.Err())
		for _, outcome := range outcomes {
			if outcome.persistErr != nil {
				resultErr = errors.Join(resultErr, outcome.persistErr)
			}
			// Provider protocols require one result for every completed call. Use
			// the cleanup context so Ctrl-C cannot leave an orphaned function_call
			// that makes the provider reject the user's next prompt.
			_, err := r.config.Sessions.AppendMessage(cleanup, r.dto.ID, toolResultMessage(outcome))
			if err != nil {
				resultErr = errors.Join(resultErr, err)
			}
		}
		return resultErr
	}
	for _, outcome := range outcomes {
		if outcome.persistErr != nil {
			return outcome.persistErr
		}
		if !outcome.settled {
			return errors.New("agent: tool call did not settle")
		}
	}
	for _, outcome := range outcomes {
		_, err := r.config.Sessions.AppendMessage(ctx, r.dto.ID, toolResultMessage(outcome))
		if err != nil {
			return err
		}
	}
	return nil
}

func toolResultMessage(outcome toolOutcome) protocol.Message {
	text := outcome.modelText
	if outcome.interrupted {
		reason := "tool execution interrupted"
		if outcome.err != nil {
			reason += ": " + outcome.err.Error()
		}
		text = "Error: " + reason
	} else if outcome.err != nil {
		text = "Error: " + outcome.err.Error()
	}
	return protocol.Message{Role: protocol.RoleTool, Content: []protocol.ContentPart{{Type: protocol.ContentToolResult, Text: text, ToolCallID: outcome.call.call.ID}}}
}

func (r *agentSession) finishOnCleanup(messageID string, parts []protocol.ContentPart, finish protocol.FinishReason, errorText, status string) error {
	return r.finishAssistantOnCleanup(messageID, session.AssistantFinal{Parts: parts, FinishReason: finish, Error: errorText, Status: status})
}

func (r *agentSession) finishAssistantOnCleanup(messageID string, final session.AssistantFinal) error {
	ctx, cancel := context.WithTimeout(context.Background(), r.config.CleanupTimeout)
	defer cancel()
	return r.config.Sessions.FinishAssistant(ctx, r.dto.ID, messageID, final)
}

func toolDefinitions(snapshot tool.Snapshot) []protocol.ToolDefinition {
	definitions := snapshot.Definitions()
	result := make([]protocol.ToolDefinition, 0, len(definitions))
	for _, definition := range definitions {
		result = append(result, protocol.ToolDefinition{Name: definition.ID, Description: definition.Description, InputSchema: definition.Schema})
	}
	return result
}
