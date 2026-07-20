package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/amirulashraf/parrot-coder/internal/permission"
)

type ReadOutputTool struct{ MaxBytes int64 }

func NewReadOutputTool(max int64) *ReadOutputTool {
	if max <= 0 {
		max = 1 << 20
	}
	return &ReadOutputTool{max}
}
func (*ReadOutputTool) ID() string { return "read_output" }
func (*ReadOutputTool) Description() string {
	return "Read a bounded byte range from an opaque managed output ID."
}
func (*ReadOutputTool) DescribeRequest(raw json.RawMessage) (string, error) {
	var input readOutputInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return "", err
	}
	return fmt.Sprintf("Read %d bytes at offset %d from managed output %q", input.Limit, input.Offset, input.ID), nil
}
func (*ReadOutputTool) JSONSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"},"offset":{"type":"integer"},"limit":{"type":"integer"}},"required":["id","offset","limit"],"additionalProperties":false}`)
}

type readOutputInput struct {
	ID     string `json:"id"`
	Offset int64  `json:"offset"`
	Limit  int64  `json:"limit"`
}

func (t *ReadOutputTool) Plan(ctx context.Context, raw json.RawMessage, call CallContext) (Plan, error) {
	if call.Outputs == nil {
		return Plan{}, errors.New("output store is required")
	}
	var input readOutputInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return Plan{}, err
	}
	if input.Limit > t.MaxBytes {
		return Plan{}, errors.New("output read limit exceeded")
	}
	req, err := permission.NewRequest(t.ID(), raw, []permission.Resource{{Kind: "managed_output", Identifier: input.ID, Operation: "read"}}, nil)
	if err != nil {
		return Plan{}, err
	}
	return NewPlan(t.ID(), raw, []permission.Request{req}, nil, input)
}
func (t *ReadOutputTool) Execute(ctx context.Context, plan Plan, call CallContext) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	input := plan.Data.(readOutputInput)
	b, err := call.Outputs.Read(input.ID, input.Offset, input.Limit)
	if err != nil {
		return Result{}, err
	}
	text := string(bytesToUTF8(b))
	return Result{Text: text, ModelText: text, Metadata: map[string]any{"bytes": len(b)}}, nil
}
