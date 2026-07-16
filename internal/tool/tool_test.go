package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/amirulashraf/parrot-coder/internal/diagnostics"
	"github.com/amirulashraf/parrot-coder/internal/permission"
	"github.com/amirulashraf/parrot-coder/internal/workspace"
)

type testTool struct {
	id    string
	stale bool
}

func (t testTool) ID() string                                    { return t.id }
func (testTool) Description() string                             { return "test" }
func (testTool) DescribeRequest(json.RawMessage) (string, error) { return "Test request", nil }
func (testTool) JSONSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"value":{"type":"string"}},"required":["value"],"additionalProperties":false}`)
}
func (t testTool) Plan(_ context.Context, raw json.RawMessage, _ CallContext) (Plan, error) {
	p, err := NewPlan(t.id, raw, nil, nil, nil)
	if t.stale {
		p.OperationHash = "stale"
	}
	return p, err
}
func (testTool) Execute(_ context.Context, _ Plan, _ CallContext) (Result, error) {
	return Result{Text: "ok"}, nil
}

func TestRegistryDuplicateAndDeterministicDefinitions(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(testTool{id: "z"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(testTool{id: "a"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(testTool{id: "a"}); err == nil {
		t.Fatal("duplicate accepted")
	}
	s := r.Materialize()
	defs := s.Definitions()
	if defs[0].ID != "a" || defs[1].ID != "z" {
		t.Fatalf("not sorted: %#v", defs)
	}
	defs[0].Schema[0] = 'x'
	if s.Definitions()[0].Schema[0] != '{' {
		t.Fatal("snapshot definitions are mutable")
	}
	if err := r.Register(testTool{id: "later"}); err == nil {
		t.Fatal("mutable after materialization")
	}
}

func TestExecutorSchemaAndStalePlan(t *testing.T) {
	for _, raw := range []string{`{"value":"x","extra":1}`, `{"value":"x"} trailing`, `{"value":1}`, `{}`} {
		r := NewRegistry()
		_ = r.Register(testTool{id: "test"})
		e := Executor{Snapshot: r.Materialize()}
		if _, err := e.Execute(context.Background(), "test", json.RawMessage(raw), CallContext{}); err == nil {
			t.Errorf("invalid input accepted: %s", raw)
		}
	}
	r := NewRegistry()
	_ = r.Register(testTool{id: "test", stale: true})
	e := Executor{Snapshot: r.Materialize()}
	if _, err := e.Execute(context.Background(), "test", json.RawMessage(`{"value":"x"}`), CallContext{}); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("got %v", err)
	}
}

func TestExecutorDiagnosticsOmitInputAndOutput(t *testing.T) {
	state := t.TempDir()
	run, err := diagnostics.Start(state, diagnostics.Build{Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	registry := NewRegistry()
	if err := registry.Register(testTool{id: "test"}); err != nil {
		t.Fatal(err)
	}
	executor := Executor{Snapshot: registry.Materialize()}
	const secret = "sensitive-tool-input"
	call := CallContext{SessionID: "ses_test", ToolCallID: "call_test"}
	if _, err := executor.Execute(context.Background(), "test", json.RawMessage(`{"value":"`+secret+`"}`), call); err != nil {
		t.Fatal(err)
	}
	if _, err := executor.Execute(context.Background(), "test", json.RawMessage(`{"value":7,"private":"`+secret+`"}`), call); err == nil {
		t.Fatal("invalid tool input accepted")
	}
	run.Finish(0)

	data, err := os.ReadFile(filepath.Join(state, "diagnostics", "parrot.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if strings.Contains(text, secret) || strings.Contains(text, `"text":"ok"`) {
		t.Fatalf("diagnostics exposed tool content: %s", text)
	}
	for _, expected := range []string{
		`"event":"tool_execution_started"`, `"event":"tool_execution_finished"`,
		`"session_id":"ses_test"`, `"tool_call_id":"call_test"`, `"tool":"test"`,
		`"output_bytes":2`, `"status":"error"`, `"error_type":`,
	} {
		if !strings.Contains(text, expected) {
			t.Fatalf("diagnostics missing %s: %s", expected, text)
		}
	}
}

func TestExecutorStoresOversizedOutput(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "outputs")
	store, err := NewOutputStore(OutputConfig{Directory: dir, PreviewBytes: 8, PreviewLines: 4, PerOutput: 100, Total: 100, Retention: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	r := NewRegistry()
	_ = r.Register(outputTestTool{})
	e := Executor{Snapshot: r.Materialize(), MaxOutputBytes: 4}
	result, err := e.Execute(context.Background(), "output_test", json.RawMessage(`{}`), CallContext{Outputs: store})
	if err != nil {
		t.Fatal(err)
	}
	id, ok := result.Metadata["output_id"].(string)
	if !ok || strings.Contains(result.Text, filepath.Base(dir)) {
		t.Fatalf("managed output not opaque: %#v", result)
	}
	b, err := store.Read(id, 0, 100)
	if err != nil || string(b) != "long model output" {
		t.Fatalf("stored %q: %v", b, err)
	}
}

func TestExecutorKeepsBoundedSuccessWhenOutputStorageFails(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "outputs")
	store, err := NewOutputStore(OutputConfig{Directory: dir, PreviewBytes: 4, PreviewLines: 2, PerOutput: 5, Total: 5, Retention: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	r := NewRegistry()
	_ = r.Register(outputTestTool{})
	e := Executor{Snapshot: r.Materialize(), MaxOutputBytes: 4}
	result, err := e.Execute(context.Background(), "output_test", json.RawMessage(`{}`), CallContext{Outputs: store})
	if err != nil {
		t.Fatal(err)
	}
	if lossy, _ := result.Metadata["output_lossy"].(bool); !lossy {
		t.Fatalf("result does not report lossy output: %#v", result)
	}
	if len(result.Text) > 4 {
		t.Fatalf("bounded output has %d bytes, want at most 4", len(result.Text))
	}
}

type outputTestTool struct{}

func (outputTestTool) ID() string          { return "output_test" }
func (outputTestTool) Description() string { return "output test" }
func (outputTestTool) DescribeRequest(json.RawMessage) (string, error) {
	return "Produce test output", nil
}
func (outputTestTool) JSONSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","additionalProperties":false}`)
}
func (outputTestTool) Plan(_ context.Context, raw json.RawMessage, _ CallContext) (Plan, error) {
	return NewPlan("output_test", raw, nil, nil, nil)
}
func (outputTestTool) Execute(context.Context, Plan, CallContext) (Result, error) {
	return Result{Text: "long model output"}, nil
}

