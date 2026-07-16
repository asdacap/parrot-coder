package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	v1 "github.com/amirulashraf/parrot-coder/internal/api/v1"
	"github.com/amirulashraf/parrot-coder/internal/client"
	"github.com/amirulashraf/parrot-coder/internal/terminal"
)

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
var errInvalidModalAnswer = errors.New("invalid modal answer")

const maxToolBlockLines = 10

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
	id               string
	messageID        string
	label            string
	toolName         string
	style            terminal.TextStyle
	input            map[string]any
	error            string
	status           string
	tokens           int
	hasUsage         bool
	outputTokens     int
	reasoningTokens  int
	toolUses         int
	started          time.Time
	ended            time.Time
	terminal         bool
	reasoning        bool
	reasoningSummary bool
	block            string
}

type enhancedModal struct {
	kind            string
	prompt          string
	context         []string
	state           *terminal.EditorState
	permission      *v1.Permission
	question        *v1.QuestionRequest
	index           int
	selected        int
	choices         []terminal.Candidate
	answers         []v1.Answer
	customInput     bool
	selectedOptions map[string]bool
}

type enhancedInputOutcome struct {
	exit   bool
	code   int
	retain bool
}

type enhancedChatRuntime struct {
	shell *chatShell
	state *terminal.EditorState

	busy             bool
	idleSeen         bool
	status           string
	spinner          int
	interruptCount   int
	streamed         strings.Builder
	reasoningText    strings.Builder
	reasoningSummary bool
	reasoningParts   map[string]string
	streamMessageID  string
	pending          []queuedChatInput
	modal            *enhancedModal
	inputMode        enhancedInputMode
	knownMessages    map[string]bool
	activity         []enhancedActivityItem
	completedTools   []enhancedActivityItem
	planReviewID     string
	lastCompleteID   string
	borderCommitted  bool
	contextTokens    int

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
		return exitWithReason(s.ctx, exitError, "enhanced_editor_start_failed", err)
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
			return exitWithReason(s.ctx, outcome.code, chatExitReason(outcome.code), nil)
		}
	}
	if err := runtime.render(); err != nil {
		return s.enhancedRenderError(err)
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
			return exitWithReason(s.ctx, exitInterrupt, "chat_context_canceled", s.ctx.Err())
		case <-interrupts:
			if err := runtime.requestInterrupt(); errors.Is(err, errSecondInterrupt) {
				return exitWithReason(s.ctx, exitInterrupt, "second_interrupt", err)
			}
			_ = runtime.render()
		case result, ok := <-pump.events:
			pumpStopped := false
			if !ok {
				return exitWithReason(s.ctx, exitInterrupt, "chat_input_stopped", nil)
			}
			if result.err != nil {
				if errors.Is(result.err, context.Canceled) {
					continue
				}
				runtime.commitError(result.err.Error())
				return exitWithReason(s.ctx, exitError, "chat_input_failed", result.err)
			}
			if result.key.Kind == terminal.KeyModeSwitch {
				close(result.ack)
				if err := runtime.cycleMode(); err != nil {
					runtime.status = err.Error()
				}
				if err := runtime.render(); err != nil {
					return s.enhancedRenderError(err)
				}
				continue
			}
			if runtime.busy && isExecutionHaltKey(result.key) {
				close(result.ack)
				runtime.cancelModal()
				if err := runtime.requestInterrupt(); errors.Is(err, errSecondInterrupt) {
					return exitWithReason(s.ctx, exitInterrupt, "second_interrupt", err)
				}
				if err := runtime.render(); err != nil {
					return s.enhancedRenderError(err)
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
						return exitWithReason(s.ctx, exitInterrupt, "second_interrupt", err)
					}
					runtime.commitError(err.Error())
					runtime.cancelModal()
				}
				if !done {
					// Ignore text-editing keys; the permission prompt is a selection.
				}
				if err := runtime.render(); err != nil {
					return s.enhancedRenderError(err)
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
						if errors.Is(err, errInvalidModalAnswer) {
							runtime.status = err.Error()
						} else {
							runtime.commitError(err.Error())
							runtime.cancelModal()
						}
					}
					if err := runtime.render(); err != nil {
						return s.enhancedRenderError(err)
					}
					continue
				}
			}
			action := targetState.Handle(result.key)
			if !action.Done {
				close(result.ack)
				if err := runtime.render(); err != nil {
					return s.enhancedRenderError(err)
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
						return exitWithReason(s.ctx, exitInterrupt, "second_interrupt", err)
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
							return exitWithReason(s.ctx, exitInterrupt, "second_interrupt", err)
						}
					}
				case errors.Is(action.Err, terminal.ErrCanceled):
					_ = state.Reset("")
				case errors.Is(action.Err, io.EOF):
					if !runtime.busy {
						return exitWithReason(s.ctx, exitOK, "chat_input_closed", nil)
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
				return exitWithReason(s.ctx, outcome.code, chatExitReason(outcome.code), nil)
			}
			pump = startEnhancedKeyPump(s.ctx, s.decoder, &runtime.inputMode)
			if err := runtime.render(); err != nil {
				return s.enhancedRenderError(err)
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
				return s.enhancedRenderError(err)
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
				return s.enhancedRenderError(err)
			}
		}
	}
}

