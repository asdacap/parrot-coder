package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/amirulashraf/parrot-coder/internal/permission"
	"github.com/amirulashraf/parrot-coder/internal/process"
)

type WritePermissionTool struct {
	BasePresentation
	Runner *process.Runner
}

func NewWritePermissionTool(runner *process.Runner) *WritePermissionTool {
	return &WritePermissionTool{Runner: runner}
}

func (*WritePermissionTool) ID() string { return "request_write_permission" }

// PermissionChoices names the answers after what this tool hands out: granting
// it adds a write path to the sandbox for the rest of the session, which is a
// capability rather than a one-off operation.
func (*WritePermissionTool) PermissionChoices() []permission.Choice {
	return []permission.Choice{
		{Value: "grant", Decision: "allow", Label: "grant", Description: "Allow sandboxed writes to this path for the current session"},
		{Value: "reject", Decision: "deny", Label: "reject", Description: "Reject this request"},
		{Value: "reject with reason", Decision: "deny", Label: "reject with reason", Description: "Reject and provide feedback to the agent", RequiresReason: true},
	}
}
func (t *WritePermissionTool) Descriptor() Descriptor {
	return Descriptor{
		ID:                   t.ID(),
		Description:          "Request session-scoped permission for sandboxed shell commands to write to an existing file or directory.",
		Schema:               t.JSONSchema(),
		Presentation:         t.Presentation(),
		SystemPromptGuidance: `request_write_permission grants session-scoped write access to an existing file or directory for sandboxed shell commands. After a successful grant, exec_command and other sandboxed tools can write to that path. This is necessary because the sandbox presents paths without write permission as read-only filesystems, even when the underlying filesystem is writable.`,
	}
}
func (*WritePermissionTool) JSONSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"Existing file or directory to make writable for sandboxed shell commands in the current session"}},"required":["path"],"additionalProperties":false}`)
}

type writePermissionInput struct {
	Path         string `json:"path"`
	ResolvedPath string `json:"-"`
}

func (*WritePermissionTool) DescribeRequest(raw json.RawMessage) (string, error) {
	var input writePermissionInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return "", err
	}
	path, err := filepath.Abs(input.Path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	return "Allow sandboxed shell writes for this session to:\n" + resolved, nil
}

func (t *WritePermissionTool) Plan(_ context.Context, raw json.RawMessage, call CallContext) (Plan, error) {
	if call.Workspace == nil {
		return Plan{}, errors.New("request_write_permission: workspace is required")
	}
	if call.SessionID == "" {
		return Plan{}, errors.New("request_write_permission: session is required")
	}
	var input writePermissionInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return Plan{}, err
	}
	if input.Path == "" {
		return Plan{}, errors.New("request_write_permission: path is required")
	}
	path, err := filepath.Abs(input.Path)
	if err != nil {
		return Plan{}, fmt.Errorf("request_write_permission: absolute path: %w", err)
	}
	input.ResolvedPath, err = filepath.EvalSymlinks(path)
	if err != nil {
		return Plan{}, fmt.Errorf("request_write_permission: resolve path: %w", err)
	}
	info, err := os.Stat(input.ResolvedPath)
	if err != nil {
		return Plan{}, fmt.Errorf("request_write_permission: stat path: %w", err)
	}
	relative, relErr := filepath.Rel(call.Workspace.Root(), input.ResolvedPath)
	if relErr == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return Plan{}, errors.New("request_write_permission: workspace paths are already writable; protected workspace metadata cannot be granted")
	}
	kind := "file"
	if info.IsDir() {
		kind = "directory"
	}
	review, _ := json.Marshal(map[string]string{"path": input.ResolvedPath, "kind": kind, "scope": "session"})
	request, err := permission.NewRequest(t.ID(), review)
	if err != nil {
		return Plan{}, err
	}
	return NewPlan(t.ID(), raw, []permission.Request{request}, review, input)
}

func (t *WritePermissionTool) Execute(_ context.Context, plan Plan, call CallContext) (Result, error) {
	if call.SecurityProfile != nil && call.SecurityProfile.IsReadOnly() {
		return Result{}, errors.New("request_write_permission is not permitted by the current security profile")
	}
	runner := t.Runner
	if runner == nil {
		runner = call.Processes
	}
	if runner == nil {
		return Result{}, errors.New("request_write_permission: process runner is required")
	}
	input := plan.Data.(writePermissionInput)
	resolved, err := filepath.EvalSymlinks(input.ResolvedPath)
	if err != nil || resolved != input.ResolvedPath {
		return Result{}, errors.New("request_write_permission: path changed after approval")
	}
	if err := runner.AllowWrite(call.SessionID, input.ResolvedPath); err != nil {
		return Result{}, err
	}
	text := "Sandboxed shell writes allowed for this session: " + input.ResolvedPath
	return Result{Text: text, ModelText: text}, nil
}
