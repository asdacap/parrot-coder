package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/amirulashraf/parrot-coder/internal/change"
	"github.com/amirulashraf/parrot-coder/internal/permission"
	"github.com/amirulashraf/parrot-coder/internal/question"
	"github.com/amirulashraf/parrot-coder/internal/workspace"
)

type recordingAuthorizer struct {
	mu      sync.Mutex
	request permission.Request
	calls   int
	before  func()
}

func (a *recordingAuthorizer) Authorize(_ context.Context, request permission.Request) (permission.Decision, error) {
	a.mu.Lock()
	a.request = request
	a.calls++
	before := a.before
	a.mu.Unlock()
	if before != nil {
		before()
	}
	return permission.Allow, nil
}

func workspaceToolHarness(t *testing.T) (context.Context, *workspace.Workspace, *change.Service) {
	t.Helper()
	ctx := context.Background()
	ws, err := workspace.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return ctx, ws, change.NewService(change.Config{})
}

func TestEditReviewHashAndCommitIntegration(t *testing.T) {
	ctx, ws, changes := workspaceToolHarness(t)
	path := filepath.Join(ws.Root(), "file")
	before := []byte("before\n")
	if err := os.WriteFile(path, before, 0o600); err != nil {
		t.Fatal(err)
	}
	raw := json.RawMessage(`{"path":"file","expected_sha256":"` + change.SHA256(before) + `","new":"after"}`)
	edit := NewEditTool(changes)
	planned, err := edit.Plan(ctx, raw, CallContext{Workspace: ws})
	if err != nil {
		t.Fatal(err)
	}
	// A workspace edit is confined by the sandbox, so it is never prompted; the
	// review still records the exact diff and preimage hash.
	if len(planned.Permissions) != 0 {
		t.Fatalf("edit requested approval: %#v", planned.Permissions)
	}
	if !strings.Contains(string(planned.Review), `"diff"`) || !strings.Contains(string(planned.Review), change.SHA256(before)) {
		t.Fatalf("review lacks exact diff/hash: %s", planned.Review)
	}

	registry := NewRegistry()
	if err := registry.Register(edit); err != nil {
		t.Fatal(err)
	}
	authorizer := &recordingAuthorizer{}
	executor := Executor{Snapshot: registry.Materialize(), Permissions: authorizer}
	result, err := executor.Execute(ctx, "edit", raw, CallContext{Workspace: ws})
	if err != nil {
		t.Fatal(err)
	}
	if result.Metadata["files"] != 1 || authorizer.calls != 0 {
		t.Fatalf("result/authorizations = %#v / %d", result, authorizer.calls)
	}
	if !strings.HasSuffix(result.Text, "sha256: "+change.SHA256([]byte("after"))+"\n") {
		t.Fatalf("edit result lacks after sha256: %q", result.Text)
	}
	if described, err := edit.DescribeRequest(planned.CanonicalInput); err != nil || described != `Edit workspace file "file"` {
		t.Fatalf("describe = %q, %v", described, err)
	}
	if !strings.Contains(result.Text, "--- a/file") || !strings.Contains(result.Text, "+++ b/file") ||
		!strings.Contains(result.Text, "-before") || !strings.Contains(result.Text, "+after") {
		t.Fatalf("edit result lacks before/after diff: %q", result.Text)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "after" {
		t.Fatalf("committed file = %q", data)
	}
}

func TestEditRevalidatesBeforeMutating(t *testing.T) {
	ctx, ws, changes := workspaceToolHarness(t)
	path := filepath.Join(ws.Root(), "file")
	before := []byte("before")
	if err := os.WriteFile(path, before, 0o600); err != nil {
		t.Fatal(err)
	}
	raw := json.RawMessage(`{"path":"file","expected_sha256":"` + change.SHA256(before) + `","new":"after"}`)
	edit := NewEditTool(changes)
	planned, err := edit.Plan(ctx, raw, CallContext{Workspace: ws})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("changed after planning"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := edit.Execute(ctx, planned, CallContext{Workspace: ws}); !errors.Is(err, change.ErrStale) {
		t.Fatalf("stale execution error = %v", err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "changed after planning" {
		t.Fatalf("stale execution overwrote file: %q", data)
	}
}

func TestReadReturnsSHA256ForFilesAndRoundTripsIntoEdit(t *testing.T) {
	ctx, ws, changes := workspaceToolHarness(t)
	before := []byte("line one\nline two\n")
	if err := os.WriteFile(filepath.Join(ws.Root(), "file"), before, 0o600); err != nil {
		t.Fatal(err)
	}
	registry := NewRegistry()
	for _, tool := range []Tool{NewReadTool(ReadConfig{}), NewEditTool(changes)} {
		if err := registry.Register(tool); err != nil {
			t.Fatal(err)
		}
	}
	executor := Executor{Snapshot: registry.Materialize(), Permissions: &recordingAuthorizer{}}

	partial, err := executor.Execute(ctx, "read", json.RawMessage(`{"path":"file","limit":1}`), CallContext{Workspace: ws})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(partial.Text, "sha256: "+change.SHA256(before)+"\n") {
		t.Fatalf("partial read lacks whole-file sha256: %q", partial.Text)
	}

	raw := json.RawMessage(`{"path":"file","expected_sha256":"` + change.SHA256(before) + `","new":"after"}`)
	if _, err := executor.Execute(ctx, "edit", raw, CallContext{Workspace: ws}); err != nil {
		t.Fatalf("read-hash edit round trip: %v", err)
	}

	listing, err := executor.Execute(ctx, "read", json.RawMessage(`{"path":"."}`), CallContext{Workspace: ws})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(listing.Text, "sha256:") {
		t.Fatalf("directory listing must not include sha256: %q", listing.Text)
	}
}

func TestQuestionToolBlocksForReply(t *testing.T) {
	broker := question.NewBroker(nil)
	registry := NewRegistry()
	if err := registry.Register(NewQuestionTool(broker)); err != nil {
		t.Fatal(err)
	}
	executor := Executor{Snapshot: registry.Materialize()}
	raw := json.RawMessage(`{"questions":[{"id":"q","prompt":"Choose","options":[{"id":"a","label":"A"}]}]}`)
	done := make(chan error, 1)
	go func() {
		result, err := executor.Execute(context.Background(), "question", raw, CallContext{SessionID: "session"})
		if err == nil && !strings.Contains(result.Text, `"a"`) {
			err = errors.New("answer missing from result")
		}
		done <- err
	}()
	deadline := time.Now().Add(time.Second)
	for len(broker.Pending()) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	pending := broker.Pending()
	if len(pending) != 1 {
		t.Fatal("question tool did not block")
	}
	if err := broker.Reply(pending[0].ID, question.Response{Answers: []question.Answer{{QuestionID: "q", OptionIDs: []string{"a"}}}}); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestWritePermissionDescriptionUsesCanonicalPath(t *testing.T) {
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(target, alias); err != nil {
		t.Fatal(err)
	}
	description, err := NewWritePermissionTool(nil).DescribeRequest(json.RawMessage(`{"path":"` + alias + `"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(description, target) || strings.Contains(description, alias) {
		t.Fatalf("description = %q", description)
	}
}

func TestWritePermissionRejectsWorkspacePath(t *testing.T) {
	_, ws, _ := workspaceToolHarness(t)
	path := filepath.Join(ws.Root(), ".git")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := NewWritePermissionTool(nil).Plan(context.Background(), json.RawMessage(`{"path":"`+path+`"}`), CallContext{Workspace: ws, SessionID: "session"})
	if err == nil || !strings.Contains(err.Error(), "workspace paths are already writable") {
		t.Fatalf("error = %v", err)
	}
}

func TestUnrestrictedShellRequiresPermission(t *testing.T) {
	_, ws, _ := workspaceToolHarness(t)
	planned, err := NewUnrestrictedShellTool(nil).Plan(context.Background(), json.RawMessage(`{"shell":"/bin/sh","command":"true"}`), CallContext{Workspace: ws})
	if err != nil {
		t.Fatal(err)
	}
	if len(planned.Permissions) != 1 || planned.Permissions[0].ToolID != "unrestricted_shell" {
		t.Fatalf("permission = %#v", planned.Permissions)
	}
}

func TestShellReviewOmitsEnvironmentValues(t *testing.T) {
	_, ws, _ := workspaceToolHarness(t)
	raw := json.RawMessage(`{"shell":"/bin/sh","command":"printf ok","env":{"API_TOKEN":"top-secret"}}`)
	tool := NewShellTool(nil)
	planned, err := tool.Plan(context.Background(), raw, CallContext{Workspace: ws})
	if err != nil {
		t.Fatal(err)
	}
	review := string(planned.Review)
	if strings.Contains(review, "top-secret") || !strings.Contains(review, "API_TOKEN") || !strings.Contains(review, "inside the OS sandbox") {
		t.Fatalf("unsafe or incomplete review: %s", review)
	}
	// The sandboxed variant is confined, so it plans no approval.
	if len(planned.Permissions) != 0 {
		t.Fatalf("sandboxed shell requested approval: %#v", planned.Permissions)
	}
	description, err := tool.DescribeRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(description, "top-secret") || !strings.Contains(description, "printf ok") || !strings.Contains(description, "API_TOKEN") || !strings.Contains(description, "Run shell command") {
		t.Fatalf("unsafe or incomplete request description: %q", description)
	}
	unrestrictedDescription, err := NewUnrestrictedShellTool(nil).DescribeRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(unrestrictedDescription, "without the OS sandbox") || !strings.Contains(unrestrictedDescription, "full local authority") {
		t.Fatalf("unrestricted description = %q", unrestrictedDescription)
	}
}

func TestShellDefaultsShellAndWorkingDirectory(t *testing.T) {
	_, ws, _ := workspaceToolHarness(t)
	t.Setenv("SHELL", "/bin/sh")
	planned, err := NewShellTool(nil).Plan(context.Background(), json.RawMessage(`{"command":"printf ok"}`), CallContext{Workspace: ws})
	if err != nil {
		t.Fatal(err)
	}
	input := planned.Data.(shellInput)
	if input.Shell != "/bin/sh" || input.Cwd != ws.Root() || input.ResolvedCwd != ws.Root() {
		t.Fatalf("defaults = %#v", input)
	}
	schema := string(NewShellTool(nil).JSONSchema())
	if strings.Contains(schema, `"required":["shell"`) || !strings.Contains(schema, `"required":["command"]`) {
		t.Fatalf("shell should be optional in schema: %s", schema)
	}
}

func TestShellAllowsExternalWorkingDirectory(t *testing.T) {
	_, ws, _ := workspaceToolHarness(t)
	external := t.TempDir()
	external, err := filepath.EvalSymlinks(external)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range []*ShellTool{NewShellTool(nil), NewUnrestrictedShellTool(nil)} {
		planned, err := tool.Plan(context.Background(), json.RawMessage(fmt.Sprintf(`{"command":"pwd","cwd":%q}`, external)), CallContext{Workspace: ws})
		if err != nil {
			t.Fatalf("%s: %v", tool.ID(), err)
		}
		input := planned.Data.(shellInput)
		if input.Cwd != external || input.ResolvedCwd != external {
			t.Fatalf("%s cwd = %q, resolved = %q", tool.ID(), input.Cwd, input.ResolvedCwd)
		}
	}
}

func TestApplyPatchUsesOpenCodePatchTextParameter(t *testing.T) {
	_, ws, changes := workspaceToolHarness(t)
	tool := NewApplyPatchTool(changes)
	schema := string(tool.JSONSchema())
	if !strings.Contains(schema, `"required":["patchText"]`) || strings.Contains(schema, `"required":["patch"]`) {
		t.Fatalf("apply_patch schema is not OpenCode-compatible: %s", schema)
	}
	raw := json.RawMessage(`{"patchText":"file\n<<<<<<< SEARCH\n=======\ncontent\n>>>>>>> REPLACE\n"}`)
	planned, err := tool.Plan(context.Background(), raw, CallContext{Workspace: ws})
	if err != nil {
		t.Fatal(err)
	}
	if len(planned.Permissions) != 0 {
		t.Fatalf("plan = %#v", planned)
	}
}

func TestApplyPatchReportsMissingSearchMatch(t *testing.T) {
	ctx, ws, changes := workspaceToolHarness(t)
	if err := os.WriteFile(filepath.Join(ws.Root(), "file"), []byte("existing\ncontent\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	raw := json.RawMessage(`{"patchText":"file\n<<<<<<< SEARCH\nmissing\nmatch\n=======\nreplacement\n>>>>>>> REPLACE\n"}`)
	_, err := NewApplyPatchTool(changes).Plan(ctx, raw, CallContext{Workspace: ws})
	if !errors.Is(err, change.ErrConflict) || !strings.Contains(err.Error(), `"missing\nmatch"`) {
		t.Fatalf("error = %v, want ErrConflict identifying the missing match", err)
	}
}

func TestApplyPatchFormatParameter(t *testing.T) {
	_, ws, changes := workspaceToolHarness(t)
	tool := NewApplyPatchTool(changes)
	if schema := string(tool.JSONSchema()); !strings.Contains(schema, `"enum":["aider","unified"]`) {
		t.Fatalf("apply_patch schema is missing the format enum: %s", schema)
	}
	unified := json.RawMessage(`{"patchText":"--- /dev/null\n+++ b/created\n@@ -0,0 +1 @@\n+content\n","format":"unified"}`)
	planned, err := tool.Plan(context.Background(), unified, CallContext{Workspace: ws})
	if err != nil {
		t.Fatal(err)
	}
	if len(planned.Permissions) != 0 || !strings.Contains(string(planned.Review), "created") {
		t.Fatalf("plan = %#v", planned)
	}
	if described, err := tool.DescribeRequest(unified); err != nil || !strings.Contains(described, "unified diff") {
		t.Fatalf("describe = %q, %v", described, err)
	}
	if _, err := tool.Plan(context.Background(), json.RawMessage(`{"patchText":"x","format":"bogus"}`), CallContext{Workspace: ws}); err == nil {
		t.Fatal("unknown format accepted")
	}
}

func TestTodoToolSchemaUsesOpenCodeEnums(t *testing.T) {
	schema := string(NewTodoWriteTool(nil).JSONSchema())
	for _, expected := range []string{`"enum":["pending","in_progress","completed","cancelled"]`, `"enum":["high","medium","low"]`} {
		if !strings.Contains(schema, expected) {
			t.Fatalf("todo schema missing %s: %s", expected, schema)
		}
	}
}
