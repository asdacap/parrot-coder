package tool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/amirulashraf/parrot-coder/internal/change"
	"github.com/amirulashraf/parrot-coder/internal/permission"
	"github.com/amirulashraf/parrot-coder/internal/process"
	"github.com/amirulashraf/parrot-coder/internal/question"
	"github.com/amirulashraf/parrot-coder/internal/session"
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

type ApplyPatchTool struct {
	Changes   *change.Service
	Snapshots *snapshot.Service
}

func NewApplyPatchTool(changes *change.Service, snapshots *snapshot.Service) *ApplyPatchTool {
	return &ApplyPatchTool{Changes: changes, Snapshots: snapshots}
}

func (*ApplyPatchTool) ID() string { return "apply_patch" }
func (*ApplyPatchTool) Description() string {
	return "Apply an OpenCode Begin Patch containing reviewed add, update, delete, or move operations."
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
	return json.RawMessage(`{"type":"object","properties":{"patchText":{"type":"string","description":"The full patch text that describes all changes to be made"}},"required":["patchText"],"additionalProperties":false}`)
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

func mutationToolPlan(toolID string, raw json.RawMessage, planned change.Plan) (Plan, error) {
	resources := make([]permission.Resource, len(planned.Mutations))
	files := make([]map[string]any, len(planned.Mutations))
	for i, mutation := range planned.Mutations {
		operation := "write"
		if !mutation.Before.Exists {
			operation = "create"
		} else if !mutation.After.Exists {
			operation = "delete"
		}
		resources[i] = permission.Resource{Kind: "filesystem", Identifier: mutation.Path, Operation: operation, Attributes: map[string]string{
			"before_sha256": mutation.Before.SHA256,
			"after_sha256":  mutation.After.SHA256,
		}}
		files[i] = map[string]any{"path": mutation.Path, "operation": operation, "before_sha256": mutation.Before.SHA256, "after_sha256": mutation.After.SHA256}
	}
	review, err := json.Marshal(map[string]any{"diff": planned.Diff, "files": files})
	if err != nil {
		return Plan{}, err
	}
	request, err := permission.NewRequest(toolID, raw, resources, review)
	if err != nil {
		return Plan{}, err
	}
	return NewPlan(toolID, raw, []permission.Request{request}, review, mutationPlan{planned})
}

func executeMutation(ctx context.Context, changes *change.Service, snapshots *snapshot.Service, plan Plan, call CallContext) (Result, error) {
	if changes == nil {
		changes = call.Changes
	}
	if snapshots == nil {
		snapshots = call.Snapshots
	}
	if changes == nil || snapshots == nil || call.Workspace == nil || call.SessionID == "" {
		return Result{}, errors.New("mutation tool requires change, snapshot, workspace, and session services")
	}
	planned, ok := plan.Data.(mutationPlan)
	if !ok {
		return Result{}, errors.New("mutation tool received incompatible plan")
	}
	if err := changes.Commit(ctx, call.Workspace, planned.Change); err != nil {
		return Result{}, err
	}
	entries := make([]snapshot.Entry, len(planned.Change.Mutations))
	for i, mutation := range planned.Change.Mutations {
		entries[i] = snapshot.Entry{Path: mutation.Path, Before: snapshotState(mutation.Before), After: snapshotState(mutation.After)}
	}
	transaction, err := snapshots.Record(ctx, call.Workspace, call.SessionID, entries)
	if err != nil {
		rollbackCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return Result{}, errors.Join(err, changes.Rollback(rollbackCtx, call.Workspace, planned.Change))
	}
	return Result{Text: planned.Change.Diff, Metadata: map[string]any{"transaction_id": transaction.ID, "files": len(entries)}}, nil
}

func snapshotState(state change.FileState) snapshot.State {
	return snapshot.State{Path: state.Path, Exists: state.Exists, Mode: state.Mode, SymlinkTarget: state.SymlinkTarget, Data: append([]byte(nil), state.Data...), SHA256: state.SHA256}
}

type ShellTool struct{ Runner *process.Runner }

func NewShellTool(runner *process.Runner) *ShellTool { return &ShellTool{Runner: runner} }

func (*ShellTool) ID() string { return "shell" }
func (*ShellTool) Description() string {
	return "Execute an arbitrary process through a shell in the workspace. The shell and working directory are detected from the environment and workspace when omitted. Shell permission permits arbitrary process execution."
}
func (*ShellTool) DescribeRequest(raw json.RawMessage) (string, error) {
	var input shellInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return "", err
	}
	description := "Run shell command:\n" + input.Command
	if input.Cwd != "" {
		description += "\nWorking directory: " + input.Cwd
	}
	if len(input.Env) > 0 {
		names := make([]string, 0, len(input.Env))
		for name := range input.Env {
			names = append(names, name)
		}
		sort.Strings(names)
		description += "\nEnvironment variables: " + strings.Join(names, ", ")
	}
	return description, nil
}
func (*ShellTool) JSONSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"shell":{"type":"string","description":"Absolute shell path; automatically detected when omitted"},"command":{"type":"string"},"cwd":{"type":"string","description":"Working directory; defaults to the workspace root"},"env":{"type":"object","additionalProperties":true},"timeout_ms":{"type":"integer","minimum":0}},"required":["command"],"additionalProperties":false}`)
}

type shellInput struct {
	Shell         string            `json:"shell"`
	Command       string            `json:"command"`
	Cwd           string            `json:"cwd"`
	Env           map[string]string `json:"env"`
	TimeoutMS     int64             `json:"timeout_ms"`
	ResolvedShell string            `json:"-"`
	ResolvedCwd   string            `json:"-"`
}

func (t *ShellTool) Plan(ctx context.Context, raw json.RawMessage, call CallContext) (Plan, error) {
	if call.Workspace == nil {
		return Plan{}, errors.New("shell: workspace is required")
	}
	var input shellInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return Plan{}, err
	}
	if input.Command == "" || input.TimeoutMS < 0 || input.TimeoutMS > int64((24*time.Hour)/time.Millisecond) {
		return Plan{}, errors.New("shell: command and a nonnegative timeout are required")
	}
	if input.Shell == "" {
		var err error
		input.Shell, err = process.DefaultShell()
		if err != nil {
			return Plan{}, fmt.Errorf("shell: detect executable: %w", err)
		}
	}
	if !filepath.IsAbs(input.Shell) {
		return Plan{}, errors.New("shell: shell must be an absolute path when specified")
	}
	resolvedShell, err := filepath.EvalSymlinks(input.Shell)
	if err != nil {
		return Plan{}, fmt.Errorf("shell: resolve executable: %w", err)
	}
	shellInfo, err := os.Stat(resolvedShell)
	if err != nil || !shellInfo.Mode().IsRegular() || shellInfo.Mode().Perm()&0o111 == 0 {
		return Plan{}, errors.New("shell: shell is not an executable regular file")
	}
	if input.Cwd == "" {
		input.Cwd = call.Workspace.Root()
	}
	resolved, err := call.Workspace.ResolveRead(input.Cwd)
	if err != nil {
		return Plan{}, err
	}
	sum := sha256.Sum256([]byte(input.Command))
	resource := permission.Resource{Kind: "process", Identifier: resolvedShell, Operation: "execute", Attributes: map[string]string{
		"cwd": resolved, "command_sha256": hex.EncodeToString(sum[:]),
	}}
	environmentNames := make([]string, 0, len(input.Env))
	for name := range input.Env {
		environmentNames = append(environmentNames, name)
	}
	sort.Strings(environmentNames)
	review, _ := json.Marshal(map[string]any{"warning": "This permits arbitrary process execution.", "shell": resolvedShell, "command": input.Command, "cwd": resolved, "environment_names": environmentNames, "timeout_ms": input.TimeoutMS})
	request, err := permission.NewRequest(t.ID(), raw, []permission.Resource{resource}, review)
	if err != nil {
		return Plan{}, err
	}
	input.ResolvedShell, input.ResolvedCwd = resolvedShell, resolved
	return NewPlan(t.ID(), raw, []permission.Request{request}, review, input)
}

func (t *ShellTool) Execute(ctx context.Context, plan Plan, call CallContext) (Result, error) {
	runner := t.Runner
	if runner == nil {
		runner = call.Processes
	}
	if runner == nil {
		return Result{}, errors.New("shell: process runner is required")
	}
	if call.Workspace == nil {
		return Result{}, errors.New("shell: workspace is required")
	}
	input := plan.Data.(shellInput)
	resolvedShell, err := filepath.EvalSymlinks(input.Shell)
	if err != nil || resolvedShell != input.ResolvedShell {
		return Result{}, errors.New("shell: executable changed after planning")
	}
	resolvedCwd, err := call.Workspace.ResolveRead(input.Cwd)
	if err != nil || resolvedCwd != input.ResolvedCwd {
		return Result{}, errors.New("shell: cwd changed after planning")
	}
	result, err := runner.Run(ctx, process.Request{Shell: input.ResolvedShell, Command: input.Command, Cwd: input.Cwd, Env: input.Env, Timeout: time.Duration(input.TimeoutMS) * time.Millisecond})
	if err != nil {
		return Result{}, err
	}
	metadata := map[string]any{"exit_code": result.ExitCode, "timed_out": result.TimedOut, "cancelled": result.Cancelled, "truncated": result.Truncated}
	if result.OutputID != "" {
		metadata["output_id"] = result.OutputID
		metadata["output_bytes"] = result.OutputSize
	}
	return Result{Text: result.Output, Metadata: metadata}, nil
}

type TodoReadTool struct{ Service *session.TodoService }

func NewTodoReadTool(service *session.TodoService) *TodoReadTool {
	return &TodoReadTool{Service: service}
}
func (*TodoReadTool) ID() string          { return "todoread" }
func (*TodoReadTool) Description() string { return "Read the current session's ordered todo list." }
func (*TodoReadTool) DescribeRequest(json.RawMessage) (string, error) {
	return "Read the current todo list", nil
}
func (*TodoReadTool) JSONSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","additionalProperties":false}`)
}
func (t *TodoReadTool) Plan(_ context.Context, raw json.RawMessage, _ CallContext) (Plan, error) {
	return NewPlan(t.ID(), raw, nil, nil, nil)
}
func (t *TodoReadTool) Execute(ctx context.Context, _ Plan, call CallContext) (Result, error) {
	service := t.Service
	if service == nil {
		service = call.Todos
	}
	if service == nil || call.SessionID == "" {
		return Result{}, errors.New("todoread: service and session are required")
	}
	items, err := service.List(ctx, call.SessionID)
	if err != nil {
		return Result{}, err
	}
	data, _ := json.Marshal(items)
	return Result{Text: string(data), Metadata: map[string]any{"count": len(items)}}, nil
}

