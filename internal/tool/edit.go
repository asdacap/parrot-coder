package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/amirulashraf/parrot-coder/internal/change"
)

type EditTool struct {
	BasePresentation
	WritableTool
	Changes *change.Service
}

func NewEditTool(changes *change.Service) *EditTool { return &EditTool{Changes: changes} }

func (*EditTool) ID() string { return "edit" }
func (*EditTool) Presentation() Presentation {
	return Presentation{
		Result: ResultText,
		Label:  LabelSpec{Fields: []LabelField{{Names: []string{"path", "file", "filePath"}}}},
	}
}

func (*EditTool) Description() string {
	return "Replace the entire content of a workspace file after hash-bound diff review; creation must be explicit."
}
func (*EditTool) DescribeRequest(raw json.RawMessage) (string, error) {
	var input change.Edit
	if err := json.Unmarshal(raw, &input); err != nil {
		return "", err
	}
	operation := "Edit"
	if input.Create {
		operation = "Create"
	}
	return fmt.Sprintf("%s workspace file %q", operation, input.Path), nil
}
func (*EditTool) JSONSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"expected_sha256":{"type":"string"},"new":{"type":"string"},"create":{"type":"boolean"}},"required":["path","new"],"additionalProperties":false}`)
}

type mutationPlan struct {
	Change change.Plan
}

func (t *EditTool) Plan(ctx context.Context, raw json.RawMessage, call CallContext) (Plan, error) {
	service := t.Changes
	if service == nil {
		service = call.Changes
	}
	if service == nil || call.Workspace == nil {
		return Plan{}, errors.New("edit: change service and workspace are required")
	}
	var input change.Edit
	if err := json.Unmarshal(raw, &input); err != nil {
		return Plan{}, err
	}
	planned, err := service.PlanEdit(ctx, call.Workspace, input)
	if err != nil {
		return Plan{}, err
	}
	return mutationToolPlan(t.ID(), raw, planned)
}

func (t *EditTool) Execute(ctx context.Context, plan Plan, call CallContext) (Result, error) {
	result, err := executeMutation(ctx, t.Changes, plan, call)
	if err != nil {
		return result, err
	}
	planned := plan.Data.(mutationPlan).Change
	if len(planned.Mutations) == 1 {
		result.Text += "sha256: " + planned.Mutations[0].After.SHA256 + "\n"
	}
	return result, nil
}
