package chatview

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	v1 "github.com/amirulashraf/parrot-coder/internal/api/v1"
	"github.com/amirulashraf/parrot-coder/internal/terminal"
	"go.yaml.in/yaml/v3"
)

// Label strategies, mirroring tool.LabelKind. They name a rendering approach
// rather than a tool, so the renderer never branches on tool identity.
const (
	toolLabelPatchTargets = "patch_targets"
	toolLabelItemCount    = "item_count"
)

// Output modes, mirroring tool.OutputMode.
const (
	ToolOutputTail = "tail"
	ToolOutputNone = "none"
)

// Result renderers, mirroring tool.ResultRender.
const (
	ToolResultText  = "text"
	ToolResultDiff  = "diff"
	ToolResultCode  = "code"
	ToolResultTodos = "todos"
)

// Completed-label strategies, mirroring tool.CompletedLabelKind.
const toolCompletedLabelAnswers = "answers"

// Failure renderers, mirroring tool.FailureRender.
const ToolFailureErrorBlock = "error_block"

// Presentations holds the display metadata declared by the connected server's
// tools, so renderers branch on what a tool does rather than on its identity.
//
// The zero value is valid and describes nothing: every lookup then returns an
// empty presentation and the caller falls back to generic rendering. That is
// what a server predating the tools endpoint yields, and it is also the state
// while a connection is being established.
type Presentations struct {
	byID          map[string]v1.ToolPresentation
	activityNames map[string]string
}

func NewPresentations(list v1.ToolList) Presentations {
	byID := make(map[string]v1.ToolPresentation, len(list.Items))
	for _, item := range list.Items {
		byID[item.ID] = item.Presentation
	}
	return Presentations{byID: byID, activityNames: make(map[string]string)}
}

// For returns the declared presentation of a tool, or the empty presentation
// when the tool is unknown to this client.
func (p Presentations) For(name string) v1.ToolPresentation { return p.byID[name] }

// Redact returns a display-only copy of input with every field the tool
// declared sensitive replaced by a length summary. Exact input remains
// available to authorization and execution.
func (p Presentations) Redact(name string, input map[string]any) map[string]any {
	fields := p.For(name).Redact
	if input == nil || len(fields) == 0 {
		return RedactToolInputForDisplay(name, input)
	}
	return redactFields(input, fields)
}

// Payload decodes a tool activity event, redacting declared fields.
func (p Presentations) Payload(item v1.Event) (string, string, map[string]any, string) {
	callID, name, input, result := toolActivityRaw(item)
	return callID, name, p.Redact(name, input), result
}

// EnrichLabelInput copies label fields returned by a tool into input. This lets
// a tool report values that are allocated during execution, such as a generated
// task name, without coupling the renderer to that tool's identity.
func (p Presentations) EnrichLabelInput(name string, input map[string]any, result string) map[string]any {
	if result == "" {
		return input
	}
	var values map[string]any
	if json.Unmarshal([]byte(result), &values) != nil {
		return input
	}
	out := make(map[string]any, len(input)+len(values))
	for key, value := range input {
		out[key] = value
	}
	for _, field := range p.For(name).Label.Fields {
		for _, key := range field.Names {
			if _, exists := out[key]; !exists {
				if value, ok := values[key]; ok {
					out[key] = value
				}
			}
		}
	}
	return out
}

// CompletedLabel enriches an invocation label with declared result details.
func (p Presentations) CompletedLabel(name string, input map[string]any, result string) string {
	label := p.Label(name, input)
	presentation := p.For(name)
	if presentation.CompletedLabel == toolCompletedLabelAnswers {
		if answers := completedAnswerSummary(input, result); answers != "" {
			return label + " · " + answers
		}
		return label
	}
	noun := presentation.ResultCountNoun
	if noun == "" {
		return label
	}
	count := 0
	for _, line := range strings.Split(strings.TrimSpace(result), "\n") {
		if strings.TrimSpace(line) != "" {
			count++
		}
	}
	if count != 1 {
		if strings.HasSuffix(noun, "s") || strings.HasSuffix(noun, "x") || strings.HasSuffix(noun, "z") || strings.HasSuffix(noun, "ch") || strings.HasSuffix(noun, "sh") {
			noun += "es"
		} else {
			noun += "s"
		}
	}
	return fmt.Sprintf("%s · %d %s", label, count, noun)
}

type completedAnswersDto struct {
	Answers []completedAnswerDto `json:"answers"`
}

type completedAnswerDto struct {
	QuestionID string   `json:"question_id"`
	OptionIDs  []string `json:"option_ids"`
	Custom     string   `json:"custom"`
}

