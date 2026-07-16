package tool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/amirulashraf/parrot-coder/internal/permission"
	"github.com/amirulashraf/parrot-coder/internal/process"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type ShellTool struct {
	Runner       *process.Runner
	unrestricted bool
}

func NewShellTool(runner *process.Runner) *ShellTool { return &ShellTool{Runner: runner} }
func NewUnrestrictedShellTool(runner *process.Runner) *ShellTool {
	return &ShellTool{Runner: runner, unrestricted: true}
}

func (t *ShellTool) ID() string {
	if t.unrestricted {
		return "unrestricted_shell"
	}
	return "shell"
}
func (t *ShellTool) Description() string {
	if t.unrestricted {
		return "Execute an arbitrary process through a shell without the OS sandbox. This requires permission and runs with the invoking user's local authority."
	}
	return "Execute an arbitrary process through a shell in the workspace. The shell and working directory are detected from the environment and workspace when omitted. Shell permission permits arbitrary process execution."
}
func (t *ShellTool) DescribeRequest(raw json.RawMessage) (string, error) {
	var input shellInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return "", err
	}
	description := "Run shell command:\n" + input.Command
	if t.unrestricted {
		description = "Run shell command without the OS sandbox (full local authority):\n" + input.Command
	}
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
	warning := "This permits arbitrary process execution inside the OS sandbox."
	if t.unrestricted {
		warning = "This permits arbitrary process execution without the OS sandbox, using the invoking user's local authority."
	}
	review, _ := json.Marshal(map[string]any{"warning": warning, "shell": resolvedShell, "command": input.Command, "cwd": resolved, "environment_names": environmentNames, "timeout_ms": input.TimeoutMS})
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
	request := process.Request{Shell: input.ResolvedShell, Command: input.Command, Cwd: input.Cwd, Env: input.Env, Timeout: time.Duration(input.TimeoutMS) * time.Millisecond, Output: call.Output, SessionID: call.SessionID}
	var result process.Result
	if t.unrestricted {
		result, err = runner.RunUnrestricted(ctx, request)
	} else {
		result, err = runner.Run(ctx, request)
	}
	if err != nil {
		return Result{}, err
	}
	metadata := map[string]any{"exit_code": result.ExitCode, "timed_out": result.TimedOut, "cancelled": result.Cancelled, "truncated": result.Truncated}
	metadata["output_tail"] = result.OutputTail
	if result.OutputID != "" {
		metadata["output_id"] = result.OutputID
		metadata["output_bytes"] = result.OutputSize
	}
	return Result{Text: result.Output, Metadata: metadata}, nil
}
