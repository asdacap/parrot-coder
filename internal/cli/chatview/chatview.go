package chatview

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode"

	v1 "github.com/amirulashraf/parrot-coder/internal/api/v1"
	"github.com/amirulashraf/parrot-coder/internal/terminal"
	"go.yaml.in/yaml/v3"
)

const (
	MaxToolBlockLines   = 10
	maxShellOutputLines = 3
	maxShellOutputBytes = 16 << 10
)

var SpinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

type ShellOutputTail struct {
	lines    []string
	pending  []rune
	carriage bool
}

func (t *ShellOutputTail) Write(delta string) {
	for _, char := range delta {
		if t.carriage {
			t.carriage = false
			if char != '\n' {
				t.pending = t.pending[:0]
			}
		}
		switch char {
		case '\n':
			t.lines = append(t.lines, string(t.pending))
			t.pending = t.pending[:0]
		case '\r':
			t.carriage = true
		default:
			t.pending = append(t.pending, char)
			if len(t.pending) > maxShellOutputBytes {
				t.pending = t.pending[len(t.pending)-maxShellOutputBytes:]
			}
		}
	}
	if len(t.lines) > maxShellOutputLines {
		t.lines = t.lines[len(t.lines)-maxShellOutputLines:]
	}
}

func (t ShellOutputTail) String() string {
	lines := append([]string(nil), t.lines...)
	if len(t.pending) != 0 {
		lines = append(lines, string(t.pending))
	}
	if len(lines) > maxShellOutputLines {
		lines = lines[len(lines)-maxShellOutputLines:]
	}
	return strings.Join(lines, "\n")
}

