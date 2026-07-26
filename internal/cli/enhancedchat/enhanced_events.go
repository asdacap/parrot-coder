package enhancedchat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	v1 "github.com/amirulashraf/parrot-coder/internal/api/v1"
	"github.com/amirulashraf/parrot-coder/internal/cli/chatview"
	"github.com/amirulashraf/parrot-coder/internal/terminal"
)

func (r *enhancedChatRuntime) ensureStream(sessionID string) error {
	if r.stream != nil && r.streamSessionID == sessionID {
		return nil
	}
	r.stopStream()
	messages, err := r.shell.api.Messages(r.shell.ctx, sessionID)
	if err != nil {
		return err
	}
	snapshotMessages := make(map[string]bool, len(messages.Items))
	for _, item := range messages.Items {
		snapshotMessages[item.ID] = true
	}
	newSession := r.eventSessionID != sessionID
	if newSession {
		r.eventSessionID = sessionID
		r.eventAfter = -1
		r.contextTokens = 0
		r.lastCompleteID = ""
		r.turnCompleteID = ""
		// Activity identities are unique per tree, but the old session's activity
		// tree is meaningless to the new one. Rebuild it rather than carry stale nodes.
		r.resetRuntimeActivityTracker()
		r.knownMessages = make(map[string]bool, len(messages.Items))
		r.unsyncedMessages = make(map[string]bool)
		for _, item := range messages.Items {
			if item.Sequence > r.eventAfter {
				r.eventAfter = item.Sequence
			}
			if item.Status != "active" {
				r.knownMessages[item.ID] = true
				if item.Role == "assistant" && item.Status == "complete" {
					r.lastCompleteID = item.ID
					r.turnCompleteID = item.ID
				}
			}
			if item.Role == "assistant" && item.Status != "active" {
				var usage v1.Usage
				if json.Unmarshal(item.Usage, &usage) == nil {
					if tokens := contextTokenCount(usage); tokens > 0 {
						r.contextTokens = tokens
					}
				}
			}
		}
	}
	if r.unsyncedMessages == nil {
		r.unsyncedMessages = make(map[string]bool)
	}
	// Disposable deltas are not replayed. If an assistant was already active
	// when this connection took its snapshot, its subsequent deltas would form
	// only a suffix (or contain a gap after reconnecting). Wait for the durable
	// message instead of presenting that incomplete text as a cumulative stream.
	r.markActiveAssistantsUnsynced(messages)
	after := r.eventAfter
	stream, err := r.shell.api.Events(r.shell.ctx, sessionID, &after)
	if err != nil {
		return err
	}
	connected, err := stream.Next()
	if err != nil || connected.Type != v1.EventServerConnected {
		_ = stream.Close()
		if err == nil {
			err = errors.New("event stream did not send server.connected")
		}
		return err
	}
	// Close the race between the repository snapshot and live subscription.
	// Any assistant first observed during that window may have emitted deltas
	// before the stream existed, so it must also settle from durable content.
	connectedMessages, err := r.shell.api.Messages(r.shell.ctx, sessionID)
	if err != nil {
		_ = stream.Close()
		return err
	}
	r.markNewAssistantsUnsynced(connectedMessages, snapshotMessages)
	r.stream = stream
	r.streamSessionID = sessionID
	r.streamGeneration++
	generation := r.streamGeneration
	done := make(chan struct{})
	exited := make(chan struct{})
	r.streamDone = done
	r.streamExited = exited
	go func() {
		defer close(exited)
		for {
			item, nextErr := stream.Next()
			select {
			case r.events <- enhancedSessionEvent{generation: generation, event: item, err: nextErr}:
			case <-done:
				return
			}
			if nextErr != nil {
				return
			}
		}
	}()
	return r.reconcileRuntime()
}

// resetRuntimeActivityTracker keeps the connected server's shared presentation
// state so friendly names learned by a new tree are also available to tool
// activity labels.
func (r *enhancedChatRuntime) resetRuntimeActivityTracker() {
	r.runtimeActivities = runtimeActivityStreamTracker{presentation: r.presentation(), rootSessionID: r.shell.current.ID}
}

