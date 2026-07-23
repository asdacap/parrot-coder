package enhancedchat

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	v1 "github.com/amirulashraf/parrot-coder/internal/api/v1"
	"github.com/amirulashraf/parrot-coder/internal/cli/chatview"
	"github.com/amirulashraf/parrot-coder/internal/terminal"
)

func (r *enhancedChatRuntime) activityRows(now time.Time, columns int) []string {
	styled := r.styledActivityRows(now, columns)
	rows := make([]string, len(styled))
	for i := range styled {
		text := styled[i].Text
		if styled[i].Markdown {
			text = singleLineReasoningSummary(text)
		}
		rows[i] = styled[i].Prefix + text + styled[i].Suffix
	}
	return rows
}

func (r *enhancedChatRuntime) activityFrames(now time.Time, columns int) []terminal.LiveFrame {
	var frames []terminal.LiveFrame
	if tracker := r.subagents.Tracker(); tracker != nil {
		for _, task := range tracker.Tasks() {
			frames = append(frames, terminal.LiveFrame{TaskID: task.TaskID, SessionID: task.SessionID, ParentSessionID: task.ParentSessionID})
		}
	}
	visible := make([]enhancedActivityItem, 0, len(r.activity))
	for _, item := range r.activity {
		if !isModelineThinkingActivity(item) {
			visible = append(visible, item)
		}
	}
	if len(visible) > 4 {
		visible = visible[len(visible)-4:]
	}
	statusByTask := make(map[string]int)
	for _, item := range visible {
		taskID := item.taskID
		if taskID == "" {
			taskID = "task_main"
		}
		if item.mainStatus {
			if previous, ok := statusByTask[taskID]; ok {
				frames[previous].MainStatus = false
			}
			statusByTask[taskID] = len(frames)
		}
		frames = append(frames, terminal.LiveFrame{
			TaskID: taskID, SessionID: item.sessionID, ParentSessionID: item.parentSessionID, MainStatus: item.mainStatus,
			StyledActivity: []terminal.StyledText{styledActivity(item, now, columns)},
		})
	}
	return frames
}

func styledActivity(item enhancedActivityItem, now time.Time, columns int) terminal.StyledText {
	if item.reasoningSummary {
		return reasoningSummaryActivity(item, now)
	}
	line := formatActivity(item, now)
	if item.reasoning && item.status == "thinking" {
		line = formatReasoningActivity(item, now, columns)
	}
	if output := item.output.String(); output != "" {
		line += "\n" + output
	}
	return terminal.StyledText{Text: line, Style: item.style}
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
		rows = append(rows, styledActivity(item, now, columns))
	}
	return rows
}

func reasoningSummaryActivity(item enhancedActivityItem, now time.Time) terminal.StyledText {
	end := now
	if !item.ended.IsZero() {
		end = item.ended
	}
	elapsed := end.Sub(item.started)
	if elapsed < 0 {
		elapsed = 0
	}
	separator := " "
	if item.status != "success" && item.status != "failure" {
		separator = ": "
	}
	return terminal.StyledText{
		Text: item.label, Style: item.style, Markdown: true,
		Prefix: activityTitle(item.status, elapsed) + separator,
		Suffix: formatActivityUsage(item) + fmt.Sprintf(" · %.1fs", elapsed.Seconds()),
	}
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
	frame := int(elapsed/(100*time.Millisecond)) % len(spinnerFrames)
	header := spinnerFrames[frame] + " Thinking:"

	lines := strings.Split(item.label, "\n")
	nonEmpty := make([]string, 0, 3)
	for _, l := range lines {
		if trimmed := strings.TrimSpace(l); trimmed != "" {
			nonEmpty = append(nonEmpty, trimmed)
		}
	}
	if len(nonEmpty) > 3 {
		nonEmpty = nonEmpty[len(nonEmpty)-3:]
	}
	suffix := fmt.Sprintf("%s · %.1fs", formatActivityUsage(item), elapsed.Seconds())
	if len(nonEmpty) == 0 {
		return header + " Thinking…" + suffix
	}

	indent := strings.Repeat(" ", utf8.RuneCountInString(header)+1)
	out := header + " " + nonEmpty[0] + suffix
	for i := 1; i < len(nonEmpty); i++ {
		out += "\n" + indent + nonEmpty[i]
	}
	return out
}

