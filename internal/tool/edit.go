package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/amirulashraf/parrot-coder/internal/change"
	"github.com/amirulashraf/parrot-coder/internal/workspace"
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
	if looksLikeUnifiedDiff(input.New) && !existingFileIsUnifiedDiff(call.Workspace, input.Path) {
		return Plan{}, fmt.Errorf("edit: the content looks like a unified diff, but edit replaces the whole file. Use apply_patch to apply the diff instead")
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

// looksLikeUnifiedDiff reports whether text appears to be a git-style unified
// diff rather than a plain file replacement. This is a heuristic: a file whose
// first non-empty line starts with "--- " and whose second starts with "+++ "
// is almost certainly a patch, not a file whose content happens to resemble one.
func looksLikeUnifiedDiff(text string) bool {
	lines := strings.Split(text, "\n")
	firstNonEmpty := -1
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "```") {
			continue
		}
		firstNonEmpty = i
		break
	}
	if firstNonEmpty < 0 || !strings.HasPrefix(strings.TrimRight(lines[firstNonEmpty], "\r"), "--- ") {
		return false
	}
	for j := firstNonEmpty + 1; j < len(lines); j++ {
		trimmed := strings.TrimSpace(lines[j])
		if trimmed == "" || strings.HasPrefix(trimmed, "```") {
			continue
		}
		return strings.HasPrefix(trimmed, "+++ ")
	}
	return false
}

// existingFileIsUnifiedDiff reads the existing file at the given path (if it
// exists) and reports whether its content looks like a unified diff. This lets
// the edit tool allow edits to files that legitimately contain diff-like content
// (e.g. patch files, test fixtures).
func existingFileIsUnifiedDiff(ws *workspace.Workspace, path string) bool {
	resolved, err := ws.ResolveRead(path)
	if err != nil {
		return false
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		return false
	}
	return looksLikeUnifiedDiff(string(data))
}
