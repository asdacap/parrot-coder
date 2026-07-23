package tool

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/amirulashraf/parrot-coder/internal/mcp"
	"github.com/amirulashraf/parrot-coder/internal/skill"
	"github.com/amirulashraf/parrot-coder/internal/webfetch"
	"github.com/amirulashraf/parrot-coder/internal/workspace"
)

func TestSkillToolLoadsExactBodyAndMetadata(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, ".parrot", "skills", "review", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	body := "Use exact review instructions.\n"
	if err := os.WriteFile(path, []byte("---\nname: review\ndescription: Review changes\nagent: explorer\nmodel: local/model\nallowed-tools: [read, grep]\n---\n"+body), 0o600); err != nil {
		t.Fatal(err)
	}
	registry, err := skill.Discover(skill.Options{ProjectRoot: root, CWD: root})
	if err != nil {
		t.Fatal(err)
	}
	item := NewSkillTool(registry)
	plan, err := item.Plan(context.Background(), json.RawMessage(`{"name":"review"}`), CallContext{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := item.Execute(context.Background(), plan, CallContext{})
	if err != nil || result.Text != body || result.Metadata["agent"] != "explorer" || !strings.Contains(item.Description(), "review") {
		t.Fatalf("result = %#v, error = %v", result, err)
	}
}

type fakeMCPCaller struct {
	arguments json.RawMessage
	result    mcp.ToolResult
	err       error
}

func (f *fakeMCPCaller) CallTool(_ context.Context, _ string, arguments json.RawMessage) (mcp.ToolResult, error) {
	f.arguments = append(json.RawMessage(nil), arguments...)
	return f.result, f.err
}

func TestMCPToolArgumentsAndApplicationError(t *testing.T) {
	caller := &fakeMCPCaller{result: mcp.ToolResult{Content: []mcp.Content{{Type: "text", Text: "ok"}}, StructuredContent: json.RawMessage(`{"value":1}`)}}
	item, err := NewMCPTool(caller, mcp.ToolDefinition{Name: "mcp_fixture_echo", Server: "fixture", Tool: "echo", Description: "Echo", InputSchema: json.RawMessage(`{"type":"object","properties":{"value":{"type":"string","description":"value"},"labels":{"type":"object","additionalProperties":{"type":"string"}}},"required":["value"]}`)})
	if err != nil {
		t.Fatal(err)
	}
	registry := NewRegistry()
	if err := registry.Register(item); err != nil {
		t.Fatal(err)
	}
	authorizer := &recordingAuthorizer{}
	result, err := (Executor{Snapshot: registry.Materialize(), Permissions: authorizer}).Execute(context.Background(), item.ID(), json.RawMessage(`{"value":"ok"}`), CallContext{})
	if err != nil {
		t.Fatal(err)
	}
	// MCP calls are not sandbox-escaping, so they are never authorized.
	if string(caller.arguments) != `{"value":"ok"}` || result.Text != "ok" || authorizer.calls != 0 {
		t.Fatalf("call/result/authorizations = %s / %#v / %d", caller.arguments, result, authorizer.calls)
	}
	caller.err = &mcp.ApplicationError{Server: "fixture", Tool: "echo", Result: mcp.ToolResult{IsError: true}}
	registry = NewRegistry()
	_ = registry.Register(item)
	_, err = (Executor{Snapshot: registry.Materialize(), Permissions: authorizer}).Execute(context.Background(), item.ID(), json.RawMessage(`{"value":"bad"}`), CallContext{})
	var applicationErr *mcp.ApplicationError
	if !errors.As(err, &applicationErr) {
		t.Fatalf("application error = %v", err)
	}
}

func TestWebFetchToolFetchAndRevalidation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "fetched")
	}))
	defer server.Close()
	item := NewWebFetchTool(webfetch.New(webfetch.Config{AllowPrivate: true}))
	registry := NewRegistry()
	_ = registry.Register(item)
	authorizer := &recordingAuthorizer{}
	result, err := (Executor{Snapshot: registry.Materialize(), Permissions: authorizer}).Execute(context.Background(), item.ID(), json.RawMessage(`{"url":"`+server.URL+`#fragment","method":"get"}`), CallContext{})
	if err != nil || result.Text != "fetched" {
		t.Fatalf("result = %#v, %v", result, err)
	}
	// GET/HEAD are the only methods normalizeFetch accepts, so a bounded fetch
	// is confined and never authorized.
	if authorizer.calls != 0 {
		t.Fatalf("authorizations = %d, want 0", authorizer.calls)
	}
	planned, err := item.Plan(context.Background(), json.RawMessage(`{"url":"`+server.URL+`"}`), CallContext{})
	if err != nil {
		t.Fatal(err)
	}
	data := planned.Data.(webFetchPlan)
	data.Input.URL = "https://example.com"
	planned.Data = data
	if _, err := item.Execute(context.Background(), planned, CallContext{}); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("revalidation error = %v", err)
	}
}

func TestGitDiffToolReadsUncommittedChangesAndRejectsOptionRefs(t *testing.T) {
	root := t.TempDir()
	runGit := func(args ...string) {
		command := exec.Command("git", args...)
		command.Dir = root
		command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "HOME="+t.TempDir())
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	runGit("init")
	runGit("config", "user.email", "test@example.com")
	runGit("config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("before\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit("add", "tracked.txt")
	runGit("commit", "-m", "initial")
	if err := os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("after\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "new.txt"), []byte("new\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ws, err := workspace.New(root)
	if err != nil {
		t.Fatal(err)
	}
	item := NewGitDiffTool()
	call := CallContext{Workspace: ws}
	plan, err := item.Plan(context.Background(), json.RawMessage(`{"target":"uncommitted"}`), call)
	if err != nil {
		t.Fatal(err)
	}
	result, err := item.Execute(context.Background(), plan, call)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Text, "-before") || !strings.Contains(result.Text, "+after") || !strings.Contains(result.Text, "?? new.txt") {
		t.Fatalf("git diff output = %q", result.Text)
	}
	if _, err := item.Plan(context.Background(), json.RawMessage(`{"target":"base","ref":"--help"}`), call); err == nil {
		t.Fatal("option-like Git ref was accepted")
	}
}
