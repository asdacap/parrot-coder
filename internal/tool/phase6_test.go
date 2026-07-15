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
	"github.com/amirulashraf/parrot-coder/internal/event"
	"github.com/amirulashraf/parrot-coder/internal/permission"
	"github.com/amirulashraf/parrot-coder/internal/question"
	"github.com/amirulashraf/parrot-coder/internal/session"
	"github.com/amirulashraf/parrot-coder/internal/snapshot"
	"github.com/amirulashraf/parrot-coder/internal/store"
	"github.com/amirulashraf/parrot-coder/internal/workspace"
)

type recordingAuthorizer struct {
	mu      sync.Mutex
	request permission.Request
	before  func()
}

func (a *recordingAuthorizer) Authorize(_ context.Context, request permission.Request) (permission.Decision, error) {
	a.mu.Lock()
	a.request = request
	before := a.before
	a.mu.Unlock()
	if before != nil {
		before()
	}
	return permission.Allow, nil
}

func phase6Harness(t *testing.T) (context.Context, *workspace.Workspace, *change.Service, *snapshot.Service, string) {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "parrot.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	sessions := session.NewService(db, event.NewRepository(db))
	created, err := sessions.Create(ctx, session.CreateParams{Title: "tools"})
	if err != nil {
		t.Fatal(err)
	}
	ws, err := workspace.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return ctx, ws, change.NewService(change.Config{}), snapshot.NewService(db, snapshot.Config{}), created.ID
}

func TestEditPermissionReviewHashAndSnapshotIntegration(t *testing.T) {
	ctx, ws, changes, snapshots, sessionID := phase6Harness(t)
	path := filepath.Join(ws.Root(), "file")
	before := []byte("before\n")
	if err := os.WriteFile(path, before, 0o600); err != nil {
		t.Fatal(err)
	}
	raw := json.RawMessage(`{"path":"file","expected_sha256":"` + change.SHA256(before) + `","old":"before","new":"after"}`)
	edit := NewEditTool(changes, snapshots)
	planned, err := edit.Plan(ctx, raw, CallContext{Workspace: ws, SessionID: sessionID})
	if err != nil {
		t.Fatal(err)
	}
	if len(planned.Permissions) != 1 || planned.Permissions[0].Verify() != nil || planned.OperationHash == "" {
		t.Fatalf("invalid permission-bound plan: %#v", planned)
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
	result, err := executor.Execute(ctx, "edit", raw, CallContext{Workspace: ws, SessionID: sessionID})
	if err != nil {
		t.Fatal(err)
	}
	if result.Metadata["transaction_id"] == "" || authorizer.request.OperationHash == "" {
		t.Fatalf("result/request = %#v / %#v", result, authorizer.request)
	}
	if !strings.Contains(result.Text, "--- a/file") || !strings.Contains(result.Text, "+++ b/file") ||
		!strings.Contains(result.Text, "-before") || !strings.Contains(result.Text, "+after") {
		t.Fatalf("edit result lacks before/after diff: %q", result.Text)
	}
	if _, err := snapshots.Undo(ctx, ws, sessionID); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != string(before) {
		t.Fatalf("undo = %q", data)
	}
}

func TestEditRevalidatesAfterPermission(t *testing.T) {
	ctx, ws, changes, snapshots, sessionID := phase6Harness(t)
	path := filepath.Join(ws.Root(), "file")
	before := []byte("before")
	if err := os.WriteFile(path, before, 0o600); err != nil {
		t.Fatal(err)
	}
	raw := json.RawMessage(`{"path":"file","expected_sha256":"` + change.SHA256(before) + `","old":"before","new":"after"}`)
	registry := NewRegistry()
	_ = registry.Register(NewEditTool(changes, snapshots))
	authorizer := &recordingAuthorizer{before: func() { _ = os.WriteFile(path, []byte("changed while asking"), 0o600) }}
	executor := Executor{Snapshot: registry.Materialize(), Permissions: authorizer}
	if _, err := executor.Execute(ctx, "edit", raw, CallContext{Workspace: ws, SessionID: sessionID}); !errors.Is(err, change.ErrStale) {
		t.Fatalf("stale execution error = %v", err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "changed while asking" {
		t.Fatalf("stale execution overwrote file: %q", data)
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

func TestShellReviewBindsCanonicalResourcesWithoutEnvironmentValues(t *testing.T) {
	_, ws, _, _, _ := phase6Harness(t)
	raw := json.RawMessage(`{"shell":"/bin/sh","command":"printf ok","env":{"API_TOKEN":"top-secret"}}`)
	planned, err := NewShellTool(nil).Plan(context.Background(), raw, CallContext{Workspace: ws})
	if err != nil {
		t.Fatal(err)
	}
	review := string(planned.Review)
	if strings.Contains(review, "top-secret") || !strings.Contains(review, "API_TOKEN") || !strings.Contains(review, "arbitrary process execution") {
		t.Fatalf("unsafe or incomplete review: %s", review)
	}
	if len(planned.Permissions) != 1 || planned.Permissions[0].Resources[0].Identifier == "" || planned.Permissions[0].Resources[0].Attributes["command_sha256"] == "" {
		t.Fatalf("shell resources = %#v", planned.Permissions)
	}
}

func TestShellDefaultsShellAndWorkingDirectory(t *testing.T) {
	_, ws, _, _, _ := phase6Harness(t)
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

func TestRegisterPhase6Definitions(t *testing.T) {
	registry := NewRegistry()
	if err := RegisterPhase6(registry, Phase6Services{}); err != nil {
		t.Fatal(err)
	}
	definitions := registry.Definitions()
	want := []string{"apply_patch", "edit", "question", "shell", "todoread", "todowrite"}
	if len(definitions) != len(want) {
		t.Fatalf("definitions = %#v", definitions)
	}
	for i := range want {
		if definitions[i].ID != want[i] {
			t.Fatalf("definition[%d] = %q", i, definitions[i].ID)
		}
	}
}

func TestApplyPatchUsesOpenCodePatchTextParameter(t *testing.T) {
	_, ws, changes, _, _ := phase6Harness(t)
	tool := NewApplyPatchTool(changes, nil)
	schema := string(tool.JSONSchema())
	if !strings.Contains(schema, `"required":["patchText"]`) || strings.Contains(schema, `"required":["patch"]`) {
		t.Fatalf("apply_patch schema is not OpenCode-compatible: %s", schema)
	}
	raw := json.RawMessage(`{"patchText":"*** Begin Patch\n*** Add File: file\n+content\n*** End Patch"}`)
	planned, err := tool.Plan(context.Background(), raw, CallContext{Workspace: ws})
	if err != nil {
		t.Fatal(err)
	}
	if len(planned.Permissions) != 1 {
		t.Fatalf("plan = %#v", planned)
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
