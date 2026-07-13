// Package question brokers typed model questions separately from permissions.
package question

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"sync"
)

var ErrRejected = errors.New("question: request rejected")

type Option struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

type Question struct {
	ID       string   `json:"id"`
	Header   string   `json:"header,omitempty"`
	Prompt   string   `json:"prompt"`
	Options  []Option `json:"options,omitempty"`
	Multiple bool     `json:"multiple,omitempty"`
	Custom   bool     `json:"custom,omitempty"`
}

type Request struct {
	SessionID string     `json:"session_id,omitempty"`
	Questions []Question `json:"questions"`
}

type Answer struct {
	QuestionID string   `json:"question_id"`
	OptionIDs  []string `json:"option_ids,omitempty"`
	Custom     string   `json:"custom,omitempty"`
}

type Response struct {
	Answers []Answer `json:"answers"`
}

type Pending struct {
	ID      string  `json:"id"`
	Request Request `json:"request"`
}

type Prompter interface {
	Prompt(context.Context, Pending) (Response, error)
}

type result struct {
	response Response
	err      error
}

type pendingState struct {
	pending Pending
	result  chan result
}

type Broker struct {
	mu       sync.Mutex
	prompter Prompter
	pending  map[string]*pendingState
}

func NewBroker(prompter Prompter) *Broker {
	return &Broker{prompter: prompter, pending: make(map[string]*pendingState)}
}

// Ask blocks until exactly one reply, rejection, or context cancellation.
func (b *Broker) Ask(ctx context.Context, request Request) (Response, error) {
	if b == nil {
		return Response{}, errors.New("question: broker is required")
	}
	if err := validateRequest(request); err != nil {
		return Response{}, err
	}
	id, err := requestID()
	if err != nil {
		return Response{}, err
	}
	state := &pendingState{pending: Pending{ID: id, Request: cloneRequest(request)}, result: make(chan result, 1)}
	b.mu.Lock()
	b.pending[id] = state
	b.mu.Unlock()
	if b.prompter != nil {
		go func() {
			response, promptErr := b.prompter.Prompt(ctx, state.pending)
			if promptErr != nil {
				_ = b.Reject(id)
				return
			}
			_ = b.Reply(id, response)
		}()
	}
	select {
	case outcome := <-state.result:
		return outcome.response, outcome.err
	case <-ctx.Done():
		b.mu.Lock()
		delete(b.pending, id)
		b.mu.Unlock()
		return Response{}, ctx.Err()
	}
}

func (b *Broker) Pending() []Pending {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	result := make([]Pending, 0, len(b.pending))
	for _, state := range b.pending {
		result = append(result, Pending{ID: state.pending.ID, Request: cloneRequest(state.pending.Request)})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func (b *Broker) Reply(id string, response Response) error {
	return b.settle(id, response, nil)
}

func (b *Broker) Reject(id string) error {
	return b.settle(id, Response{}, ErrRejected)
}

func (b *Broker) settle(id string, response Response, responseErr error) error {
	b.mu.Lock()
	state, ok := b.pending[id]
	if !ok {
		b.mu.Unlock()
		return errors.New("question: request is unknown or already settled")
	}
	if responseErr == nil {
		if err := validateResponse(state.pending.Request, response); err != nil {
			b.mu.Unlock()
			return err
		}
	}
	delete(b.pending, id)
	b.mu.Unlock()
	state.result <- result{response: cloneResponse(response), err: responseErr}
	return nil
}

func validateRequest(request Request) error {
	if len(request.Questions) == 0 || len(request.Questions) > 32 {
		return errors.New("question: between 1 and 32 questions are required")
	}
	questionIDs := make(map[string]struct{}, len(request.Questions))
	for _, item := range request.Questions {
		if item.ID == "" || item.Prompt == "" {
			return errors.New("question: question ID and prompt are required")
		}
		if _, duplicate := questionIDs[item.ID]; duplicate {
			return fmt.Errorf("question: duplicate question ID %q", item.ID)
		}
		questionIDs[item.ID] = struct{}{}
		optionIDs := make(map[string]struct{}, len(item.Options))
		for _, option := range item.Options {
			if option.ID == "" || option.Label == "" {
				return errors.New("question: option ID and label are required")
			}
			if _, duplicate := optionIDs[option.ID]; duplicate {
				return fmt.Errorf("question: duplicate option ID %q", option.ID)
			}
			optionIDs[option.ID] = struct{}{}
		}
		if len(item.Options) == 0 && !item.Custom {
			return errors.New("question: a question requires options or custom input")
		}
	}
	return nil
}

func validateResponse(request Request, response Response) error {
	if len(response.Answers) != len(request.Questions) {
		return errors.New("question: every question must be answered exactly once")
	}
	questions := make(map[string]Question, len(request.Questions))
	for _, item := range request.Questions {
		questions[item.ID] = item
	}
	seen := make(map[string]struct{}, len(response.Answers))
	for _, answer := range response.Answers {
		item, ok := questions[answer.QuestionID]
		if !ok {
			return fmt.Errorf("question: unknown question ID %q", answer.QuestionID)
		}
		if _, duplicate := seen[answer.QuestionID]; duplicate {
			return fmt.Errorf("question: duplicate answer for %q", answer.QuestionID)
		}
		seen[answer.QuestionID] = struct{}{}
		if answer.Custom != "" && !item.Custom {
			return fmt.Errorf("question: custom answer is disabled for %q", answer.QuestionID)
		}
		if !item.Multiple && len(answer.OptionIDs) > 1 {
			return fmt.Errorf("question: multiple choices are disabled for %q", answer.QuestionID)
		}
		allowed := make(map[string]struct{}, len(item.Options))
		for _, option := range item.Options {
			allowed[option.ID] = struct{}{}
		}
		chosen := make(map[string]struct{}, len(answer.OptionIDs))
		for _, optionID := range answer.OptionIDs {
			if _, ok := allowed[optionID]; !ok {
				return fmt.Errorf("question: unknown option %q", optionID)
			}
			if _, duplicate := chosen[optionID]; duplicate {
				return fmt.Errorf("question: duplicate option %q", optionID)
			}
			chosen[optionID] = struct{}{}
		}
		if len(answer.OptionIDs) == 0 && answer.Custom == "" {
			return fmt.Errorf("question: answer for %q is empty", answer.QuestionID)
		}
	}
	return nil
}

func requestID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func cloneRequest(request Request) Request {
	out := request
	out.Questions = make([]Question, len(request.Questions))
	for i, item := range request.Questions {
		out.Questions[i] = item
		out.Questions[i].Options = append([]Option(nil), item.Options...)
	}
	return out
}

func cloneResponse(response Response) Response {
	out := Response{Answers: make([]Answer, len(response.Answers))}
	for i, answer := range response.Answers {
		out.Answers[i] = answer
		out.Answers[i].OptionIDs = append([]string(nil), answer.OptionIDs...)
	}
	return out
}