func TestOutputStoreUTF8QuotasModesAndRetention(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	s, err := NewOutputStore(OutputConfig{Directory: dir, PreviewBytes: 5, PreviewLines: 4, PerOutput: 20, Total: 25, Retention: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	out, err := s.Store(context.Background(), strings.NewReader("αβγδεζηθ"))
	if err != nil {
		t.Fatal(err)
	}
	if !utf8.ValidString(out.Preview) {
		t.Fatalf("invalid UTF-8: %q", out.Preview)
	}
	info, err := os.Stat(filepath.Join(dir, out.ID))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o", info.Mode().Perm())
	}
	dirInfo, _ := os.Stat(dir)
	if dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %o", dirInfo.Mode().Perm())
	}
	if _, err := s.Store(context.Background(), bytes.NewReader(make([]byte, 21))); err == nil {
		t.Fatal("per-output quota ignored")
	}
	if _, err := s.Store(context.Background(), bytes.NewReader(make([]byte, 10))); err == nil {
		t.Fatal("total quota ignored")
	}
	if _, err := s.Read("../secret", 0, 1); err == nil {
		t.Fatal("arbitrary path accepted")
	}
	if err := os.Chtimes(filepath.Join(dir, out.ID), time.Now().Add(-2*time.Hour), time.Now().Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.Cleanup(time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Read(out.ID, 0, 1); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retained output: %v", err)
	}
}

func toolHarness(t *testing.T) (*workspace.Workspace, Executor, CallContext) {
	t.Helper()
	root := t.TempDir()
	w, err := workspace.New(root)
	if err != nil {
		t.Fatal(err)
	}
	r := NewRegistry()
	for _, item := range []Tool{NewReadTool(ReadConfig{MaxLines: 20, MaxBytes: 128, MaxEntries: 20}), NewGlobTool(GlobConfig{}), NewGrepTool(GrepConfig{MaxLineBytes: 128})} {
		if err := r.Register(item); err != nil {
			t.Fatal(err)
		}
	}
	broker := permission.NewBroker(DefaultReadOnlyPolicy(), true, nil)
	call := CallContext{Workspace: w}
	return w, Executor{Snapshot: r.Materialize(), Permissions: broker, MaxOutputBytes: 4096}, call
}

func TestDefaultWorkspacePolicyAllowsOnlyReviewedMutationTools(t *testing.T) {
	request := func(toolID, kind, operation string) permission.Request {
		return permission.Request{ToolID: toolID, Resources: []permission.Resource{{Kind: kind, Identifier: "/workspace/file", Operation: operation}}}
	}
	tests := []struct {
		name     string
		request  permission.Request
		decision permission.Decision
		readOnly permission.Decision
	}{
		{name: "read", request: request("read", "filesystem", "read"), decision: permission.Allow, readOnly: permission.Allow},
		{name: "web fetch get", request: request("web_fetch", "network", "GET"), decision: permission.Allow, readOnly: permission.Allow},
		{name: "web fetch head", request: request("web_fetch", "network", "HEAD"), decision: permission.Allow, readOnly: permission.Allow},
		{name: "other network tool", request: request("other", "network", "GET"), decision: permission.Ask, readOnly: permission.Ask},
		{name: "web fetch unsupported operation", request: request("web_fetch", "network", "POST"), decision: permission.Ask, readOnly: permission.Ask},
		{name: "edit write", request: request("edit", "filesystem", "write"), decision: permission.Allow, readOnly: permission.Ask},
		{name: "edit create", request: request("edit", "filesystem", "create"), decision: permission.Allow, readOnly: permission.Ask},
		{name: "patch delete", request: request("apply_patch", "filesystem", "delete"), decision: permission.Allow, readOnly: permission.Ask},
		{name: "formatter write", request: request("format", "filesystem", "write"), decision: permission.Ask, readOnly: permission.Ask},
		{name: "edit external capability", request: request("edit", "external_filesystem", "write"), decision: permission.Ask, readOnly: permission.Ask},
		{name: "shell", request: request("shell", "process", "execute"), decision: permission.Ask, readOnly: permission.Ask},
	}
	workspacePolicy := DefaultWorkspacePolicy()
	readOnlyPolicy := DefaultReadOnlyPolicy()
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got, _, _ := workspacePolicy.Evaluate(test.request); got != test.decision {
				t.Fatalf("workspace decision = %q, want %q", got, test.decision)
			}
			if got, _, _ := readOnlyPolicy.Evaluate(test.request); got != test.readOnly {
				t.Fatalf("read-only decision = %q, want %q", got, test.readOnly)
			}
		})
	}
}

