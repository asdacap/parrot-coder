package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

const maxGitDiffBytes = 4 << 20

// GitDiffTool exposes only fixed, read-only Git operations. Review workers use
// it to inspect changes without receiving the general shell tool.
type GitDiffTool struct{}

func NewGitDiffTool() Tool        { return &GitDiffTool{} }
func (t *GitDiffTool) ID() string { return "git_diff" }
func (t *GitDiffTool) Description() string {
	return "Read a bounded Git diff for uncommitted changes, a base branch, or a commit. Uncommitted output also lists untracked paths."
}
func (t *GitDiffTool) DescribeRequest(raw json.RawMessage) (string, error) {
	var input gitDiffInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return "", err
	}
	if input.Target == "" {
		input.Target = "uncommitted"
	}
	return "Read Git diff for " + input.Target, nil
}
func (t *GitDiffTool) JSONSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"target":{"type":"string","enum":["uncommitted","base","commit"]},"ref":{"type":"string","description":"Required branch or commit when target is base or commit."}},"additionalProperties":false}`)
}

type gitDiffInput struct {
	Target string `json:"target"`
	Ref    string `json:"ref"`
}

func (t *GitDiffTool) Plan(_ context.Context, raw json.RawMessage, call CallContext) (Plan, error) {
	if call.Workspace == nil {
		return Plan{}, errors.New("git_diff: workspace is required")
	}
	var input gitDiffInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return Plan{}, err
	}
	if input.Target == "" {
		input.Target = "uncommitted"
	}
	if input.Target != "uncommitted" && input.Target != "base" && input.Target != "commit" {
		return Plan{}, errors.New("git_diff: target must be uncommitted, base, or commit")
	}
	input.Ref = strings.TrimSpace(input.Ref)
	if input.Target == "uncommitted" {
		if input.Ref != "" {
			return Plan{}, errors.New("git_diff: ref is not valid for uncommitted changes")
		}
	} else if !validGitRef(input.Ref) {
		return Plan{}, errors.New("git_diff: a valid ref is required for base or commit")
	}
	return NewPlan(t.ID(), raw, nil, nil, input)
}

func (t *GitDiffTool) Execute(ctx context.Context, plan Plan, call CallContext) (Result, error) {
	input, ok := plan.Data.(gitDiffInput)
	if !ok || call.Workspace == nil {
		return Result{}, errors.New("git_diff: incompatible plan or missing workspace")
	}
	root := call.Workspace.Root()
	var output string
	var truncated bool
	var err error
	switch input.Target {
	case "uncommitted":
		// Read index and worktree diffs separately so unborn repositories do not
		// fail merely because HEAD does not exist.
		output, truncated, err = runBoundedGit(ctx, root, "diff", "--cached", "--no-ext-diff", "--no-textconv", "--find-renames", "--")
		if err == nil {
			var unstaged string
			var unstagedTruncated bool
			unstaged, unstagedTruncated, err = runBoundedGit(ctx, root, "diff", "--no-ext-diff", "--no-textconv", "--find-renames", "--")
			truncated = truncated || unstagedTruncated
			if unstaged != "" {
				output += "\n" + unstaged
			}
		}
		if err == nil {
			var status string
			var statusTruncated bool
			status, statusTruncated, err = runBoundedGit(ctx, root, "status", "--short", "--untracked-files=all")
			truncated = truncated || statusTruncated
			if err == nil && status != "" {
				output += "\n\nGit status (including untracked paths):\n" + status
			}
		}
	case "base":
		var base string
		base, truncated, err = runBoundedGit(ctx, root, "merge-base", "HEAD", input.Ref)
		if err == nil {
			base = strings.TrimSpace(base)
			var diffTruncated bool
			output, diffTruncated, err = runBoundedGit(ctx, root, "diff", "--no-ext-diff", "--no-textconv", "--find-renames", base, "--")
			truncated = truncated || diffTruncated
		}
	case "commit":
		output, truncated, err = runBoundedGit(ctx, root, "show", "--format=fuller", "--no-ext-diff", "--no-textconv", "--find-renames", input.Ref, "--")
	}
	if err != nil {
		return Result{}, err
	}
	if strings.TrimSpace(output) == "" {
		output = "No changes found."
	}
	if truncated {
		output += "\n\n[git_diff output truncated; narrow the review target before drawing conclusions.]"
	}
	return Result{Text: output, ModelText: output, Metadata: map[string]any{"target": input.Target, "ref": input.Ref, "truncated": truncated}}, nil
}

func validGitRef(ref string) bool {
	if ref == "" || strings.HasPrefix(ref, "-") {
		return false
	}
	for _, r := range ref {
		if r < 0x20 || r == 0x7f || r == ' ' {
			return false
		}
	}
	return true
}

func runBoundedGit(ctx context.Context, root string, args ...string) (string, bool, error) {
	// Disable optional writes and repository-configured helper execution. In
	// particular, core.fsmonitor and diff textconv/external drivers must not turn
	// a read-only reviewer operation into arbitrary process execution.
	gitArgs := []string{"--no-pager", "--no-optional-locks", "-c", "core.fsmonitor=false", "-c", "diff.external="}
	gitArgs = append(gitArgs, args...)
	command := exec.CommandContext(ctx, "git", gitArgs...)
	command.Dir = root
	var stdout strings.Builder
	var stderr strings.Builder
	stdoutWriter := &limitedStringWriter{builder: &stdout, remaining: maxGitDiffBytes}
	stderrWriter := &limitedStringWriter{builder: &stderr, remaining: 64 << 10}
	command.Stdout = stdoutWriter
	command.Stderr = stderrWriter
	if err := command.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if stderrWriter.truncated {
			message += " [stderr truncated]"
		}
		if message == "" {
			message = err.Error()
		}
		return "", false, fmt.Errorf("git_diff: git %s: %s", args[0], message)
	}
	return stdout.String(), stdoutWriter.truncated, nil
}

type limitedStringWriter struct {
	builder   *strings.Builder
	remaining int
	truncated bool
}

func (w *limitedStringWriter) Write(p []byte) (int, error) {
	original := len(p)
	if len(p) > w.remaining {
		p = p[:w.remaining]
		w.truncated = true
	}
	_, _ = w.builder.Write(p)
	w.remaining -= len(p)
	return original, nil
}
