package tool

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/amirulashraf/parrot-coder/internal/change"
)

type ApplyPatchTool struct {
	BasePresentation
	WritableTool
	Changes *change.Service
}

func NewApplyPatchTool(changes *change.Service) *ApplyPatchTool {
	return &ApplyPatchTool{Changes: changes}
}

func (*ApplyPatchTool) ID() string { return "apply_patch" }
func (*ApplyPatchTool) Presentation() Presentation {
	return Presentation{Label: LabelSpec{Kind: LabelPatchTargets, Source: []string{"patchText", "patch"}}}
}

func (*ApplyPatchTool) Description() string {
	return "Apply reviewed workspace edits written as aider SEARCH/REPLACE blocks: a file path on its own line, then '<<<<<<< SEARCH', the exact existing lines, '=======', the replacement lines, and '>>>>>>> REPLACE'. An empty SEARCH section creates the file."
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
	return json.RawMessage(`{"type":"object","properties":{"patchText":{"type":"string","description":"One or more aider SEARCH/REPLACE blocks. Each block is a workspace-relative file path on its own line, then <<<<<<< SEARCH, the exact lines to replace, =======, the replacement lines, and >>>>>>> REPLACE. Repeat blocks under the same path for several edits to one file; leave the SEARCH section empty to create a new file."}},"required":["patchText"],"additionalProperties":false}`)
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
	return executeMutation(ctx, t.Changes, plan, call)
}