func TestReadBinaryHugeLineAndSymlinkSwap(t *testing.T) {
	w, e, call := toolHarness(t)
	root := w.Root()
	if err := os.WriteFile(filepath.Join(root, "binary"), []byte{'a', 0, 'b'}, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Execute(context.Background(), "read", json.RawMessage(`{"path":"binary"}`), call); err == nil {
		t.Fatal("binary accepted")
	}
	if err := os.WriteFile(filepath.Join(root, "huge"), []byte(strings.Repeat("x", 200)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Execute(context.Background(), "read", json.RawMessage(`{"path":"huge"}`), call); err == nil {
		t.Fatal("huge line accepted")
	}
	if runtime.GOOS == "darwin" || runtime.GOOS == "linux" {
		inside := filepath.Join(root, "inside")
		outside := t.TempDir()
		_ = os.WriteFile(inside, []byte("safe"), 0o600)
		_ = os.WriteFile(filepath.Join(outside, "file"), []byte("secret"), 0o600)
		read := NewReadTool(ReadConfig{})
		p, err := read.Plan(context.Background(), json.RawMessage(`{"path":"inside"}`), call)
		if err != nil {
			t.Fatal(err)
		}
		_ = os.Remove(inside)
		if err := os.Symlink(filepath.Join(outside, "file"), inside); err != nil {
			t.Fatal(err)
		}
		if _, err := read.Execute(context.Background(), p, call); err == nil {
			t.Fatal("symlink swap accepted")
		}
	}
}

func TestGlobAndGrepDeterministicAndCancellation(t *testing.T) {
	w, e, call := toolHarness(t)
	for name, data := range map[string]string{"b.txt": "hit two\n", "a.txt": "hit one\n", ".hidden.txt": "hit hidden\n", "skip.bin": "x\x00hit\n"} {
		if err := os.WriteFile(filepath.Join(w.Root(), name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	glob, err := e.Execute(context.Background(), "glob", json.RawMessage(`{"pattern":"**/*.txt"}`), call)
	if err != nil {
		t.Fatal(err)
	}
	if glob.Text != ".hidden.txt\na.txt\nb.txt" {
		t.Fatalf("glob order/content: %q", glob.Text)
	}
	grep, err := e.Execute(context.Background(), "grep", json.RawMessage(`{"pattern":"hit"}`), call)
	if err != nil {
		t.Fatal(err)
	}
	want := ".hidden.txt:1:hit hidden\na.txt:1:hit one\nb.txt:1:hit two\n"
	if grep.Text != want {
		t.Fatalf("grep order/content: %q", grep.Text)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.Execute(ctx, "glob", json.RawMessage(`{"pattern":"**"}`), call); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

func TestGrepDefaultLimits(t *testing.T) {
	grep := NewGrepTool(GrepConfig{})
	if grep.Config.MaxFiles != 100000 {
		t.Fatalf("MaxFiles = %d, want 100000", grep.Config.MaxFiles)
	}
	if grep.Config.MaxVisited != 1000000 {
		t.Fatalf("MaxVisited = %d, want 1000000", grep.Config.MaxVisited)
	}
}