func (s *chatShell) enhancedRenderError(err error) int {
	// The renderer is the normal enhanced-chat error surface. If it failed,
	// preserve its last live frame and write the failure directly to stderr.
	// chatCommand deliberately skips live-buffer cleanup for this error exit.
	fmt.Fprintln(s.stderr, "parrot: enhanced chat render failed:", terminal.Sanitize(err.Error()))
	reason := "enhanced_render_failed"
	if class := terminal.RenderErrorClass(err); class != "" {
		reason += "_" + class
	}
	return exitWithReason(s.ctx, exitError, reason, err)
}

func (r *enhancedChatRuntime) render() error {
	now := time.Now()
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
		Stream: stream, PromptContext: r.modalContext(), StyledActivity: r.styledActivityRows(now, r.shell.renderer.Columns()), Pending: pending,
		InputLeft: r.inputModeLabel(), InputCenter: r.modelineThinking(now), InputRight: r.shell.modelineModelLabel(r.contextTokens),
		Prompt: prompt, Busy: busy, Spinner: spinnerFrames[r.spinner],
		ShowDivider: r.modal != nil || message != "" || len(r.activity) > 0 || !r.borderCommitted,
	})
}

func (r *enhancedChatRuntime) modalContext() []string {
	if r.modal == nil {
		return nil
	}
	return r.modal.context
}

func (r *enhancedChatRuntime) activityRows(now time.Time, columns int) []string {
	styled := r.styledActivityRows(now, columns)
	rows := make([]string, len(styled))
	for i := range styled {
		rows[i] = styled[i].Text
	}
	return rows
}

func (r *enhancedChatRuntime) styledActivityRows(now time.Time, columns int) []terminal.StyledText {
	if len(r.activity) == 0 {
		return nil
	}
	visible := make([]enhancedActivityItem, 0, len(r.activity))
	for _, item := range r.activity {
		if isModelineThinkingActivity(item) {
			continue
		}
		visible = append(visible, item)
	}
	rows := make([]terminal.StyledText, 0, len(visible))
	start := 0
	if len(visible) > 4 {
		start = len(visible) - 4
	}
	for _, item := range visible[start:] {
		line := formatActivity(item, now)
		if item.reasoning && item.status == "thinking" {
			line = formatReasoningActivity(item, now, columns)
		}
		rows = append(rows, terminal.StyledText{Text: line, Style: item.style})
	}
	return rows
}

// modelineThinking moves the one untitled thinking placeholder out of the
// activity list. Provider-supplied reasoning summaries still have titles and
// remain as ordinary Thought rows above the modeline.
func (r *enhancedChatRuntime) modelineThinking(now time.Time) string {
	for i := len(r.activity) - 1; i >= 0; i-- {
		if isModelineThinkingActivity(r.activity[i]) {
			return formatModelineThinking(r.activity[i], now)
		}
	}
	return ""
}

func formatModelineThinking(item enhancedActivityItem, now time.Time) string {
	elapsed := now.Sub(item.started)
	if elapsed < 0 {
		elapsed = 0
	}
	frame := int(elapsed/(100*time.Millisecond)) % len(spinnerFrames)
	return fmt.Sprintf("%s Thinking…%s · %.1fs", spinnerFrames[frame], formatActivityUsage(item), elapsed.Seconds())
}

func isModelineThinkingActivity(item enhancedActivityItem) bool {
	// The initial assistant placeholder is the only thinking item that has not
	// been identified as reasoning. Raw reasoning and titled summaries retain
	// their existing activity-row behavior.
	return item.status == "thinking" && !item.terminal && !item.reasoning
}

func formatReasoningActivity(item enhancedActivityItem, now time.Time, columns int) string {
	elapsed := now.Sub(item.started)
	if elapsed < 0 {
		elapsed = 0
	}
	prefix := activityTitle("thinking", elapsed) + ": "
	suffix := fmt.Sprintf("%s · %.1fs", formatActivityUsage(item), elapsed.Seconds())
	width := max(1, columns-len(prefix)-len(suffix)-1)
	label := singleLineReasoningSummary(item.label)
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
	detail := ""
	if item.status == "failure" && strings.TrimSpace(item.error) != "" {
		detail = " · " + strings.TrimSpace(item.error)
	}
	separator := ": "
	if item.status == "success" || item.status == "failure" {
		separator = " "
	}
	label := item.label
	if item.reasoning {
		label = singleLineReasoningSummary(label)
	}
	return fmt.Sprintf("%s%s%s%s%s · %.1fs", activityTitle(item.status, elapsed), separator, label, detail, formatActivityUsage(item), elapsed.Seconds())
}

func singleLineReasoningSummary(summary string) string {
	lines := strings.Split(summary, "\n")
	for i := range lines {
		line := strings.TrimSpace(lines[i])
		for _, prefix := range []string{"- ", "* ", "+ ", "> ", "### ", "## ", "# "} {
			if strings.HasPrefix(line, prefix) {
				line = strings.TrimSpace(strings.TrimPrefix(line, prefix))
				break
			}
		}
		lines[i] = line
	}
	summary = strings.Join(strings.Fields(strings.Join(lines, " ")), " ")
	for _, marker := range []string{"**", "*", "`"} {
		summary = stripPairedMarkdownMarker(summary, marker)
	}
	return strings.TrimSpace(summary)
}

func stripPairedMarkdownMarker(value, marker string) string {
	for {
		start := strings.Index(value, marker)
		if start < 0 {
			return value
		}
		rest := value[start+len(marker):]
		end := strings.Index(rest, marker)
		if end < 0 {
			return value
		}
		value = value[:start] + rest[:end] + rest[end+len(marker):]
	}
}

