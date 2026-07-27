package enhancedchat

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	v1 "github.com/amirulashraf/parrot-coder/internal/api/v1"
	"github.com/amirulashraf/parrot-coder/internal/terminal"
)

func (r *enhancedChatRuntime) handleInput(value string) enhancedInputOutcome {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return enhancedInputOutcome{}
	}
	if strings.HasPrefix(trimmed, "/") {
		name, arguments := slashParts(trimmed)
		if name == "/run" {
			if arguments == "" {
				r.commitError("run requires a prompt")
				return enhancedInputOutcome{}
			}
			value = arguments
		} else if isBuiltinSlash(name) {
			return r.handleBuiltin(name, arguments)
		} else {
			expansion, err := r.shell.commands.Expand(strings.TrimPrefix(name, "/"), arguments)
			if err != nil {
				r.commitError(fmt.Sprintf("unknown slash command %q: %v", name, err))
				return enhancedInputOutcome{}
			}
			if r.busy && !expansion.Subtask && (expansion.Agent != "" || expansion.Model != "") {
				r.commitError("custom command changes model or agent while the session is active")
				return enhancedInputOutcome{}
			}
			if !r.busy && !expansion.Subtask {
				if expansion.Agent != "" {
					if err := r.shell.selectAgent(expansion.Agent); err != nil {
						r.commitError(err.Error())
						return enhancedInputOutcome{}
					}
					r.borderCommitted = false
				}
				if expansion.Model != "" {
					if err := r.shell.selectModel(expansion.Model); err != nil {
						r.commitError(err.Error())
						return enhancedInputOutcome{}
					}
					r.borderCommitted = false
				}
			}
			if expansion.Subtask {
				value = subtaskPrompt(expansion)
			} else {
				value = expansion.Prompt
			}
		}
	}
	if err := r.submitPrompt(value); err != nil {
		if !errors.Is(err, terminal.ErrCanceled) && !errors.Is(err, terminal.ErrInterrupted) {
			r.commitError(err.Error())
		}
		return enhancedInputOutcome{retain: true}
	}
	return enhancedInputOutcome{}
}

type compactionAPI interface {
	Compact(context.Context, string) (v1.Compaction, error)
}

func (r *enhancedChatRuntime) handleBuiltin(name, arguments string) enhancedInputOutcome {
	if name == "/exit" {
		if r.busy {
			_ = r.requestInterrupt()
		}
		return enhancedInputOutcome{exit: true, code: exitOK}
	}
	if r.busy && !safeBusySlash(name) {
		r.commitError(name + " is unavailable while the agent is working")
		return enhancedInputOutcome{}
	}
	if name == "/compact" {
		r.startCompaction(arguments, "usage: /compact [ID]")
		return enhancedInputOutcome{}
	}
	if fields := strings.Fields(arguments); name == "/session" && len(fields) > 0 && fields[0] == "compact" {
		r.startCompaction(strings.Join(fields[1:], " "), "usage: /session compact [ID]")
		return enhancedInputOutcome{}
	}
	if name == "/resume" {
		item, err := r.shell.chooseSession(arguments)
		if err != nil {
			if !errors.Is(err, terminal.ErrCanceled) && !errors.Is(err, terminal.ErrInterrupted) {
				r.commitError(err.Error())
			}
			return enhancedInputOutcome{}
		}
		r.shell.setCurrent(item)
		if err := r.ensureStream(item.ID); err != nil {
			r.commitError(err.Error())
			return enhancedInputOutcome{}
		}
		if r.shell.config == nil || r.shell.config.ResumeSession == nil {
			r.commitError("connected server does not support explicit resume")
			return enhancedInputOutcome{}
		}
		if err := r.shell.config.ResumeSession(item.ID); err != nil {
			r.commitError(err.Error())
			return enhancedInputOutcome{}
		}
		r.busy = true
		r.status = "resuming"
		return enhancedInputOutcome{}
	}
	previousSession := r.shell.current.ID
	exit, code := r.shell.slash(name, arguments)
	r.shell.refreshState()
	if name == "/new" || name == "/clear" || name == "/session" || name == "/connect" || r.shell.current.ID != previousSession {
		r.stopStream()
	}
	// A builtin command commits at most a short status line, so the live frame's
	// own boundary is enough. Committing a permanent rule here left a divider
	// stranded above and below every status line in the transcript.
	r.borderCommitted = false
	return enhancedInputOutcome{exit: exit, code: code}
}

