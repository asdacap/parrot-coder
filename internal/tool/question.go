package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/amirulashraf/parrot-coder/internal/question"
)

type QuestionTool struct {
	BasePresentation
	Broker *question.Broker
}

func NewQuestionTool(broker *question.Broker) *QuestionTool { return &QuestionTool{Broker: broker} }
func (*QuestionTool) ID() string                            { return "question" }
func (*QuestionTool) Presentation() Presentation {
	return Presentation{Label: LabelSpec{Fields: []LabelField{{
		Names: []string{"questions"}, Array: true,
		Item: []string{"header", "prompt", "question"}, Overflow: true,
	}}}}
}

func (*QuestionTool) Description() string {
	return "Ask typed questions and block until the user replies or rejects them. Use this tool rather than asking the user through normal message."
}
func (*QuestionTool) DescribeRequest(raw json.RawMessage) (string, error) {
	var input question.Request
	if err := json.Unmarshal(raw, &input); err != nil {
		return "", err
	}
	return fmt.Sprintf("Ask %d question(s)", len(input.Questions)), nil
}
func (*QuestionTool) JSONSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"questions":{"type":"array","items":{"type":"object","properties":{"id":{"type":"string"},"header":{"type":"string"},"prompt":{"type":"string"},"options":{"type":"array","items":{"type":"object","properties":{"id":{"type":"string"},"label":{"type":"string"},"description":{"type":"string"}},"required":["id","label"],"additionalProperties":false}},"multiple":{"type":"boolean"},"custom":{"type":"boolean"}},"required":["id","prompt"],"additionalProperties":false}}},"required":["questions"],"additionalProperties":false}`)
}
func (t *QuestionTool) Plan(_ context.Context, raw json.RawMessage, call CallContext) (Plan, error) {
	var input question.Request
	if err := json.Unmarshal(raw, &input); err != nil {
		return Plan{}, err
	}
	input.SessionID = call.SessionID
	return NewPlan(t.ID(), raw, nil, nil, input)
}
func (t *QuestionTool) Execute(ctx context.Context, plan Plan, call CallContext) (Result, error) {

	broker := t.Broker
	if broker == nil {
		broker = call.Questions
	}
	if broker == nil {
		return Result{}, errors.New("question: broker is required")
	}
	response, err := broker.Ask(ctx, plan.Data.(question.Request))
	if err != nil {
		return Result{}, err
	}
	data, _ := json.Marshal(response)
	text := string(data)
	return Result{Text: text, ModelText: text}, nil
}
