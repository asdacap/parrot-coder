package enhancedchat

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	v1 "github.com/amirulashraf/parrot-coder/internal/api/v1"
	"github.com/amirulashraf/parrot-coder/internal/terminal"
)

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
		r.shell.setCurrent(item)
		if err := r.ensureStream(item.ID); err != nil {
			r.commitError(err.Error())
			_ = r.ensureInputBorder()
			return enhancedInputOutcome{}
		}
		if r.shell.config == nil || r.shell.config.ResumeSession == nil {
			r.commitError("connected server does not support explicit resume")
			_ = r.ensureInputBorder()
			return enhancedInputOutcome{}
		}
		if err := r.shell.config.ResumeSession(item.ID); err != nil {
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
	r.shell.refreshState()
	if name == "/new" || name == "/clear" || name == "/session" || name == "/connect" || r.shell.current.ID != previousSession {
		r.stopStream()
	}
	// A builtin command commits at most a short status line, so the live frame's
	// own boundary is enough. Committing a permanent rule here left a divider
	// stranded above and below every status line in the transcript.
	r.borderCommitted = false
	return enhancedInputOutcome{exit: exit, code: code}
}

func safeBusySlash(name string) bool {
	switch name {
	case "/help", "/version", "/chat", "/models", "/usage", "/modes", "/agents", "/sessions", "/goal", "/status", "/thinking", "/exit", "/continue":
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
			MessageID: messageID, Content: content, Delivery: "steer",
		})
		if err != nil {
			return err
		}
		r.addPending(queuedChatInput{inputID: accepted.InputID, messageID: accepted.MessageID, content: content})
		r.status = "steering"
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
		r.shell.setCurrent(item)
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
