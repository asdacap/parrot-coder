package chatview

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"unicode"

	v1 "github.com/amirulashraf/parrot-coder/internal/api/v1"
	managedtask "github.com/amirulashraf/parrot-coder/internal/task"
	"github.com/amirulashraf/parrot-coder/internal/terminal"
	"go.yaml.in/yaml/v3"
)

const (
	MaxToolBlockLines   = 10
	maxShellOutputLines = 10
	maxShellOutputBytes = 16 << 10
)

// AgentEmptyResponseText is shown when an assistant completes without
// emitting any text and did not issue a tool request.
const AgentEmptyResponseText = "(agent responded with empty string)"

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

// FormatTokenCount keeps small counts exact and abbreviates larger counts with
// one decimal place, scaling from thousands (k) to millions (M) so large totals
// stay readable instead of growing to unwieldy thousands like 11646.5k.
func FormatTokenCount(tokens int) string {
	if tokens < 1000 {
		return fmt.Sprintf("%d", tokens)
	}
	if tokens < 1000000 {
		return fmt.Sprintf("%.1fk", float64(tokens)/1000)
	}
	return fmt.Sprintf("%.1fM", float64(tokens)/1000000)
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

var (
	aggregateDiagnosticHeader  = regexp.MustCompile(`^patch planning failed with \d+ errors:$`)
	aggregateDiagnosticHeading = regexp.MustCompile(`^\[(\d+)/(\d+)\](?:\s|$)`)
)

// FormatFailureErrorBlock makes a full tool error visually distinct without
// changing its text. Numbered aggregate headings are preserved exactly and
// separated by the blank lines supplied by the producer.
func FormatFailureErrorBlock(errorText string) string {
	errorText = strings.TrimSpace(strings.ReplaceAll(errorText, "\r\n", "\n"))
	if errorText == "" {
		return ""
	}
	sections, aggregate := aggregateDiagnosticSections(errorText)
	if !aggregate {
		return "✗ Error\n" + errorText
	}
	for i, section := range sections {
		sections[i] = "✗ " + section
	}
	return strings.Join(sections, "\n\n")
}

// FailureErrorSummary returns only status-row-safe text for a dedicated error
// block. The body itself remains exclusively in the permanent block.
func FailureErrorSummary(errorText string) string {
	sections, aggregate := aggregateDiagnosticSections(strings.TrimSpace(strings.ReplaceAll(errorText, "\r\n", "\n")))
	if !aggregate {
		return ""
	}
	first, _, _ := strings.Cut(sections[0], "\n")
	matches := aggregateDiagnosticHeading.FindStringSubmatch(first)
	if len(matches) == 3 {
		return matches[2] + " errors"
	}
	return fmt.Sprintf("%d errors", len(sections))
}

func aggregateDiagnosticSections(errorText string) ([]string, bool) {
	sections := strings.Split(errorText, "\n\n")
	if len(sections) < 2 || !aggregateDiagnosticHeader.MatchString(sections[0]) {
		return sections, false
	}
	sections = sections[1:]
	for _, section := range sections {
		first, _, _ := strings.Cut(section, "\n")
		if !aggregateDiagnosticHeading.MatchString(first) {
			return sections, false
		}
	}
	return sections, true
}

// RedactToolInputForDisplay returns a presentation-only copy. Exact tool input
// remains available to authorization and execution but stdin is never rendered.
//
// Deprecated: this is the fallback for servers which do not declare which of a
// tool's fields are sensitive. Prefer Presentations.Redact, which asks the tool.
func RedactToolInputForDisplay(name string, input map[string]any) map[string]any {
	if input == nil || name != "write_stdin" {
		return input
	}
	return redactFields(input, []string{"chars", "input"})
}

// redactFields copies input, replacing the named fields with a length summary.
func redactFields(input map[string]any, fields []string) map[string]any {
	if input == nil {
		return nil
	}
	redacted := make(map[string]any, len(input))
	for key, value := range input {
		redacted[key] = value
	}
	for _, key := range fields {
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

// ToolActivityPayload decodes a tool activity event and applies the fallback
// redaction. Prefer Presentations.Payload, which redacts the fields the tool
// itself declared sensitive.
func ToolActivityPayload(data json.RawMessage) (string, string, map[string]any, string) {
	callID, name, input, result := toolActivityRaw(data)
	return callID, name, RedactToolInputForDisplay(name, input), result
}

// toolActivityRaw decodes without redacting. Callers must redact before display.
func toolActivityRaw(data json.RawMessage) (string, string, map[string]any, string) {
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
	case "read":
		add(firstString(input, "path", "file", "filePath"))
	case "glob":
		quoted(firstString(input, "pattern"))
	case "rg":
		quoted(firstString(input, "pattern"))
		path := firstString(input, "path")
		if path == "" {
			path = "."
		}
		add(path)
	case "apply_patch":
		details = append(details, patchActivityTargets(firstString(input, "patchText", "patch"))...)
	case "exec_command":
		add(firstString(input, "name"))
		add(firstString(input, "cmd"))
	case "write_stdin":
		add(firstString(input, "task_id"))
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
	case "agent_spawn":
		add(firstString(input, "agent"))
		add(firstString(input, "prompt"))
	case "agent_send":
		add(firstString(input, "session_id"))
		add(firstString(input, "message"))
	case "task_interrupt":
		add(firstString(input, "task_id"))
	case "wait_agent":
		add(firstString(input, "session_id"))
	default:
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

type streamToolCall struct {
	name     string
	input    map[string]any
	style    terminal.TextStyle
	result   string
	stream   string
	failure  string
	liveOnly bool
	output   ShellOutputTail
}

type StreamToolTracker struct {
	// Presentation is what each tool declared about itself. Its zero value
	// describes nothing, so an unset tracker renders through the fallbacks.
	Presentation Presentations
	calls        map[string]streamToolCall
	pending      map[string]ShellOutputTail
	done         map[string]bool
}

type StreamToolReport struct {
	Line      string
	Label     string
	Block     string
	BlockKind string
	Terminal  bool
	Hidden    bool
	LiveOnly  bool
	Style     terminal.TextStyle
}

type taskMessageState struct {
	text             strings.Builder
	reasoning        strings.Builder
	reasoningSummary bool
	reasoningDone    bool
}

// TaskReport is one renderable unit derived from a flat task event. TaskID
// groups frames, while SessionID and ParentSessionID locate them in the session
// hierarchy. MainStatus marks the report as primary status.
type TaskReport struct {
	ID              string
	TaskID          string
	SessionID       string
	ParentSessionID string
	Line            string
	Block           string
	BlockKind       string
	BlockLanguage   string
	Terminal        bool
	EmitPlain       bool
	Skip            bool
	MainStatus      bool
	Style           terminal.TextStyle
}

// TaskInfo is the read-only public projection of one task tracked from the
// event stream. It intentionally contains values rather than tracker-owned
// state so callers cannot mutate the tree.
type TaskInfo struct {
	TaskID          string
	SessionID       string
	ParentSessionID string
	Kind            string
	Agent           string
	Name            string
	Status          string
}

// taskNode is one task in the tracker. Session ancestry determines hierarchy.
type taskNode struct {
	id              string
	sessionID       string
	parentSessionID string
	kind            string
	agent           string
	name            string
	status          string
	error           string
	orphan          bool

	messages         map[string]*taskMessageState
	tools            *StreamToolTracker
	done             map[string]bool
	progress         *v1.TaskProgress
	progressID       string
	progressDone     bool
	progressFlushed  bool
	finished         bool
	lifecycleFlushed bool

	direct TaskUsage
}

// TaskTracker rebuilds the task tree from flat task events and renders task
// activity. The tracker derives hierarchy from the session_id and
// parent_session_id carried by task.start. Any event for a task the tracker has
// never seen produces an unknown-task error.
type TaskTracker struct {
	// Presentation is forwarded to every per-task tool tracker. Its zero value
	// describes nothing, so an unset tracker renders through the fallbacks.
	Presentation    Presentations
	tasks           map[string]*taskNode
	sessions        map[string]*taskNode
	unknownReported map[string]bool
}

func NewTaskTracker() *TaskTracker {
	tracker := &TaskTracker{tasks: make(map[string]*taskNode), sessions: make(map[string]*taskNode)}
	tracker.tasks[managedtask.MainTaskID] = &taskNode{id: managedtask.MainTaskID, kind: string(managedtask.KindMain)}
	return tracker
}

func taskAgentLabel(agent, name string) string {
	agent = strings.TrimSpace(agent)
	name = strings.TrimSpace(name)
	if agent == "" {
		return name
	}
	if name == "" {
		return agent
	}
	return agent + ":" + name
}

// EventLine renders agent and subagent events with one layout. Indent is the
// task depth and agent is the optional label shown in brackets. The event owns
// its leading icon; a generic activity icon is added if it does not supply one.
func EventLine(indent int, agent, event string) string {
	if indent < 0 {
		indent = 0
	}
	icon, event := splitEventIcon(event)
	if icon == "" {
		icon = "•"
	}
	indentation := strings.Repeat("  ", indent)
	agent = strings.TrimSpace(agent)
	prefix := indentation + icon + " "
	if agent != "" {
		prefix += "[" + agent + "] "
	}

	lines := strings.Split(event, "\n")
	lines[0] = prefix + lines[0]
	continuation := indentation
	if agent != "" {
		continuation += "[" + agent + "] "
	}
	for i := 1; i < len(lines); i++ {
		lines[i] = continuation + lines[i]
	}
	return strings.Join(lines, "\n")
}

func prefixTaskText(prefix, text string) string {
	lines := strings.Split(text, "\n")
	for i := range lines {
		lines[i] = prefix + lines[i]
	}
	return strings.Join(lines, "\n")
}

func splitEventIcon(event string) (string, string) {
	trimmed := strings.TrimLeft(event, " ")
	icons := append([]string{"•", "○", "◌", "◐", "✓", "✗", "■", "↻"}, SpinnerFrames...)
	for _, icon := range icons {
		if rest, ok := strings.CutPrefix(trimmed, icon+" "); ok {
			return icon, rest
		}
	}
	return "", event
}

// depth resolves a task's indentation by walking the parent chain to the main
// task. Orphaned tasks, whose ancestry is unknown, render at depth one.
func (t *TaskTracker) depth(node *taskNode) int {
	depth := 0
	for current := node; current != nil; {
		if current.id == managedtask.MainTaskID || current.parentSessionID == "" {
			return depth
		}
		parent := t.sessions[current.parentSessionID]
		if parent == nil || depth > len(t.tasks) {
			return depth + 1
		}
		depth++
		current = parent
	}
	return depth
}

func (t *TaskTracker) eventLine(node *taskNode, event string) string {
	label := taskAgentLabel(node.agent, node.name)
	if label == "" {
		label = "agent"
	}
	return EventLine(max(1, t.depth(node)), label, event)
}

// unknownTask reports an event for a task the tracker never registered. The
// error is emitted once per unknown task; later events for the same unknown
// task are dropped so one bad id cannot flood the transcript.
func (t *TaskTracker) unknownTask(taskID, eventType string) []TaskReport {
	if t.unknownReported == nil {
		t.unknownReported = make(map[string]bool)
	}
	if t.unknownReported[taskID] {
		return nil
	}
	t.unknownReported[taskID] = true
	return []TaskReport{{ID: "unknown-task:" + taskID, Line: "✗ unknown task " + taskID + " (" + eventType + ")", Terminal: true, EmitPlain: true, Style: terminal.TextStyleDefault}}
}

func (t *TaskTracker) known(id string) *taskNode {
	if id == "" {
		return nil
	}
	return t.tasks[id]
}

// Tasks returns a deterministic snapshot of the tracked task tree. Parents
// precede their children and siblings are ordered by task id. A task whose
// parent is absent is treated as an additional root.
func (t *TaskTracker) Tasks() []TaskInfo {
	children := make(map[string][]*taskNode)
	roots := make([]*taskNode, 0)
	for _, node := range t.tasks {
		if node.parentSessionID == "" || t.sessions[node.parentSessionID] == nil {
			roots = append(roots, node)
		} else {
			children[node.parentSessionID] = append(children[node.parentSessionID], node)
		}
	}
	sortNodes := func(nodes []*taskNode) {
		sort.Slice(nodes, func(i, j int) bool { return nodes[i].id < nodes[j].id })
	}
	sortNodes(roots)
	for _, nodes := range children {
		sortNodes(nodes)
	}

	result := make([]TaskInfo, 0, len(t.tasks))
	seen := make(map[string]bool, len(t.tasks))
	var appendTree func(*taskNode)
	appendTree = func(node *taskNode) {
		if seen[node.id] {
			return
		}
		seen[node.id] = true
		result = append(result, TaskInfo{TaskID: node.id, SessionID: node.sessionID, ParentSessionID: node.parentSessionID, Kind: node.kind, Agent: node.agent, Name: node.name, Status: node.status})
		for _, child := range children[node.sessionID] {
			appendTree(child)
		}
	}
	for _, root := range roots {
		appendTree(root)
	}
	// Malformed cyclic ancestry has no root. Keep the projection complete and
	// deterministic without allowing such input to recurse forever.
	remaining := make([]*taskNode, 0)
	for _, node := range t.tasks {
		if !seen[node.id] {
			remaining = append(remaining, node)
		}
	}
	sortNodes(remaining)
	for _, node := range remaining {
		appendTree(node)
	}
	return result
}

// TaskUsage is what one task spent. Cost is the runner's price for the tokens,
// so it is reported alongside them rather than accounted separately.
type TaskUsage struct {
	InputTokens  int
	OutputTokens int
	CachedTokens int
	Cost         float64
}

func (u *TaskUsage) add(other TaskUsage) {
	u.InputTokens += other.InputTokens
	u.OutputTokens += other.OutputTokens
	u.CachedTokens += other.CachedTokens
	u.Cost += other.Cost
}

// AddUsage folds one turn of provider usage into the task that spent it. Every
// session on the stream reports usage the same way — the main session under the
// main task id, each subagent under its own — so the tree is the single owner
// of what has been spent and the main task is nothing but its root. An event
// without a task id belongs to the main task; usage for a task the tree has
// never seen is dropped rather than counted against the wrong one. Counts
// accumulate because a usage event reports only its own turn.
func (t *TaskTracker) AddUsage(taskID string, usage v1.Usage) {
	if taskID == "" {
		taskID = managedtask.MainTaskID
	}
	node := t.tasks[taskID]
	if node == nil {
		return
	}
	node.direct.add(TaskUsage{InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens, CachedTokens: usage.CachedInputTokens, Cost: usage.InputCost + usage.OutputCost})
}

// CumulativeUsage returns what the task and all its descendants spent. This
// walks the tree once per call — safe for small task counts (<100).
func (t *TaskTracker) CumulativeUsage(taskID string) TaskUsage {
	node := t.tasks[taskID]
	if node == nil {
		return TaskUsage{}
	}
	return t.nodeCumulativeUsage(node)
}

func (t *TaskTracker) nodeCumulativeUsage(node *taskNode) TaskUsage {
	total := node.direct
	for _, child := range t.tasks {
		if child != node && child.parentSessionID == node.sessionID {
			total.add(t.nodeCumulativeUsage(child))
		}
	}
	return total
}

// CodeDisplayStatus formats the transcript row introducing an atomic source
// block. Location metadata belongs in the status rather than in the source so
// the latter can be passed unchanged to syntax highlighting.
func CodeDisplayStatus(display v1.CodeDisplay) string {
	location := strings.NewReplacer("\n", " ", "\r", " ", "\t", " ").Replace(terminal.Sanitize(display.Path))
	if display.StartLine > 0 {
		if location == "" {
			location = fmt.Sprintf("line %d", display.StartLine)
		} else {
			location += fmt.Sprintf(":%d", display.StartLine)
		}
	}
	if location == "" {
		return "↳ Code"
	}
	return "↳ Code · " + location
}

// IsTaskEvent reports whether an event belongs to the task tree rather than to
// the main transcript: task lifecycle events always do, and any event
// attributed to a task other than the session's main task does. Every renderer
// splits the stream with this one predicate, so the main task cannot be a task
// to one of them and not to another.
func IsTaskEvent(item v1.Event) bool {
	switch item.Type {
	case v1.EventTaskStart, v1.EventTaskWorking, v1.EventTaskIdle, v1.EventTaskFinished:
		return true
	}
	return item.TaskID != "" && item.TaskID != managedtask.MainTaskID
}

// Apply folds one flat event into the task tree and returns what to render.
// Events belonging to the session's main task return nil; the caller renders
// those through the main transcript path instead.
func (t *TaskTracker) Apply(item v1.Event, thinking bool) ([]TaskReport, error) {
	reports, err := t.apply(item, thinking)
	if err != nil {
		return nil, err
	}
	ownerID := item.TaskID
	if ownerID == "" {
		ownerID = managedtask.MainTaskID
	}
	owner := t.tasks[ownerID]
	for i := range reports {
		reports[i].TaskID = ownerID
		if owner != nil {
			reports[i].SessionID = owner.sessionID
			reports[i].ParentSessionID = owner.parentSessionID
		}
		reports[i].MainStatus = item.Type == v1.EventTaskFinished
	}
	if item.Type == v1.EventTaskProgress || item.Type == v1.EventTaskStart || item.Type == v1.EventTaskWorking || item.Type == v1.EventTaskIdle || item.Type == v1.EventTaskFinished {
		reports = append(reports, t.taskStatusReports(item.TaskID)...)
	}
	return reports, nil
}

func (t *TaskTracker) taskActive(node *taskNode) bool {
	if node == nil {
		return false
	}
	selfActive := node.status == "working"
	if node.progress != nil {
		selfActive = !node.progressDone
	}
	if selfActive {
		return true
	}
	for _, child := range t.tasks {
		if child.parentSessionID == node.sessionID && t.taskActive(child) {
			return true
		}
	}
	return false
}

func (t *TaskTracker) activeChildCount(node *taskNode) int {
	count := 0
	for _, child := range t.tasks {
		if child.parentSessionID == node.sessionID && t.taskActive(child) {
			count++
		}
	}
	return count
}

func (t *TaskTracker) taskStatusReports(taskID string) []TaskReport {
	var reports []TaskReport
	for node := t.tasks[taskID]; node != nil && node.id != managedtask.MainTaskID; node = t.sessions[node.parentSessionID] {
		children := t.activeChildCount(node)
		if node.progress == nil {
			if !node.finished || node.lifecycleFlushed || children != 0 {
				continue
			}
			icon, body, style := "✓", "completed", terminal.TextStyleMuted
			if node.status != "" && node.status != "succeeded" {
				icon, body, style = "✗", node.status, terminal.TextStyleDefault
				if node.error != "" {
					body += ": " + cleanActivityDetail(node.error)
				}
			}
			node.lifecycleFlushed = true
			reports = append(reports, TaskReport{
				ID: node.id + ":lifecycle", TaskID: node.id, SessionID: node.sessionID, ParentSessionID: node.parentSessionID,
				Line: t.eventLine(node, icon+" "+body), Terminal: true,
				EmitPlain: true, MainStatus: true, Style: style,
			})
			continue
		}
		if node.progressFlushed {
			continue
		}
		body := fmt.Sprintf("agent: %s · %s tokens · %d tools", node.progress.Agent, FormatTokenCount(node.progress.Usage.TotalTokens), node.progress.ToolUses)
		if children > 0 {
			unit := "active task"
			if children != 1 {
				unit += "s"
			}
			body += fmt.Sprintf(" · %d %s", children, unit)
		}
		terminalEvent := node.progressDone && children == 0
		icon := SpinnerFrames[0]
		if terminalEvent {
			icon = "✓"
			if node.progress.Status != "" && node.progress.Status != "succeeded" {
				icon = "✗"
				if node.error != "" {
					body += ": " + cleanActivityDetail(node.error)
				}
			}
			node.progressFlushed = true
		}
		reports = append(reports, TaskReport{
			ID: node.progressID, TaskID: node.id, SessionID: node.sessionID, ParentSessionID: node.parentSessionID,
			Line: t.eventLine(node, icon+" "+body), Terminal: terminalEvent,
			EmitPlain: terminalEvent, MainStatus: true, Style: terminal.TextStyleMuted,
		})
	}
	return reports
}

func (t *TaskTracker) apply(item v1.Event, thinking bool) ([]TaskReport, error) {
	if item.Type == v1.EventTaskStart || item.Type == v1.EventTaskWorking || item.Type == v1.EventTaskIdle || item.Type == v1.EventTaskFinished {
		return t.applyLifecycle(item)
	}
	taskID := item.TaskID
	if taskID == "" {
		taskID = managedtask.MainTaskID
	}
	node := t.known(taskID)
	if node == nil {
		return t.unknownTask(taskID, item.Type), nil
	}
	if taskID == managedtask.MainTaskID {
		return nil, nil
	}
	if !v1.KnownEvent(item.Type) {
		return nil, nil
	}
	scope := node.id + ":"
	switch item.Type {
	case "session.assistant.started":
		var payload struct {
			MessageID string `json:"message_id"`
		}
		if err := json.Unmarshal(item.Data, &payload); err != nil {
			return nil, err
		}
		messageID := payload.MessageID
		if messageID == "" {
			messageID = "assistant"
		}
		key := scope + "message:" + messageID
		if node.done[key] {
			return nil, nil
		}
		if node.messages == nil {
			node.messages = make(map[string]*taskMessageState)
		}
		if node.messages[key] == nil {
			node.messages[key] = &taskMessageState{}
		}
		return nil, nil
	case v1.EventMessagePartDelta:
		payload, err := v1.DecodeEventData(item)
		if err != nil {
			return nil, err
		}
		delta := payload.(*v1.MessagePartDelta)
		messageID := delta.MessageID
		if messageID == "" {
			messageID = "assistant"
		}
		key := scope + "message:" + messageID
		if node.done[key] {
			return nil, nil
		}
		if node.messages == nil {
			node.messages = make(map[string]*taskMessageState)
		}
		state := node.messages[key]
		if state == nil {
			state = &taskMessageState{}
			node.messages[key] = state
		}
		switch delta.Kind {
		case "text":
			state.text.WriteString(delta.Delta)
			if strings.TrimSpace(state.text.String()) == "" {
				return nil, nil
			}
			line := t.eventLine(node, "○ response: "+state.text.String())
			return []TaskReport{{ID: key + ":response", Line: line, Style: terminal.TextStyleMuted}}, nil
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
			line := t.eventLine(node, icon+" Thought: "+SingleLineReasoningSummary(state.reasoning.String()))
			return []TaskReport{{ID: key + ":reasoning", Line: line, Terminal: delta.Done, EmitPlain: delta.Done, Style: terminal.TextStyleMuted}}, nil
		case "reasoning":
			if !thinking || state.reasoningSummary {
				return nil, nil
			}
			state.reasoning.WriteString(delta.Delta)
			if strings.TrimSpace(state.reasoning.String()) == "" {
				return nil, nil
			}
			line := t.eventLine(node, SpinnerFrames[0]+" Reasoning: "+SingleLineReasoningSummary(state.reasoning.String()))
			return []TaskReport{{ID: key + ":reasoning", Line: line, Style: terminal.TextStyleMuted}}, nil
		case "tool_input":
			return nil, nil
		default:
			return []TaskReport{{ID: key + ":status", Line: t.eventLine(node, "status: "+delta.Kind), Style: terminal.TextStyleMuted}}, nil
		}
	case "session.assistant.complete", "session.assistant.error", "session.assistant.interrupted":
		var payload struct {
			MessageID string `json:"message_id"`
			Error     string `json:"error"`
		}
		if err := json.Unmarshal(item.Data, &payload); err != nil {
			return nil, err
		}
		messageID := payload.MessageID
		if messageID == "" {
			messageID = "assistant"
		}
		key := scope + "message:" + messageID
		state := node.messages[key]
		if node.done == nil {
			node.done = make(map[string]bool)
		}
		node.done[key] = true
		status := "○"
		if item.Type == "session.assistant.error" {
			status = "✗"
		} else if item.Type == "session.assistant.interrupted" {
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
		delete(node.messages, key)
		var reports []TaskReport
		if state != nil && !state.reasoningDone && strings.TrimSpace(state.reasoning.String()) != "" {
			reasoning := TaskReport{ID: key + ":reasoning", Terminal: true, Skip: text != "", Style: terminal.TextStyleMuted}
			if text == "" || state.reasoningSummary {
				label := "Reasoning: "
				if state.reasoningSummary {
					label = "Thought: "
				}
				reasoning.Line = t.eventLine(node, "✓ "+label+SingleLineReasoningSummary(state.reasoning.String()))
				reasoning.EmitPlain = true
				reasoning.Skip = false
			}
			reports = append(reports, reasoning)
		}
		if text == "" && item.Type == "session.assistant.complete" {
			return append(reports, TaskReport{ID: key + ":response", Terminal: true, Skip: true}), nil
		}
		if text == "" {
			text = "response complete"
		}
		return append(reports, TaskReport{ID: key + ":response", Line: t.eventLine(node, status+" "+text), Terminal: true, EmitPlain: true, Style: terminal.TextStyleMuted}), nil
	case "session.tool.pending", "session.tool.running", "session.tool.success", "session.tool.failure", "session.tool.interrupted":
		if node.tools == nil {
			node.tools = &StreamToolTracker{Presentation: t.Presentation}
		}
		callID, _, _, _ := t.Presentation.Payload(item.Data)
		report := node.tools.DescribeReport(item)
		line := report.Line
		if report.Label != "" {
			line = strings.Replace(line, "tool", report.Label, 1)
		}
		block := report.Block
		if block != "" && report.BlockKind != ToolResultDiff {
			block = prefixTaskText(strings.Repeat("  ", max(1, t.depth(node)))+"  ", block)
		}
		return []TaskReport{{ID: scope + "tool:" + callID, Line: t.eventLine(node, line), Block: block, BlockKind: report.BlockKind, Terminal: report.Terminal, EmitPlain: !report.LiveOnly, Skip: report.Hidden || report.Terminal && report.LiveOnly, Style: report.Style}}, nil
	case v1.EventCodeDisplay:
		payload, err := v1.DecodeEventData(item)
		if err != nil {
			return nil, err
		}
		display := payload.(*v1.CodeDisplay)
		return []TaskReport{{
			ID: scope + "code:" + display.ToolCallID, Line: t.eventLine(node, CodeDisplayStatus(*display)),
			Block: display.Source, BlockKind: ToolResultCode, BlockLanguage: display.Language,
			Terminal: true, EmitPlain: true, Style: terminal.TextStyleMuted,
		}}, nil
	case v1.EventToolOutputDelta:
		if node.tools == nil {
			node.tools = &StreamToolTracker{Presentation: t.Presentation}
		}
		payload, err := v1.DecodeEventData(item)
		if err != nil {
			return nil, err
		}
		output := payload.(*v1.ToolOutputDelta)
		report := node.tools.Output(output)
		if report.Line == "" {
			return nil, nil
		}
		block := prefixTaskText(strings.Repeat("  ", max(1, t.depth(node)))+"  ", report.Block)
		return []TaskReport{{ID: scope + "tool:" + output.ToolCallID, Line: t.eventLine(node, report.Line), Block: block, Style: report.Style}}, nil
	case v1.EventTaskProgress:
		payload, err := v1.DecodeEventData(item)
		if err != nil {
			return nil, err
		}
		progress := payload.(*v1.TaskProgress)
		id := scope + "task:" + progress.ToolCallID
		if progress.ToolCallID == "" {
			id = scope + "task:" + progress.TaskID
		}
		if node.done[id] {
			return nil, nil
		}
		if node.progressID != id {
			node.progressFlushed = false
		}
		copy := *progress
		node.progress, node.progressID = &copy, id
		node.progressDone = progress.Status != "pending" && progress.Status != "running"
		if node.progressDone {
			if node.done == nil {
				node.done = make(map[string]bool)
			}
			node.done[id] = true
		}
		return nil, nil
	case v1.EventSessionStatus:
		payload, err := v1.DecodeEventData(item)
		if err != nil {
			return nil, err
		}
		status := payload.(*v1.SessionStatus)
		if status.Kind == "running" || status.Kind == "idle" || status.Kind == "finish" || status.Kind == "usage" || status.Kind == "tool_call_complete" {
			return nil, nil
		}
		line := "status: " + status.Kind
		if status.Message != "" {
			line = status.Message
		}
		return []TaskReport{{ID: scope + "status:" + status.MessageID, Line: t.eventLine(node, "↻ "+line), Terminal: true, EmitPlain: true, Style: terminal.TextStyleMuted}}, nil
	case "session.context.initialized", "session.context.changed", "session.context.replaced":
		lines := AgentsLoadedActivities(item)
		reports := make([]TaskReport, 0, len(lines))
		for i, line := range lines {
			reports = append(reports, TaskReport{ID: fmt.Sprintf("%scontext:%d", scope, i), Line: t.eventLine(node, line), Terminal: true, EmitPlain: true, Style: terminal.TextStyleMuted})
		}
		return reports, nil
	default:
		return nil, nil
	}
}

// applyLifecycle folds one task lifecycle event into the tree. task.start is
// the only event which introduces a task; every other lifecycle event for an
// unregistered task is an unknown-task error.
func (t *TaskTracker) applyLifecycle(item v1.Event) ([]TaskReport, error) {
	payload, err := v1.DecodeEventData(item)
	if err != nil {
		return nil, err
	}
	event := payload.(*v1.TaskEvent)
	if event.TaskID == "" {
		return nil, nil
	}
	switch item.Type {
	case v1.EventTaskStart:
		node := t.tasks[event.TaskID]
		if node == nil {
			node = &taskNode{id: event.TaskID}
			t.tasks[event.TaskID] = node
		}
		node.kind = event.Kind
		if event.Agent != "" {
			node.agent = event.Agent
		}
		if event.Name != "" {
			node.name = event.Name
			if t.Presentation.taskNames == nil {
				t.Presentation.taskNames = make(map[string]string)
			}
			t.Presentation.taskNames[event.TaskID] = event.Name
		}
		node.status = "working"
		node.sessionID = event.SessionID
		node.parentSessionID = event.ParentSessionID
		if event.SessionID != "" {
			t.sessions[event.SessionID] = node
		}
		if event.ParentSessionID != "" && t.sessions[event.ParentSessionID] == nil {
			node.orphan = true
			return t.unknownTask(event.ParentSessionID, "parent session of "+event.SessionID), nil
		}
		return nil, nil
	case v1.EventTaskWorking:
		node := t.known(event.TaskID)
		if node == nil {
			return t.unknownTask(event.TaskID, item.Type), nil
		}
		node.status = "working"
		node.error = ""
		node.progress, node.progressID = nil, ""
		node.progressDone, node.progressFlushed, node.finished, node.lifecycleFlushed = false, false, false, false
		return nil, nil
	case v1.EventTaskIdle:
		node := t.known(event.TaskID)
		if node == nil {
			return t.unknownTask(event.TaskID, item.Type), nil
		}
		node.status = "idle"
		return nil, nil
	case v1.EventTaskFinished:
		node := t.known(event.TaskID)
		if node == nil {
			return t.unknownTask(event.TaskID, item.Type), nil
		}
		node.status = event.Status
		node.error = event.Error
		node.finished = true
		node.lifecycleFlushed = false
		if node.id == managedtask.MainTaskID {
			return nil, nil
		}
		return nil, nil
	default:
		return nil, nil
	}
}

// ToolActivityStyle returns the transcript style shared by the standard and
// enhanced chat renderers. Read-only discovery and retrieval tools are muted
// so they remain visible without competing with actions that change state.
func ToolActivityStyle(name string) terminal.TextStyle {
	switch name {
	case "read", "rg", "glob", "web_fetch":
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
	callID, name, input, result := t.Presentation.Payload(item.Data)
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
		call.style = t.Presentation.Style(name)
		call.result = t.Presentation.Result(name)
		call.stream = t.Presentation.Output(name)
		call.failure = t.Presentation.Failure(name)
		call.liveOnly = t.Presentation.LiveOnly(name)
	}
	if input != nil {
		call.input = input
	}
	if result != "" {
		call.input = t.Presentation.EnrichLabelInput(call.name, call.input, result)
	}
	if callID != "" {
		t.calls[callID] = call
	}
	status := strings.TrimPrefix(item.Type, "session.tool.")
	terminalEvent := status == "success" || status == "failure" || status == "interrupted"
	block, blockKind := "", ""
	if status == "success" && call.result == ToolResultText && strings.TrimSpace(result) != "" {
		block = TruncateToolBlock(result, MaxToolBlockLines)
	} else if status == "success" && call.result == ToolResultDiff && strings.TrimSpace(result) != "" {
		block, blockKind = result, ToolResultDiff
	} else if status == "success" && call.result == ToolResultTodos {
		if formatted, _, ok := FormatTodoWriteBlock(result, call.input); ok {
			block = formatted
		}
	} else if status == "failure" && call.failure == ToolFailureErrorBlock {
		block = FormatFailureErrorBlock(ToolActivityError(item.Data))
	} else if status == "failure" && call.input != nil {
		block = TruncateToolBlock(FormatFailedToolRequest(call.input), MaxToolBlockLines)
	}
	if terminalEvent {
		if inputBlock := t.Presentation.CompletedInputBlock(call.name, call.input); inputBlock != "" {
			block, blockKind = inputBlock, ""
		}
	}
	if terminalEvent && call.stream == ToolOutputTail {
		output := ToolActivityOutputTail(item.Data)
		if output == "" {
			output = call.output.String()
		}
		if output == "" && result != "" {
			call.output.Write(result)
			output = call.output.String()
		}
		if output != "" {
			block, blockKind = output, ""
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
	if call.failure == ToolFailureErrorBlock {
		errorText = FailureErrorSummary(errorText)
	}
	if call.stream == ToolOutputNone {
		errorText, block = "", ""
	}
	return StreamToolReport{
		Line: StreamToolStatus(status, errorText), Label: t.Presentation.Label(call.name, call.input), Block: block, BlockKind: blockKind,
		Terminal: terminalEvent, Hidden: !terminalEvent && t.Presentation.TerminalOnly(call.name), LiveOnly: call.liveOnly, Style: style,
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
	if call.stream == ToolOutputNone {
		return StreamToolReport{Line: "  ◐ Running " + t.Presentation.Label(call.name, call.input), Style: call.style}
	}
	call.output.Write(item.Delta)
	t.calls[item.ToolCallID] = call
	return StreamToolReport{Line: "  ◐ Running " + t.Presentation.Label(call.name, call.input), Block: call.output.String(), Style: call.style}
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

// PermissionContextLines renders the prompt context for a permission request.
//
// Deprecated: prefer Presentations.PermissionContextLines, which asks the tool
// whether its description would echo a redacted value.
func PermissionContextLines(item v1.Permission) []string {
	var empty Presentations
	return empty.PermissionContextLines(item)
}

// PermissionContextLines renders the prompt context for a permission request.
// A tool which declared LabelInPermission has its label shown instead of its
// own description, because that description would echo a redacted value.
func (p Presentations) PermissionContextLines(item v1.Permission) []string {
	if p.labelInPermission(item.ToolID) {
		var input map[string]any
		if json.Unmarshal(item.CanonicalInput, &input) == nil {
			return []string{p.Label(item.ToolID, p.Redact(item.ToolID, input))}
		}
		return []string{item.ToolID}
	}
	description := strings.TrimSpace(strings.NewReplacer("\r\n", " ", "\r", " ", "\n", " ").Replace(item.Description))
	if description == "" {
		return nil
	}
	return []string{description}
}

func (p Presentations) labelInPermission(name string) bool {
	if presentation, declared := p.byID[name]; declared {
		return presentation.LabelInPermission
	}
	return name == "write_stdin"
}