func (r *enhancedChatRuntime) startCompaction(argument, usage string) {
	fields := strings.Fields(argument)
	if len(fields) > 1 {
		r.commitError(usage)
		return
	}
	sessionID := r.shell.current.ID
	if len(fields) == 1 {
		sessionID = fields[0]
	}
	if sessionID == "" {
		r.commitError("no active session")
		return
	}
	api, ok := r.shell.api.(compactionAPI)
	if !ok {
		r.commitError("connected server does not support compaction")
		return
	}
	r.ensureCompactionState()
	if r.compactions[sessionID] != nil {
		r.commitError("compaction already in progress for session " + sessionID)
		return
	}
	activityID, err := opaqueID("compaction")
	if err != nil {
		r.commitError(err.Error())
		return
	}
	if r.ctx == nil {
		r.ctx = r.shell.ctx
	}
	if r.compactionResults == nil {
		r.compactionResults = make(chan enhancedCompactionResult, 1)
	}
	r.compactions[sessionID] = &enhancedCompactionRequest{activityID: activityID}
	r.upsertActivity(activityID, "Compaction · "+sessionID, "running", false, false, false)
	for i := range r.activity {
		if r.activity[i].id == activityID {
			r.activity[i].sessionID = sessionID
			break
		}
	}
	ctx := r.ctx
	results := r.compactionResults
	go func() {
		result, err := api.Compact(ctx, sessionID)
		select {
		case results <- enhancedCompactionResult{activityID: activityID, sessionID: sessionID, result: result, err: err}:
		case <-ctx.Done():
		}
	}()
}

func (r *enhancedChatRuntime) settleCompaction(settled enhancedCompactionResult) error {
	r.ensureCompactionState()
	pending := r.compactions[settled.sessionID]
	if pending == nil || pending.activityID != settled.activityID {
		return nil
	}

	// The HTTP response is authoritative. Release the per-session duplicate guard
	// before replaying so a missing terminal SSE can never block the next command.
	delete(r.compactions, settled.sessionID)
	attemptID := ""
	if settled.err == nil {
		attemptID = settled.result.AttemptID
	}
	if attemptID != "" {
		r.compactionTombstones[enhancedCompactionTombstone{sessionID: settled.sessionID, attemptID: attemptID}] = struct{}{}
	}
	var replayErr error
	for _, event := range pending.events {
		if attemptID != "" && event.attemptID == attemptID {
			continue
		}
		replayErr = errors.Join(replayErr, r.renderRuntimeActivityEvent(event.item))
	}

	status := "success"
	reason := ""
	if settled.err != nil {
		status = "failure"
		reason = settled.err.Error()
	} else if settled.result.Status != "complete" {
		status = "failure"
		reason = settled.result.Reason
		if reason == "" {
			reason = "compaction did not complete"
		}
	}
	// Replay first, then settle the HTTP activity, preserving event arrival order
	// ahead of the authoritative request result in the transcript.
	r.settleCompactionActivity(settled.activityID, settled.sessionID, status, reason)
	return errors.Join(replayErr, r.flushCompletedActivities())
}

func (r *enhancedChatRuntime) settleCompactionActivity(activityID, sessionID, status, reason string) {
	for i := range r.activity {
		if r.activity[i].id != activityID {
			continue
		}
		r.activity[i].status = status
		r.activity[i].error = reason
		r.activity[i].terminal = true
		r.activity[i].ended = time.Now()
		if status == "success" {
			r.activity[i].label = "Compaction: complete · " + sessionID
		}
		break
	}
	r.queueCompletedActivity(activityID)
}

