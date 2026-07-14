package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	v1 "github.com/amirulashraf/parrot-coder/internal/api/v1"
	"github.com/amirulashraf/parrot-coder/internal/client"
	"github.com/amirulashraf/parrot-coder/internal/terminal"
)

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
var errInvalidModalAnswer = errors.New("invalid modal answer")

func isExecutionHaltKey(key terminal.Key) bool {
	return key.Kind == terminal.KeyEscape || key.Kind == terminal.KeyInterrupt
}

type enhancedKeyResult struct {
	key   terminal.Key
	err   error
	ack   chan struct{}
	epoch int64
}

type enhancedKeyPump struct {
	cancel context.CancelFunc
	done   chan struct{}
	events <-chan enhancedKeyResult
}

type enhancedInputMode struct {
	mu    sync.RWMutex
	epoch int64
}

func (m *enhancedInputMode) current() int64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.epoch
}

func (m *enhancedInputMode) advance() {
	m.mu.Lock()
	m.epoch++
	m.mu.Unlock()
}

func startEnhancedKeyPump(parent context.Context, decoder *terminal.KeyDecoder, modes ...*enhancedInputMode) *enhancedKeyPump {
	ctx, cancel := context.WithCancel(parent)
	events := make(chan enhancedKeyResult, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer close(events)
		for {
			key, err := decoder.ReadKey(ctx)
			epoch := int64(0)
			var mode *enhancedInputMode
			if len(modes) > 0 {
				mode = modes[0]
			}
			if mode != nil {
				mode.mu.RLock()
				epoch = mode.epoch
			}
			ack := make(chan struct{})
			select {
			case events <- enhancedKeyResult{key: key, err: err, ack: ack, epoch: epoch}:
			case <-ctx.Done():
				if mode != nil {
					mode.mu.RUnlock()
				}
				return
			}
			if mode != nil {
				mode.mu.RUnlock()
			}
			select {
			case <-ack:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	return &enhancedKeyPump{cancel: cancel, done: done, events: events}
}

func (p *enhancedKeyPump) stop() {
	if p == nil {
		return
	}
	p.cancel()
	<-p.done
}

type enhancedSessionEvent struct {
	generation int
	event      v1.Event
	err        error
}

type queuedChatInput struct {
	inputID   string
	messageID string
	content   string
}

type enhancedActivityItem struct {
	id        string
	label     string
	status    string
	started   time.Time
	ended     time.Time
	terminal  bool
	reasoning bool
}

type enhancedModal struct {
	kind       string
	prompt     string
	context    []string
	state      *terminal.EditorState
	permission *v1.Permission
	question   *v1.QuestionRequest
	index      int
	selected   int
	choices    []terminal.Candidate
	answers    []v1.Answer
}

type enhancedInputOutcome struct {
	exit   bool
	code   int
	retain bool
}

type enhancedChatRuntime struct {
	shell *chatShell
	state *terminal.EditorState

	busy            bool
	idleSeen        bool
	status          string
	spinner         int
	interruptCount  int
	streamed        strings.Builder
	reasoningText   strings.Builder
	streamMessageID string
	pending         []queuedChatInput
	modal           *enhancedModal
	inputMode       enhancedInputMode
	knownMessages   map[string]bool
	activity        []enhancedActivityItem
	completedTools  []enhancedActivityItem
	borderCommitted bool

	stream           *client.EventStream
	streamSessionID  string
	eventSessionID   string
	eventAfter       int64
	streamGeneration int
	streamDone       chan struct{}
	streamExited     chan struct{}
	events           chan enhancedSessionEvent
}

func (s *chatShell) runEnhanced(first string) int {
	state, err := s.editor.Start("")
	if err != nil {
		s.commitError(err.Error())
		return exitError
	}
	runtime := &enhancedChatRuntime{
		shell: s, state: state, knownMessages: make(map[string]bool),
		events: make(chan enhancedSessionEvent, 128),
	}
	defer runtime.stopStream()

	if first != "" {
		outcome := runtime.handleInput(first)
		if outcome.retain {
			_ = state.Reset(first)
		}
		if outcome.exit {
			return outcome.code
		}
	}
	if err := runtime.render(); err != nil {
		s.commitError(err.Error())
		return exitError
	}

	pump := startEnhancedKeyPump(s.ctx, s.decoder, &runtime.inputMode)
	defer func() { pump.stop() }()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	interrupts, _ := s.ctx.Value(interruptKey{}).(<-chan os.Signal)
	ticks := 0

	for {
		select {
		case <-s.ctx.Done():
			return exitInterrupt
		case <-interrupts:
			if err := runtime.requestInterrupt(); errors.Is(err, errSecondInterrupt) {
				return exitInterrupt
			}
			_ = runtime.render()
		case result, ok := <-pump.events:
			pumpStopped := false
			if !ok {
				return exitInterrupt
			}
			if result.err != nil {
				if errors.Is(result.err, context.Canceled) {
					continue
				}
				runtime.commitError(result.err.Error())
				return exitError
			}
			if result.key.Kind == terminal.KeyModeSwitch {
				close(result.ack)
				if err := runtime.cycleMode(); err != nil {
					runtime.status = err.Error()
				}
				if err := runtime.render(); err != nil {
					return exitError
				}
				continue
			}
			if runtime.busy && isExecutionHaltKey(result.key) {
				close(result.ack)
				runtime.cancelModal()
				if err := runtime.requestInterrupt(); errors.Is(err, errSecondInterrupt) {
					return exitInterrupt
				}
				if err := runtime.render(); err != nil {
					return exitError
				}
				continue
			}

			targetState := state
			modalAction := false
			if runtime.modal != nil && result.epoch == runtime.inputMode.current() {
				targetState = runtime.modal.state
				modalAction = true
			}
			if modalAction && runtime.modal.kind == "permission" {
				done, err := runtime.handlePermissionModalKey(result.key)
				close(result.ack)
				if done {
					pump.stop()
					pumpStopped = true
				}
				if err != nil {
					if errors.Is(err, errSecondInterrupt) {
						return exitInterrupt
					}
					runtime.commitError(err.Error())
					runtime.cancelModal()
				}
				if !done {
					// Ignore text-editing keys; the permission prompt is a selection.
				}
				if err := runtime.render(); err != nil {
					return exitError
				}
				if pumpStopped {
					pump = startEnhancedKeyPump(s.ctx, s.decoder, &runtime.inputMode)
				}
				continue
			}
			if modalAction && runtime.modal.kind == "question" && len(runtime.modal.choices) > 0 {
				handled, err := runtime.handleQuestionModalKey(result.key)
				if handled {
					close(result.ack)
					if err != nil {
						runtime.status = err.Error()
					}
					if err := runtime.render(); err != nil {
						return exitError
					}
					continue
				}
			}
			action := targetState.Handle(result.key)
			if !action.Done {
				close(result.ack)
				if err := runtime.render(); err != nil {
					return exitError
				}
				continue
			}

			pump.stop()
			outcome := enhancedInputOutcome{}
			if modalAction {
				switch {
				case errors.Is(action.Err, terminal.ErrInterrupted):
					runtime.cancelModal()
					if err := runtime.requestInterrupt(); errors.Is(err, errSecondInterrupt) {
						return exitInterrupt
					}
				case errors.Is(action.Err, terminal.ErrCanceled), errors.Is(action.Err, io.EOF):
					runtime.cancelModal()
				case action.Err != nil:
					runtime.commitError(action.Err.Error())
					runtime.cancelModal()
				default:
					if err := runtime.answerModal(action.Value); err != nil {
						if errors.Is(err, errInvalidModalAnswer) {
							runtime.status = err.Error()
							_ = runtime.modal.state.Reset("")
						} else {
							runtime.commitError(err.Error())
							runtime.cancelModal()
						}
					}
				}
			} else {
				switch {
				case errors.Is(action.Err, terminal.ErrInterrupted):
					if state.Value() != "" {
						_ = state.Reset("")
					} else if runtime.busy {
						if err := runtime.requestInterrupt(); errors.Is(err, errSecondInterrupt) {
							return exitInterrupt
						}
					}
				case errors.Is(action.Err, terminal.ErrCanceled):
					_ = state.Reset("")
				case errors.Is(action.Err, io.EOF):
					if !runtime.busy {
						return exitOK
					}
					runtime.status = "agent is still working"
				case action.Err != nil:
					runtime.commitError(action.Err.Error())
					_ = runtime.ensureInputBorder()
					_ = state.Reset("")
				case strings.TrimSpace(action.Value) != "":
					outcome = runtime.handleInput(action.Value)
					if !outcome.retain {
						_ = state.Reset("")
					}
				default:
					_ = state.Reset("")
				}
			}
			if outcome.exit {
				return outcome.code
			}
			pump = startEnhancedKeyPump(s.ctx, s.decoder, &runtime.inputMode)
			if err := runtime.render(); err != nil {
				return exitError
			}
		case result := <-runtime.events:
			if result.generation != runtime.streamGeneration {
				continue
			}
			if result.err != nil {
				runtime.status = "event stream closed"
				runtime.stopStream()
			} else if err := runtime.handleEvent(result.event); err != nil {
				runtime.commitError(err.Error())
				_ = runtime.ensureInputBorder()
				runtime.stopStream()
			} else if result.event.Sequence != nil {
				runtime.eventAfter = *result.event.Sequence
			}
			if err := runtime.render(); err != nil {
				return exitError
			}
		case <-ticker.C:
			ticks++
			if runtime.busy {
				runtime.spinner = (runtime.spinner + 1) % len(spinnerFrames)
			}
			if runtime.busy && ticks%5 == 0 {
				runtime.detectModal()
			}
			if runtime.busy && ticks%10 == 0 {
				var err error
				if runtime.stream == nil {
					err = runtime.ensureStream(s.current.ID)
				} else {
					err = runtime.reconcileRuntime()
				}
				if err != nil {
					runtime.status = "runtime reconciliation failed"
				}
			}
			if err := runtime.render(); err != nil {
				return exitError
			}
		}
	}
}

func (r *enhancedChatRuntime) render() error {
	pending := make([]string, len(r.pending))
	for i, item := range r.pending {
		pending[i] = item.content
	}
	message := r.streamed.String()
	var stream *terminal.StreamMessage
	if message != "" && r.streamMessageID != "" {
		stream = &terminal.StreamMessage{ID: r.streamMessageID, Prefix: "- ", Text: message}
	}
	prompt := r.state.PromptState()
	if r.modal != nil {
		prompt = r.modal.state.PromptState()
		prompt.Prefix = r.modal.prompt
		if len(r.modal.choices) > 0 {
			prompt.Completions = r.modal.choices
			prompt.Selected = r.modal.selected
		}
		if r.modal.kind == "permission" {
			prompt.Text = ""
			prompt.Cursor = 0
		}
	}
	busy := r.busy
	if r.modal != nil && r.modal.kind == "permission" {
		busy = false
	}
	return r.shell.renderer.Frame(terminal.LiveFrame{
		Stream: stream, Context: r.modalContext(), Activity: r.activityRows(time.Now(), r.shell.renderer.Columns()), Status: r.status, Pending: pending,
		InputLeft: r.inputModeLabel(), InputRight: r.shell.selection.modelLabel(),
		Prompt: prompt, Busy: busy, Spinner: spinnerFrames[r.spinner],
		ShowDivider: r.modal != nil || message != "" || r.status != "" || len(r.activity) > 0 || !r.borderCommitted,
	})
}

func (r *enhancedChatRuntime) modalContext() []string {
	if r.modal == nil {
		return nil
	}
	return r.modal.context
}

func (r *enhancedChatRuntime) activityRows(now time.Time, columns int) []string {
	if len(r.activity) == 0 {
		return nil
	}
	rows := make([]string, 0, len(r.activity))
	start := 0
	if len(r.activity) > 4 {
		start = len(r.activity) - 4
	}
	for _, item := range r.activity[start:] {
		if item.reasoning {
			rows = append(rows, formatReasoningActivity(item, now, columns))
		} else {
			rows = append(rows, formatActivity(item, now))
		}
	}
	return rows
}

func formatReasoningActivity(item enhancedActivityItem, now time.Time, columns int) string {
	const prefix = "Thought: "
	elapsed := now.Sub(item.started)
	if elapsed < 0 {
		elapsed = 0
	}
	suffix := fmt.Sprintf(" · %.1fs", elapsed.Seconds())
	width := max(1, columns-len(prefix)-len(suffix)-1)
	label := item.label
	if strings.TrimSpace(label) == "" {
		label = "Thinking…"
	}
	offset := int(elapsed / (100 * time.Millisecond))
	return prefix + terminal.Marquee(label, width, offset) + suffix
}

func formatActivity(item enhancedActivityItem, now time.Time) string {
	end := now
	if !item.ended.IsZero() {
		end = item.ended
	}
	elapsed := end.Sub(item.started)
	if elapsed < 0 {
		elapsed = 0
	}
	return fmt.Sprintf("%s: %s · %.1fs", activityTitle(item.status), item.label, elapsed.Seconds())
}

func activityTitle(status string) string {
	switch status {
	case "thinking":
		return "Thought"
	case "pending":
		return "Queued tool"
	case "running":
		return "Tool"
	case "success":
		return "Done"
	case "failure":
		return "Failed"
	case "interrupted":
		return "Interrupted"
	default:
		return "Status"
	}
}

func (r *enhancedChatRuntime) startAssistantActivity(messageID string) {
	if messageID == "" {
		messageID = "assistant"
	}
	r.upsertActivity(messageID, "Verifying status and context", "thinking", false, false)
}

func (r *enhancedChatRuntime) startReasoningActivity(messageID, label string) {
	if messageID == "" {
		messageID = "assistant"
	}
	r.upsertActivity(messageID, label, "thinking", false, true)
}

func (r *enhancedChatRuntime) upsertActivity(id, label, status string, terminal, reasoning bool) {
	now := time.Now()
	for i := range r.activity {
		if r.activity[i].id != id {
			continue
		}
		previous := r.activity[i].status
		if label != "" {
			r.activity[i].label = label
		}
		r.activity[i].status = status
		r.activity[i].terminal = terminal
		r.activity[i].reasoning = reasoning
		if r.activity[i].started.IsZero() || status == "running" && previous == "pending" {
			r.activity[i].started = now
		}
		if terminal {
			r.activity[i].ended = now
		} else {
			r.activity[i].ended = time.Time{}
		}
		return
	}
	if label == "" {
		label = id
	}
	r.activity = append(r.activity, enhancedActivityItem{id: id, label: label, status: status, started: now, terminal: terminal, reasoning: reasoning})
	if len(r.activity) > 12 {
		r.activity = r.activity[len(r.activity)-12:]
	}
}

func (r *enhancedChatRuntime) queueCompletedTool(id string) {
	for i := range r.activity {
		if r.activity[i].id != id {
			continue
		}
		r.completedTools = append(r.completedTools, r.activity[i])
		r.activity = append(r.activity[:i], r.activity[i+1:]...)
		return
	}
}

// flushCompletedTools commits terminal tool statuses only between assistant
// messages. This keeps an activity line from splitting a streaming response.
func (r *enhancedChatRuntime) flushCompletedTools() error {
	if r.streamMessageID != "" || r.streamed.Len() != 0 || r.shell == nil || r.shell.renderer == nil {
		return nil
	}
	for len(r.completedTools) > 0 {
		item := r.completedTools[0]
		if err := r.shell.renderer.Commit(formatActivity(item, time.Now())); err != nil {
			return err
		}
		r.completedTools = r.completedTools[1:]
		r.borderCommitted = false
	}
	return nil
}

func (r *enhancedChatRuntime) inputModeLabel() string {
	mode := r.shell.selection.agent
	if mode == "" {
		mode = "unknown"
	}
	return "mode: " + mode
}

func (r *enhancedChatRuntime) cycleMode() error {
	previous := r.shell.selection.agent
	next, err := r.shell.nextAgent(previous)
	if err != nil {
		return err
	}
	if next == previous {
		r.status = "mode: " + next
		return nil
	}
	if err := r.shell.applyAgent(next, false); err != nil {
		return err
	}
	r.status = "mode: " + next
	return nil
}

func (r *enhancedChatRuntime) ensureInputBorder() error {
	if r.borderCommitted {
		return nil
	}
	if err := r.shell.renderer.CommitDivider(); err != nil {
		return err
	}
	r.borderCommitted = true
	return nil
}

func (r *enhancedChatRuntime) commitUser(content string) error {
	if err := r.ensureInputBorder(); err != nil {
		return err
	}
	if err := r.shell.commitUser(content); err != nil {
		return err
	}
	r.borderCommitted = true
	return nil
}

func (r *enhancedChatRuntime) commitError(message string) {
	r.shell.commitError(message)
	r.borderCommitted = false
	_ = r.ensureInputBorder()
}

func (r *enhancedChatRuntime) detectModal() {
	if r.modal != nil || r.shell.current.ID == "" {
		return
	}
	permissions, permissionErr := r.shell.api.Permissions(r.shell.ctx, r.shell.current.ID)
	if permissionErr != nil {
		r.status = "permission check failed"
	} else if len(permissions.Items) > 0 {
		item := permissions.Items[0]
		state, err := r.shell.editor.Start("")
		if err != nil {
			r.status = "permission editor unavailable"
			return
		}
		r.modal = &enhancedModal{
			kind: "permission", state: state, permission: &item,
			prompt: "permission decision: ", choices: permissionChoices(),
		}
		r.inputMode.advance()
		r.showPermissionContext(item)
		return
	}
	questions, questionErr := r.shell.api.Questions(r.shell.ctx, r.shell.current.ID)
	if questionErr != nil {
		r.status = "question check failed"
	} else if len(questions.Items) > 0 && len(questions.Items[0].Questions) > 0 {
		request := questions.Items[0]
		state, err := r.shell.editor.Start("")
		if err != nil {
			r.status = "question editor unavailable"
			return
		}
		r.modal = &enhancedModal{kind: "question", state: state, question: &request}
		r.inputMode.advance()
		r.updateQuestionPrompt()
		r.showQuestionContext(request.Questions[0])
	}
}

func permissionChoices() []terminal.Candidate {
	return []terminal.Candidate{
		{Value: "yes", Description: "Allow this request once"},
		{Value: "no", Description: "Deny this request"},
		{Value: "allow all for session", Description: "Allow matching requests for this session"},
		{Value: "allow all for workspace", Description: "Allow matching requests for this workspace"},
		{Value: "allow all for process", Description: "Allow matching requests until Parrot exits"},
		{Value: "enable yolo", Description: "Disable all permission checks for this session"},
	}
}

func permissionReplyFromAnswer(value string) v1.PermissionReply {
	answer := strings.ToLower(strings.TrimSpace(value))
	reply := v1.PermissionReply{Decision: "deny"}
	switch answer {
	case "y", "yes", "once":
		reply.Decision = "allow"
	case "session", "allow all for session":
		reply.Decision, reply.Scope = "allow", "session"
	case "workspace", "allow all for workspace":
		reply.Decision, reply.Scope = "allow", "workspace"
	case "process", "allow all for process":
		reply.Decision, reply.Scope = "allow", "process"
	case "yolo", "enable yolo":
		reply.Decision, reply.Scope = "allow", "yolo"
	}
	return reply
}

func (r *enhancedChatRuntime) handlePermissionModalKey(key terminal.Key) (bool, error) {
	if r.modal == nil || r.modal.kind != "permission" {
		return false, nil
	}
	choices := r.modal.choices
	if len(choices) == 0 {
		choices = permissionChoices()
		r.modal.choices = choices
	}
	switch key.Kind {
	case terminal.KeyUp:
		r.modal.selected = (r.modal.selected - 1 + len(choices)) % len(choices)
	case terminal.KeyDown, terminal.KeyTab:
		r.modal.selected = (r.modal.selected + 1) % len(choices)
	case terminal.KeyEnter, terminal.KeyNewline:
		selected := r.modal.selected
		if selected < 0 {
			selected = 0
		} else if selected >= len(choices) {
			selected = len(choices) - 1
		}
		return true, r.answerModal(choices[selected].Value)
	case terminal.KeyEscape, terminal.KeyEOF:
		r.cancelModal()
		return true, nil
	case terminal.KeyInterrupt:
		r.cancelModal()
		return true, r.requestInterrupt()
	case terminal.KeyRune:
		switch strings.ToLower(string(key.Rune)) {
		case "y":
			return true, r.answerModal("yes")
		case "n":
			return true, r.answerModal("no")
		case "s":
			return true, r.answerModal("allow all for session")
		case "w":
			return true, r.answerModal("allow all for workspace")
		case "p":
			return true, r.answerModal("allow all for process")
		case "o":
			return true, r.answerModal("enable yolo")
		}
	}
	return false, nil
}

// handleQuestionModalKey makes option-bearing questions use the expandable
// input selection buffer. Enter first copies the option into the input and
// collapses the menu; pressing Enter again submits it through the editor.
func (r *enhancedChatRuntime) handleQuestionModalKey(key terminal.Key) (bool, error) {
	if r.modal == nil || r.modal.kind != "question" || len(r.modal.choices) == 0 {
		return false, nil
	}
	switch key.Kind {
	case terminal.KeyUp:
		r.modal.selected = (r.modal.selected - 1 + len(r.modal.choices)) % len(r.modal.choices)
		return true, nil
	case terminal.KeyDown, terminal.KeyTab:
		r.modal.selected = (r.modal.selected + 1) % len(r.modal.choices)
		return true, nil
	case terminal.KeyEnter:
		selected := r.modal.selected
		if selected < 0 {
			selected = 0
		} else if selected >= len(r.modal.choices) {
			selected = len(r.modal.choices) - 1
		}
		if err := r.modal.state.Reset(r.modal.choices[selected].Value); err != nil {
			return true, err
		}
		r.modal.choices = nil
		return true, nil
	}
	return false, nil
}

func (r *enhancedChatRuntime) updateQuestionPrompt() {
	if r.modal == nil || r.modal.question == nil || r.modal.index >= len(r.modal.question.Questions) {
		return
	}
	question := r.modal.question.Questions[r.modal.index]
	r.modal.choices = make([]terminal.Candidate, 0, len(question.Options))
	r.modal.selected = 0
	options := make([]string, len(question.Options))
	for i, option := range question.Options {
		options[i] = option.ID
		description := option.Label
		if option.Description != "" {
			description += " - " + option.Description
		}
		r.modal.choices = append(r.modal.choices, terminal.Candidate{Value: option.ID, Description: description})
	}
	suffix := ""
	if len(options) > 0 {
		suffix = " [" + strings.Join(options, "/") + "]"
	}
	r.modal.prompt = question.Prompt + suffix + ": "
}

func (r *enhancedChatRuntime) showPermissionContext(permission v1.Permission) {
	if r.modal == nil {
		return
	}
	context := []string{"permission: " + permission.ToolID, "reason: " + permission.Reason}
	for _, resource := range permission.Resources {
		context = append(context, fmt.Sprintf("resource: %s %s %s", resource.Kind, resource.Operation, resource.Identifier))
	}
	r.modal.context = context
}

func (r *enhancedChatRuntime) showQuestionContext(question v1.Question) {
	if r.modal == nil {
		return
	}
	r.modal.context = []string{"question: " + question.Prompt}
}

func (r *enhancedChatRuntime) answerModal(value string) error {
	if r.modal == nil {
		return nil
	}
	modal := r.modal
	switch modal.kind {
	case "permission":
		reply := permissionReplyFromAnswer(value)
		if err := r.shell.api.ReplyPermission(r.shell.ctx, r.shell.current.ID, modal.permission.ID, reply); err != nil {
			return err
		}
		r.finishModal()
	case "question":
		if strings.TrimSpace(value) == "" {
			if err := r.shell.api.ReplyQuestion(r.shell.ctx, r.shell.current.ID, modal.question.ID, v1.QuestionReply{Reject: true}); err != nil {
				return err
			}
			r.finishModal()
			return nil
		}
		question := modal.question.Questions[modal.index]
		answer := v1.Answer{QuestionID: question.ID}
		values := strings.FieldsFunc(strings.TrimSpace(value), func(r rune) bool { return r == ',' || r == ' ' || r == '\t' })
		matched := 0
		for _, candidate := range values {
			for _, option := range question.Options {
				if candidate == option.ID {
					answer.OptionIDs = append(answer.OptionIDs, candidate)
					matched++
				}
			}
		}
		if matched > 0 && matched != len(values) {
			return fmt.Errorf("%w: use only listed option IDs", errInvalidModalAnswer)
		}
		if len(answer.OptionIDs) == 0 {
			if !question.Custom {
				return fmt.Errorf("%w: choose one of the listed option IDs", errInvalidModalAnswer)
			}
			answer.Custom = strings.TrimSpace(value)
		} else if !question.Multiple && len(answer.OptionIDs) > 1 {
			return fmt.Errorf("%w: choose one option", errInvalidModalAnswer)
		}
		modal.answers = append(modal.answers, answer)
		modal.index++
		if modal.index < len(modal.question.Questions) {
			r.updateQuestionPrompt()
			r.showQuestionContext(modal.question.Questions[modal.index])
			return modal.state.Reset("")
		}
		if err := r.shell.api.ReplyQuestion(r.shell.ctx, r.shell.current.ID, modal.question.ID, v1.QuestionReply{Answers: modal.answers}); err != nil {
			return err
		}
		r.finishModal()
	}
	return nil
}

func (r *enhancedChatRuntime) cancelModal() {
	if r.modal == nil {
		return
	}
	modal := r.modal
	if modal.kind == "permission" && modal.permission != nil {
		_ = r.shell.api.ReplyPermission(r.shell.ctx, r.shell.current.ID, modal.permission.ID, v1.PermissionReply{Decision: "deny"})
	} else if modal.kind == "question" && modal.question != nil {
		_ = r.shell.api.ReplyQuestion(r.shell.ctx, r.shell.current.ID, modal.question.ID, v1.QuestionReply{Reject: true})
	}
	r.finishModal()
}

func (r *enhancedChatRuntime) finishModal() {
	if r.modal == nil {
		return
	}
	r.modal = nil
	r.inputMode.advance()
}

func (r *enhancedChatRuntime) handleInput(value string) enhancedInputOutcome {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return enhancedInputOutcome{}
	}
	if strings.HasPrefix(trimmed, "/") {
		name, arguments := slashParts(trimmed)
		if isBuiltinSlash(name) {
			return r.handleBuiltin(name, arguments)
		}
		expansion, err := r.shell.commands.Expand(strings.TrimPrefix(name, "/"), arguments)
		if err != nil {
			r.commitError(fmt.Sprintf("unknown slash command %q: %v", name, err))
			_ = r.ensureInputBorder()
			return enhancedInputOutcome{}
		}
		if r.busy && !expansion.Subtask && (expansion.Agent != "" || expansion.Model != "") {
			r.commitError("custom command changes model or agent while the session is active")
			_ = r.ensureInputBorder()
			return enhancedInputOutcome{}
		}
		if !r.busy && !expansion.Subtask {
			if expansion.Agent != "" {
				if err := r.shell.selectAgent(expansion.Agent); err != nil {
					r.commitError(err.Error())
					_ = r.ensureInputBorder()
					return enhancedInputOutcome{}
				}
				r.borderCommitted = false
			}
			if expansion.Model != "" {
				if err := r.shell.selectModel(expansion.Model); err != nil {
					r.commitError(err.Error())
					_ = r.ensureInputBorder()
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
	if err := r.submitPrompt(value); err != nil {
		if !errors.Is(err, terminal.ErrCanceled) && !errors.Is(err, terminal.ErrInterrupted) {
			r.commitError(err.Error())
			_ = r.ensureInputBorder()
		}
		return enhancedInputOutcome{retain: true}
	}
	return enhancedInputOutcome{}
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
		_ = r.ensureInputBorder()
		return enhancedInputOutcome{}
	}
	if name == "/resume" {
		item, err := r.shell.chooseSession(arguments)
		if err != nil {
			if !errors.Is(err, terminal.ErrCanceled) && !errors.Is(err, terminal.ErrInterrupted) {
				r.commitError(err.Error())
			}
			_ = r.ensureInputBorder()
			return enhancedInputOutcome{}
		}
		r.shell.current = item
		r.shell.selection = selectionFromSession(item, r.shell.selection.agent)
		if err := r.ensureStream(item.ID); err != nil {
			r.commitError(err.Error())
			_ = r.ensureInputBorder()
			return enhancedInputOutcome{}
		}
		resumer, ok := r.shell.api.(resumableClient)
		if !ok {
			r.commitError("connected server does not support explicit resume")
			_ = r.ensureInputBorder()
			return enhancedInputOutcome{}
		}
		if err := resumer.Resume(r.shell.ctx, item.ID); err != nil {
			r.commitError(err.Error())
			_ = r.ensureInputBorder()
			return enhancedInputOutcome{}
		}
		r.busy = true
		r.status = "resuming"
		return enhancedInputOutcome{}
	}
	previousSession := r.shell.current.ID
	exit, code := r.shell.slash(name, arguments)
	if name == "/new" || name == "/session" || name == "/connect" || r.shell.current.ID != previousSession {
		r.stopStream()
	}
	r.borderCommitted = false
	_ = r.ensureInputBorder()
	return enhancedInputOutcome{exit: exit, code: code}
}

func safeBusySlash(name string) bool {
	switch name {
	case "/help", "/models", "/agents", "/sessions", "/status", "/thinking", "/exit":
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
			MessageID: messageID, Content: content, Delivery: "queue",
		})
		if err != nil {
			return err
		}
		r.addPending(queuedChatInput{inputID: accepted.InputID, messageID: accepted.MessageID, content: content})
		r.status = "queued"
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
		item, err := createChatSession(r.shell.ctx, r.shell.api, r.shell.projectID, content, r.shell.selection)
		if err != nil {
			return err
		}
		r.shell.current = item
	}
	if err := r.ensureStream(r.shell.current.ID); err != nil {
		return err
	}
	messageID, err := opaqueID("msg")
	if err != nil {
		return err
	}
	if _, err := r.shell.api.Prompt(r.shell.ctx, r.shell.current.ID, v1.PromptRequest{
		MessageID: messageID, Content: content, Delivery: "steer",
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

func (r *enhancedChatRuntime) ensureStream(sessionID string) error {
	if r.stream != nil && r.streamSessionID == sessionID {
		return nil
	}
	r.stopStream()
	messages, err := r.shell.api.Messages(r.shell.ctx, sessionID)
	if err != nil {
		return err
	}
	newSession := r.eventSessionID != sessionID
	if newSession {
		r.eventSessionID = sessionID
		r.eventAfter = -1
		r.knownMessages = make(map[string]bool, len(messages.Items))
		for _, item := range messages.Items {
			if item.Sequence > r.eventAfter {
				r.eventAfter = item.Sequence
			}
			if item.Status != "active" {
				r.knownMessages[item.ID] = true
			}
		}
	}
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

func (r *enhancedChatRuntime) handleEvent(item v1.Event) error {
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
		if delta.MessageID != "" && delta.MessageID != r.streamMessageID {
			r.streamed.Reset()
			r.reasoningText.Reset()
			r.streamMessageID = delta.MessageID
		}
		if delta.Kind == "text" {
			r.streamed.WriteString(delta.Delta)
			r.status = ""
		} else if delta.Kind == "reasoning" {
			r.reasoningText.WriteString(delta.Delta)
			r.startReasoningActivity(delta.MessageID, r.reasoningText.String())
			if r.shell.options.thinking {
				r.status = delta.Kind
			}
		} else if delta.Kind != "reasoning" || r.shell.options.thinking {
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
		case "finish", "usage":
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
	case "session.assistant.started":
		var payload struct {
			MessageID string `json:"message_id"`
		}
		if err := json.Unmarshal(item.Data, &payload); err != nil {
			return err
		}
		if payload.MessageID != "" && payload.MessageID != r.streamMessageID {
			r.streamed.Reset()
			r.reasoningText.Reset()
			r.streamMessageID = payload.MessageID
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
			r.upsertActivity(payload.MessageID, "Verifying status and context", status, true, false)
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

func (r *enhancedChatRuntime) handleToolActivity(item v1.Event) {
	callID, name := toolActivityPayload(item.Data)
	if callID == "" {
		callID = fmt.Sprintf("tool-%d", time.Now().UnixNano())
	}
	status := strings.TrimPrefix(item.Type, "session.tool.")
	terminal := status == "success" || status == "failure" || status == "interrupted"
	r.upsertActivity(callID, name, status, terminal, false)
	if terminal {
		r.queueCompletedTool(callID)
		if err := r.flushCompletedTools(); err != nil {
			r.status = "tool activity flush failed"
		}
	}
}

func toolActivityPayload(data json.RawMessage) (string, string) {
	var raw map[string]any
	if len(data) == 0 || json.Unmarshal(data, &raw) != nil {
		return "", ""
	}
	callID := firstString(raw, "call_id", "callID", "id", "ID")
	name := firstString(raw, "name", "Name", "tool", "tool_name", "toolID", "tool_id")
	if nested, ok := raw["call"].(map[string]any); ok {
		if callID == "" {
			callID = firstString(nested, "call_id", "callID", "id", "ID")
		}
		if name == "" {
			name = firstString(nested, "name", "Name", "tool", "tool_name", "toolID", "tool_id")
		}
	}
	return callID, name
}

func firstString(raw map[string]any, keys ...string) string {
	for _, key := range keys {
		value, ok := raw[key]
		if !ok {
			continue
		}
		if text, ok := value.(string); ok && text != "" {
			return text
		}
	}
	return ""
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
	return nil
}

func (r *enhancedChatRuntime) commitCompletedAssistants(messageID string) error {
	ctx, cancel := context.WithTimeout(r.shell.ctx, 5*time.Second)
	defer cancel()
	messages, err := r.shell.api.Messages(ctx, r.shell.current.ID)
	if err != nil {
		return err
	}
	for _, item := range messages.Items {
		if item.Role != "assistant" || item.Status == "active" || r.knownMessages[item.ID] || messageID != "" && item.ID != messageID {
			continue
		}
		if item.Content != "" {
			if item.ID == r.streamMessageID {
				if err := r.shell.renderer.CommitStream(terminal.StreamMessage{ID: item.ID, Prefix: "- ", Text: item.Content}, true); err != nil {
					return err
				}
			} else if err := r.shell.renderer.CommitMessage("- ", item.Content, true); err != nil {
				return err
			}
			r.borderCommitted = true
		}
		if item.Error != "" {
			r.commitError(item.Error)
		}
		r.knownMessages[item.ID] = true
	}
	if messageID == "" || messageID == r.streamMessageID {
		r.streamed.Reset()
		r.reasoningText.Reset()
		r.streamMessageID = ""
	}
	if err := r.flushCompletedTools(); err != nil {
		return err
	}
	r.status = ""
	return nil
}