type TodoWriteTool struct{ Service *session.TodoService }

func NewTodoWriteTool(service *session.TodoService) *TodoWriteTool {
	return &TodoWriteTool{Service: service}
}
func (*TodoWriteTool) ID() string { return "todowrite" }
func (*TodoWriteTool) Description() string {
	return "Transactionally replace the current session's ordered todo list."
}
func (*TodoWriteTool) DescribeRequest(raw json.RawMessage) (string, error) {
	var input todoWriteInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return "", err
	}
	return fmt.Sprintf("Replace the todo list with %d items", len(input.Todos)), nil
}
func (*TodoWriteTool) JSONSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"todos":{"type":"array","items":{"type":"object","properties":{"id":{"type":"string"},"content":{"type":"string","minLength":1},"status":{"type":"string","enum":["pending","in_progress","completed","cancelled"]},"priority":{"type":"string","enum":["high","medium","low"]}},"required":["content","status","priority"],"additionalProperties":false}}},"required":["todos"],"additionalProperties":false}`)
}

type todoWriteInput struct {
	Todos []session.Todo `json:"todos"`
}

func (t *TodoWriteTool) Plan(_ context.Context, raw json.RawMessage, _ CallContext) (Plan, error) {
	var input todoWriteInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return Plan{}, err
	}
	return NewPlan(t.ID(), raw, nil, nil, input)
}
func (t *TodoWriteTool) Execute(ctx context.Context, plan Plan, call CallContext) (Result, error) {
	service := t.Service
	if service == nil {
		service = call.Todos
	}
	if service == nil || call.SessionID == "" {
		return Result{}, errors.New("todowrite: service and session are required")
	}
	items, err := service.Replace(ctx, call.SessionID, plan.Data.(todoWriteInput).Todos)
	if err != nil {
		return Result{}, err
	}
	data, _ := json.Marshal(items)
	return Result{Text: string(data), Metadata: map[string]any{"count": len(items)}}, nil
}

type QuestionTool struct{ Broker *question.Broker }

func NewQuestionTool(broker *question.Broker) *QuestionTool { return &QuestionTool{Broker: broker} }
func (*QuestionTool) ID() string                            { return "question" }
func (*QuestionTool) Description() string {
	return "Ask typed questions and block until the user replies or rejects them."
}
func (*QuestionTool) DescribeRequest(raw json.RawMessage) (string, error) {
	var input question.Request
	if err := json.Unmarshal(raw, &input); err != nil {
		return "", err
	}
	return fmt.Sprintf("Ask %d question(s)", len(input.Questions)), nil
}
func (*QuestionTool) JSONSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"questions":{"type":"array","items":{"type":"object","properties":{"id":{"type":"string"},"header":{"type":"string"},"prompt":{"type":"string"},"options":{"type":"array","items":{"type":"object","properties":{"id":{"type":"string"},"label":{"type":"string"},"description":{"type":"string"}},"required":["id","label"],"additionalProperties":false}},"multiple":{"type":"boolean"},"custom":{"type":"boolean"}},"required":["id","prompt"],"additionalProperties":false}}},"required":["questions"],"additionalProperties":false}`)
}
func (t *QuestionTool) Plan(_ context.Context, raw json.RawMessage, call CallContext) (Plan, error) {
	var input question.Request
	if err := json.Unmarshal(raw, &input); err != nil {
		return Plan{}, err
	}
	input.SessionID = call.SessionID
	return NewPlan(t.ID(), raw, nil, nil, input)
}
func (t *QuestionTool) Execute(ctx context.Context, plan Plan, call CallContext) (Result, error) {
	broker := t.Broker
	if broker == nil {
		broker = call.Questions
	}
	if broker == nil {
		return Result{}, errors.New("question: broker is required")
	}
	response, err := broker.Ask(ctx, plan.Data.(question.Request))
	if err != nil {
		return Result{}, err
	}
	data, _ := json.Marshal(response)
	return Result{Text: string(data)}, nil
}

type Phase6Services struct {
	Changes   *change.Service
	Snapshots *snapshot.Service
	Processes *process.Runner
	Todos     *session.TodoService
	Questions *question.Broker
}

// RegisterPhase6 registers all Phase 6 tools. Build profiles expose registered
// tools by default; plan and explore remain restricted by agent enforcement.
func RegisterPhase6(registry *Registry, services Phase6Services) error {
	if registry == nil {
		return errors.New("tool: registry is required")
	}
	for _, item := range []Tool{
		NewEditTool(services.Changes, services.Snapshots),
		NewApplyPatchTool(services.Changes, services.Snapshots),
		NewShellTool(services.Processes),
		NewTodoReadTool(services.Todos),
		NewTodoWriteTool(services.Todos),
		NewQuestionTool(services.Questions),
	} {
		if err := registry.Register(item); err != nil {
			return fmt.Errorf("tool: register %s: %w", item.ID(), err)
		}
	}
	return nil
}