func formatActivityUsage(item enhancedActivityItem) string {
	var parts []string
	if item.hasUsage {
		unit := "tokens"
		if item.tokens == 1 {
			unit = "token"
		}
		parts = append(parts, fmt.Sprintf("%s %s", formatTokenCount(item.tokens), unit))
	}
	if item.toolUses > 0 {
		unit := "tools"
		if item.toolUses == 1 {
			unit = "tool"
		}
		parts = append(parts, fmt.Sprintf("%d %s", item.toolUses, unit))
	}
	if len(parts) == 0 {
		return ""
	}
	return " · " + strings.Join(parts, " · ")
}

// formatTokenCount keeps small counts exact and abbreviates larger counts to
// the nearest hundred tokens (one decimal place in thousands).
func formatTokenCount(tokens int) string {
	if tokens < 1000 {
		return fmt.Sprintf("%d", tokens)
	}
	return fmt.Sprintf("%.1fk", float64(tokens)/1000)
}

func activityTitle(status string, elapsed time.Duration) string {
	switch status {
	case "thinking":
		frame := int(elapsed/(100*time.Millisecond)) % len(spinnerFrames)
		return spinnerFrames[frame] + " Thought"
	case "pending":
		return "○ Queued tool"
	case "running":
		frame := int(elapsed/(100*time.Millisecond)) % len(spinnerFrames)
		return spinnerFrames[frame] + " Working"
	case "success":
		return "✓"
	case "failure":
		return "✗"
	case "interrupted":
		return "■ Interrupted"
	default:
		return "Status"
	}
}

func (r *enhancedChatRuntime) startAssistantActivity(messageID string) {
	if messageID == "" {
		messageID = "assistant"
	}
	// A disposable summary delta (and even its done event) can arrive before the
	// durable assistant-started event. Do not recreate a generic Thinking row
	// after that summary has already established this message's activity.
	if r.reasoningSummary && messageID == r.streamMessageID {
		return
	}
	// Live deltas and durable assistant lifecycle events are delivered on
	// separate channels. If a delta arrived first, it already created the
	// message's activity row and the delayed started event must not add another.
	for i := range r.activity {
		if r.activity[i].id == messageID || r.activity[i].messageID == messageID {
			return
		}
	}
	r.upsertActivity(messageID, "Thinking…", "thinking", false, false, false)
	r.markActivityMessage(messageID, messageID)
	r.syncAssistantActivityUsageByID(messageID)
}

func (r *enhancedChatRuntime) startReasoningActivity(messageID, partID, label string, summary bool) {
	if messageID == "" {
		messageID = "assistant"
	}
	activityID := messageID
	if partID != "" {
		activityID = messageID + "\x00" + partID
		// Reuse the initial assistant/raw-reasoning row for the first summary
		// part. Later summary parts receive their own rows.
		found := false
		for i := range r.activity {
			if r.activity[i].id == activityID {
				found = true
				break
			}
		}
		if !found {
			for i := range r.activity {
				if r.activity[i].id == messageID && (r.activity[i].messageID == "" || r.activity[i].messageID == messageID) {
					r.activity[i].id = activityID
					break
				}
			}
		}
	}
	r.upsertActivity(activityID, cleanReasoningActivityLabel(label), "thinking", false, true, summary)
	r.markActivityMessage(activityID, messageID)
	r.syncAssistantActivityUsageByID(activityID)
}

func (r *enhancedChatRuntime) markActivityMessage(activityID, messageID string) {
	for i := range r.activity {
		if r.activity[i].id == activityID {
			r.activity[i].messageID = messageID
			return
		}
	}
}

// finishReasoningSummaryPart commits a provider-finalized summary immediately.
// Another part merely receiving a delta is not enough to produce a checkmark;
// explicit completion (or the later answer/assistant boundary fallback) is.
func (r *enhancedChatRuntime) finishReasoningSummaryPart(messageID, partID string) error {
	if messageID == "" {
		messageID = "assistant"
	}
	activityID := messageID
	if partID != "" {
		activityID += "\x00" + partID
	}
	now := time.Now()
	for i := 0; i < len(r.activity); i++ {
		item := &r.activity[i]
		if item.id != activityID || !item.reasoningSummary {
			continue
		}
		item.status = "success"
		item.terminal = true
		item.ended = now
		if r.shell == nil || r.shell.renderer == nil {
			return nil
		}
		if singleLineReasoningSummary(item.label) != "" {
			if err := r.shell.renderer.CommitStyled(terminal.StyledText{Text: formatActivity(*item, now), Style: item.style}); err != nil {
				return err
			}
			r.borderCommitted = false
		}
		r.activity = append(r.activity[:i], r.activity[i+1:]...)
		return nil
	}
	return nil
}

func cleanReasoningActivityLabel(label string) string {
	label = strings.ReplaceAll(label, "****", " ")
	label = strings.ReplaceAll(label, "**", "")
	return strings.Join(strings.Fields(label), " ")
}

func (r *enhancedChatRuntime) resetReasoning() {
	r.reasoningText.Reset()
	r.reasoningSummary = false
	r.reasoningParts = nil
}

