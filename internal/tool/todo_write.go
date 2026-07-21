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
	Service *session.TodoService
}

func NewTodoWriteTool(service *session.TodoService) *TodoWriteTool {
	return &TodoWriteTool{Service: service}
}
func (*TodoWriteTool) ID() string { return "todowrite" }
func (*TodoWriteTool) Presentation() Presentation {
	return Presentation{
		Result: ResultTodos,
		Label:  LabelSpec{Kind: LabelItemCount, Source: []string{"todos"}, Prefix: "TODO", Noun: "item"},
	}
}

func (*TodoWriteTool) Description() string {
	return "Transactionally replace the current session's ordered todo list."
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
func (t *TodoWriteTool) Execute(ctx context.Context, plan Plan, call CallContext) (Result, error) {
	if call.SecurityProfile != nil && call.SecurityProfile.IsReadOnly() {
		return Result{}, errors.New("todo_write is not permitted by the current security profile")
	}
	service := t.Service
	if service == nil {
		service = call.Todos
	}
	if service == nil || call.SessionID == "" {
		return Result{}, errors.New("todowrite: service and session are required")
	}
	items, err := service.Replace(ctx, call.SessionID, plan.Data.(todoWriteInput).Todos)
	if err != nil {
		return Result{}, err
	}
	data, _ := json.Marshal(items)
	text := string(data)
	return Result{Text: text, ModelText: text, Metadata: map[string]any{"count": len(items)}}, nil
}