func formatActivity(item enhancedActivityItem, now time.Time) string {
	if item.rendered != "" {
		if item.mainStatus && item.status == "running" {
			elapsed := now.Sub(item.started)
			if elapsed < 0 {
				elapsed = 0
			}
			frame := int(elapsed/(100*time.Millisecond)) % len(spinnerFrames)
			return strings.Replace(item.rendered, spinnerFrames[0], spinnerFrames[frame], 1)
		}
		return item.rendered
	}
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

func formatTokenCount(tokens int) string { return chatview.FormatTokenCount(tokens) }

// formatCost returns a human-friendly USD cost string. Sub-cent amounts show
// four decimal places so very cheap turns remain informative; larger amounts
// show two decimal places.
func formatCost(cost float64) string {
	if cost < 0.01 {
		return fmt.Sprintf("$%.4f", cost)
	}
	return fmt.Sprintf("$%.2f", cost)
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
			if err := r.shell.renderer.CommitStyled(terminal.StyledText{Prefix: "· ", Text: item.label, Style: item.style, Markdown: true}); err != nil {
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
	lines := strings.Split(label, "\n")
	var fenceChar rune
	fenceLength := 0
	for i, line := range lines {
		if fenceChar != 0 {
			if reasoningFenceClosing(line, fenceChar, fenceLength) {
				fenceChar = 0
				fenceLength = 0
			}
			continue
		}
		if marker, length, ok := reasoningFenceOpening(line); ok {
			fenceChar = marker
			fenceLength = length
			continue
		}
		lines[i] = separateAdjacentBold(line)
	}
	return strings.Join(lines, "\n")
}

func reasoningFenceOpening(line string) (rune, int, bool) {
	trimmed, ok := trimReasoningFenceIndent(line)
	if !ok {
		return 0, 0, false
	}
	if trimmed == "" || trimmed[0] != '`' && trimmed[0] != '~' {
		return 0, 0, false
	}
	marker := rune(trimmed[0])
	count := 0
	for count < len(trimmed) && rune(trimmed[count]) == marker {
		count++
	}
	if count < 3 || marker == '`' && strings.ContainsRune(trimmed[count:], '`') {
		return 0, 0, false
	}
	return marker, count, true
}

func reasoningFenceClosing(line string, marker rune, minimum int) bool {
	trimmed, ok := trimReasoningFenceIndent(line)
	if !ok {
		return false
	}
	count := 0
	for count < len(trimmed) && rune(trimmed[count]) == marker {
		count++
	}
	return count >= minimum && strings.TrimSpace(trimmed[count:]) == ""
}

func trimReasoningFenceIndent(line string) (string, bool) {
	spaces := 0
	for spaces < len(line) && line[spaces] == ' ' {
		spaces++
	}
	return line[spaces:], spaces <= 3
}

func separateAdjacentBold(line string) string {
	var output strings.Builder
	codeTicks := 0
	for position := 0; position < len(line); {
		if line[position] == '`' {
			end := position
			for end < len(line) && line[end] == '`' {
				end++
			}
			count := end - position
			if codeTicks == 0 {
				codeTicks = count
			} else if codeTicks == count {
				codeTicks = 0
			}
			output.WriteString(line[position:end])
			position = end
			continue
		}
		if codeTicks == 0 && strings.HasPrefix(line[position:], "****") &&
			(position == 0 || line[position-1] != '*') && position+4 < len(line) && line[position+4] != '*' &&
			strings.Contains(line[:position], "**") && strings.Contains(line[position+4:], "**") &&
			!markdownMarkerEscaped(line, position) {
			output.WriteString("**\n\n**")
			position += 4
			continue
		}
		output.WriteByte(line[position])
		position++
	}
	return output.String()
}

func markdownMarkerEscaped(value string, position int) bool {
	backslashes := 0
	for position > 0 && value[position-1] == '\\' {
		backslashes++
		position--
	}
	return backslashes%2 != 0
}

func (r *enhancedChatRuntime) resetReasoning() {
	r.reasoningText.Reset()
	r.reasoningSummary = false
	r.reasoningParts = nil
}

// updateAssistantUsage describes the message a usage event belongs to: the
// context it leaves behind and the tokens shown on its activity row. What was
// spent is not accounted here — recordUsage charges it to the task tree, which
// is the one place the main task and its subagents are added up together.
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
			var err error
			if r.activity[i].reasoningSummary {
				err = r.shell.renderer.CommitStyled(terminal.StyledText{Prefix: "· ", Text: r.activity[i].label, Style: r.activity[i].style, Markdown: true})
			} else {
				err = r.shell.renderer.Commit(formatActivity(r.activity[i], time.Now()))
			}
			if err != nil {
				return err
			}
			r.borderCommitted = false
		}
		r.activity = append(r.activity[:i], r.activity[i+1:]...)
	}
	return nil
}

// shouldFlushActivityItem decides whether a completed assistant row is kept as
// transcript output. A reasoning summary with visible text is retained as
// Markdown above the answer; raw chain-of-thought is discarded. A
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
			if err := r.shell.renderer.CommitStyled(terminal.StyledText{Prefix: "· ", Text: item.label, Style: item.style, Markdown: true}); err != nil {
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