func (r *enhancedChatRuntime) markActiveAssistantsUnsynced(messages v1.MessageList) {
	for _, item := range messages.Items {
		if item.Role == "assistant" && item.Status == "active" {
			r.unsyncedMessages[item.ID] = true
		}
	}
}

func (r *enhancedChatRuntime) markNewAssistantsUnsynced(messages v1.MessageList, snapshotMessages map[string]bool) {
	for _, item := range messages.Items {
		if item.Role == "assistant" && !snapshotMessages[item.ID] {
			r.unsyncedMessages[item.ID] = true
		}
	}
}

func (r *enhancedChatRuntime) reconcileRuntime() error {
	if !r.busy {
		return nil
	}
	runtimeState, err := r.shell.api.Runtime(r.shell.ctx)
	if err != nil {
		return err
	}
	active := false
	for _, item := range runtimeState.Active {
		if item.SessionID == r.shell.current.ID {
			active = true
			break
		}
	}
	if active {
		return nil
	}
	if err := r.commitCompletedAssistants(""); err != nil {
		return err
	}
	r.busy = false
	r.idleSeen = false
	r.status = ""
	r.interruptCount = 0
	return nil
}

func (r *enhancedChatRuntime) stopStream() {
	if r.stream == nil {
		return
	}
	close(r.streamDone)
	_ = r.stream.Close()
	<-r.streamExited
	r.stream = nil
	r.streamDone = nil
	r.streamExited = nil
	r.streamSessionID = ""
	r.streamGeneration++
}

// formatRuntimeActivityTokenUsage returns a humanized token-usage snippet.
func formatRuntimeActivityTokenUsage(usage chatview.RuntimeActivityUsage) string {
	if usage.InputTokens == 0 && usage.OutputTokens == 0 {
		return "-"
	}
	part := fmt.Sprintf("+%si +%so", formatTokenCount(usage.InputTokens), formatTokenCount(usage.OutputTokens))
	if usage.CachedTokens > 0 && usage.InputTokens > 0 {
		part += fmt.Sprintf(" (+%.2f%% cache)", float64(usage.CachedTokens)/float64(usage.InputTokens)*100)
	}
	return part
}

// recordUsage folds one usage event into the runtime activity tree, whichever
// session reported it. Routing every usage event through the tree keeps the
// modeline's tokens and cost describing the same work.
func (r *enhancedChatRuntime) recordUsage(item v1.Event) {
	if item.Type != v1.EventSessionStatus {
		return
	}
	payload, err := v1.DecodeEventData(item)
	if err != nil {
		return
	}
	status := payload.(*v1.SessionStatus)
	if status.Kind != "usage" || status.Usage == nil {
		return
	}
	r.runtimeActivities.addUsage(r.shell.current.ID, item.SessionID, *status.Usage)
	r.refreshRuntimeUsage()
}

// refreshRuntimeUsage recaches what the modeline reports: the session's own
// usage plus every descendant of the root runtime activity.
func (r *enhancedChatRuntime) refreshRuntimeUsage() {
	if tracker := r.runtimeActivities.Tracker(); tracker != nil {
		r.runtimeUsage = tracker.CumulativeUsage(r.shell.current.ID, "")
	}
}

