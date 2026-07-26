package enhancedchat

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode"

	v1 "github.com/amirulashraf/parrot-coder/internal/api/v1"
	"github.com/amirulashraf/parrot-coder/internal/cli/chatview"
	"github.com/amirulashraf/parrot-coder/internal/terminal"
)

func (r *enhancedChatRuntime) updateToolOutput(output *v1.ToolOutputDelta) {
	if output == nil || output.ToolCallID == "" || r.completedToolIDs[output.ToolCallID] {
		return
	}
	for i := range r.activity {
		if r.activity[i].id == output.ToolCallID {
			if r.presentation().Output(r.activity[i].toolName) == chatview.ToolOutputNone {
				return
			}
			r.activity[i].output.Write(output.Delta)
			return
		}
	}
	if r.pendingToolOutput == nil {
		r.pendingToolOutput = make(map[string]shellOutputTail)
	}
	pending := r.pendingToolOutput[output.ToolCallID]
	pending.Write(output.Delta)
	r.pendingToolOutput[output.ToolCallID] = pending
}

func (r *enhancedChatRuntime) handleToolActivity(item v1.Event) {
	presentation := r.presentation()
	callID, name, input, result := presentation.Payload(item)
	errorText := toolActivityError(item.Data)
	if callID == "" {
		callID = fmt.Sprintf("tool-%d", time.Now().UnixNano())
	}
	label := name
	if input != nil {
		label = presentation.Label(name, input)
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
		if r.activity[i].id == callID {
			if pending, ok := r.pendingToolOutput[callID]; ok {
				r.activity[i].output = pending
				delete(r.pendingToolOutput, callID)
			}
			break
		}
	}
	for i := range r.activity {
		if r.activity[i].id != callID {
			continue
		}
		if name != "" {
			r.activity[i].toolName = name
			if presentation.Output(name) == chatview.ToolOutputNone {
				r.activity[i].output = shellOutputTail{}
			}
		}
		r.activity[i].hidden = !terminalEvent && presentation.TerminalOnly(r.activity[i].toolName)
		if status == "failure" || status == "interrupted" {
			r.activity[i].style = terminal.TextStyleDefault
		} else {
			r.activity[i].style = presentation.Style(r.activity[i].toolName)
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
	if terminalEvent && status == "success" {
		for i := range r.activity {
			if r.activity[i].id == callID {
				r.activity[i].label = presentation.CompletedLabel(r.activity[i].toolName, r.activity[i].input, result)
				break
			}
		}
	}
	if terminalEvent && status == "success" && strings.TrimSpace(result) != "" {
		for i := range r.activity {
			if r.activity[i].id != callID {
				continue
			}
			switch presentation.Result(r.activity[i].toolName) {
			case chatview.ToolResultText:
				r.activity[i].block = truncateToolBlock(result, maxToolBlockLines)
			case chatview.ToolResultDiff:
				r.activity[i].block, r.activity[i].blockKind = result, chatview.ToolResultDiff
			}
			break
		}
	}
	if terminalEvent && status == "success" {
		for i := range r.activity {
			if r.activity[i].id != callID {
				continue
			}
			if presentation.Result(r.activity[i].toolName) == chatview.ToolResultTodos {
				if block, count, ok := formatTodoWriteBlock(result, r.activity[i].input); ok {
					r.activity[i].block, r.activity[i].blockKind = block, ""
					r.activity[i].label = todoWriteActivityLabel(r.activity[i].toolName, count)
				}
			}
			break
		}
	}
	if presentation.Output(name) == chatview.ToolOutputNone {
		errorText = ""
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
			if r.activity[i].id != callID {
				continue
			}
			if presentation.Failure(r.activity[i].toolName) == chatview.ToolFailureErrorBlock {
				r.activity[i].error = chatview.FailureErrorSummary(errorText)
				r.activity[i].block, r.activity[i].blockKind = chatview.FormatFailureErrorBlock(errorText), ""
			} else if r.activity[i].input != nil {
				r.activity[i].block, r.activity[i].blockKind = truncateToolBlock(formatFailedToolRequest(r.activity[i].input), maxToolBlockLines), ""
			}
			break
		}
	}
	if terminalEvent {
		for i := range r.activity {
			if r.activity[i].id == callID {
				if block := presentation.CompletedInputBlock(r.activity[i].toolName, r.activity[i].input); block != "" {
					r.activity[i].block, r.activity[i].blockKind = block, ""
				}
			}
		}
		for i := range r.activity {
			if r.activity[i].id == callID && presentation.Output(r.activity[i].toolName) == chatview.ToolOutputTail {
				output := toolActivityOutputTail(item.Data)
				if output == "" {
					output = r.activity[i].output.String()
				}
				if output == "" && result != "" {
					var tail shellOutputTail
					tail.Write(result)
					output = tail.String()
				}
				if output != "" {
					r.activity[i].block, r.activity[i].blockKind = output, ""
				}
				break
			}
		}
	}
	liveOnly := presentation.LiveOnly(name)
	if terminalEvent && !liveOnly {
		for i := range r.activity {
			if r.activity[i].id == callID {
				liveOnly = presentation.LiveOnly(r.activity[i].toolName)
				break
			}
		}
	}
	if terminalEvent && liveOnly {
		for i := range r.activity {
			if r.activity[i].id == callID {
				r.activity = append(r.activity[:i], r.activity[i+1:]...)
				break
			}
		}
		if r.completedToolIDs == nil {
			r.completedToolIDs = make(map[string]bool)
		}
		r.completedToolIDs[callID] = true
		delete(r.pendingToolOutput, callID)
		return
	}
	if terminalEvent {
		if r.completedToolIDs == nil {
			r.completedToolIDs = make(map[string]bool)
		}
		r.completedToolIDs[callID] = true
		delete(r.pendingToolOutput, callID)
		r.queueCompletedActivity(callID)
		if err := r.flushCompletedActivities(); err != nil {
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
		return chatview.TodoPendingIcon
	case "in_progress":
		return chatview.TodoInProgressIcon
	case "completed":
		return chatview.TodoCompletedIcon
	case "cancelled":
		return chatview.TodoCancelledIcon
	default:
		return chatview.TodoUnknownStatusIcon
	}
}

func toolActivityError(data json.RawMessage) string {
	raw, ok := decodeJSONObject(data)
	if !ok {
		return ""
	}
	return firstString(raw, "error", "error_message", "message")
}

func toolActivityOutputTail(data json.RawMessage) string {
	raw, ok := decodeJSONObject(data)
	if !ok {
		return ""
	}
	return firstString(raw, "output_tail")
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
	return callID, name, chatview.RedactToolInputForDisplay(name, input), result
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

func todoWriteActivityLabel(_ string, count int) string {
	noun := "items"
	if count == 1 {
		noun = "item"
	}
	return fmt.Sprintf("TODO · %d %s", count, noun)
}

// toolActivityLabel delegates to the shared renderer. It survives only for the
// legacy fallback path; declared tools are labelled via Presentations.Label.
func toolActivityLabel(name string, input map[string]any) string {
	return chatview.ToolActivityLabel(name, chatview.RedactToolInputForDisplay(name, input))
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
	// An aider block names its file on the last non-blank line before the
	// SEARCH marker, so report the paths that actually introduce a block. A
	// unified diff names it on the +++ header instead, falling back to the ---
	// header when the target is /dev/null for a deletion.
	seen := make(map[string]bool)
	var targets []string
	candidate, source := "", ""
	add := func(path string) {
		path = cleanActivityDetail(path)
		if path == "" || seen[path] {
			return
		}
		seen[path] = true
		targets = append(targets, path)
	}
	for _, line := range strings.Split(patch, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "```") {
			continue
		}
		switch {
		case strings.HasPrefix(trimmed, "--- "):
			source = diffHeaderPath(trimmed)
		case strings.HasPrefix(trimmed, "+++ "):
			if path := diffHeaderPath(trimmed); path != "" {
				add(path)
			} else {
				add(source)
			}
		case trimmed == "<<<<<<< SEARCH":
			add(candidate)
		default:
			candidate = trimmed
		}
	}
	if len(targets) <= 2 {
		return targets
	}
	return []string{targets[0], targets[1], fmt.Sprintf("+%d more", len(targets)-2)}
}

// diffHeaderPath returns the workspace path named by a unified diff --- or +++
// header, or an empty string for /dev/null.
func diffHeaderPath(header string) string {
	value := strings.TrimSpace(header[len("--- "):])
	if index := strings.IndexByte(value, '\t'); index >= 0 {
		value = strings.TrimSpace(value[:index])
	}
	if value == "/dev/null" {
		return ""
	}
	for _, prefix := range []string{"a/", "b/"} {
		if after, ok := strings.CutPrefix(value, prefix); ok {
			return after
		}
	}
	return value
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