func (r *enhancedChatRuntime) updateAssistantUsage(messageID string, usage *v1.Usage) {
	if usage == nil {
		return
	}
	// Total tokens are the best provider-supplied measurement of the context
	// that will be carried into the next turn. Some providers only report input
	// tokens, so retain that as a useful fallback.
	if tokens := contextTokenCount(*usage); tokens > 0 {
		r.contextTokens = tokens
	}
	if messageID == "" {
		messageID = r.streamMessageID
	}
	// Aggregate usage belongs on the newest matching row, which remains visible
	// when the live UI limits a long sequence of reasoning summaries to its last
	// rows.
	activityIndex := -1
	for i := range r.activity {
		if r.activity[i].id == messageID || r.activity[i].messageID == messageID {
			activityIndex = i
		}
	}
	if activityIndex >= 0 {
		if usage.OutputTokens > 0 {
			r.activity[activityIndex].outputTokens = usage.OutputTokens
		}
		if usage.ReasoningTokens > 0 {
			r.activity[activityIndex].reasoningTokens = usage.ReasoningTokens
		}
		r.syncAssistantActivityUsage(&r.activity[activityIndex])
	}
}

// syncAssistantActivityUsage picks the token count shown for an assistant or
// reasoning row. Reasoning rows prefer the provider's reasoning-token count and
// fall back to output tokens only when reasoning tokens are unavailable. Output
// tokens that accrued during the pre-summary thinking phase are cleared when a
// row becomes a reasoning summary (see upsertActivity) so they do not leak into
// the summary's displayed usage.
func (r *enhancedChatRuntime) syncAssistantActivityUsage(item *enhancedActivityItem) {
	item.tokens = item.outputTokens
	if item.reasoning && item.reasoningTokens > 0 {
		item.tokens = item.reasoningTokens
	}
	item.hasUsage = item.tokens > 0
}

func (r *enhancedChatRuntime) syncAssistantActivityUsageByID(id string) {
	for i := range r.activity {
		if r.activity[i].id == id {
			r.syncAssistantActivityUsage(&r.activity[i])
			return
		}
	}
}

func (r *enhancedChatRuntime) completeAssistantActivity(id, status string) {
	now := time.Now()
	for i := range r.activity {
		if r.activity[i].id != id && r.activity[i].messageID != id {
			continue
		}
		r.activity[i].status = status
		r.activity[i].terminal = true
		r.activity[i].ended = now
	}
}

func (r *enhancedChatRuntime) finishAssistantActivity(id string, noContent bool) error {
	for i := 0; i < len(r.activity); {
		if r.activity[i].id != id && r.activity[i].messageID != id {
			i++
			continue
		}
		if r.shouldFlushActivityItem(r.activity[i], noContent) && r.shell != nil && r.shell.renderer != nil {
			if err := r.shell.renderer.Commit(formatActivity(r.activity[i], time.Now())); err != nil {
				return err
			}
			r.borderCommitted = false
		}
		r.activity = append(r.activity[:i], r.activity[i+1:]...)
	}
	return nil
}

// shouldFlushActivityItem decides whether a completed assistant row is kept as
// ordinary one-line transcript output. A reasoning summary with visible text is
// retained (rendered above the answer); raw chain-of-thought is discarded. A
// plain assistant row is only flushed when the message carried no content.
func (r *enhancedChatRuntime) shouldFlushActivityItem(item enhancedActivityItem, noContent bool) bool {
	if item.reasoning {
		return item.reasoningSummary && singleLineReasoningSummary(item.label) != ""
	}
	return noContent
}

func contextTokenCount(usage v1.Usage) int {
	if usage.TotalTokens > 0 {
		return usage.TotalTokens
	}
	return usage.InputTokens
}

