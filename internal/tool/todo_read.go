package tool

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/amirulashraf/parrot-coder/internal/session"
)

type TodoReadTool struct {
	BasePresentation
	Service   *session.TodoService
	SessionID string
}

func NewTodoReadTool(service *session.TodoService, sessionID string) *TodoReadTool {
	return &TodoReadTool{Service: service, SessionID: sessionID}
}
func (*TodoReadTool) ID() string { return "todoread" }
func (t *TodoReadTool) Descriptor() Descriptor {
	return Descriptor{ID: t.ID(), Description: "Read the current session's ordered todo list.", Schema: t.JSONSchema(), Presentation: t.Presentation()}
}
func (*TodoReadTool) DescribeRequest(json.RawMessage) (string, error) {
	return "Read the current todo list", nil
}
func (*TodoReadTool) JSONSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","additionalProperties":false}`)
}
func (t *TodoReadTool) Plan(_ context.Context, raw json.RawMessage, _ CallContext) (Plan, error) {
	return NewPlan(t.ID(), raw, nil, nil, nil)
}
func (t *TodoReadTool) Execute(ctx context.Context, _ Plan, _ CallContext) (Result, error) {
	if t.Service == nil || t.SessionID == "" {
		return Result{}, errors.New("todoread: service and session are required")
	}
	items, err := t.Service.List(ctx, t.SessionID)
	if err != nil {
		return Result{}, err
	}
	data, _ := json.Marshal(items)
	text := string(data)
	return Result{Text: text, ModelText: text, Metadata: map[string]any{"count": len(items)}}, nil
}
