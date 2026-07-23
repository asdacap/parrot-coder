package tool

import (
	"context"
	"encoding/json"
	"errors"

	statusinfo "github.com/amirulashraf/parrot-coder/internal/status"
)

type StatusTool struct {
	BasePresentation
	Registry *statusinfo.Registry
}

func NewStatusTool(registry *statusinfo.Registry) *StatusTool { return &StatusTool{Registry: registry} }
func (*StatusTool) ID() string                                { return "status" }
func (*StatusTool) Description() string {
	return "Query current runtime, mode, and profile status without adding it to the system prompt."
}
func (*StatusTool) DescribeRequest(json.RawMessage) (string, error) {
	return "Query current status", nil
}
func (*StatusTool) JSONSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","additionalProperties":false}`)
}
func (t *StatusTool) Plan(_ context.Context, raw json.RawMessage, _ CallContext) (Plan, error) {
	return NewPlan(t.ID(), raw, nil, nil, nil)
}
func (t *StatusTool) Execute(ctx context.Context, _ Plan, call CallContext) (Result, error) {
	if t.Registry == nil {
		return Result{}, errors.New("status: registry is required")
	}
	text, err := t.Registry.Observe(ctx, call.StatusQuery, call.StatusProvider)
	if err != nil {
		return Result{}, err
	}
	if text == "" {
		text = "No status is currently available."
	}
	return Result{Text: text, ModelText: modelText(text)}, nil
}