// handleRuntimeActivityEvent renders one flat event through the runtime activity
// tracker. The tracker owns the hierarchy; this runtime only maps the resulting
// reports onto the live activity list and the transcript.
func (r *enhancedChatRuntime) handleRuntimeActivityEvent(item v1.Event) error {
	if r.runtimeActivities.Tracker() == nil {
		r.runtimeActivities.presentation = r.presentation()
	}
	thinking := r.shell != nil && r.shell.options.thinking
	reports, err := r.runtimeActivities.describe(item, thinking)
	if err != nil {
		return err
	}
	// For progress events, enhance the report line with token breakdown.
	if item.Type == v1.EventAgentSessionProgress && len(reports) > 0 {
		payload, decodeErr := v1.DecodeEventData(item)
		if decodeErr == nil {
			progress := payload.(*v1.AgentSessionProgress)
			if progress.Usage.TotalTokens > 0 {
				// The tree's total covers the agent's descendants too; its progress
				// report covers only itself and stands in until usage is recorded.
				usage := r.runtimeActivities.Tracker().CumulativeUsage(item.SessionID, "")
				if usage.InputTokens == 0 && usage.OutputTokens == 0 {
					usage = chatview.RuntimeActivityUsage{InputTokens: progress.Usage.InputTokens, OutputTokens: progress.Usage.OutputTokens, CachedTokens: progress.Usage.CachedInputTokens}
				}
				oldToken := fmt.Sprintf("· %s tokens", chatview.FormatTokenCount(progress.Usage.TotalTokens))
				tokenPart := formatRuntimeActivityTokenUsage(usage)
				if tokenPart != "-" {
					for i := range reports {
						reports[i].line = strings.Replace(reports[i].line, oldToken, "· "+tokenPart, 1)
					}
				}
			}
		}
	}
	// Update cached cumulative tokens after any runtime activity event.
	r.refreshRuntimeUsage()
	for _, report := range reports {
		text := report.line
		if report.block != "" {
			text += "\n" + report.block
		}
		if report.skip {
			for i := 0; i < len(r.activity); i++ {
				if r.activity[i].id == report.id {
					r.activity = append(r.activity[:i], r.activity[i+1:]...)
					break
				}
			}
			continue
		}
		if report.terminal {
			for i := 0; i < len(r.activity); i++ {
				if r.activity[i].id == report.id {
					r.activity = append(r.activity[:i], r.activity[i+1:]...)
					break
				}
			}
			if r.shell == nil || r.shell.renderer == nil {
				continue
			}
			styled := terminal.StyledText{Text: text, Style: report.style}
			if report.blockKind == chatview.ToolResultDiff {
				styled.Text = report.line
				err = r.shell.renderer.CommitDiffBlock(styled, report.block)
			} else if report.blockKind == chatview.ToolResultCode {
				styled.Text = report.line
				err = r.shell.renderer.CommitCodeBlock(styled, report.block, report.blockLanguage)
			} else if report.block != "" {
				err = r.shell.renderer.CommitStyledBlock(styled)
			} else {
				err = r.shell.renderer.CommitStyled(styled)
			}
			if err != nil {
				return err
			}
			r.borderCommitted = false
			continue
		}
		found := false
		for i := range r.activity {
			if r.activity[i].id != report.id {
				continue
			}
			r.activity[i].processID = report.processID
			r.activity[i].sessionID = report.sessionID
			r.activity[i].parentSessionID = report.parentSessionID
			r.activity[i].mainStatus = report.mainStatus
			r.activity[i].rendered = text
			r.activity[i].style = report.style
			found = true
			break
		}
		if !found {
			r.activity = append(r.activity, enhancedActivityItem{id: report.id, processID: report.processID, sessionID: report.sessionID, parentSessionID: report.parentSessionID, mainStatus: report.mainStatus, rendered: text, style: report.style, status: "running", started: time.Now()})
		}
	}
	return nil
}

