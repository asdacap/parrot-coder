package tool

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/amirulashraf/parrot-coder/internal/change"
	"github.com/amirulashraf/parrot-coder/internal/snapshot"
)

type ApplyPatchTool struct {
	Changes   *change.Service
	Snapshots *snapshot.Service
}

func NewApplyPatchTool(changes *change.Service, snapshots *snapshot.Service) *ApplyPatchTool {
	return &ApplyPatchTool{Changes: changes, Snapshots: snapshots}
}

func (*ApplyPatchTool) ID() string { return "apply_patch" }
func (*ApplyPatchTool) Description() string {
	return "Apply a Begin Patch containing reviewed add, update, delete, or move operations. Use '*** Update File: OLD' followed by '*** Move to: NEW' for Codex-style moves; '*** Move File: OLD -> NEW' is also accepted."
}
func (*ApplyPatchTool) DescribeRequest(raw json.RawMessage) (string, error) {
	var input struct {
		Patch string `json:"patch"`
	}
	if err := json.Unmarshal(raw, &input); err != nil {
		return "", err
	}
	return "Apply the reviewed workspace patch", nil
}
func (*ApplyPatchTool) JSONSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"patchText":{"type":"string","description":"The full Begin Patch text. Operations are *** Add File, *** Update File, and *** Delete File. For a move, put *** Move to: NEW immediately after *** Update File: OLD; *** Move File: OLD -> NEW is accepted as an alias."}},"required":["patchText"],"additionalProperties":false}`)
}

func (t *ApplyPatchTool) Plan(ctx context.Context, raw json.RawMessage, call CallContext) (Plan, error) {
	service := t.Changes
	if service == nil {
		service = call.Changes
	}
	if service == nil || call.Workspace == nil {
		return Plan{}, errors.New("apply_patch: change service and workspace are required")
	}
	var input struct {
		PatchText string `json:"patchText"`
	}
	if err := json.Unmarshal(raw, &input); err != nil {
		return Plan{}, err
	}
	planned, err := service.PlanPatch(ctx, call.Workspace, input.PatchText)
	if err != nil {
		return Plan{}, err
	}
	return mutationToolPlan(t.ID(), raw, planned)
}

func (t *ApplyPatchTool) Execute(ctx context.Context, plan Plan, call CallContext) (Result, error) {
	return executeMutation(ctx, t.Changes, t.Snapshots, plan, call)
}