func completedAnswerSummary(input map[string]any, result string) string {
	var response completedAnswersDto
	if json.Unmarshal([]byte(result), &response) != nil {
		return ""
	}
	answers := make(map[string]completedAnswerDto, len(response.Answers))
	for _, answer := range response.Answers {
		answers[answer.QuestionID] = answer
	}
	questions, _ := input["questions"].([]any)
	var summaries []string
	for _, rawQuestion := range questions {
		question, _ := rawQuestion.(map[string]any)
		questionID, _ := question["id"].(string)
		answer, ok := answers[questionID]
		if !ok {
			continue
		}
		labels := make(map[string]string)
		options, _ := question["options"].([]any)
		for _, rawOption := range options {
			option, _ := rawOption.(map[string]any)
			optionID, _ := option["id"].(string)
			label, _ := option["label"].(string)
			labels[optionID] = label
		}
		var selected []string
		for _, optionID := range answer.OptionIDs {
			label := labels[optionID]
			if label == "" {
				label = optionID
			}
			selected = append(selected, label)
		}
		if answer.Custom != "" {
			selected = append(selected, answer.Custom)
		}
		if summary := cleanActivityDetail(strings.Join(selected, ", ")); summary != "" {
			summaries = append(summaries, summary)
		}
	}
	return strings.Join(summaries, "; ")
}

// Label summarises an invocation from its input, following the strategy the
// tool declared. A tool this client does not know falls back to the generic
// label, which inspects field names rather than tool identity.
func (p Presentations) Label(name string, input map[string]any) string {
	if _, declared := p.byID[name]; !declared {
		return ToolActivityLabel(name, input)
	}
	spec := p.For(name).Label
	var details []string
	add := func(value string) {
		if value = cleanActivityDetail(value); value != "" {
			details = append(details, value)
		}
	}

	switch spec.Kind {
	case string(toolLabelItemCount):
		items, ok := firstArray(input, spec.Source...)
		if !ok {
			return name
		}
		return itemCountLabel(spec.Prefix, spec.Noun, len(items))
	case string(toolLabelPatchTargets):
		details = append(details, patchActivityTargets(firstString(input, spec.Source...))...)
	default:
		for _, field := range spec.Fields {
			if field.Array {
				items, ok := firstArray(input, field.Names...)
				if !ok {
					continue
				}
				if len(items) > 0 {
					if element, ok := items[0].(map[string]any); ok {
						add(firstString(element, field.Item...))
					}
				}
				if field.Overflow && len(items) > 1 {
					details = append(details, fmt.Sprintf("+%d more", len(items)-1))
				}
				continue
			}
			value := firstString(input, field.Names...)
			if field.TaskName {
				if activityName := p.activityNames[value]; activityName != "" {
					value = activityName
				}
			}
			if value == "" {
				value = field.Default
			}
			if field.Quote {
				if cleaned := cleanActivityDetail(value); cleaned != "" {
					details = append(details, fmt.Sprintf("%q", cleaned))
				}
				continue
			}
			add(value)
		}
	}
	if len(details) == 0 {
		return name
	}
	return name + " · " + strings.Join(details, " · ")
}

// itemCountLabel renders a collection size, pluralising the declared noun.
func itemCountLabel(prefix, noun string, count int) string {
	if noun == "" {
		noun = "item"
	}
	if count != 1 {
		noun += "s"
	}
	if prefix == "" {
		prefix = "items"
	}
	return fmt.Sprintf("%s · %d %s", prefix, count, noun)
}

func firstArray(raw map[string]any, keys ...string) ([]any, bool) {
	for _, key := range keys {
		if value, ok := raw[key].([]any); ok {
			return value, true
		}
	}
	return nil, false
}

// Result reports how a successful result body should render.
func (p Presentations) Result(name string) string {
	if presentation, declared := p.byID[name]; declared {
		return presentation.Result
	}
	return legacyResult(name)
}

// Output reports how streamed output should be handled.
func (p Presentations) Output(name string) string {
	if presentation, declared := p.byID[name]; declared {
		return presentation.Output
	}
	return legacyOutput(name)
}

// Failure reports how a failed invocation's error should render.
func (p Presentations) Failure(name string) string { return p.For(name).Failure }

// legacyResult and legacyOutput reproduce the behaviour of the tool-ID switch
// this package used before tools declared their own presentation. They apply
// only to a tool the connected server did not describe, which means a server
// predating the tools endpoint. Without them such a server would silently lose
// diff blocks, streamed tails and stdin redaction.
func legacyResult(name string) string {
	switch name {
	case "apply_patch":
		return ToolResultDiff
	case "todowrite", "todo_write":
		return ToolResultTodos
	default:
		return ""
	}
}

