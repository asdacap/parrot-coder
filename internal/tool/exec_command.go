package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/amirulashraf/parrot-coder/internal/permission"
	"github.com/amirulashraf/parrot-coder/internal/process"
)

const execCommandSchema = `{"type":"object","properties":{"cmd":{"type":"string","description":"Shell command to execute."},"name":{"type":"string","description":"Optional friendly name for the shell task. A name is generated if the command remains running."},"workdir":{"type":"string","description":"Working directory for the command. Defaults to the turn cwd."},"env":{"type":"object","additionalProperties":{"type":"string"},"description":"Environment variables for the command. Values override the default output-hygiene settings."},"tty":{"type":"boolean","description":"True allocates a PTY for the command; false or omitted uses plain pipes."},"yield_time_ms":{"type":"number","description":"Wait before yielding output. Defaults to 10000 ms; effective range is 250-30000 ms."},"max_output_tokens":{"type":"number","description":"Output token budget. Defaults to 10000 tokens; larger requests may be capped by policy."},"shell":{"type":"string","description":"Shell binary to launch. Defaults to the user's default shell."},"sandbox_permissions":{"type":"string","enum":["current_security_context","disable_sandbox"],"description":"Per-command sandbox override. Defaults to \u0060current_security_context\u0060; use \u0060disable_sandbox\u0060 for unsandboxed execution."},"justification":{"type":"string","description":"User-facing approval question for \u0060disable_sandbox\u0060; omit otherwise."}},"required":["cmd"],"additionalProperties":false}`

type ExecCommandTool struct {
	BasePresentation
	Runner *process.Runner
}

type execCommandInput struct {
	Command            string            `json:"cmd"`
	Name               string            `json:"name"`
	Workdir            string            `json:"workdir"`
	Env                map[string]string `json:"env"`
	TTY                bool              `json:"tty"`
	YieldTimeMS        uint64            `json:"yield_time_ms"`
	MaxOutputTokens    *int              `json:"max_output_tokens"`
	Shell              string            `json:"shell"`
	SandboxPermissions string            `json:"sandbox_permissions"`
	Justification      string            `json:"justification"`
	ResolvedShell      string            `json:"-"`
	ResolvedWorkdir    string            `json:"-"`
}

func NewExecCommandTool(runner *process.Runner) *ExecCommandTool {
	return &ExecCommandTool{Runner: runner}
}

func (*ExecCommandTool) ID() string { return "exec_command" }
func (*ExecCommandTool) Presentation() Presentation {
	return Presentation{
		Output: OutputTail,
		Label:  LabelSpec{Fields: []LabelField{{Names: []string{"cmd"}}}},
	}
}

func (*ExecCommandTool) Description() string {
	return "Runs a command in a PTY, returning output or a task ID for ongoing interaction."
}

func (*ExecCommandTool) SystemPromptGuidance() string {
	return `exec_command runs in a sandbox by default (current_security_context). Directories outside the workspace that lack write permission appear as if they are mounted read-only — the sandbox intercepts writes and returns "Read-only file system" even though the underlying mount is writable. Use request_write_permission to grant session-scoped write access to a specific path, or set sandbox_permissions to "disable_sandbox" (requires justification and user approval) to bypass the sandbox entirely.`
}

func (*ExecCommandTool) JSONSchema() json.RawMessage { return json.RawMessage(execCommandSchema) }

func (*ExecCommandTool) DescribeRequest(raw json.RawMessage) (string, error) {
	var input execCommandInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return "", err
	}
	description := "Run command in the OS sandbox:\n" + input.Command
	if input.SandboxPermissions == "disable_sandbox" {
		description = "Run command without the OS sandbox (full local authority):\n" + input.Command
	}
	if input.Workdir != "" {
		description += "\nWorking directory: " + input.Workdir
	}
	if len(input.Env) > 0 {
		description += "\nEnvironment variables: " + strings.Join(sortedEnvironmentNames(input.Env), ", ")
	}
	return description, nil
}

