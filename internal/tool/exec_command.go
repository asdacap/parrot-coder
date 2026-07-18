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
	"strings"
	"time"

	"github.com/amirulashraf/parrot-coder/internal/permission"
	"github.com/amirulashraf/parrot-coder/internal/process"
)

const execCommandSchema = `{"type":"object","properties":{"cmd":{"type":"string","description":"Shell command to execute."},"workdir":{"type":"string","description":"Working directory for the command. Defaults to the turn cwd."},"tty":{"type":"boolean","description":"True allocates a PTY for the command; false or omitted uses plain pipes."},"yield_time_ms":{"type":"number","description":"Wait before yielding output. Defaults to 10000 ms; effective range is 250-30000 ms."},"max_output_tokens":{"type":"number","description":"Output token budget. Defaults to 10000 tokens; larger requests may be capped by policy."},"shell":{"type":"string","description":"Shell binary to launch. Defaults to the user's default shell."},"sandbox_permissions":{"type":"string","enum":["use_default","require_escalated"],"description":"Per-command sandbox override. Defaults to \u0060use_default\u0060; use \u0060require_escalated\u0060 for unsandboxed execution."},"justification":{"type":"string","description":"User-facing approval question for \u0060require_escalated\u0060; omit otherwise."},"prefix_rule":{"type":"array","items":{"type":"string"},"description":"Reusable approval prefix for \u0060cmd\u0060, only with \u0060sandbox_permissions: \u0022require_escalated\u0022\u0060; for example [\"git\", \"pull\"]."}},"required":["cmd"],"additionalProperties":false}`

type ExecCommandTool struct{ Runner *process.Runner }

type execCommandInput struct {
	Command            string   `json:"cmd"`
	Workdir            string   `json:"workdir"`
	TTY                bool     `json:"tty"`
	YieldTimeMS        uint64   `json:"yield_time_ms"`
	MaxOutputTokens    *int     `json:"max_output_tokens"`
	Shell              string   `json:"shell"`
	SandboxPermissions string   `json:"sandbox_permissions"`
	Justification      string   `json:"justification"`
	PrefixRule         []string `json:"prefix_rule"`
	ResolvedShell      string   `json:"-"`
	ResolvedWorkdir    string   `json:"-"`
}

func NewExecCommandTool(runner *process.Runner) *ExecCommandTool {
	return &ExecCommandTool{Runner: runner}
}

func (*ExecCommandTool) ID() string { return "exec_command" }

func (*ExecCommandTool) Description() string {
	return "Runs a command in a PTY, returning output or a session ID for ongoing interaction."
}

func (*ExecCommandTool) JSONSchema() json.RawMessage { return json.RawMessage(execCommandSchema) }

