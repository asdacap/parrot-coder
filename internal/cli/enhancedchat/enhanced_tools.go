package enhancedchat

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
	"unicode"

	v1 "github.com/amirulashraf/parrot-coder/internal/api/v1"
	"github.com/amirulashraf/parrot-coder/internal/cli/chatview"
	"github.com/amirulashraf/parrot-coder/internal/terminal"
)

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

func (r *enhancedChatRuntime) updateToolOutput(output *v1.ToolOutputDelta) {
	if output == nil || output.ToolCallID == "" || r.completedToolIDs[output.ToolCallID] {
		return
	}
	for i := range r.activity {
		if r.activity[i].id == output.ToolCallID {
			if r.activity[i].toolName == "write_stdin" {
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
			if name == "write_stdin" {
				r.activity[i].output = shellOutputTail{}
			}
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
	if name == "write_stdin" {
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
			if r.activity[i].id == callID && r.activity[i].input != nil {
				r.activity[i].block = truncateToolBlock(formatFailedToolRequest(r.activity[i].input), maxToolBlockLines)
				break
			}
		}
	}
	if terminalEvent {
		for i := range r.activity {
			if r.activity[i].id == callID && (r.activity[i].toolName == "shell" || r.activity[i].toolName == "exec_command") {
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
					r.activity[i].block = output
				}
				break
			}
		}
	}
	if terminalEvent {
		if r.completedToolIDs == nil {
			r.completedToolIDs = make(map[string]bool)
		}
		r.completedToolIDs[callID] = true
		delete(r.pendingToolOutput, callID)
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
	if name == "exec_command" || name == "write_stdin" {
		return chatview.ToolActivityLabel(name, chatview.RedactToolInputForDisplay(name, input))
	}
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
	case "agent_spawn":
		add(firstString(input, "agent"))
		add(firstString(input, "prompt"))
	case "agent_send":
		add(firstString(input, "agent_id"))
		add(firstString(input, "message"))
	case "agent_interrupt":
		add(firstString(input, "agent_id"))
	case "agent_wait":
		if ids, ok := input["ids"].([]any); ok {
			add(fmt.Sprintf("%d agents", len(ids)))
		}
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
