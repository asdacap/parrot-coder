package enhancedchat

import (
	"fmt"
	"strings"
	"time"

	v1 "github.com/amirulashraf/parrot-coder/internal/api/v1"
	"github.com/amirulashraf/parrot-coder/internal/cli/chatview"
	"github.com/amirulashraf/parrot-coder/internal/terminal"
)

func (r *enhancedChatRuntime) modalContext() []string {
	if r.modal == nil {
		return nil
	}
	return r.modal.context
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
			prompt: "permission decision: ", choices: permissionChoicesFor(item),
			createdAt: time.Now(),
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
		r.modal = &enhancedModal{kind: "question", state: state, question: &request, createdAt: time.Now()}
		r.inputMode.advance()
		r.updateQuestionPrompt()
		r.showQuestionContext(request.Questions[0])
	}
}

func permissionChoices() []terminal.Candidate {
	return []terminal.Candidate{
		{Value: "yes", Description: "Allow this request"},
		{Value: "no", Description: "Deny this request"},
		{Value: "reject with reason", Description: "Deny and provide feedback to the agent"},
	}
}

// permissionChoicesFor renders the answers the requesting tool declared, so a
// tool which labels its own answers simply does not offer the standard ones.
func permissionChoicesFor(item v1.Permission) []terminal.Candidate {
	declared := chatview.PermissionChoiceLabels(item)
	candidates := make([]terminal.Candidate, 0, len(declared))
	for _, choice := range declared {
		candidates = append(candidates, terminal.Candidate{Value: choice.Value, Description: choice.Description})
	}
	return candidates
}

func permissionReplyFromAnswer(value string) v1.PermissionReply {
	answer := strings.ToLower(strings.TrimSpace(value))
	reply := v1.PermissionReply{Decision: "deny"}
	switch answer {
	case "y", "yes", "once", "grant":
		reply.Decision = "allow"
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
		value := choices[selected].Value
		if value == "reject with reason" {
			r.modal.customInput = true
			r.modal.prompt = "rejection reason: "
			r.modal.choices = nil
			return true, r.modal.state.Reset("")
		}
		return true, r.answerModal(value)
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

func (r *enhancedChatRuntime) handleTurnCompleteModalKey(key terminal.Key) (bool, error) {
	if r.modal == nil || r.modal.kind != "turn_complete" || len(r.modal.choices) == 0 {
		return false, nil
	}
	switch key.Kind {
	case terminal.KeyUp:
		r.modal.selected = (r.modal.selected - 1 + len(r.modal.choices)) % len(r.modal.choices)
	case terminal.KeyDown, terminal.KeyTab:
		r.modal.selected = (r.modal.selected + 1) % len(r.modal.choices)
	case terminal.KeyEnter, terminal.KeyNewline:
		selected := min(max(r.modal.selected, 0), len(r.modal.choices)-1)
		value := r.modal.choices[selected].Value
		if r.modal.turnComplete.CustomChoice != "" && value == r.modal.turnComplete.CustomChoice {
			r.modal.customInput = true
			r.modal.choices = nil
			r.modal.prompt = r.modal.turnComplete.CustomPrompt
			return true, r.modal.state.Reset("")
		}
		return true, r.answerModal(value)
	case terminal.KeyEscape, terminal.KeyEOF:
		r.cancelModal()
	case terminal.KeyInterrupt:
		r.cancelModal()
		return true, r.requestInterrupt()
	}
	// Choice-bearing turn completion dialogs are selections, not editable input.
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
	case "turn_complete":
		result, err := modal.turnComplete.Handle(value)
		if err != nil {
			return err
		}
		r.shell.refreshState()
		if result.ValidationError != "" {
			return fmt.Errorf("%w: %s", errInvalidModalAnswer, result.ValidationError)
		}
		r.finishModal()
		if result.Prompt != "" {
			return r.submitPrompt(result.Prompt)
		}
		return nil
	case "permission":
		var reply v1.PermissionReply
		switch declared, ok := chatview.PermissionReplyForChoice(*modal.permission, value, strings.TrimSpace(value)); {
		case modal.customInput && strings.TrimSpace(value) != "":
			reply = v1.PermissionReply{Decision: "deny", Reason: strings.TrimSpace(value)}
		case modal.customInput:
			return fmt.Errorf("%w: enter a rejection reason", errInvalidModalAnswer)
		case ok:
			reply = declared
		default:
			reply = permissionReplyFromAnswer(value)
		}
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

func (r *enhancedChatRuntime) timeoutModal() {
	if r.modal == nil {
		return
	}
	modal := r.modal
	if modal.kind == "permission" && modal.permission != nil {
		_ = r.shell.api.ReplyPermission(r.shell.ctx, r.shell.current.ID, modal.permission.ID, v1.PermissionReply{Decision: "deny", Reason: "user is away"})
	}
	r.status = "permission denied: user is away"
	r.finishModal()
}