func (t *ExecCommandTool) Plan(_ context.Context, raw json.RawMessage, call CallContext) (Plan, error) {
	if call.Workspace == nil {
		return Plan{}, errors.New("exec_command: workspace is required")
	}
	var input execCommandInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return Plan{}, err
	}
	if input.Command == "" {
		return Plan{}, errors.New("exec_command: cmd is required")
	}
	if input.YieldTimeMS == 0 {
		input.YieldTimeMS = uint64(process.DefaultExecYieldTime / time.Millisecond)
	}
	if input.MaxOutputTokens != nil && *input.MaxOutputTokens < 0 {
		return Plan{}, errors.New("exec_command: max_output_tokens must be nonnegative")
	}
	if input.SandboxPermissions == "" {
		input.SandboxPermissions = "current_security_context"
	}
	if input.SandboxPermissions != "current_security_context" && input.SandboxPermissions != "disable_sandbox" {
		return Plan{}, errors.New("exec_command: unsupported sandbox_permissions")
	}
	if input.SandboxPermissions == "disable_sandbox" && input.Justification == "" {
		return Plan{}, errors.New("exec_command: justification is required for unsandboxed execution")
	}
	if input.SandboxPermissions != "disable_sandbox" && input.Justification != "" {
		return Plan{}, errors.New("exec_command: justification requires unsandboxed execution")
	}
	if input.Shell == "" {
		var err error
		input.Shell, err = process.DefaultShell()
		if err != nil {
			return Plan{}, err
		}
	}
	if !filepath.IsAbs(input.Shell) {
		return Plan{}, errors.New("exec_command: shell must be absolute")
	}
	resolvedShell, err := filepath.EvalSymlinks(input.Shell)
	if err != nil {
		return Plan{}, fmt.Errorf("exec_command: resolve shell: %w", err)
	}
	info, err := os.Stat(resolvedShell)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return Plan{}, errors.New("exec_command: shell is not an executable regular file")
	}
	base := call.Workspace.Root()
	if t.Runner != nil {
		base = t.Runner.WorkingDirectory()
	} else if call.Processes != nil {
		base = call.Processes.WorkingDirectory()
	}
	resolvedWorkdir, err := process.ResolveWorkingDirectory(input.Workdir, base)
	if err != nil {
		return Plan{}, fmt.Errorf("exec_command: resolve workdir: %w", err)
	}
	input.ResolvedShell, input.ResolvedWorkdir = resolvedShell, resolvedWorkdir
	review, _ := json.Marshal(map[string]any{
		"warning": input.SandboxPermissions, "shell": resolvedShell, "command": input.Command,
		"cwd": resolvedWorkdir, "tty": input.TTY, "yield_time_ms": input.YieldTimeMS,
		"max_output_tokens": input.MaxOutputTokens, "justification": input.Justification,
		"environment_names": sortedEnvironmentNames(input.Env),
	})
	// Only an unsandboxed command needs approval: it escapes the sandbox which
	// otherwise confines the command.
	var requests []permission.Request
	if input.SandboxPermissions == "disable_sandbox" {
		request, err := permission.NewRequest(t.ID(), review)
		if err != nil {
			return Plan{}, err
		}
		requests = []permission.Request{request}
	}
	return NewPlan(t.ID(), raw, requests, review, input)
}

func (t *ExecCommandTool) Execute(ctx context.Context, plan Plan, call CallContext) (Result, error) {
	runner := t.Runner
	if runner == nil {
		runner = call.Processes
	}
	if runner == nil {
		return Result{}, errors.New("exec_command: process runner is required")
	}
	input := plan.Data.(execCommandInput)
	resolvedShell, err := filepath.EvalSymlinks(input.Shell)
	if err != nil || resolvedShell != input.ResolvedShell {
		return Result{}, errors.New("exec_command: shell changed after planning")
	}
	resolvedWorkdir, err := process.ResolveWorkingDirectory(input.Workdir, runner.WorkingDirectory())
	if err != nil || resolvedWorkdir != input.ResolvedWorkdir {
		return Result{}, errors.New("exec_command: workdir changed after planning")
	}
	result, err := runner.RunPersistent(ctx, process.PersistentRequest{
		Shell: input.ResolvedShell, Command: input.Command, Name: input.Name, Cwd: input.ResolvedWorkdir, Env: input.Env,
		SessionID: call.SessionID, Yield: time.Duration(input.YieldTimeMS) * time.Millisecond,
		MaxOutputTokens: input.MaxOutputTokens, TTY: input.TTY, Output: call.Output,
		Unrestricted:    input.SandboxPermissions == "disable_sandbox",
		SecurityProfile: call.SecurityProfile,
	})
	if err != nil {
		return Result{}, err
	}
	text := formatPersistentResult(result)
	return Result{Text: text, ModelText: modelText(text)}, nil
}

func formatPersistentResult(result process.PersistentResult) string {
	text := fmt.Sprintf("Chunk ID: %s\nWall time: %.4f seconds", result.ChunkID, result.WallTime.Seconds())
	if result.ExitCode != nil {
		text += fmt.Sprintf("\nProcess exited with code %d", *result.ExitCode)
	}
	if result.ProcessID != nil {
		text += fmt.Sprintf("\nShell task %s running with task ID %s", result.Name, *result.ProcessID)
	}
	text += fmt.Sprintf("\nOriginal token count: %d\nOutput:\n%s", result.OriginalTokenCount, result.Output)
	return text
}
