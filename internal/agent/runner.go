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
	"github.com/amirulashraf/parrot-coder/internal/protocol"
	"github.com/amirulashraf/parrot-coder/internal/provider"
	"github.com/amirulashraf/parrot-coder/internal/session"
	"github.com/amirulashraf/parrot-coder/internal/tool"
	"github.com/amirulashraf/parrot-coder/internal/workspace"
)

type SessionRuntime interface {
	Get(context.Context, string) (session.Session, error)
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
	RepairActive(context.Context, string) error
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

type RunnerConfig struct {
	Sessions           SessionRuntime
	Contexts           ContextRuntime
	Agents             *Registry
	Profiles           ProfileResolver
	Providers          ProviderResolver
	ToolSnapshot       func() tool.Snapshot
	ToolExecutor       func(tool.Snapshot) tool.Executor
	Workspace          *workspace.Workspace
	Outputs            *tool.OutputStore
	Live               LivePublisher
	Compactor          Compactor
	MaxConcurrentTools int
	CleanupTimeout     time.Duration
	// ToolPanicLogger, when set, receives diagnostics for a tool call whose
	// Plan or Execute panicked and was recovered into a failure. It is purely
	// an observability seam: the panic is always reported to the model as a
	// tool failure whether or not a logger is configured.
	ToolPanicLogger func(ctx context.Context, sessionID, toolName string, recovered any, stack []byte)
}

type Runner struct{ config RunnerConfig }

func NewRunner(config RunnerConfig) (*Runner, error) {
	if config.Profiles == nil {
		config.Profiles = config.Agents
	}
	if config.Sessions == nil || config.Profiles == nil || config.Providers == nil || config.ToolSnapshot == nil || config.ToolExecutor == nil {
		return nil, errors.New("agent: runner dependencies are required")
	}
	if config.MaxConcurrentTools <= 0 {
		config.MaxConcurrentTools = 4
	}
	if config.CleanupTimeout <= 0 {
		config.CleanupTimeout = 5 * time.Second
	}
	return &Runner{config}, nil
}

func (r *Runner) Drain(ctx context.Context, sessionID string) error {
	if err := r.config.Sessions.RepairActive(ctx, sessionID); err != nil {
		return err
	}
	turn := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		cutoff, err := r.config.Sessions.LatestSequence(ctx, sessionID)
		if err != nil {
			return err
		}
		epoch, epochErr := r.config.Sessions.CurrentContextEpoch(ctx, sessionID)
		initial := errors.Is(epochErr, session.ErrNotFound)
		if epochErr != nil && !initial {
			return epochErr
		}
		if initial {
			if r.config.Contexts == nil {
				return errors.New("agent: context runtime is required for initialization")
			}
			epoch, err = r.config.Contexts.Initialize(ctx, sessionID, cutoff)
			if err != nil {
				return err
			}
		}
		if !initial && r.config.Contexts != nil {
			reconciled, reconcileErr := r.config.Contexts.Reconcile(ctx, sessionID)
			if reconcileErr != nil && reconciled.ID == "" {
				return reconcileErr
			}
			if reconciled.ID != "" {
				epoch = reconciled
			}
		}
		promoted, err := r.config.Sessions.PromoteSteers(ctx, sessionID, cutoff)
		if err != nil {
			return err
		}

		selected, err := r.config.Sessions.Get(ctx, sessionID)
		if err != nil {
			return err
		}
		profile, err := r.config.Profiles.GetProfile(selected.Agent)
		if err != nil {
			return err
		}
		providerClient, model, err := r.config.Providers.Resolve(selected.Provider, selected.Model)
		if err != nil {
			return err
		}
		history, err := r.config.Sessions.ListModelHistory(ctx, sessionID, epoch.HistoryCutoff)
		if err != nil {
			return err
		}
		ready := len(promoted) > 0
		if len(history) > 0 {
			lastRole := history[len(history)-1].Role
			ready = ready || lastRole == protocol.RoleUser || lastRole == protocol.RoleTool
		}
		if !ready {
			queued, err := r.config.Sessions.PromoteNextQueue(ctx, sessionID)
			if err != nil || len(queued) == 0 {
				return err
			}
			history, err = r.config.Sessions.ListModelHistory(ctx, sessionID, epoch.HistoryCutoff)
			if err != nil {
				return err
			}
		}

		snapshot := r.config.ToolSnapshot()
		definitions := toolDefinitions(snapshot, profile)
		turn++
		instructions := runnerInstructions(epoch.Baseline, profile, turn >= profile.MaxTurns)
		if turn >= profile.MaxTurns {
			definitions = nil
		}
		if r.config.Compactor != nil {
			result, compactErr := r.config.Compactor.Compact(ctx, compaction.Request{
				SessionID: sessionID, ProviderID: selected.Provider, Model: model,
				Instructions: profileInstructions(profile, turn >= profile.MaxTurns), Tools: definitions,
			})
			if compactErr != nil {
				return compactErr
			}
			if result.Status == "complete" {
				epoch, err = r.config.Sessions.CurrentContextEpoch(ctx, sessionID)
				if err != nil {
					return err
				}
				history, err = r.config.Sessions.ListModelHistory(ctx, sessionID, epoch.HistoryCutoff)
				if err != nil {
					return err
				}
				instructions = runnerInstructions(epoch.Baseline, profile, turn >= profile.MaxTurns)
			}
		}
		request := protocol.Request{Model: model.ID, Instructions: instructions, Messages: history, Tools: definitions}
		if selected.Variant != "" {
			variant, ok := model.Capabilities.Variants[selected.Variant]
			if !ok {
				return fmt.Errorf("agent: unknown model variant %q", selected.Variant)
			}
			request.Reasoning = &protocol.ReasoningOptions{Effort: variant.ReasoningEffort, Summary: "auto"}
		}
		calls, finish, err := r.loggedProviderTurn(ctx, sessionID, selected.Provider, turn, providerClient, request)
		if err != nil {
			var failure *providerTurnFailure
			if !errors.As(err, &failure) || !failure.overflow || !failure.retrySafe || r.config.Compactor == nil {
				return err
			}
			result, compactErr := r.config.Compactor.Compact(ctx, compaction.Request{
				SessionID: sessionID, ProviderID: selected.Provider, Model: model,
				Instructions: profileInstructions(profile, turn >= profile.MaxTurns), Tools: definitions, Force: true,
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
			if recordErr := recorder.RecordCompactionRetry(ctx, sessionID, failure.code, result.RecordID); recordErr != nil {
				return recordErr
			}
			epoch, err = r.config.Sessions.CurrentContextEpoch(ctx, sessionID)
			if err != nil {
				return err
			}
			history, err = r.config.Sessions.ListModelHistory(ctx, sessionID, epoch.HistoryCutoff)
			if err != nil {
				return err
			}
			request.Instructions = runnerInstructions(epoch.Baseline, profile, turn >= profile.MaxTurns)
			request.Messages = history
			calls, finish, err = r.loggedProviderTurn(ctx, sessionID, selected.Provider, turn, providerClient, request)
			if err != nil {
				return err
			}
		}
		if len(calls) > 0 {
			if turn >= profile.MaxTurns {
				return errors.New("agent: provider returned tools after max-turn tool omission")
			}
			if err := r.executeTools(ctx, sessionID, profile, snapshot, calls); err != nil {
				return err
			}
			continue
		}
		if finish == protocol.FinishStop || finish == protocol.FinishLength || finish == protocol.FinishContentFilter || finish == protocol.FinishIncomplete {
			queued, err := r.config.Sessions.PromoteNextQueue(ctx, sessionID)
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

func (r *Runner) loggedProviderTurn(ctx context.Context, sessionID, providerID string, turn int, client provider.Provider, request protocol.Request) ([]completedCall, protocol.FinishReason, error) {
	started := time.Now()
	diagnostics.Event("provider_turn_started",
		"session_id", sessionID, "provider", providerID, "model", request.Model, "turn", turn,
		"message_count", len(request.Messages), "tool_count", len(request.Tools),
	)
	calls, finish, err := r.providerTurn(ctx, sessionID, client, request)
	attributes := []any{
		"session_id", sessionID, "provider", providerID, "model", request.Model, "turn", turn,
		"finish_reason", finish, "tool_call_count", len(calls), "duration_ms", time.Since(started).Milliseconds(),
	}
	if err != nil {
		diagnostics.Error("provider_turn_finished", append(attributes, "status", "error", "error_type", diagnostics.ErrorType(err))...)
	} else {
		diagnostics.Event("provider_turn_finished", append(attributes, "status", "success")...)
	}
	return calls, finish, err
}

func (r *Runner) providerTurn(ctx context.Context, sessionID string, client provider.Provider, request protocol.Request) ([]completedCall, protocol.FinishReason, error) {
	assistant, err := r.config.Sessions.StartAssistant(ctx, sessionID)
	if err != nil {
		return nil, "", err
	}
	stream, err := provider.StreamWithHeaderRetry(ctx, client, request)
	if err != nil {
		finishErr := r.finishOnCleanup(sessionID, assistant.ID, nil, protocol.FinishError, err.Error(), "error")
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
			_ = r.finishOnCleanup(sessionID, assistant.ID, parts, protocol.FinishError, nextErr.Error(), status)
			return nil, "", nextErr
		}
		if r.config.Live != nil {
			item.MessageID = assistant.ID
			r.config.Live.Publish(sessionID, item)
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
			code := ""
			if item.ProviderError != nil {
				message = item.ProviderError.Message
				code = item.ProviderError.Code
			}
			// Tool calls from a failed provider turn are not executed, so they must
			// not be retained without corresponding tool results.
			parts := finalParts(text.String(), preferredReasoning(reasoning.String(), reasoningSummary.String()), nil)
			finishErr := r.finishAssistantOnCleanup(sessionID, assistant.ID, session.AssistantFinal{Parts: parts, Usage: usage, FinishReason: protocol.FinishError, Error: message, Status: "error"})
			overflow := item.ProviderError != nil && canonicalOverflow(item.ProviderError.Type, item.ProviderError.Code)
			return nil, protocol.FinishError, &providerTurnFailure{err: errors.Join(errors.New(message), finishErr), code: code, overflow: overflow, retrySafe: finishErr == nil && text.Len() == 0 && reasoning.Len() == 0 && reasoningSummary.Len() == 0 && len(calls) == 0}
		}
	}
	parts := finalParts(text.String(), preferredReasoning(reasoning.String(), reasoningSummary.String()), calls)
	final := session.AssistantFinal{Parts: parts, Usage: usage, FinishReason: finish, Status: "complete"}
	if err := r.config.Sessions.FinishAssistant(ctx, sessionID, assistant.ID, final); err != nil {
		if ctx.Err() != nil {
			final.Status = "interrupted"
			final.FinishReason = protocol.FinishError
			final.Error = ctx.Err().Error()
			if cleanupErr := r.finishAssistantOnCleanup(sessionID, assistant.ID, final); cleanupErr != nil {
				return nil, finish, errors.Join(err, cleanupErr)
			}
		}
		return nil, finish, err
	}
	for _, call := range calls {
		if _, err := r.config.Sessions.AddToolCall(ctx, sessionID, assistant.ID, call.call); err != nil {
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
	return httpErr.Code, canonicalOverflow(httpErr.Type, httpErr.Code)
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

func runnerInstructions(baseline string, profile Profile, final bool) string {
	instructions := baseline
	if instructions != "" {
		instructions += "\n\n"
	}
	instructions += profileInstructions(profile, final)
	return instructions
}

func profileInstructions(profile Profile, final bool) string {
	instructions := profile.Prompt
	if len(profile.HardRules) > 0 {
		instructions += "\n\nHard rules:\n- " + strings.Join(profile.HardRules, "\n- ")
	}
	if final {
		instructions += "\n\nThis is the final turn. Do not call tools; provide the best final answer now."
	}
	return instructions
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

func (r *Runner) executeTools(ctx context.Context, sessionID string, profile Profile, snapshot tool.Snapshot, calls []completedCall) error {
	executor := r.config.ToolExecutor(snapshot)
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
			if !profile.AllowsTool(call.call.Name) {
				err := fmt.Errorf("tool %q denied by agent %q", call.call.Name, profile.ID)
				settleErr := r.config.Sessions.SettleTool(ctx, sessionID, call.call.ID, "failure", "", err.Error())
				outcomes[i] = toolOutcome{call: call, err: err, settled: settleErr == nil, persistErr: settleErr}
				return
			}
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				outcomes[i] = toolOutcome{call: call, err: ctx.Err(), interrupted: true}
				return
			}
			defer func() { <-sem }()
			if err := r.config.Sessions.StartTool(ctx, sessionID, call.call.ID); err != nil {
				outcomes[i] = toolOutcome{call: call, persistErr: err}
				return
			}
			var onPanic func(recovered any, stack []byte)
			if logger := r.config.ToolPanicLogger; logger != nil {
				onPanic = func(recovered any, stack []byte) {
					logger(ctx, sessionID, call.call.Name, recovered, stack)
				}
			}
			result, err := executeToolCall(ctx, executor, call, tool.CallContext{Workspace: r.config.Workspace, Outputs: r.config.Outputs, SessionID: sessionID, Agent: profile.ID, ToolCallID: call.call.ID, Output: &toolOutputWriter{live: r.config.Live, sessionID: sessionID, callID: call.call.ID}}, onPanic)
			outcome := toolOutcome{call: call, text: result.Text, err: err, interrupted: ctx.Err() != nil}
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
			settleErr := settleTool(settleCtx, r.config.Sessions, sessionID, call.call.ID, status, result, errorText)
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
				if err := r.config.Sessions.SettleTool(cleanup, sessionID, outcomes[i].call.call.ID, "interrupted", "", ctx.Err().Error()); err != nil {
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
			_, err := r.config.Sessions.AppendMessage(cleanup, sessionID, toolResultMessage(outcome))
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
		_, err := r.config.Sessions.AppendMessage(ctx, sessionID, toolResultMessage(outcome))
		if err != nil {
			return err
		}
	}
	return nil
}

func toolResultMessage(outcome toolOutcome) protocol.Message {
	text := outcome.text
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

func (r *Runner) finishOnCleanup(sessionID, messageID string, parts []protocol.ContentPart, finish protocol.FinishReason, errorText, status string) error {
	return r.finishAssistantOnCleanup(sessionID, messageID, session.AssistantFinal{Parts: parts, FinishReason: finish, Error: errorText, Status: status})
}

func (r *Runner) finishAssistantOnCleanup(sessionID, messageID string, final session.AssistantFinal) error {
	ctx, cancel := context.WithTimeout(context.Background(), r.config.CleanupTimeout)
	defer cancel()
	return r.config.Sessions.FinishAssistant(ctx, sessionID, messageID, final)
}

func toolDefinitions(snapshot tool.Snapshot, profile Profile) []protocol.ToolDefinition {
	definitions := snapshot.Definitions()
	result := make([]protocol.ToolDefinition, 0, len(definitions))
	for _, definition := range definitions {
		if profile.AllowsTool(definition.ID) {
			result = append(result, protocol.ToolDefinition{Name: definition.ID, Description: definition.Description, InputSchema: definition.Schema})
		}
	}
	return result
}
