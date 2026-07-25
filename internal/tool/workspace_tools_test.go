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
	"github.com/amirulashraf/parrot-coder/internal/security"
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

type patchSecurityProfile struct {
	readOnly bool
	rules    []security.Rule
}

func (p patchSecurityProfile) IsReadOnly() bool       { return p.readOnly }
func (p patchSecurityProfile) Rules() []security.Rule { return p.rules }

func TestApplyPatchHonorsOrderedSecurityRules(t *testing.T) {
	ctx, ws, changes := workspaceToolHarness(t)
	tool := NewApplyPatchTool(changes)
	workspaceFile := filepath.Join(ws.Root(), "workspace")
	externalDirectory := t.TempDir()
	externalFile := filepath.Join(externalDirectory, "plan.md")
	sibling := filepath.Join(externalDirectory, "sibling.md")
	for _, path := range []string{workspaceFile, externalFile, sibling} {
		if err := os.WriteFile(path, []byte("old\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	patch := func(paths ...string) json.RawMessage {
		var text strings.Builder
		for _, path := range paths {
			text.WriteString(path + "\n<<<<<<< SEARCH\nold\n=======\nnew\n>>>>>>> REPLACE\n")
		}
		encoded, err := json.Marshal(map[string]string{"patchText": text.String()})
		if err != nil {
			t.Fatal(err)
		}
		return encoded
	}
	read := func(path string) string {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}

	allowed := patchSecurityProfile{readOnly: true, rules: []security.Rule{{Path: externalFile, Action: security.ActionAllowWrite}}}
	planned, err := tool.Plan(ctx, patch(externalFile), CallContext{Workspace: ws, SecurityProfile: allowed})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tool.Execute(ctx, planned, CallContext{Workspace: ws, SecurityProfile: allowed}); err != nil {
		t.Fatal(err)
	}
	if got := read(externalFile); got != "new\n" {
		t.Fatalf("allowed external file = %q", got)
	}
	if err := os.WriteFile(externalFile, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := tool.Plan(ctx, patch(sibling), CallContext{Workspace: ws, SecurityProfile: allowed}); !errors.Is(err, workspace.ErrOutsideRoot) {
		t.Fatalf("sibling plan error = %v, want ErrOutsideRoot", err)
	}
	planned, err = tool.Plan(ctx, patch(externalFile, workspaceFile), CallContext{Workspace: ws, SecurityProfile: allowed})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tool.Execute(ctx, planned, CallContext{Workspace: ws, SecurityProfile: allowed}); err == nil {
		t.Fatal("mixed authorized and unauthorized patch succeeded")
	}
	if got := read(externalFile); got != "old\n" {
		t.Fatalf("authorized file changed before whole-patch rejection: %q", got)
	}
	if got := read(workspaceFile); got != "old\n" {
		t.Fatalf("workspace file changed before whole-patch rejection: %q", got)
	}

	denied := patchSecurityProfile{readOnly: true, rules: []security.Rule{
		{Path: externalDirectory, Action: security.ActionAllowWrite},
		{Path: externalFile, Action: security.ActionDenyWrite},
	}}
	planned, err = tool.Plan(ctx, patch(externalFile), CallContext{Workspace: ws, SecurityProfile: denied})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tool.Execute(ctx, planned, CallContext{Workspace: ws, SecurityProfile: denied}); err == nil {
		t.Fatal("later deny_write rule was ignored")
	}

	writable := patchSecurityProfile{}
	planned, err = tool.Plan(ctx, patch("workspace"), CallContext{Workspace: ws, SecurityProfile: writable})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tool.Execute(ctx, planned, CallContext{Workspace: ws, SecurityProfile: writable}); err != nil {
		t.Fatal(err)
	}
	if got := read(workspaceFile); got != "new\n" {
		t.Fatalf("writable profile workspace file = %q", got)
	}
}

func TestApplyPatchReportsProblematicSearchBlock(t *testing.T) {
	ctx, ws, changes := workspaceToolHarness(t)
	if err := os.WriteFile(filepath.Join(ws.Root(), "file"), []byte("existing\ncontent\nexisting\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		search string
		want   string
	}{
		{name: "missing", search: "missing\nmatch", want: "failed to find this SEARCH block:\n<<<<<<< SEARCH\nmissing\nmatch\n======="},
		{name: "ambiguous", search: "existing", want: "this SEARCH block matched 2 locations; include more surrounding lines:\n<<<<<<< SEARCH\nexisting\n======="},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			patchText := "file\n<<<<<<< SEARCH\n" + tc.search + "\n=======\nreplacement\n>>>>>>> REPLACE\n"
			raw, err := json.Marshal(map[string]string{"patchText": patchText})
			if err != nil {
				t.Fatal(err)
			}
			_, err = NewApplyPatchTool(changes).Plan(ctx, raw, CallContext{Workspace: ws})
			if !errors.Is(err, change.ErrConflict) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want ErrConflict identifying the problematic SEARCH block", err)
			}
		})
	}
}

func TestApplyPatchPlanReportsAllConflictBlocks(t *testing.T) {
	ctx, ws, changes := workspaceToolHarness(t)
	if err := os.WriteFile(filepath.Join(ws.Root(), "file"), []byte("existing\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	searches := []string{"missing first", "missing second", "missing third"}
	patchText := ""
	for i, search := range searches {
		path := ""
		if i == 0 {
			path = "file"
		}
		patchText += changeAiderBlock(path, search, "replacement")
	}
	raw, err := json.Marshal(map[string]string{"patchText": patchText})
	if err != nil {
		t.Fatal(err)
	}

	plan, err := NewApplyPatchTool(changes).Plan(ctx, raw, CallContext{Workspace: ws})
	if !errors.Is(err, change.ErrConflict) {
		t.Fatalf("error = %v, want ErrConflict", err)
	}
	if plan.ToolID != "" || len(plan.CanonicalInput) != 0 || plan.Data != nil || len(plan.Permissions) != 0 || len(plan.Review) != 0 {
		t.Fatalf("plan = %#v, want zero Plan", plan)
	}
	position := -1
	for _, search := range searches {
		next := strings.Index(err.Error(), search)
		if next <= position {
			t.Fatalf("error = %q, want conflict block %q after byte %d", err, search, position)
		}
		position = next
	}
}

func changeAiderBlock(path, search, replace string) string {
	if path != "" {
		path += "\n"
	}
	return path + "<<<<<<< SEARCH\n" + search + "\n=======\n" + replace + "\n>>>>>>> REPLACE\n"
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