func (r *enhancedChatRuntime) handleEvent(item v1.Event) error {
	r.recordUsage(item)
	if isRuntimeActivityEvent(item, r.shell.current.ID) {
		return r.handleRuntimeActivityEvent(item)
	}
	switch item.Type {
	case v1.EventMessagePartDelta:
		payload, err := v1.DecodeEventData(item)
		if err != nil {
			return err
		}
		delta := payload.(*v1.MessagePartDelta)
		if delta.MessageID != "" && r.knownMessages[delta.MessageID] {
			return r.settleIdle()
		}
		if delta.MessageID != "" && r.unsyncedMessages[delta.MessageID] {
			return r.settleIdle()
		}
		accepted, err := r.beginAssistantMessage(delta.MessageID)
		if err != nil {
			return err
		}
		if !accepted {
			return r.settleIdle()
		}
		if delta.Kind == "text" {
			if delta.Delta != "" && r.streamed.Len() == 0 {
				if err := r.flushReasoningBeforeAnswer(delta.MessageID); err != nil {
					return err
				}
			}
			r.streamed.WriteString(delta.Delta)
			r.status = ""
		} else if delta.Kind == "tool_input" {
			r.toolInput = true
		} else if delta.Kind == "reasoning_summary" {
			// Once answer text has started, complete rows may already be permanent
			// scrollback. A delayed summary can no longer be placed before them, so
			// discard it rather than rendering a misleading transcript order.
			if r.streamed.Len() != 0 {
				break
			}
			if !r.reasoningSummary {
				r.reasoningText.Reset()
				r.reasoningSummary = true
			}
			if delta.Done {
				// The done event carries the provider's authoritative complete text.
				// Use it when present rather than appending it to prior deltas.
				if delta.Delta != "" {
					if delta.PartID == "" {
						r.reasoningText.Reset()
						r.reasoningText.WriteString(delta.Delta)
						r.startReasoningActivity(delta.MessageID, "", delta.Delta, true)
					} else {
						if r.reasoningParts == nil {
							r.reasoningParts = make(map[string]string)
						}
						r.reasoningParts[delta.PartID] = delta.Delta
						r.startReasoningActivity(delta.MessageID, delta.PartID, delta.Delta, true)
					}
				}
				if err := r.finishReasoningSummaryPart(delta.MessageID, delta.PartID); err != nil {
					return err
				}
			} else if delta.PartID == "" {
				r.reasoningText.WriteString(delta.Delta)
				r.startReasoningActivity(delta.MessageID, "", r.reasoningText.String(), true)
			} else {
				if r.reasoningParts == nil {
					r.reasoningParts = make(map[string]string)
				}
				r.reasoningParts[delta.PartID] += delta.Delta
				r.startReasoningActivity(delta.MessageID, delta.PartID, r.reasoningParts[delta.PartID], true)
			}
			if r.shell.options.thinking {
				r.status = "reasoning"
			}
		} else if delta.Kind == "reasoning" {
			if !r.reasoningSummary {
				r.reasoningText.WriteString(delta.Delta)
				r.startReasoningActivity(delta.MessageID, "", r.reasoningText.String(), false)
			}
			if r.shell.options.thinking {
				r.status = delta.Kind
			}
		} else if delta.Kind != "reasoning" && delta.Kind != "reasoning_summary" || r.shell.options.thinking {
			r.status = delta.Kind
		}
	case v1.EventSessionStatus:
		payload, err := v1.DecodeEventData(item)
		if err != nil {
			return err
		}
		status := payload.(*v1.SessionStatus)
		switch status.Kind {
		case "running":
			r.busy = true
			r.idleSeen = false
		case "idle":
			r.idleSeen = true
		case "usage":
			r.updateAssistantUsage(status.MessageID, status.Usage)
		case "finish":
		case "interrupted":
			r.busy = false
			r.idleSeen = false
			r.status = ""
			r.interruptCount = 0
		case "error":
			r.busy = false
			r.idleSeen = false
			r.interruptCount = 0
			if err := r.commitCompletedAssistants(""); err != nil {
				return err
			}
			r.status = "error"
		case "provider_error":
			r.status = status.Kind
		case "provider_retry":
			// A retry notice is transient progress, not transcript content: flush
			// it immediately instead of buffering it into the assistant stream.
			message := status.Message
			if message == "" {
				message = "Provider error: servers are overloaded. Retrying at the provider level."
			}
			if r.shell != nil {
				r.shell.commitNotice(message)
			}
		case "status_prompt", "max_turns_reached":
			if r.shell != nil {
				r.shell.commitNotice(status.Message)
			}
		default:
			r.status = status.Kind
		}
	case v1.EventSessionInputAdmitted:
		payload, err := v1.DecodeEventData(item)
		if err != nil {
			return err
		}
		admitted := payload.(*v1.SessionInputAdmitted)
		if admitted.Delivery == "queue" {
			r.addPending(queuedChatInput{inputID: admitted.InputID, messageID: admitted.MessageID, content: admitted.Content})
		}
	case v1.EventSessionInputPromoted:
		payload, err := v1.DecodeEventData(item)
		if err != nil {
			return err
		}
		promoted := payload.(*v1.SessionInputPromoted)
		if err := r.promotePending(promoted.InputID, promoted.MessageID); err != nil {
			return err
		}
		r.status = "working"
	case v1.EventCodeDisplay:
		payload, err := v1.DecodeEventData(item)
		if err != nil {
			return err
		}
		display := payload.(*v1.CodeDisplay)
		if r.shell != nil && r.shell.renderer != nil {
			if err := r.shell.renderer.CommitCodeBlock(terminal.MutedText(chatview.CodeDisplayStatus(*display)), display.Source, display.Language); err != nil {
				return err
			}
			r.borderCommitted = false
		}
	case v1.EventToolOutputDelta:
		payload, err := v1.DecodeEventData(item)
		if err != nil {
			return err
		}
		r.updateToolOutput(payload.(*v1.ToolOutputDelta))
	case "session.context.initialized", "session.context.changed", "session.context.replaced":
		for _, line := range agentsLoadedActivities(item) {
			if r.shell != nil && r.shell.renderer != nil {
				if err := r.shell.renderer.CommitStyled(terminal.MutedText(line)); err != nil {
					return err
				}
			}
		}
	case "session.assistant.started":
		var payload struct {
			MessageID string `json:"message_id"`
		}
		if err := json.Unmarshal(item.Data, &payload); err != nil {
			return err
		}
		accepted, err := r.beginAssistantMessage(payload.MessageID)
		if err != nil {
			return err
		}
		if !accepted {
			return r.settleIdle()
		}
		r.startAssistantActivity(payload.MessageID)
		r.status = "working"
	case "session.assistant.complete", "session.assistant.error", "session.assistant.interrupted":
		var payload struct {
			MessageID string `json:"message_id"`
		}
		if err := json.Unmarshal(item.Data, &payload); err != nil {
			return err
		}
		if payload.MessageID != "" {
			status := "success"
			if item.Type == "session.assistant.error" {
				status = "failure"
			} else if item.Type == "session.assistant.interrupted" {
				status = "interrupted"
			}
			r.completeAssistantActivity(payload.MessageID, status)
			if item.Type == "session.assistant.complete" {
				r.lastCompleteID = payload.MessageID
			}
		}
		if err := r.commitCompletedAssistants(payload.MessageID); err != nil {
			return err
		}
	case "session.tool.pending", "session.tool.running", "session.tool.success", "session.tool.failure", "session.tool.interrupted":
		r.handleToolActivity(item)
		r.status = "tool " + strings.TrimPrefix(item.Type, "session.tool.")
	}
	return r.settleIdle()
}

