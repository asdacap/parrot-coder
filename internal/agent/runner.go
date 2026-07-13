package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

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

type RunnerConfig struct {
	Sessions           SessionRuntime
	Contexts           ContextRuntime
	Agents             *Registry
	Providers          ProviderResolver
	ToolSnapshot       func() tool.Snapshot
	ToolExecutor       func(tool.Snapshot) tool.Executor
	Workspace          *workspace.Workspace
	Outputs            *tool.OutputStore
	Live               LivePublisher
	MaxConcurrentTools int
	CleanupTimeout     time.Duration
}

type Runner struct{ config RunnerConfig }

func NewRunner(config RunnerConfig) (*Runner, error) {
	if config.Sessions == nil || config.Agents == nil || config.Providers == nil || config.ToolSnapshot == nil || config.ToolExecutor == nil {
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
		profile, err := r.config.Agents.Get(selected.Agent)
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
		instructions := epoch.Baseline
		if instructions != "" {
			instructions += "\n\n"
		}
		instructions += profile.Prompt
		if len(profile.HardRules) > 0 {
			instructions += "\n\nHard rules:\n- " + strings.Join(profile.HardRules, "\n- ")
		}
		if turn >= profile.MaxTurns {
			definitions = nil
			instructions += "\n\nThis is the final turn. Do not call tools; provide the best final answer now."
		}
		request := protocol.Request{Model: model.ID, Instructions: instructions, Messages: history, Tools: definitions}
		calls, finish, err := r.providerTurn(ctx, sessionID, providerClient, request)
		if err != nil {
			return err
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

func (r *Runner) providerTurn(ctx context.Context, sessionID string, client provider.Provider, request protocol.Request) ([]completedCall, protocol.FinishReason, error) {
	assistant, err := r.config.Sessions.StartAssistant(ctx, sessionID)
	if err != nil {
		return nil, "", err
	}
	stream, err := client.Stream(ctx, request)
	if err != nil {
		_ = r.finishOnCleanup(sessionID, assistant.ID, nil, protocol.FinishError, err.Error(), "error")
		return nil, "", err
	}
	defer stream.Close()
	var text, reasoning strings.Builder
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
			parts := finalParts(text.String(), reasoning.String(), calls)
			_ = r.finishOnCleanup(sessionID, assistant.ID, parts, protocol.FinishError, nextErr.Error(), status)
			return nil, "", nextErr
		}
		if r.config.Live != nil {
			r.config.Live.Publish(sessionID, item)
		}
		switch item.Type {
		case protocol.EventTextDelta:
			text.WriteString(item.Text)
		case protocol.EventReasoningDelta:
			reasoning.WriteString(item.Text)
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
			if item.ProviderError != nil {
				message = item.ProviderError.Message
			}
			parts := finalParts(text.String(), reasoning.String(), calls)
			_ = r.finishAssistantOnCleanup(sessionID, assistant.ID, session.AssistantFinal{Parts: parts, Usage: usage, FinishReason: protocol.FinishError, Error: message, Status: "error"})
			return nil, protocol.FinishError, errors.New(message)
		}
	}
	parts := finalParts(text.String(), reasoning.String(), calls)
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

type toolOutcome struct {
	call        completedCall
	text        string
	err         error
	interrupted bool
	settled     bool
	persistErr  error
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
			result, err := executor.Execute(ctx, call.call.Name, json.RawMessage(call.call.Input), tool.CallContext{Workspace: r.config.Workspace, Outputs: r.config.Outputs, SessionID: sessionID})
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
			settleErr := r.config.Sessions.SettleTool(settleCtx, sessionID, call.call.ID, status, result.Text, errorText)
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
				if err := r.config.Sessions.SettleTool(cleanup, sessionID, outcomes[i].call.call.ID, "interrupted", "", ctx.Err().Error()); err != nil {
					outcomes[i].persistErr = err
				} else {
					outcomes[i].settled = true
					outcomes[i].persistErr = nil
				}
			}
		}
		for _, outcome := range outcomes {
			if outcome.persistErr != nil {
				return errors.Join(ctx.Err(), outcome.persistErr)
			}
		}
		return ctx.Err()
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
		text := outcome.text
		if outcome.err != nil {
			text = "Error: " + outcome.err.Error()
		}
		_, err := r.config.Sessions.AppendMessage(ctx, sessionID, protocol.Message{Role: protocol.RoleTool, Content: []protocol.ContentPart{{Type: protocol.ContentToolResult, Text: text, ToolCallID: outcome.call.call.ID}}})
		if err != nil {
			return err
		}
	}
	return nil
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
