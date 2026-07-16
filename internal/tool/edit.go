package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/amirulashraf/parrot-coder/internal/change"
	"github.com/amirulashraf/parrot-coder/internal/snapshot"
)

type EditTool struct {
	Changes   *change.Service
	Snapshots *snapshot.Service
}

func NewEditTool(changes *change.Service, snapshots *snapshot.Service) *EditTool {
	return &EditTool{Changes: changes, Snapshots: snapshots}
}

func (*EditTool) ID() string { return "edit" }
func (*EditTool) Description() string {
	return "Replace exact text in a workspace file after hash-bound diff review; creation must be explicit."
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
	return json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"expected_sha256":{"type":"string"},"old":{"type":"string"},"new":{"type":"string"},"replace_all":{"type":"boolean"},"create":{"type":"boolean"}},"required":["path","new"],"additionalProperties":false}`)
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
	return executeMutation(ctx, t.Changes, t.Snapshots, plan, call)
}
