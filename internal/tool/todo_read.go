package tool

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/amirulashraf/parrot-coder/internal/session"
)

type TodoReadTool struct{ Service *session.TodoService }

func NewTodoReadTool(service *session.TodoService) *TodoReadTool {
	return &TodoReadTool{Service: service}
}
func (*TodoReadTool) ID() string          { return "todoread" }
func (*TodoReadTool) Description() string { return "Read the current session's ordered todo list." }
func (*TodoReadTool) DescribeRequest(json.RawMessage) (string, error) {
	return "Read the current todo list", nil
}
func (*TodoReadTool) JSONSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","additionalProperties":false}`)
}
func (t *TodoReadTool) Plan(_ context.Context, raw json.RawMessage, _ CallContext) (Plan, error) {
	return NewPlan(t.ID(), raw, nil, nil, nil)
}
func (t *TodoReadTool) Execute(ctx context.Context, _ Plan, call CallContext) (Result, error) {
	service := t.Service
	if service == nil {
		service = call.Todos
	}
	if service == nil || call.SessionID == "" {
		return Result{}, errors.New("todoread: service and session are required")
	}
	items, err := service.List(ctx, call.SessionID)
	if err != nil {
		return Result{}, err
	}
	data, _ := json.Marshal(items)
	text := string(data)
	return Result{Text: text, ModelText: text, Metadata: map[string]any{"count": len(items)}}, nil
}
