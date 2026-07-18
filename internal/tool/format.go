package tool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/amirulashraf/parrot-coder/internal/change"
	"github.com/amirulashraf/parrot-coder/internal/formatter"
	"github.com/amirulashraf/parrot-coder/internal/permission"
	"os"
)

type FormatTool struct {
	Formatters *formatter.Registry
	Changes    *change.Service
}

func NewFormatTool(formatters *formatter.Registry, changes *change.Service) *FormatTool {
	return &FormatTool{Formatters: formatters, Changes: changes}
}
func (*FormatTool) ID() string { return "format" }
func (*FormatTool) Description() string {
	return "Run the configured formatter during planning, review its command and exact proposed diff, then commit those bytes without rerunning it."
}
func (*FormatTool) DescribeRequest(raw json.RawMessage) (string, error) {
	var input struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(raw, &input); err != nil {
		return "", err
	}
	return fmt.Sprintf("Format workspace file %q", input.Path), nil
}
func (*FormatTool) JSONSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"expected_sha256":{"type":"string"}},"required":["path","expected_sha256"],"additionalProperties":false}`)
}
func (t *FormatTool) Plan(ctx context.Context, raw json.RawMessage, call CallContext) (Plan, error) {
	if t.Formatters == nil || call.Workspace == nil {
		return Plan{}, errors.New("format: formatter registry and workspace are required")
	}
	var input struct {
		Path           string `json:"path"`
		ExpectedSHA256 string `json:"expected_sha256"`
	}
	if err := json.Unmarshal(raw, &input); err != nil {
		return Plan{}, err
	}
	formatterPlan, err := t.Formatters.Plan(input.Path, input.ExpectedSHA256)
	if err != nil {
		return Plan{}, err
	}
	formatted, err := t.Formatters.Format(ctx, formatterPlan)
	if err != nil {
		return Plan{}, err
	}
	commandJSON, _ := json.Marshal(formatterPlan.Command)
	commandDigest := sha256.Sum256(commandJSON)
	commandHash := hex.EncodeToString(commandDigest[:])
	if !formatted.Changed {
		review, _ := json.Marshal(map[string]any{"path": formatted.Path, "formatter": formatterPlan.Formatter, "command": formatterPlan.Command, "command_sha256": commandHash, "before_sha256": formatted.BeforeHash, "after_sha256": formatted.AfterHash, "diff": ""})
		return NewPlan(t.ID(), raw, nil, review, formatNoop{Path: formatted.Path, Hash: formatted.BeforeHash, Formatter: formatterPlan.Formatter})
	}
	path, err := call.Workspace.ResolveRead(input.Path)
	if err != nil || path != formatted.Path {
		return Plan{}, errors.New("format: path changed while preparing proposal")
	}
	before, err := os.ReadFile(path)
	if err != nil || change.SHA256(before) != formatted.BeforeHash {
		return Plan{}, change.ErrStale
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return Plan{}, errors.New("format: target must be a regular file")
	}
	beforeState := change.FileState{Path: path, Exists: true, Mode: info.Mode().Perm(), Data: append([]byte(nil), before...), SHA256: formatted.BeforeHash}
	afterState := change.FileState{Path: path, Exists: true, Mode: info.Mode().Perm(), Data: append([]byte(nil), formatted.Proposed...), SHA256: formatted.AfterHash}
	changePlan := change.Plan{Mutations: []change.Mutation{{RequestedPath: input.Path, Path: path, Before: beforeState, After: afterState}}, Diff: formatted.Diff}
	review, _ := json.Marshal(map[string]any{"path": path, "formatter": formatterPlan.Formatter, "command": formatterPlan.Command, "command_sha256": commandHash, "before_sha256": formatted.BeforeHash, "after_sha256": formatted.AfterHash, "diff": formatted.Diff})
	request, err := permission.NewRequest(t.ID(), raw, []permission.Resource{{Kind: "filesystem", Identifier: path, Operation: "write", Attributes: map[string]string{"before_sha256": formatted.BeforeHash, "after_sha256": formatted.AfterHash}}}, review)
	if err != nil {
		return Plan{}, err
	}
	return NewPlan(t.ID(), raw, []permission.Request{request}, review, mutationPlan{changePlan})
}
func (t *FormatTool) Execute(ctx context.Context, plan Plan, call CallContext) (Result, error) {
	if noop, ok := plan.Data.(formatNoop); ok {
		path, err := call.Workspace.ResolveRead(noop.Path)
		if err != nil || path != noop.Path {
			return Result{}, change.ErrStale
		}
		data, err := os.ReadFile(path)
		if err != nil || change.SHA256(data) != noop.Hash {
			return Result{}, change.ErrStale
		}
		return Result{Text: "File is already formatted.", Metadata: map[string]any{"path": noop.Path, "formatter": noop.Formatter, "changed": false}}, nil
	}
	return executeMutation(ctx, t.Changes, plan, call)
}

type formatNoop struct {
	Path      string
	Hash      string
	Formatter string
}