func SingleLineReasoningSummary(summary string) string {
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

func FormatTokenCount(tokens int) string {
	if tokens < 1000 {
		return fmt.Sprintf("%d", tokens)
	}
	return fmt.Sprintf("%.1fk", float64(tokens)/1000)
}

func TruncateToolBlock(block string, maxLines int) string {
	block = strings.ReplaceAll(block, "\r\n", "\n")
	block = strings.TrimRight(block, "\r\n")
	lines := strings.Split(block, "\n")
	if maxLines <= 0 || len(lines) <= maxLines {
		return block
	}
	remaining := len(lines) - maxLines
	return strings.Join(lines[:maxLines], "\n") + fmt.Sprintf("\n… %d more lines", remaining)
}

func FormatFailedToolRequest(input map[string]any) string {
	encoded, err := json.Marshal(input)
	if err != nil {
		return ""
	}
	lines := []string{"request:"}
	for _, line := range strings.Split(FormatJSONAsYAML(encoded), "\n") {
		lines = append(lines, "  "+line)
	}
	return strings.Join(lines, "\n")
}

// RedactToolInputForDisplay returns a presentation-only copy. Exact tool input
// remains available to authorization and execution but stdin is never rendered.
func RedactToolInputForDisplay(name string, input map[string]any) map[string]any {
	if input == nil || name != "write_stdin" {
		return input
	}
	redacted := make(map[string]any, len(input))
	for key, value := range input {
		redacted[key] = value
	}
	for _, key := range []string{"chars", "input"} {
		value, exists := redacted[key]
		if !exists {
			continue
		}
		if text, ok := value.(string); ok {
			redacted[key] = fmt.Sprintf("<redacted: %d chars>", len([]rune(text)))
		} else {
			redacted[key] = "<redacted>"
		}
	}
	return redacted
}

type todoActivityItem struct {
	content  string
	status   string
	priority string
}

// FormatTodoWriteBlock prefers the normalized tool result, but falls back to
// the submitted replacement when older servers omit result data.
func FormatTodoWriteBlock(result string, input map[string]any) (string, int, bool) {
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

func ToolActivityError(data json.RawMessage) string {
	raw, ok := decodeJSONObject(data)
	if !ok {
		return ""
	}
	return firstString(raw, "error", "error_message", "message")
}

func ToolActivityOutputTail(data json.RawMessage) string {
	raw, ok := decodeJSONObject(data)
	if !ok {
		return ""
	}
	return firstString(raw, "output_tail")
}

func ToolActivityPayload(data json.RawMessage) (string, string, map[string]any, string) {
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
	return callID, name, RedactToolInputForDisplay(name, input), result
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

func ToolActivityLabel(name string, input map[string]any) string {
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
	case "exec_command":
		add(firstString(input, "cmd"))
	case "write_stdin":
		if value, ok := input["session_id"]; ok {
			add(fmt.Sprint(value))
		}
		add(firstString(input, "chars"))
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
		if move, ok := strings.CutPrefix(line, "*** Move File: "); ok {
			source, destination, valid := strings.Cut(move, " -> ")
			if valid {
				for _, path := range []string{source, destination} {
					path = cleanActivityDetail(path)
					if path != "" && !seen[path] {
						seen[path] = true
						targets = append(targets, path)
					}
				}
			}
			continue
		}
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

type streamToolCall struct {
	name   string
	input  map[string]any
	style  terminal.TextStyle
	output ShellOutputTail
}

type StreamToolTracker struct {
	calls   map[string]streamToolCall
	pending map[string]ShellOutputTail
	done    map[string]bool
}

type StreamToolReport struct {
	Line     string
	Label    string
	Block    string
	Terminal bool
	Style    terminal.TextStyle
}

type subagentMessageState struct {
	text             strings.Builder
	reasoning        strings.Builder
	reasoningSummary bool
	reasoningDone    bool
}

type SubagentReport struct {
	ID        string
	Line      string
	Block     string
	Terminal  bool
	EmitPlain bool
	Skip      bool
	Style     terminal.TextStyle
}

type SubagentStreamTracker struct {
	messages map[string]*subagentMessageState
	tools    map[string]*StreamToolTracker
}

func subagentPrefix(item *v1.SubagentEvent) string {
	if item == nil {
		return ""
	}
	depth := item.Depth
	if depth < 1 {
		depth = 1
	}
	name := strings.TrimSpace(item.TaskName)
	if name == "" {
		name = "task"
	}
	return strings.Repeat("  ", depth) + "[" + name + "] "
}

func prefixSubagentText(prefix, text string) string {
	lines := strings.Split(text, "\n")
	for i := range lines {
		lines[i] = prefix + lines[i]
	}
	return strings.Join(lines, "\n")
}

func prefixSubagentActivity(prefix, text string) string {
	lines := strings.Split(text, "\n")
	icons := append([]string{"○", "◌", "✓", "✗", "■"}, SpinnerFrames...)
	for _, icon := range icons {
		if rest, ok := strings.CutPrefix(lines[0], icon+" "); ok {
			indent := prefix[:len(prefix)-len(strings.TrimLeft(prefix, " "))]
			lines[0] = indent + icon + " " + strings.TrimSpace(prefix) + " " + rest
			for i := 1; i < len(lines); i++ {
				lines[i] = prefix + lines[i]
			}
			return strings.Join(lines, "\n")
		}
	}
	return prefix + text
}

func (t *SubagentStreamTracker) Describe(item *v1.SubagentEvent, thinking bool) ([]SubagentReport, error) {
	if item == nil || item.TaskID == "" || item.Depth < 1 || !v1.KnownEvent(item.Event.Type) {
		return nil, nil
	}
	scope := fmt.Sprintf("%d:%s:", item.Depth, item.TaskID)
	prefix := subagentPrefix(item)
	switch item.Event.Type {
	case "session.assistant.started":
		var payload struct {
			MessageID string `json:"message_id"`
		}
		if err := json.Unmarshal(item.Event.Data, &payload); err != nil {
			return nil, err
		}
		messageID := payload.MessageID
		if messageID == "" {
			messageID = "assistant"
		}
		key := scope + "message:" + messageID
		if t.messages == nil {
			t.messages = make(map[string]*subagentMessageState)
		}
		if t.messages[key] == nil {
			t.messages[key] = &subagentMessageState{}
		}
		return nil, nil
	case v1.EventMessagePartDelta:
		payload, err := v1.DecodeEventData(item.Event)
		if err != nil {
			return nil, err
		}
		delta := payload.(*v1.MessagePartDelta)
		messageID := delta.MessageID
		if messageID == "" {
			messageID = "assistant"
		}
		key := scope + "message:" + messageID
		if t.messages == nil {
			t.messages = make(map[string]*subagentMessageState)
		}
		state := t.messages[key]
		if state == nil {
			state = &subagentMessageState{}
			t.messages[key] = state
		}
		switch delta.Kind {
		case "text":
			state.text.WriteString(delta.Delta)
			if strings.TrimSpace(state.text.String()) == "" {
				return nil, nil
			}
			line := prefixSubagentActivity(prefix, "○ response: "+state.text.String())
			return []SubagentReport{{ID: key + ":response", Line: line, Style: terminal.TextStyleMuted}}, nil
		case "reasoning_summary":
			if !state.reasoningSummary {
				state.reasoning.Reset()
				state.reasoningSummary = true
			}
			if delta.Done && delta.Delta != "" {
				state.reasoning.Reset()
				state.reasoning.WriteString(delta.Delta)
			} else {
				state.reasoning.WriteString(delta.Delta)
			}
			state.reasoningDone = delta.Done
			if strings.TrimSpace(state.reasoning.String()) == "" {
				return nil, nil
			}
			icon := SpinnerFrames[0]
			if delta.Done {
				icon = "✓"
			}
			line := prefixSubagentActivity(prefix, icon+" Thought: "+SingleLineReasoningSummary(state.reasoning.String()))
			return []SubagentReport{{ID: key + ":reasoning", Line: line, Terminal: delta.Done, EmitPlain: delta.Done, Style: terminal.TextStyleMuted}}, nil
		case "reasoning":
			if !thinking || state.reasoningSummary {
				return nil, nil
			}
			state.reasoning.WriteString(delta.Delta)
			if strings.TrimSpace(state.reasoning.String()) == "" {
				return nil, nil
			}
			line := prefixSubagentActivity(prefix, SpinnerFrames[0]+" Reasoning: "+SingleLineReasoningSummary(state.reasoning.String()))
			return []SubagentReport{{ID: key + ":reasoning", Line: line, Style: terminal.TextStyleMuted}}, nil
		case "tool_input":
			return nil, nil
		default:
			return []SubagentReport{{ID: key + ":status", Line: prefix + "status: " + delta.Kind, Style: terminal.TextStyleMuted}}, nil
		}
	case "session.assistant.complete", "session.assistant.error", "session.assistant.interrupted":
		var payload struct {
			MessageID string `json:"message_id"`
			Error     string `json:"error"`
		}
		if err := json.Unmarshal(item.Event.Data, &payload); err != nil {
			return nil, err
		}
		messageID := payload.MessageID
		if messageID == "" {
			messageID = "assistant"
		}
		key := scope + "message:" + messageID
		state := t.messages[key]
		status := "○"
		if item.Event.Type == "session.assistant.error" {
			status = "✗"
		} else if item.Event.Type == "session.assistant.interrupted" {
			status = "■"
		}
		text := ""
		if state != nil && strings.TrimSpace(state.text.String()) != "" {
			text = "response: " + state.text.String()
		}
		if payload.Error != "" {
			if text != "" {
				text += " · "
			}
			text += payload.Error
		}
		if text == "" && item.Event.Type == "session.assistant.complete" && state != nil && !state.reasoningDone && strings.TrimSpace(state.reasoning.String()) != "" {
			label := "Reasoning: "
			if state.reasoningSummary {
				label = "Thought: "
			}
			line := prefixSubagentActivity(prefix, "✓ "+label+SingleLineReasoningSummary(state.reasoning.String()))
			delete(t.messages, key)
			return []SubagentReport{{ID: key + ":reasoning", Line: line, Terminal: true, EmitPlain: true, Style: terminal.TextStyleMuted}}, nil
		}
		delete(t.messages, key)
		if text == "" && item.Event.Type == "session.assistant.complete" {
			return []SubagentReport{{ID: key + ":response", Terminal: true, Skip: true}}, nil
		}
		if text == "" {
			text = "response complete"
		}
		return []SubagentReport{{ID: key + ":response", Line: prefixSubagentActivity(prefix, status+" "+text), Terminal: true, EmitPlain: true, Style: terminal.TextStyleMuted}}, nil
	case "session.tool.pending", "session.tool.running", "session.tool.success", "session.tool.failure", "session.tool.interrupted":
		if t.tools == nil {
			t.tools = make(map[string]*StreamToolTracker)
		}
		tracker := t.tools[scope]
		if tracker == nil {
			tracker = &StreamToolTracker{}
			t.tools[scope] = tracker
		}
		callID, _, _, _ := ToolActivityPayload(item.Event.Data)
		report := tracker.DescribeReport(item.Event)
		line := report.Line
		if report.Label != "" {
			line = strings.Replace(line, "tool", report.Label, 1)
		}
		block := ""
		if report.Block != "" {
			block = prefixSubagentText(strings.Repeat("  ", max(1, item.Depth))+"  ", report.Block)
		}
		return []SubagentReport{{ID: scope + "tool:" + callID, Line: prefixSubagentActivity(prefix, line), Block: block, Terminal: report.Terminal, EmitPlain: true, Style: report.Style}}, nil
	case v1.EventToolOutputDelta:
		if t.tools == nil {
			t.tools = make(map[string]*StreamToolTracker)
		}
		if t.tools[scope] == nil {
			t.tools[scope] = &StreamToolTracker{}
		}
		payload, err := v1.DecodeEventData(item.Event)
		if err != nil {
			return nil, err
		}
		output := payload.(*v1.ToolOutputDelta)
		report := t.tools[scope].Output(output)
		if report.Line == "" {
			return nil, nil
		}
		block := prefixSubagentText(strings.Repeat("  ", max(1, item.Depth))+"  ", report.Block)
		return []SubagentReport{{ID: scope + "tool:" + output.ToolCallID, Line: prefixSubagentActivity(prefix, report.Line), Block: block, Style: report.Style}}, nil
	case v1.EventTaskProgress:
		payload, err := v1.DecodeEventData(item.Event)
		if err != nil {
			return nil, err
		}
		progress := payload.(*v1.TaskProgress)
		line := fmt.Sprintf("task: %s · %s tokens · %d tools", progress.Agent, FormatTokenCount(progress.Usage.TotalTokens), progress.ToolUses)
		terminalEvent := progress.Status != "pending" && progress.Status != "running"
		return []SubagentReport{{ID: scope + "task:" + progress.TaskID, Line: prefix + line, Terminal: terminalEvent, EmitPlain: true, Style: terminal.TextStyleMuted}}, nil
	case v1.EventSessionStatus:
		payload, err := v1.DecodeEventData(item.Event)
		if err != nil {
			return nil, err
		}
		status := payload.(*v1.SessionStatus)
		if status.Kind == "running" || status.Kind == "idle" || status.Kind == "finish" || status.Kind == "usage" || status.Kind == "tool_call_complete" {
			return nil, nil
		}
		return []SubagentReport{{ID: scope + "status:" + status.MessageID, Line: prefix + "status: " + status.Kind, Terminal: true, EmitPlain: true, Style: terminal.TextStyleMuted}}, nil
	case "session.context.initialized", "session.context.changed", "session.context.replaced":
		lines := AgentsLoadedActivities(item.Event)
		reports := make([]SubagentReport, 0, len(lines))
		for i, line := range lines {
			reports = append(reports, SubagentReport{ID: fmt.Sprintf("%scontext:%d", scope, i), Line: prefixSubagentActivity(prefix, line), Terminal: true, EmitPlain: true, Style: terminal.TextStyleMuted})
		}
		return reports, nil
	default:
		return nil, nil
	}
}

// ToolActivityStyle returns the transcript style shared by the standard and
// enhanced chat renderers. Read-only discovery and retrieval tools are muted
// so they remain visible without competing with actions that change state.
func ToolActivityStyle(name string) terminal.TextStyle {
	switch name {
	case "read", "grep", "glob", "web_fetch":
		return terminal.TextStyleMuted
	default:
		return terminal.TextStyleDefault
	}
}

// describe returns the human-facing status and any permanent detail block for
// a tool event. Pending input is retained until the terminal event because
// failure and some success payloads contain only the call ID.
func (t *StreamToolTracker) Describe(item v1.Event) (string, string, bool) {
	report := t.DescribeReport(item)
	return report.Line, report.Block, report.Terminal
}

func (t *StreamToolTracker) DescribeReport(item v1.Event) StreamToolReport {
	callID, name, input, result := ToolActivityPayload(item.Data)
	if t.calls == nil {
		t.calls = make(map[string]streamToolCall)
	}
	call := t.calls[callID]
	if pending, ok := t.pending[callID]; ok {
		call.output = pending
		delete(t.pending, callID)
	}
	if name != "" {
		call.name = name
		call.style = ToolActivityStyle(name)
	}
	if input != nil {
		call.input = input
	}
	if callID != "" {
		t.calls[callID] = call
	}
	status := strings.TrimPrefix(item.Type, "session.tool.")
	terminalEvent := status == "success" || status == "failure" || status == "interrupted"
	block := ""
	if status == "success" && call.name == "edit" && strings.TrimSpace(result) != "" {
		block = TruncateToolBlock(result, MaxToolBlockLines)
	} else if status == "success" && (call.name == "todowrite" || call.name == "todo_write") {
		if formatted, _, ok := FormatTodoWriteBlock(result, call.input); ok {
			block = formatted
		}
	} else if status == "failure" && call.input != nil {
		block = TruncateToolBlock(FormatFailedToolRequest(call.input), MaxToolBlockLines)
	}
	if terminalEvent && (call.name == "shell" || call.name == "exec_command") {
		output := ToolActivityOutputTail(item.Data)
		if output == "" {
			output = call.output.String()
		}
		if output == "" && result != "" {
			call.output.Write(result)
			output = call.output.String()
		}
		if output != "" {
			block = output
		}
	}
	style := call.style
	if status == "failure" || status == "interrupted" {
		style = terminal.TextStyleDefault
	}
	if terminalEvent && callID != "" {
		if t.done == nil {
			t.done = make(map[string]bool)
		}
		t.done[callID] = true
		delete(t.pending, callID)
		delete(t.calls, callID)
	}
	errorText := ToolActivityError(item.Data)
	if call.name == "write_stdin" {
		errorText, block = "", ""
	}
	return StreamToolReport{
		Line: StreamToolStatus(status, errorText), Label: ToolActivityLabel(call.name, call.input), Block: block,
		Terminal: terminalEvent, Style: style,
	}
}

func (t *StreamToolTracker) Output(item *v1.ToolOutputDelta) StreamToolReport {
	if item == nil || item.ToolCallID == "" {
		return StreamToolReport{}
	}
	if t.done[item.ToolCallID] {
		return StreamToolReport{}
	}
	call, ok := t.calls[item.ToolCallID]
	if !ok {
		if t.pending == nil {
			t.pending = make(map[string]ShellOutputTail)
		}
		pending := t.pending[item.ToolCallID]
		pending.Write(item.Delta)
		t.pending[item.ToolCallID] = pending
		return StreamToolReport{}
	}
	if call.name == "write_stdin" {
		return StreamToolReport{Line: "  ◐ Running " + ToolActivityLabel(call.name, call.input), Style: call.style}
	}
	call.output.Write(item.Delta)
	t.calls[item.ToolCallID] = call
	return StreamToolReport{Line: "  ◐ Running " + ToolActivityLabel(call.name, call.input), Block: call.output.String(), Style: call.style}
}

func AgentsLoadedPaths(item v1.Event) []string {
	paths, _ := agentsLoadedPathsReported(item)
	return paths
}

func agentsLoadedPathsReported(item v1.Event) ([]string, bool) {
	var payload struct {
		Paths *[]string `json:"agents_files"`
	}
	if json.Unmarshal(item.Data, &payload) != nil || payload.Paths == nil {
		return nil, false
	}
	return *payload.Paths, true
}

func AgentsLoadedActivity(path string) string {
	return "✓ Loaded AGENTS.md from " + path
}

func AgentsLoadedLines(paths []string) []string {
	if len(paths) == 0 {
		return []string{"No AGENTS.md files loaded"}
	}
	lines := make([]string, 0, len(paths))
	for _, path := range paths {
		lines = append(lines, AgentsLoadedActivity(path))
	}
	return lines
}

func AgentsLoadedActivities(item v1.Event) []string {
	paths, reported := agentsLoadedPathsReported(item)
	if !reported {
		return nil
	}
	// Changed events only identify files newly loaded because their contents
	// changed. An empty changed event therefore says nothing about the complete
	// context. Initialization and replacement events are complete snapshots, so
	// they can accurately report that no AGENTS.md files were loaded.
	if len(paths) == 0 && item.Type == "session.context.changed" {
		return nil
	}
	return AgentsLoadedLines(paths)
}

func StreamToolStatus(status, errorText string) string {
	switch status {
	case "pending":
		return "○ Queued tool"
	case "running":
		return "◌ Working: tool"
	case "success":
		return "✓ tool"
	case "failure":
		if errorText != "" {
			return "✗ tool: " + errorText
		}
		return "✗ tool"
	case "interrupted":
		return "■ Interrupted: tool"
	default:
		return "Status: tool " + status
	}
}

// permissionContextLines presents only the tool-provided, human-readable
// description. Descriptions are flattened so permission context occupies one
// line. Policy metadata, resources, canonical input, and structured review data
// remain authorization data and are deliberately not dumped into the dialog.

func FormatJSONAsYAML(input json.RawMessage) string {
	var document yaml.Node
	if err := yaml.Unmarshal(input, &document); err != nil {
		return string(input)
	}
	setYAMLBlockStyle(&document)
	var formatted bytes.Buffer
	encoder := yaml.NewEncoder(&formatted)
	encoder.SetIndent(2)
	if err := encoder.Encode(&document); err != nil {
		return string(input)
	}
	_ = encoder.Close()
	return strings.TrimSuffix(formatted.String(), "\n")
}

func setYAMLBlockStyle(node *yaml.Node) {
	node.Style = 0
	for _, child := range node.Content {
		setYAMLBlockStyle(child)
	}
}

func PermissionContextLines(item v1.Permission) []string {
	if item.ToolID == "write_stdin" {
		var input map[string]any
		if json.Unmarshal(item.CanonicalInput, &input) == nil {
			return []string{ToolActivityLabel(item.ToolID, RedactToolInputForDisplay(item.ToolID, input))}
		}
		return []string{"write_stdin"}
	}
	description := strings.TrimSpace(strings.NewReplacer("\r\n", " ", "\r", " ", "\n", " ").Replace(item.Description))
	if description == "" {
		return nil
	}
	return []string{description}
}
