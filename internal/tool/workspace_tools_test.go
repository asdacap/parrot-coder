package tool

import (
	"context"
	"encoding/json"
	"errors"
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

func TestReadReturnsSHA256OnlyForFiles(t *testing.T) {
	ctx, ws, _ := workspaceToolHarness(t)
	before := []byte("line one\nline two\n")
	if err := os.WriteFile(filepath.Join(ws.Root(), "file"), before, 0o600); err != nil {
		t.Fatal(err)
	}
	read := NewReadTool(ReadConfig{})
	partial, err := read.Plan(ctx, json.RawMessage(`{"path":"file","limit":1}`), CallContext{Workspace: ws})
	if err != nil {
		t.Fatal(err)
	}
	result, err := read.Execute(ctx, partial, CallContext{Workspace: ws})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(result.Text, "sha256: "+change.SHA256(before)+"\n") {
		t.Fatalf("partial read lacks whole-file sha256: %q", result.Text)
	}

	listingPlan, err := read.Plan(ctx, json.RawMessage(`{"path":"."}`), CallContext{Workspace: ws})
	if err != nil {
		t.Fatal(err)
	}
	listing, err := read.Execute(ctx, listingPlan, CallContext{Workspace: ws})
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
