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
	Broker    *question.Broker
	SessionID string
}

func NewQuestionTool(broker *question.Broker, sessionID string) *QuestionTool {
	return &QuestionTool{Broker: broker, SessionID: sessionID}
}
func (*QuestionTool) ID() string { return "question" }
func (*QuestionTool) Available(session AgentSession) bool {
	return session == nil || !session.IsSubagent()
}
func (*QuestionTool) Presentation() Presentation {
	return Presentation{
		Label: LabelSpec{Fields: []LabelField{{
			Names: []string{"questions"}, Array: true,
			Item: []string{"header", "prompt", "question"}, Overflow: true,
		}}},
		CompletedLabel: CompletedLabelAnswers,
	}
}

func (t *QuestionTool) Descriptor() Descriptor {
	return Descriptor{
		ID:           t.ID(),
		Description:  "Ask typed questions and block until the user replies or rejects them. Use this tool rather than asking the user through normal message.",
		Schema:       t.JSONSchema(),
		Presentation: t.Presentation(),
	}
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
func (t *QuestionTool) Plan(_ context.Context, raw json.RawMessage, _ CallContext) (Plan, error) {
	var input question.Request
	if err := json.Unmarshal(raw, &input); err != nil {
		return Plan{}, err
	}
	if t.Broker == nil || t.SessionID == "" {
		return Plan{}, errors.New("question: broker and session are required")
	}
	input.SessionID = t.SessionID
	return NewPlan(t.ID(), raw, nil, nil, input)
}
func (t *QuestionTool) Execute(ctx context.Context, plan Plan, _ CallContext) (Result, error) {
	if t.Broker == nil {
		return Result{}, errors.New("question: broker is required")
	}
	response, err := t.Broker.Ask(ctx, plan.Data.(question.Request))
	if err != nil {
		return Result{}, err
	}
	data, _ := json.Marshal(response)
	text := string(data)
	return Result{Text: text, ModelText: text}, nil
}