func (r *enhancedChatRuntime) upsertActivity(id, label, status string, terminal, reasoning, reasoningSummary bool) {
	now := time.Now()
	for i := range r.activity {
		if r.activity[i].id != id {
			continue
		}
		previous := r.activity[i].status
		wasTerminal := r.activity[i].terminal
		if label != "" {
			r.activity[i].label = label
		}
		r.activity[i].status = status
		r.activity[i].terminal = terminal
		if !r.activity[i].reasoning && reasoning {
			// Output tokens accrued while the row was still in the pre-summary
			// thinking phase belong to the answer, not this reasoning summary.
			r.activity[i].outputTokens = 0
		}
		r.activity[i].reasoning = reasoning
		r.activity[i].reasoningSummary = reasoningSummary
		if r.activity[i].started.IsZero() || (status == "running" && previous == "pending") || (!terminal && wasTerminal) {
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
	r.activity = append(r.activity, enhancedActivityItem{id: id, label: label, status: status, started: now, terminal: terminal, reasoning: reasoning, reasoningSummary: reasoningSummary})
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
	if r.assistantMessageOpen() || r.shell == nil || r.shell.renderer == nil {
		return nil
	}
	for len(r.completedTools) > 0 {
		item := r.completedTools[0]
		line := formatActivity(item, time.Now())
		var err error
		styled := terminal.StyledText{Text: line, Style: item.style}
		if item.block != "" {
			styled.Text += "\n" + item.block
			err = r.shell.renderer.CommitStyledBlock(styled)
		} else {
			err = r.shell.renderer.CommitStyled(styled)
		}
		if err != nil {
			return err
		}
		r.completedTools = r.completedTools[1:]
		r.borderCommitted = false
	}
	return nil
}

// assistantMessageOpen reports whether committing an unrelated transcript row
// could split an assistant response. The message ID is the normal signal. The
// buffered-text check is deliberately retained as a defensive guard in case a
// provider omits or reorders lifecycle metadata.
func (r *enhancedChatRuntime) assistantMessageOpen() bool {
	return r.streamMessageID != "" || r.streamed.Len() != 0
}

// flushReasoningBeforeAnswer settles visible reasoning summaries at the first
// answer-text boundary. Frame may promote complete wrapped answer rows to
// scrollback before the assistant-complete event, so waiting for completion can
// put the summaries after part of the answer. Committing them before buffering
// the first text delta guarantees the transcript order:
//
//	reasoning summaries -> assistant answer -> deferred tool reports
//
// Raw reasoning is removed rather than committed. If no renderer is available,
// leave the activity untouched so the normal completion path can settle it.
func (r *enhancedChatRuntime) flushReasoningBeforeAnswer(messageID string) error {
	if r.streamed.Len() != 0 || r.shell == nil || r.shell.renderer == nil {
		return nil
	}
	if messageID == "" {
		messageID = "assistant"
	}
	now := time.Now()
	for i := 0; i < len(r.activity); {
		item := &r.activity[i]
		if !item.reasoning || (item.messageID != messageID && item.id != messageID) {
			i++
			continue
		}
		if item.reasoningSummary && singleLineReasoningSummary(item.label) != "" {
			item.status = "success"
			item.terminal = true
			item.ended = now
			if err := r.shell.renderer.CommitStyled(terminal.StyledText{Text: formatActivity(*item, now), Style: item.style}); err != nil {
				return err
			}
			r.borderCommitted = false
		}
		// Summary rows are now permanent; raw chain-of-thought is intentionally
		// dropped. Either way, neither row remains in the mutable live region.
		r.activity = append(r.activity[:i], r.activity[i+1:]...)
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
// selection menu. Enter submits an option immediately; questions that allow a
// custom answer expose a separate row that switches the prompt to text input.
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
	case terminal.KeyRune:
		question := r.modal.question.Questions[r.modal.index]
		if key.Rune != ' ' || !question.Multiple || r.modal.selected >= len(question.Options) {
			return true, nil
		}
		option := question.Options[r.modal.selected]
		r.modal.selectedOptions[option.ID] = !r.modal.selectedOptions[option.ID]
		r.modal.choices[r.modal.selected].Description = questionOptionDescription(option, r.modal.selectedOptions[option.ID])
		return true, nil
	case terminal.KeyEnter:
		selected := r.modal.selected
		if selected < 0 {
			selected = 0
		} else if selected >= len(r.modal.choices) {
			selected = len(r.modal.choices) - 1
		}
		question := r.modal.question.Questions[r.modal.index]
		if question.Custom && selected == len(question.Options) {
			r.modal.choices = nil
			r.modal.customInput = true
			r.modal.prompt = "custom answer: "
			return true, r.modal.state.Reset("")
		}
		value := r.modal.choices[selected].Value
		if question.Multiple {
			r.modal.selectedOptions[value] = true
			selectedIDs := make([]string, 0, len(r.modal.selectedOptions))
			for _, option := range question.Options {
				if r.modal.selectedOptions[option.ID] {
					selectedIDs = append(selectedIDs, option.ID)
				}
			}
			return true, r.submitQuestionAnswer(v1.Answer{QuestionID: question.ID, OptionIDs: selectedIDs})
		}
		return true, r.submitQuestionAnswer(v1.Answer{QuestionID: question.ID, OptionIDs: []string{value}})
	case terminal.KeyEscape, terminal.KeyInterrupt, terminal.KeyEOF:
		return false, nil
	}
	// Option menus are selections, not editable input. Text editing starts only
	// after the user chooses the dedicated custom-input row.
	return true, nil
}

func (r *enhancedChatRuntime) updateQuestionPrompt() {
	if r.modal == nil || r.modal.question == nil || r.modal.index >= len(r.modal.question.Questions) {
		return
	}
	question := r.modal.question.Questions[r.modal.index]
	r.modal.customInput = question.Custom && len(question.Options) == 0
	r.modal.selectedOptions = make(map[string]bool)
	choiceCapacity := len(question.Options)
	if question.Custom && len(question.Options) > 0 {
		choiceCapacity++
	}
	r.modal.choices = make([]terminal.Candidate, 0, choiceCapacity)
	r.modal.selected = 0
	options := make([]string, len(question.Options))
	for i, option := range question.Options {
		options[i] = option.ID
		r.modal.choices = append(r.modal.choices, terminal.Candidate{Value: option.ID, Description: questionOptionDescription(option, false)})
	}
	if question.Custom && len(question.Options) > 0 {
		r.modal.choices = append(r.modal.choices, terminal.Candidate{Value: "Custom input", Description: "Type another answer"})
	}
	suffix := ""
	if len(options) > 0 {
		suffix = " [" + strings.Join(options, "/") + "]"
	}
	// The question text is already rendered in the modal context. Keep the
	// editor prefix focused on the expected answer so the full question is not
	// displayed a second time immediately below it.
	r.modal.prompt = "answer" + suffix + ": "
	if r.modal.customInput {
		r.modal.prompt = "custom answer: "
	}
}

func questionOptionDescription(option v1.Option, selected bool) string {
	description := option.Label
	if option.Description != "" {
		description += " - " + option.Description
	}
	if selected {
		description = "selected · " + description
	}
	return description
}

func (r *enhancedChatRuntime) showPermissionContext(permission v1.Permission) {
	if r.modal == nil {
		return
	}
	r.modal.context = permissionContextLines(permission)
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
	case "plan_complete":
		answer := strings.TrimSpace(value)
		switch strings.ToLower(answer) {
		case "yes", "y":
			r.finishModal()
			if err := r.shell.applyAgent("build", false); err != nil {
				return err
			}
			return r.submitPrompt("Implement the approved plan.")
		case "no", "n":
			r.finishModal()
			return nil
		case "":
			return fmt.Errorf("%w: enter yes, no, or feedback", errInvalidModalAnswer)
		default:
			r.finishModal()
			return r.submitPrompt(answer)
		}
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
		if modal.customInput {
			answer.Custom = strings.TrimSpace(value)
		} else {
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
		}
		return r.submitQuestionAnswer(answer)
	}
	return nil
}

func (r *enhancedChatRuntime) submitQuestionAnswer(answer v1.Answer) error {
	modal := r.modal
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
		if name == "/run" {
			if arguments == "" {
				r.commitError("run requires a prompt")
				_ = r.ensureInputBorder()
				return enhancedInputOutcome{}
			}
			value = arguments
		} else if isBuiltinSlash(name) {
			return r.handleBuiltin(name, arguments)
		} else {
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
	if name == "/new" || name == "/clear" || name == "/session" || name == "/connect" || r.shell.current.ID != previousSession {
		r.stopStream()
	}
	r.borderCommitted = false
	_ = r.ensureInputBorder()
	return enhancedInputOutcome{exit: exit, code: code}
}

func safeBusySlash(name string) bool {
	switch name {
	case "/help", "/version", "/chat", "/models", "/usage", "/modes", "/agents", "/sessions", "/status", "/thinking", "/exit":
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
		item, err := r.shell.createSession(content, false)
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
		r.contextTokens = 0
		r.knownMessages = make(map[string]bool, len(messages.Items))
		for _, item := range messages.Items {
			if item.Sequence > r.eventAfter {
				r.eventAfter = item.Sequence
			}
			if item.Status != "active" {
				r.knownMessages[item.ID] = true
				if item.Error == "" {
					r.lastCompleteID = item.ID
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
	case v1.EventTaskProgress:
		payload, err := v1.DecodeEventData(item)
		if err != nil {
			return err
		}
		r.updateTaskProgress(payload.(*v1.TaskProgress))
	case "session.context.initialized", "session.context.changed", "session.context.replaced":
		for _, path := range agentsLoadedPaths(item) {
			if r.shell != nil && r.shell.renderer != nil {
				if err := r.shell.renderer.CommitStyled(terminal.MutedText(agentsLoadedActivity(path))); err != nil {
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

func (r *enhancedChatRuntime) updateTaskProgress(progress *v1.TaskProgress) {
	if progress == nil {
		return
	}
	id := progress.ToolCallID
	if id == "" {
		id = progress.TaskID
	}
	for i := range r.activity {
		if r.activity[i].id != id {
			continue
		}
		r.activity[i].tokens = progress.Usage.TotalTokens
		if r.activity[i].tokens == 0 {
			r.activity[i].tokens = progress.Usage.InputTokens + progress.Usage.OutputTokens
		}
		r.activity[i].hasUsage = r.activity[i].tokens > 0
		r.activity[i].toolUses = progress.ToolUses
		return
	}
	r.upsertActivity(id, "task · "+progress.Agent, "running", false, false, false)
	r.updateTaskProgress(progress)
}

func (r *enhancedChatRuntime) handleToolActivity(item v1.Event) {
	callID, name, input, result := toolActivityPayload(item.Data)
	errorText := toolActivityError(item.Data)
	if callID == "" {
		callID = fmt.Sprintf("tool-%d", time.Now().UnixNano())
	}
	label := name
	if input != nil {
		label = toolActivityLabel(name, input)
	} else {
		for i := range r.activity {
			if r.activity[i].id == callID {
				label = ""
				break
			}
		}
	}
	status := strings.TrimPrefix(item.Type, "session.tool.")
	terminalEvent := status == "success" || status == "failure" || status == "interrupted"
	r.upsertActivity(callID, label, status, terminalEvent, false, false)
	for i := range r.activity {
		if r.activity[i].id != callID {
			continue
		}
		if name != "" {
			r.activity[i].toolName = name
		}
		if status == "failure" || status == "interrupted" {
			r.activity[i].style = terminal.TextStyleDefault
		} else {
			r.activity[i].style = toolActivityStyle(r.activity[i].toolName)
		}
		break
	}
	if input != nil {
		for i := range r.activity {
			if r.activity[i].id == callID {
				r.activity[i].input = input
				break
			}
		}
	}
	if terminalEvent && status == "success" && name == "edit" && strings.TrimSpace(result) != "" {
		for i := range r.activity {
			if r.activity[i].id == callID {
				r.activity[i].block = truncateToolBlock(result, maxToolBlockLines)
				break
			}
		}
	}
	if terminalEvent && status == "success" {
		for i := range r.activity {
			if r.activity[i].id != callID {
				continue
			}
			todoName := name
			if todoName == "" {
				todoName = todoWriteNameFromLabel(r.activity[i].label)
			}
			if todoName == "todowrite" || todoName == "todo_write" {
				if block, count, ok := formatTodoWriteBlock(result, r.activity[i].input); ok {
					r.activity[i].block = block
					r.activity[i].label = todoWriteActivityLabel(todoName, count)
				}
			}
			break
		}
	}
	if errorText != "" {
		for i := range r.activity {
			if r.activity[i].id == callID {
				r.activity[i].error = errorText
				break
			}
		}
	}
	if terminalEvent && status == "failure" {
		for i := range r.activity {
			if r.activity[i].id == callID && r.activity[i].input != nil {
				r.activity[i].block = truncateToolBlock(formatFailedToolRequest(r.activity[i].input), maxToolBlockLines)
				break
			}
		}
	}
	if terminalEvent {
		r.queueCompletedTool(callID)
		if err := r.flushCompletedTools(); err != nil {
			r.status = "tool activity flush failed"
		}
	}
}

func truncateToolBlock(block string, maxLines int) string {
	block = strings.ReplaceAll(block, "\r\n", "\n")
	block = strings.TrimRight(block, "\r\n")
	lines := strings.Split(block, "\n")
	if maxLines <= 0 || len(lines) <= maxLines {
		return block
	}
	remaining := len(lines) - maxLines
	return strings.Join(lines[:maxLines], "\n") + fmt.Sprintf("\n… %d more lines", remaining)
}

func formatFailedToolRequest(input map[string]any) string {
	encoded, err := json.Marshal(input)
	if err != nil {
		return ""
	}
	lines := []string{"request:"}
	for _, line := range strings.Split(formatJSONAsYAML(encoded), "\n") {
		lines = append(lines, "  "+line)
	}
	return strings.Join(lines, "\n")
}

type todoActivityItem struct {
	content  string
	status   string
	priority string
}

// formatTodoWriteBlock prefers the normalized tool result, but falls back to
// the submitted replacement when older servers omit result data.
func formatTodoWriteBlock(result string, input map[string]any) (string, int, bool) {
	if strings.TrimSpace(result) != "" {
		var raw any
		if json.Unmarshal([]byte(result), &raw) != nil {
			return "", 0, false
		}
		items, ok := parseTodoActivityItems(raw)
		if !ok {
			return "", 0, false
		}
		return renderTodoActivityItems(items), len(items), true
	}
	if input == nil {
		return "", 0, false
	}
	items, ok := parseTodoActivityItems(input["todos"])
	if !ok {
		return "", 0, false
	}
	return renderTodoActivityItems(items), len(items), true
}

func parseTodoActivityItems(raw any) ([]todoActivityItem, bool) {
	values, ok := raw.([]any)
	if !ok {
		return nil, false
	}
	items := make([]todoActivityItem, 0, len(values))
	for _, value := range values {
		entry, ok := value.(map[string]any)
		if !ok {
			return nil, false
		}
		content := cleanActivityDetail(firstString(entry, "content"))
		status := firstString(entry, "status")
		priority := firstString(entry, "priority")
		if content == "" || !validTodoActivityStatus(status) || !validTodoActivityPriority(priority) {
			return nil, false
		}
		items = append(items, todoActivityItem{content: content, status: status, priority: priority})
	}
	return items, true
}

func validTodoActivityStatus(status string) bool {
	switch status {
	case "pending", "in_progress", "completed", "cancelled":
		return true
	default:
		return false
	}
}

func validTodoActivityPriority(priority string) bool {
	switch priority {
	case "high", "medium", "low":
		return true
	default:
		return false
	}
}

func renderTodoActivityItems(items []todoActivityItem) string {
	if len(items) == 0 {
		return "  No todos"
	}
	lines := make([]string, 0, len(items))
	for _, item := range items {
		marker := todoActivityMarker(item.status)
		priority := cleanActivityDetail(strings.ReplaceAll(item.priority, "_", " "))
		lines = append(lines, fmt.Sprintf("  %s %s · %s", marker, priority, item.content))
	}
	return strings.Join(lines, "\n")
}

func todoActivityMarker(status string) string {
	switch status {
	case "pending":
		return "○"
	case "in_progress":
		return "◐"
	case "completed":
		return "✓"
	case "cancelled":
		return "■"
	default:
		return "•"
	}
}

func toolActivityError(data json.RawMessage) string {
	raw, ok := decodeJSONObject(data)
	if !ok {
		return ""
	}
	return firstString(raw, "error", "error_message", "message")
}

func toolActivityPayload(data json.RawMessage) (string, string, map[string]any, string) {
	raw, ok := decodeJSONObject(data)
	if !ok {
		return "", "", nil, ""
	}
	callID := firstString(raw, "call_id", "callID", "id", "ID")
	name := firstString(raw, "name", "Name", "tool", "tool_name", "toolID", "tool_id")
	input := firstObject(raw, "input", "Input", "arguments", "Arguments")
	result := firstString(raw, "result", "Result")
	if nested, ok := raw["call"].(map[string]any); ok {
		if callID == "" {
			callID = firstString(nested, "call_id", "callID", "id", "ID")
		}
		if name == "" {
			name = firstString(nested, "name", "Name", "tool", "tool_name", "toolID", "tool_id")
		}
		if input == nil {
			input = firstObject(nested, "input", "Input", "arguments", "Arguments")
		}
		if result == "" {
			result = firstString(nested, "result", "Result")
		}
	}
	return callID, name, input, result
}

func decodeJSONObject(data json.RawMessage) (map[string]any, bool) {
	if len(data) == 0 {
		return nil, false
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.UseNumber()
	var raw map[string]any
	if err := decoder.Decode(&raw); err != nil {
		return nil, false
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, false
	}
	return raw, true
}

func todoWriteNameFromLabel(label string) string {
	if label == "TODO" || strings.HasPrefix(label, "TODO · ") {
		return "todowrite"
	}
	for _, name := range []string{"todowrite", "todo_write"} {
		if label == name || strings.HasPrefix(label, name+" · ") {
			return name
		}
	}
	return ""
}

func todoWriteActivityLabel(_ string, count int) string {
	noun := "items"
	if count == 1 {
		noun = "item"
	}
	return fmt.Sprintf("TODO · %d %s", count, noun)
}

func toolActivityLabel(name string, input map[string]any) string {
	var details []string
	add := func(value string) {
		if value = cleanActivityDetail(value); value != "" {
			details = append(details, value)
		}
	}
	quoted := func(value string) {
		if value = cleanActivityDetail(value); value != "" {
			details = append(details, fmt.Sprintf("%q", value))
		}
	}

	switch name {
	case "read", "edit", "format":
		add(firstString(input, "path", "file", "filePath"))
	case "glob":
		quoted(firstString(input, "pattern"))
	case "grep":
		quoted(firstString(input, "pattern"))
		path := firstString(input, "path")
		if path == "" {
			path = "."
		}
		add(path)
	case "read_output":
		add(firstString(input, "id"))
	case "apply_patch":
		details = append(details, patchActivityTargets(firstString(input, "patchText", "patch"))...)
	case "shell":
		add(firstString(input, "command"))
	case "todowrite", "todo_write":
		if todos, ok := input["todos"].([]any); ok {
			return todoWriteActivityLabel(name, len(todos))
		}
	case "question":
		if questions, ok := input["questions"].([]any); ok && len(questions) > 0 {
			if question, ok := questions[0].(map[string]any); ok {
				add(firstString(question, "header", "prompt", "question"))
			}
			if len(questions) > 1 {
				details = append(details, fmt.Sprintf("+%d more", len(questions)-1))
			}
		}
	case "skill":
		add(firstString(input, "name"))
	case "web_fetch":
		add(firstString(input, "url"))
	case "task":
		add(firstString(input, "agent"))
		add(firstString(input, "prompt"))
	case "task_status", "task_cancel":
		add(firstString(input, "task_id"))
	default:
		if strings.HasPrefix(name, "lsp_") {
			add(firstString(input, "path", "query"))
			break
		}
		keys := make([]string, 0, len(input))
		for key := range input {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			value, ok := input[key].(string)
			if ok && !sensitiveActivityField(key) {
				add(key + "=" + value)
				break
			}
		}
	}
	if len(details) == 0 {
		return name
	}
	return name + " · " + strings.Join(details, " · ")
}

func firstObject(raw map[string]any, keys ...string) map[string]any {
	for _, key := range keys {
		if value, ok := raw[key].(map[string]any); ok {
			return value
		}
	}
	return nil
}

func cleanActivityDetail(value string) string {
	value = strings.Join(strings.FieldsFunc(value, unicode.IsControl), " ")
	value = strings.TrimSpace(value)
	runes := []rune(value)
	if len(runes) > 96 {
		value = string(runes[:93]) + "..."
	}
	return value
}

func patchActivityTargets(patch string) []string {
	prefixes := []string{"*** Add File: ", "*** Update File: ", "*** Delete File: ", "*** Move to: "}
	seen := make(map[string]bool)
	var targets []string
	for _, line := range strings.Split(patch, "\n") {
		for _, prefix := range prefixes {
			if !strings.HasPrefix(line, prefix) {
				continue
			}
			path := cleanActivityDetail(strings.TrimPrefix(line, prefix))
			if path != "" && !seen[path] {
				seen[path] = true
				targets = append(targets, path)
			}
			break
		}
	}
	if len(targets) <= 2 {
		return targets
	}
	return []string{targets[0], targets[1], fmt.Sprintf("+%d more", len(targets)-2)}
}

func sensitiveActivityField(key string) bool {
	key = strings.ToLower(key)
	for _, fragment := range []string{"authorization", "command", "content", "env", "key", "old", "new", "password", "patch", "prompt", "secret", "token"} {
		if strings.Contains(key, fragment) {
			return true
		}
	}
	return false
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
	if r.shell.selection.agent == "plan" && r.lastCompleteID != "" && r.planReviewID != r.lastCompleteID && r.modal == nil {
		state, err := r.shell.editor.Start("")
		if err != nil {
			return err
		}
		r.planReviewID = r.lastCompleteID
		r.modal = &enhancedModal{kind: "plan_complete", state: state, prompt: "Plan complete — yes to implement, no to stop, or type feedback: ", context: []string{"Review the plan before implementation."}}
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
		// Keep any retained reasoning summary as ordinary one-line transcript
		// output before the answer instead of rendering it as a multiline block,
		// and drop raw chain-of-thought rows without emitting them.
		if err := r.finishAssistantActivity(item.ID, item.Content == ""); err != nil {
			return err
		}
		if item.Content != "" {
			if item.ID == r.streamMessageID {
				if err := r.shell.renderer.CommitStream(terminal.StreamMessage{ID: item.ID, Prefix: "- ", Text: item.Content}, false); err != nil {
					return err
				}
			} else if err := r.shell.renderer.CommitMessage("- ", item.Content, false); err != nil {
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
	}
	if currentStreamSettled && (messageID == "" || messageID == r.streamMessageID) {
		r.streamed.Reset()
		r.resetReasoning()
		r.streamMessageID = ""
	}
	if err := r.flushCompletedTools(); err != nil {
		return err
	}
	r.status = ""
	return nil
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
	r.streamMessageID = messageID
	return true, nil
}
