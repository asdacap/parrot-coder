package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/amirulashraf/parrot-coder/internal/session"
)

type TodoWriteTool struct {
	BasePresentation
	Service   *session.TodoService
	SessionID string
}

func NewTodoWriteTool(service *session.TodoService, sessionID string) *TodoWriteTool {
	return &TodoWriteTool{Service: service, SessionID: sessionID}
}
func (*TodoWriteTool) ID() string { return "todowrite" }
func (*TodoWriteTool) Presentation() Presentation {
	return Presentation{
		Result: ResultTodos,
		Label:  LabelSpec{Kind: LabelItemCount, Source: []string{"todos"}, Prefix: "TODO", Noun: "item"},
	}
}

func (t *TodoWriteTool) Descriptor() Descriptor {
	return Descriptor{ID: t.ID(), Description: "Transactionally replace the current session's ordered todo list.", Schema: t.JSONSchema(), Presentation: t.Presentation()}
}
func (*TodoWriteTool) DescribeRequest(raw json.RawMessage) (string, error) {
	var input todoWriteInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return "", err
	}
	return fmt.Sprintf("Replace the todo list with %d items", len(input.Todos)), nil
}
func (*TodoWriteTool) JSONSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"todos":{"type":"array","items":{"type":"object","properties":{"id":{"type":"string"},"content":{"type":"string","minLength":1},"status":{"type":"string","enum":["pending","in_progress","completed","cancelled"]},"priority":{"type":"string","enum":["high","medium","low"]}},"required":["content","status","priority"],"additionalProperties":false}}},"required":["todos"],"additionalProperties":false}`)
}

type todoWriteInput struct {
	Todos []session.Todo `json:"todos"`
}

func (t *TodoWriteTool) Plan(_ context.Context, raw json.RawMessage, _ CallContext) (Plan, error) {
	var input todoWriteInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return Plan{}, err
	}
	return NewPlan(t.ID(), raw, nil, nil, input)
}
func (t *TodoWriteTool) Execute(ctx context.Context, plan Plan, _ CallContext) (Result, error) {
	if t.Service == nil || t.SessionID == "" {
		return Result{}, errors.New("todowrite: service and session are required")
	}
	items, err := t.Service.Replace(ctx, t.SessionID, plan.Data.(todoWriteInput).Todos)
	if err != nil {
		return Result{}, err
	}
	data, _ := json.Marshal(items)
	text := string(data)
	return Result{Text: text, ModelText: text, Metadata: map[string]any{"count": len(items)}}, nil
}
