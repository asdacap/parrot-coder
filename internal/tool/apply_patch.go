package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/amirulashraf/parrot-coder/internal/change"
	"github.com/amirulashraf/parrot-coder/internal/security"
	"github.com/amirulashraf/parrot-coder/internal/workspace"
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
	return "Apply reviewed workspace edits written as aider SEARCH/REPLACE blocks: a file path on its own line, then '<<<<<<< SEARCH', the exact existing lines, '=======', the replacement lines, and '>>>>>>> REPLACE'. An empty SEARCH section creates a missing file or matches an existing empty file. Set format to \"unified\" to supply git-style unified diff text instead."
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
	return json.RawMessage(`{"type":"object","properties":{"patchText":{"type":"string","description":"The patch text, written in the format named by the format field."},"format":{"type":"string","enum":["aider","unified"],"description":"Edit syntax of patchText, defaulting to aider. \"aider\": one or more SEARCH/REPLACE blocks, each a workspace-relative or explicitly security-authorized absolute file path on its own line, then <<<<<<< SEARCH, the exact lines to replace, =======, the replacement lines, and >>>>>>> REPLACE; repeat blocks under the same path for several edits to one file, and leave the SEARCH section empty to create a missing file or match an existing empty file. \"unified\": git diff text with --- and +++ headers and @@ hunks; a /dev/null source creates the file and a /dev/null target deletes it, and renames are rejected."}},"required":["patchText"],"additionalProperties":false}`)
}
func (*ApplyPatchTool) ErrorAdvice(raw json.RawMessage) (ErrorAdvice, error) {
	var input struct {
		PatchText string `json:"patchText"`
	}
	if err := json.Unmarshal(raw, &input); err != nil {
		return ErrorAdvice{}, err
	}
	format, err := patchFormat(raw)
	if err != nil {
		return ErrorAdvice{}, err
	}
	var patch change.Patch
	switch format {
	case change.PatchFormatAider:
		patch, err = change.ParsePatch(input.PatchText)
	case change.PatchFormatUnified:
		patch, err = change.ParseUnifiedDiff(input.PatchText)
	}
	if err != nil {
		return ErrorAdvice{}, err
	}
	advice := ErrorAdvice{Paths: make([]ErrorAdvicePath, 0, len(patch.Operations))}
	for _, operation := range patch.Operations {
		path := ErrorAdvicePath{Path: operation.Path}
		for _, hunk := range operation.Hunks {
			var lines []string
			for _, line := range hunk.Lines {
				if line.Kind == '-' {
					lines = append(lines, line.Text)
				}
			}
			if len(lines) > 0 {
				path.ExactContents = append(path.ExactContents, strings.Join(lines, "\n"))
			}
		}
		advice.Paths = append(advice.Paths, path)
	}
	return advice, nil
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
	scoped := patchWorkspace(call.Workspace, call.SecurityProfile)
	planned, err := service.PlanPatch(ctx, scoped, input.PatchText, format)
	if err != nil {
		return Plan{}, err
	}
	return mutationToolPlan(t.ID(), raw, planned, scoped)
}

func patchWorkspace(ws *workspace.Workspace, profile security.SecurityProfile) *workspace.Workspace {
	if ws == nil || profile == nil {
		return ws
	}
	var paths []workspace.ExternalPath
	for _, rule := range profile.Rules() {
		if rule.Action != security.ActionAllowWrite || ws.Contains(rule.Path) {
			continue
		}
		path, err := workspace.NewExternalPath(rule.Path)
		if err == nil {
			paths = append(paths, path)
		}
	}
	return ws.WithExternalPaths(paths...)
}

func (t *ApplyPatchTool) Execute(ctx context.Context, plan Plan, call CallContext) (Result, error) {
	planned, ok := plan.Data.(mutationPlan)
	if !ok {
		return Result{}, errors.New("apply_patch received incompatible plan")
	}
	for _, mutation := range planned.Change.Mutations {
		if !security.CanWrite(call.SecurityProfile, mutation.Path) {
			return Result{}, errors.New("apply_patch is not permitted by the current security profile")
		}
	}
	for _, directory := range planned.Change.Directories {
		if !security.CanWrite(call.SecurityProfile, directory) {
			return Result{}, errors.New("apply_patch is not permitted by the current security profile")
		}
	}
	return executeMutation(ctx, t.Changes, plan, call)
}
