package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/amirulashraf/parrot-coder/internal/change"
)

type ApplyPatchTool struct {
	BasePresentation
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
	return "Apply reviewed workspace edits written as aider SEARCH/REPLACE blocks: a file path on its own line, then '<<<<<<< SEARCH', the exact existing lines, '=======', the replacement lines, and '>>>>>>> REPLACE'. An empty SEARCH section creates the file. Set format to \"unified\" to supply git-style unified diff text instead."
}
func (*ApplyPatchTool) DescribeRequest(raw json.RawMessage) (string, error) {
	format, err := patchFormat(raw)
	if err != nil {
		return "", err
	}
	if format == change.PatchFormatUnified {
		return "Apply the reviewed workspace patch (unified diff)", nil
	}
	return "Apply the reviewed workspace patch", nil
}
func (*ApplyPatchTool) JSONSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"patchText":{"type":"string","description":"The patch text, written in the format named by the format field."},"format":{"type":"string","enum":["aider","unified"],"description":"Edit syntax of patchText, defaulting to aider. \"aider\": one or more SEARCH/REPLACE blocks, each a workspace-relative file path on its own line, then <<<<<<< SEARCH, the exact lines to replace, =======, the replacement lines, and >>>>>>> REPLACE; repeat blocks under the same path for several edits to one file, and leave the SEARCH section empty to create a new file. \"unified\": git diff text with --- and +++ headers and @@ hunks; a /dev/null source creates the file and a /dev/null target deletes it, and renames are rejected."}},"required":["patchText"],"additionalProperties":false}`)
}

// patchFormat reads the optional format field, defaulting to aider so existing
// callers keep their behaviour.
func patchFormat(raw json.RawMessage) (change.PatchFormat, error) {
	var input struct {
		Format string `json:"format"`
	}
	if err := json.Unmarshal(raw, &input); err != nil {
		return "", err
	}
	switch format := change.PatchFormat(input.Format); format {
	case "":
		return change.PatchFormatAider, nil
	case change.PatchFormatAider, change.PatchFormatUnified:
		return format, nil
	default:
		return "", fmt.Errorf("apply_patch: unknown format %q", input.Format)
	}
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
	format, err := patchFormat(raw)
	if err != nil {
		return Plan{}, err
	}
	planned, err := service.PlanPatch(ctx, call.Workspace, input.PatchText, format)
	if err != nil {
		return Plan{}, err
	}
	return mutationToolPlan(t.ID(), raw, planned)
}

func (t *ApplyPatchTool) Execute(ctx context.Context, plan Plan, call CallContext) (Result, error) {
	if call.SecurityProfile != nil && call.SecurityProfile.IsReadOnly() {
		return Result{}, errors.New("apply_patch is not permitted by the current security profile")
	}
	return executeMutation(ctx, t.Changes, plan, call)
}
