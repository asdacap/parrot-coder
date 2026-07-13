package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/amirulashraf/parrot-coder/internal/change"
	"github.com/amirulashraf/parrot-coder/internal/formatter"
	"github.com/amirulashraf/parrot-coder/internal/lsp"
	"github.com/amirulashraf/parrot-coder/internal/mcp"
	"github.com/amirulashraf/parrot-coder/internal/skill"
	"github.com/amirulashraf/parrot-coder/internal/subagent"
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
	if err := os.WriteFile(path, []byte("---\nname: review\ndescription: Review changes\nagent: explore\nmodel: local/model\nallowed-tools: [read, grep]\n---\n"+body), 0o600); err != nil {
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
	if err != nil || result.Text != body || result.Metadata["agent"] != "explore" || !strings.Contains(item.Description(), "review") {
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

func TestMCPToolPermissionArgumentsAndApplicationError(t *testing.T) {
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
	if string(caller.arguments) != `{"value":"ok"}` || result.Text != "ok" || authorizer.request.Resources[0].Kind != "mcp" || authorizer.request.Resources[0].Attributes["arguments_sha256"] == "" {
		t.Fatalf("call/result/request = %s / %#v / %#v", caller.arguments, result, authorizer.request)
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

func TestWebFetchToolPermissionAndRevalidation(t *testing.T) {
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
	resource := authorizer.request.Resources[0]
	if resource.Kind != "network" || resource.Operation != http.MethodGet || strings.Contains(resource.Identifier, "#") {
		t.Fatalf("resource = %#v", resource)
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

type fakeLSPClient struct {
	mu      sync.Mutex
	opened  map[string]bool
	lastPos lsp.Position
}

func (f *fakeLSPClient) DidOpen(_ context.Context, path, _, _ string) error {
	f.mu.Lock()
	f.opened[path] = true
	f.mu.Unlock()
	return nil
}
func (f *fakeLSPClient) DidChange(_ context.Context, path, _ string) error {
	f.mu.Lock()
	f.opened[path] = true
	f.mu.Unlock()
	return nil
}
func (f *fakeLSPClient) DocumentVersion(path string) (int, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return 1, f.opened[path]
}
func (f *fakeLSPClient) Definition(_ context.Context, path string, position lsp.Position) ([]lsp.Location, error) {
	f.lastPos = position
	return []lsp.Location{{URI: lsp.DocumentURI("file://" + path)}}, nil
}
func (f *fakeLSPClient) References(context.Context, string, lsp.Position, bool) ([]lsp.Location, error) {
	return nil, nil
}
func (f *fakeLSPClient) Hover(context.Context, string, lsp.Position) (*lsp.Hover, error) {
	return &lsp.Hover{Contents: json.RawMessage(`"hover"`)}, nil
}
func (f *fakeLSPClient) DocumentSymbols(context.Context, string) ([]lsp.SymbolInformation, error) {
	return nil, nil
}
func (f *fakeLSPClient) Symbols(context.Context, string) ([]lsp.SymbolInformation, error) {
	return nil, nil
}
func (f *fakeLSPClient) Diagnostics(lsp.DocumentURI) []lsp.Diagnostic {
	return []lsp.Diagnostic{{Message: "diagnostic"}}
}

func TestLSPWrapperOpensWorkspaceFileBeforeRead(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ws, _ := workspace.New(root)
	fake := &fakeLSPClient{opened: make(map[string]bool)}
	tools := NewLSPTools(LSPToolConfig{Client: func(context.Context, string) (LSPClient, error) { return fake, nil }, Languages: map[string]map[string]string{"go": {".go": "go"}}})
	var definition Tool
	for _, item := range tools {
		if item.ID() == "lsp_definition" {
			definition = item
		}
	}
	plan, err := definition.Plan(context.Background(), json.RawMessage(`{"server":"go","path":"main.go","line":2,"character":3}`), CallContext{Workspace: ws})
	if err != nil {
		t.Fatal(err)
	}
	result, err := definition.Execute(context.Background(), plan, CallContext{Workspace: ws})
	if err != nil || !strings.Contains(result.Text, "file://") || fake.lastPos.Line != 2 || fake.lastPos.Character != 3 {
		t.Fatalf("result = %#v, position = %#v, error = %v", result, fake.lastPos, err)
	}
}

type subagentExecutorFunc func(context.Context, subagent.Execution) (string, error)

func (f subagentExecutorFunc) Execute(ctx context.Context, execution subagent.Execution) (string, error) {
	return f(ctx, execution)
}

func TestTaskToolOutputReadOnlyBoundaryAndParentCancellation(t *testing.T) {
	started := make(chan struct{})
	manager := subagent.NewManager(subagentExecutorFunc(func(ctx context.Context, execution subagent.Execution) (string, error) {
		if execution.Request.Prompt == "wait" {
			close(started)
			<-ctx.Done()
			return "", ctx.Err()
		}
		return "child output", nil
	}), subagent.Config{})
	lookup := func(id string) (bool, error) { return id != "build", nil }
	item := NewTaskTools(manager, lookup)[0]
	call := CallContext{SessionID: "parent", Agent: "plan"}
	if _, err := item.Plan(context.Background(), json.RawMessage(`{"prompt":"write","agent":"build"}`), call); err == nil {
		t.Fatal("read-only caller delegated to writable agent")
	}
	plan, err := item.Plan(context.Background(), json.RawMessage(`{"prompt":"read","agent":"explore"}`), call)
	if err != nil {
		t.Fatal(err)
	}
	result, err := item.Execute(context.Background(), plan, call)
	if err != nil || result.Text != "child output" || result.Metadata["task_id"] == "" {
		t.Fatalf("result = %#v, %v", result, err)
	}
	waitPlan, err := item.Plan(context.Background(), json.RawMessage(`{"prompt":"wait","agent":"explore"}`), call)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := item.Execute(ctx, waitPlan, call); done <- err }()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("execute error = %v", err)
	}
	for _, task := range manager.List("parent") {
		if task.Status == subagent.StatusRunning {
			t.Fatalf("orphaned task = %#v", task)
		}
	}
}

func TestTaskStatusAndCancelToolsAreParentScoped(t *testing.T) {
	started := make(chan struct{})
	manager := subagent.NewManager(subagentExecutorFunc(func(ctx context.Context, _ subagent.Execution) (string, error) {
		close(started)
		<-ctx.Done()
		return "", ctx.Err()
	}), subagent.Config{})
	id, err := manager.Launch("parent", []string{"build"}, subagent.Request{Prompt: "wait", Agent: "explore"})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	tools := NewTaskTools(manager, func(string) (bool, error) { return false, nil })
	call := CallContext{SessionID: "parent", Agent: "build"}
	raw := json.RawMessage(`{"task_id":"` + id + `"}`)
	statusPlan, err := tools[1].Plan(context.Background(), raw, call)
	if err != nil {
		t.Fatal(err)
	}
	status, err := tools[1].Execute(context.Background(), statusPlan, call)
	if err != nil || status.Metadata["status"] != subagent.StatusRunning {
		t.Fatalf("status = %#v, %v", status, err)
	}
	if _, err := tools[1].Plan(context.Background(), raw, CallContext{SessionID: "other", Agent: "build"}); err != nil {
		t.Fatal(err)
	}
	if _, err := tools[1].Execute(context.Background(), statusPlan, CallContext{SessionID: "other", Agent: "build"}); !errors.Is(err, subagent.ErrNotFound) {
		t.Fatalf("cross-parent status error = %v", err)
	}
	cancelPlan, err := tools[2].Plan(context.Background(), raw, call)
	if err != nil {
		t.Fatal(err)
	}
	canceled, err := tools[2].Execute(context.Background(), cancelPlan, call)
	if err != nil || canceled.Metadata["status"] != subagent.StatusCanceled {
		t.Fatalf("cancel = %#v, %v", canceled, err)
	}
}

func TestPhase9FormatterHelper(t *testing.T) {
	if os.Getenv("PARROT_TOOL_FORMAT_HELPER") != "1" {
		return
	}
	data, _ := io.ReadAll(os.Stdin)
	_, _ = os.Stdout.Write(bytes.ToUpper(data))
	os.Exit(0)
}

func TestFormatToolCommitsReviewedBytesAndSnapshot(t *testing.T) {
	ctx, ws, changes, snapshots, sessionID := phase6Harness(t)
	path := filepath.Join(ws.Root(), "source.go")
	before := []byte("package p\n")
	if err := os.WriteFile(path, before, 0o600); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	formatters, err := formatter.NewRegistry(formatter.Config{Workspace: ws.Root(), Environment: map[string]string{"PARROT_TOOL_FORMAT_HELPER": "1"}}, formatter.Formatter{Name: "upper", Extensions: []string{".go"}, Command: []string{executable, "-test.run=TestPhase9FormatterHelper"}, Mode: formatter.ModeStdin})
	if err != nil {
		t.Fatal(err)
	}
	item := NewFormatTool(formatters, changes, snapshots)
	registry := NewRegistry()
	_ = registry.Register(item)
	authorizer := &recordingAuthorizer{}
	raw := json.RawMessage(`{"path":"source.go","expected_sha256":"` + change.SHA256(before) + `"}`)
	result, err := (Executor{Snapshot: registry.Materialize(), Permissions: authorizer}).Execute(ctx, item.ID(), raw, CallContext{Workspace: ws, SessionID: sessionID})
	if err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	if string(after) != "PACKAGE P\n" || result.Metadata["transaction_id"] == "" || !strings.Contains(string(authorizer.request.Review), `"command"`) {
		t.Fatalf("after/result/review = %q / %#v / %s", after, result, authorizer.request.Review)
	}
	if _, err := snapshots.Undo(ctx, ws, sessionID); err != nil {
		t.Fatal(err)
	}
	restored, _ := os.ReadFile(path)
	if !bytes.Equal(restored, before) {
		t.Fatalf("restored = %q", restored)
	}
}

func TestFormatToolNoopDoesNotRequireSnapshot(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "file.txt")
	before := []byte("already\n")
	if err := os.WriteFile(path, before, 0o600); err != nil {
		t.Fatal(err)
	}
	formatters, err := formatter.NewRegistry(formatter.Config{Workspace: root}, formatter.Formatter{Name: "identity", Extensions: []string{".txt"}, Command: []string{"/bin/cat"}, Mode: formatter.ModeStdin})
	if err != nil {
		t.Fatal(err)
	}
	ws, err := workspace.New(root)
	if err != nil {
		t.Fatal(err)
	}
	item := NewFormatTool(formatters, change.NewService(change.Config{}), nil)
	raw := json.RawMessage(`{"path":"file.txt","expected_sha256":"` + change.SHA256(before) + `"}`)
	plan, err := item.Plan(context.Background(), raw, CallContext{Workspace: ws})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Permissions) != 0 {
		t.Fatalf("no-op permissions = %#v", plan.Permissions)
	}
	result, err := item.Execute(context.Background(), plan, CallContext{Workspace: ws})
	if err != nil {
		t.Fatal(err)
	}
	if changed, _ := result.Metadata["changed"].(bool); changed {
		t.Fatalf("no-op result = %#v", result)
	}
}