func legacyOutput(name string) string {
	switch name {
	case "exec_command":
		return ToolOutputTail
	case "write_stdin":
		return ToolOutputNone
	default:
		return ""
	}
}

// Subagent reports whether invocations of a tool create child-agent activity.
func (p Presentations) Subagent(name string) bool {
	if presentation, declared := p.byID[name]; declared {
		return presentation.Subagent
	}
	return legacySubagent(name)
}

// Modeline reports whether a running top-level invocation belongs in the
// transient modeline status rather than in the activity rows.
func (p Presentations) Modeline(name string) bool { return p.For(name).Modeline }

// LiveOnly reports whether a tool's terminal event should remove its live
// activity without committing a permanent transcript row.
func (p Presentations) LiveOnly(name string) bool { return p.For(name).LiveOnly }

// TerminalOnly reports whether a tool suppresses its transient activity row.
func (p Presentations) TerminalOnly(name string) bool {
	return p.For(name).CompletedInput.TerminalOnly
}

// CompletedInputBlock formats selected non-empty fields in declared order.
func (p Presentations) CompletedInputBlock(name string, input map[string]any) string {
	fields := p.For(name).CompletedInput.Fields
	if len(fields) == 0 || input == nil {
		return ""
	}
	mapping := &yaml.Node{Kind: yaml.MappingNode}
	for _, field := range fields {
		value, exists := input[field]
		if !exists {
			continue
		}
		if text, ok := value.(string); ok && strings.TrimSpace(text) == "" {
			continue
		}
		key := &yaml.Node{Kind: yaml.ScalarNode, Value: field}
		var node yaml.Node
		if node.Encode(value) != nil {
			continue
		}
		if node.Kind == yaml.ScalarNode && strings.Contains(node.Value, "\n") {
			node.Style = yaml.LiteralStyle
		}
		mapping.Content = append(mapping.Content, key, &node)
	}
	if len(mapping.Content) == 0 {
		return ""
	}
	var formatted bytes.Buffer
	encoder := yaml.NewEncoder(&formatted)
	encoder.SetIndent(2)
	if encoder.Encode(mapping) != nil {
		return ""
	}
	_ = encoder.Close()
	return strings.TrimSuffix(formatted.String(), "\n")
}

func legacySubagent(name string) bool {
	switch name {
	case "agent_spawn", "agent_send", "wait_agent":
		return true
	default:
		return false
	}
}

// PermissionChoiceLabels renders the answers a permission request offers, in
// the order the requesting tool declared them. A request from a server which
// declares none falls back to the standard answers.
func PermissionChoiceLabels(item v1.Permission) []v1.PermissionChoice {
	if len(item.Choices) > 0 {
		return item.Choices
	}
	return legacyPermissionChoices(item.ToolID)
}

func legacyPermissionChoices(toolID string) []v1.PermissionChoice {
	if toolID == "request_write_permission" {
		return []v1.PermissionChoice{
			{Value: "grant", Decision: "allow", Label: "grant", Description: "Allow sandboxed writes to this path for the current session"},
			{Value: "reject", Decision: "deny", Label: "reject", Description: "Reject this request"},
			{Value: "reject with reason", Decision: "deny", Label: "reject with reason", Description: "Reject and provide feedback to the agent", RequiresReason: true},
		}
	}
	return []v1.PermissionChoice{
		{Value: "yes", Decision: "allow", Label: "yes", Description: "Allow this request"},
		{Value: "no", Decision: "deny", Label: "no", Description: "Deny this request"},
		{Value: "reject with reason", Decision: "deny", Label: "reject with reason", Description: "Deny and provide feedback to the agent", RequiresReason: true},
	}
}

// PermissionReplyForChoice maps a chosen value back to the reply to send.
func PermissionReplyForChoice(item v1.Permission, value, reason string) (v1.PermissionReply, bool) {
	for _, choice := range PermissionChoiceLabels(item) {
		if choice.Value != value {
			continue
		}
		reply := v1.PermissionReply{Decision: choice.Decision}
		if choice.RequiresReason {
			reply.Reason = reason
		}
		return reply, true
	}
	return v1.PermissionReply{}, false
}

// Style reports how prominently an invocation should render.
func (p Presentations) Style(name string) terminal.TextStyle {
	if p.For(name).Muted {
		return terminal.TextStyleMuted
	}
	if _, declared := p.byID[name]; declared {
		return terminal.TextStyleDefault
	}
	return ToolActivityStyle(name)
}