func (r *enhancedChatRuntime) settleIdle() error {
	if !r.idleSeen || len(r.pending) > 0 {
		return nil
	}
	if err := r.commitCompletedAssistants(""); err != nil {
		return err
	}
	r.busy = false
	r.idleSeen = false
	r.status = ""
	r.interruptCount = 0
	var callback func(TurnComplete) *TurnCompleteDialog
	if r.shell != nil && r.shell.config != nil {
		callback = r.shell.config.OnTurnComplete
	}
	if callback != nil && r.lastCompleteID != "" && r.turnCompleteID != r.lastCompleteID && r.modal == nil {
		r.turnCompleteID = r.lastCompleteID
		dialog := callback(TurnComplete{Session: r.shell.current, Mode: r.shell.selection.agent, MessageID: r.lastCompleteID})
		if dialog == nil {
			return nil
		}
		if dialog.Handle == nil {
			return errors.New("turn completion dialog handler is required")
		}
		if dialog.Markdown != "" {
			if err := r.shell.renderer.CommitMessage(chatview.AssistantMessageIcon+" ", dialog.Markdown, false); err != nil {
				return err
			}
		}
		state, err := r.shell.editor.Start("")
		if err != nil {
			return err
		}
		r.modal = &enhancedModal{
			kind: "turn_complete", state: state, prompt: dialog.Prompt, context: dialog.Context,
			choices: append([]terminal.Candidate(nil), dialog.Choices...), turnComplete: dialog,
		}
		r.inputMode.advance()
	}
	return nil
}