func (*ExecCommandTool) DescribeRequest(raw json.RawMessage) (string, error) {
	var input execCommandInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return "", err
	}
	command := input.Command
	if len(input.PrefixRule) != 0 {
		command = "commands prefixed by " + strings.Join(input.PrefixRule, " ")
	}
	description := "Run command in the OS sandbox:\n" + command
	if input.SandboxPermissions == "require_escalated" {
		description = "Run command without the OS sandbox (full local authority):\n" + command
	}
	if input.Workdir != "" {
		description += "\nWorking directory: " + input.Workdir
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
		input.SandboxPermissions = "use_default"
	}
	if input.SandboxPermissions != "use_default" && input.SandboxPermissions != "require_escalated" {
		return Plan{}, errors.New("exec_command: unsupported sandbox_permissions")
	}
	if input.SandboxPermissions == "require_escalated" && input.Justification == "" {
		return Plan{}, errors.New("exec_command: justification is required for escalated execution")
	}
	if input.SandboxPermissions != "require_escalated" && (input.Justification != "" || len(input.PrefixRule) != 0) {
		return Plan{}, errors.New("exec_command: justification and prefix_rule require escalated execution")
	}
	if len(input.PrefixRule) != 0 {
		if !safePrefixCommand(input.Command, input.PrefixRule) {
			return Plan{}, errors.New("exec_command: prefix_rule must prefix cmd")
		}
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
	operation := "execute"
	if input.SandboxPermissions == "require_escalated" {
		operation = "execute_unrestricted"
	}
	sum := sha256.Sum256([]byte(input.Command))
	resource := permission.Resource{Kind: "process", Identifier: resolvedShell, Operation: operation, Attributes: map[string]string{
		"cwd": resolvedWorkdir, "command_sha256": hex.EncodeToString(sum[:]),
	}}
	if len(input.PrefixRule) != 0 {
		prefix := sha256.Sum256([]byte(strings.Join(input.PrefixRule, "\x00")))
		resource.Attributes["prefix_rule_sha256"] = hex.EncodeToString(prefix[:])
		delete(resource.Attributes, "command_sha256")
	}
	reviewValue := map[string]any{
		"warning": input.SandboxPermissions, "shell": resolvedShell, "command": input.Command,
		"cwd": resolvedWorkdir, "tty": input.TTY, "yield_time_ms": input.YieldTimeMS,
		"max_output_tokens": input.MaxOutputTokens, "justification": input.Justification, "prefix_rule": input.PrefixRule,
	}
	review, _ := json.Marshal(reviewValue)
	permissionInput, permissionReview := raw, review
	if len(input.PrefixRule) != 0 {
		permissionInput, _ = json.Marshal(map[string]any{
			"shell": resolvedShell, "workdir": resolvedWorkdir, "sandbox_permissions": input.SandboxPermissions,
			"prefix_rule": input.PrefixRule,
		})
		delete(reviewValue, "command")
		delete(reviewValue, "justification")
		delete(reviewValue, "yield_time_ms")
		delete(reviewValue, "max_output_tokens")
		delete(reviewValue, "tty")
		permissionReview, _ = json.Marshal(reviewValue)
	}
	request, err := permission.NewRequest(t.ID(), permissionInput, []permission.Resource{resource}, permissionReview)
	if err != nil {
		return Plan{}, err
	}
	return NewPlan(t.ID(), raw, []permission.Request{request}, review, input)
}

func safePrefixCommand(command string, prefix []string) bool {
	if len(prefix) == 0 || containsShellSyntax(command) {
		return false
	}
	words := strings.Fields(command)
	if len(words) < len(prefix) {
		return false
	}
	for i := range prefix {
		if prefix[i] == "" || strings.ContainsAny(prefix[i], " \t") || containsShellSyntax(prefix[i]) || words[i] != prefix[i] {
			return false
		}
	}
	return true
}

func containsShellSyntax(value string) bool {
	return strings.ContainsAny(value, "\"'\\$;&|<>()[]{}*?!~#\r\n") || strings.ContainsRune(value, '`')
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
		Shell: input.ResolvedShell, Command: input.Command, Cwd: input.ResolvedWorkdir,
		SessionID: call.SessionID, Yield: time.Duration(input.YieldTimeMS) * time.Millisecond,
		MaxOutputTokens: input.MaxOutputTokens, TTY: input.TTY, Output: call.Output,
		Unrestricted: input.SandboxPermissions == "require_escalated",
	})
	if err != nil {
		return Result{}, err
	}
	return Result{Text: formatPersistentResult(result)}, nil
}

func formatPersistentResult(result process.PersistentResult) string {
	text := fmt.Sprintf("Chunk ID: %s\nWall time: %.4f seconds", result.ChunkID, result.WallTime.Seconds())
	if result.ExitCode != nil {
		text += fmt.Sprintf("\nProcess exited with code %d", *result.ExitCode)
	}
	if result.ProcessID != nil {
		text += fmt.Sprintf("\nProcess running with session ID %d", *result.ProcessID)
	}
	text += fmt.Sprintf("\nOriginal token count: %d\nOutput:\n%s", result.OriginalTokenCount, result.Output)
	return text
}