func (r *enhancedChatRuntime) ensureCompactionState() {
	if r.compactions == nil {
		r.compactions = make(map[string]*enhancedCompactionRequest)
	}
	if r.compactionTombstones == nil {
		r.compactionTombstones = make(map[enhancedCompactionTombstone]struct{})
	}
}

func (r *enhancedChatRuntime) handleCompactionLifecycleEvent(item v1.Event) (bool, error) {
	if item.Type != v1.EventSessionCompactionStarted && item.Type != v1.EventSessionCompactionFinished {
		return false, nil
	}
	decoded, err := v1.DecodeEventData(item)
	if err != nil {
		return true, err
	}
	attemptID := decoded.(*v1.CompactionEvent).AttemptID
	r.ensureCompactionState()

	// Check settled correlations first. A new request for the same session must
	// not accidentally capture a late duplicate from the preceding request.
	if attemptID != "" {
		if _, suppressed := r.compactionTombstones[enhancedCompactionTombstone{sessionID: item.SessionID, attemptID: attemptID}]; suppressed {
			return true, nil
		}
	}
	if pending := r.compactions[item.SessionID]; pending != nil {
		pending.events = append(pending.events, enhancedCompactionLifecycleEvent{item: item, attemptID: attemptID})
		return true, nil
	}
	return false, nil
}

func safeBusySlash(name string) bool {
	switch name {
	case "/help", "/version", "/chat", "/models", "/usage", "/modes", "/agents", "/sessions", "/goal", "/status", "/thinking", "/exit", "/continue":
		return true
	default:
		return false
	}
}

func (r *enhancedChatRuntime) submitPrompt(content string) error {
	if r.busy {
		messageID, err := opaqueID("msg")
		if err != nil {
			return err
		}
		accepted, err := r.shell.api.Prompt(r.shell.ctx, r.shell.current.ID, v1.PromptRequest{
			MessageID: messageID, Content: content,
		})
		if err != nil {
			return err
		}
		r.addPending(queuedChatInput{inputID: accepted.InputID, messageID: accepted.MessageID, content: content})
		r.status = "steering"
		return nil
	}

	if r.shell.selection.modelName() == "" {
		selected, err := r.shell.pickModel()
		if err != nil {
			return err
		}
		if err := r.shell.applyModel(selected); err != nil {
			return err
		}
		r.borderCommitted = false
	}
	if r.shell.current.ID == "" {
		item, err := r.shell.createSession(content, false)
		if err != nil {
			return err
		}
		r.shell.setCurrent(item)
	}
	if err := r.ensureStream(r.shell.current.ID); err != nil {
		return err
	}
	messageID, err := opaqueID("msg")
	if err != nil {
		return err
	}
	if _, err := r.shell.api.Prompt(r.shell.ctx, r.shell.current.ID, v1.PromptRequest{
		MessageID: messageID, Content: content,
	}); err != nil {
		return err
	}
	if err := r.commitUser(content); err != nil {
		return err
	}
	r.busy = true
	r.status = "working"
	r.interruptCount = 0
	return nil
}

func (r *enhancedChatRuntime) addPending(item queuedChatInput) {
	for _, existing := range r.pending {
		if existing.inputID == item.inputID || existing.messageID == item.messageID {
			return
		}
	}
	r.pending = append(r.pending, item)
}

func (r *enhancedChatRuntime) promotePending(inputID, messageID string) error {
	for i, item := range r.pending {
		if item.inputID != inputID && item.messageID != messageID {
			continue
		}
		r.pending = append(r.pending[:i], r.pending[i+1:]...)
		return r.commitUser(item.content)
	}
	return nil
}

func (r *enhancedChatRuntime) requestInterrupt() error {
	r.interruptCount++
	if r.interruptCount > 1 {
		return errSecondInterrupt
	}
	r.status = "interrupt requested"
	sessionID := r.shell.current.ID
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = r.shell.api.Interrupt(ctx, sessionID)
	}()
	return nil
}