func (r *enhancedChatRuntime) commitCompletedAssistants(messageID string) error {
	ctx, cancel := context.WithTimeout(r.shell.ctx, 5*time.Second)
	defer cancel()
	messages, err := r.shell.api.Messages(ctx, r.shell.current.ID)
	if err != nil {
		return err
	}
	currentStreamSettled := r.streamMessageID == ""
	for _, item := range messages.Items {
		if item.ID == r.streamMessageID {
			currentStreamSettled = item.Status != "active"
		}
		if item.Role != "assistant" || item.Status == "active" || r.knownMessages[item.ID] || messageID != "" && item.ID != messageID {
			continue
		}
		// Keep any retained reasoning summary as Markdown transcript output before
		// the answer, and drop raw chain-of-thought rows without emitting them.
		if err := r.finishAssistantActivity(item.ID, item.Content == ""); err != nil {
			return err
		}
		content := item.Content
		if content == "" && item.Error == "" && item.ID == r.streamMessageID && !r.toolInput {
			content = chatview.AgentEmptyResponseText
		}
		if content != "" {
			item.Content = content
			if err := r.commitAssistantContent(item); err != nil {
				return err
			}
			// The live labeled modeline owns the response/input boundary. Leaving
			// it uncommitted keeps that boundary heavy and avoids a thin rule above
			// the modeline once the response settles.
			r.borderCommitted = false
		}
		if item.Error != "" {
			r.commitError(item.Error)
		}
		r.knownMessages[item.ID] = true
		delete(r.unsyncedMessages, item.ID)
	}
	if currentStreamSettled && (messageID == "" || messageID == r.streamMessageID) {
		r.streamed.Reset()
		r.resetReasoning()
		r.toolInput = false
		r.streamMessageID = ""
	}
	if err := r.flushCompletedActivities(); err != nil {
		return err
	}
	r.status = ""
	return nil
}

func (r *enhancedChatRuntime) commitAssistantContent(item v1.Message) error {
	if item.ID != r.streamMessageID {
		return r.shell.renderer.CommitMessage(chatview.AssistantMessageIcon+" ", item.Content, false)
	}
	err := r.shell.renderer.CommitStream(terminal.StreamMessage{ID: item.ID, Prefix: chatview.AssistantMessageIcon + " ", Text: item.Content}, false)
	if terminal.RenderErrorClass(err) == "stream_text_changed" {
		// Some live text may already be permanent terminal scrollback and cannot
		// be replaced. Close the stream with the text the user has seen instead
		// of terminating enhanced chat. The repository remains authoritative for
		// future history and session resumes.
		return r.shell.renderer.CommitDisplayedStream(item.ID, false)
	}
	return err
}

// beginAssistantMessage synchronizes the CLI's cumulative buffer with the
// renderer before accepting a different message ID. Durable lifecycle events
// and disposable provider deltas travel through separate queues, so events for
// different assistants can be observed out of order. The repository is
// authoritative at this boundary: settle the prior assistant from its stored
// final message instead of discarding the local prefix.
//
// If the repository still considers the current assistant active, the
// conflicting event is stale or has raced persistence. Ignore it and retain
// the current stream. Returning an error here used to close and reconnect the
// event stream; the same conflicting event could then produce an endless row
// of "previous assistant message is still active" errors.
func (r *enhancedChatRuntime) beginAssistantMessage(messageID string) (bool, error) {
	if messageID == "" || messageID == r.streamMessageID {
		return true, nil
	}
	if r.streamMessageID != "" {
		previousID := r.streamMessageID
		if err := r.commitCompletedAssistants(previousID); err != nil {
			return false, err
		}
		if r.streamMessageID == previousID {
			return false, nil
		}
	}
	r.streamed.Reset()
	r.resetReasoning()
	r.toolInput = false
	r.streamMessageID = messageID
	return true, nil
}
