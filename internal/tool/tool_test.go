package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
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
	BasePresentation
	id     string
	result *Result
}

func (t testTool) ID() string                                    { return t.id }
func (testTool) Description() string                             { return "test" }
func (testTool) DescribeRequest(json.RawMessage) (string, error) { return "Test request", nil }
func (testTool) JSONSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"value":{"type":"string"}},"required":["value"],"additionalProperties":false}`)
}
func (t testTool) Plan(_ context.Context, raw json.RawMessage, _ CallContext) (Plan, error) {
	return NewPlan(t.id, raw, nil, nil, nil)
}
func (t testTool) Execute(_ context.Context, _ Plan, _ CallContext) (Result, error) {
	if t.result != nil {
		return *t.result, nil
	}
	return Result{Text: "ok", ModelText: "ok"}, nil
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
}

func TestExecutorEmptyModelText(t *testing.T) {
	execute := func(result Result) (Result, error) {
		r := NewRegistry()
		_ = r.Register(testTool{id: "test", result: &result})
		e := Executor{Snapshot: r.Materialize()}
		return e.Execute(context.Background(), "test", json.RawMessage(`{"value":"x"}`), CallContext{})
	}
	// A tool producing no output leaves both fields empty; the executor
	// substitutes the placeholder in the model copy only.
	got, err := execute(Result{})
	if err != nil {
		t.Fatal(err)
	}
	if got.ModelText != emptyModelText || got.Text != "" {
		t.Fatalf("placeholder not applied: %#v", got)
	}
	// Output without a model copy remains an error.
	if _, err := execute(Result{Text: "ok"}); err == nil || !strings.Contains(err.Error(), "without a model copy") {
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
	if result.Text != "long model output" {
		t.Fatalf("executor altered the record: %#v", result)
	}
	if !strings.Contains(result.ModelText, "read_output id "+id) {
		t.Fatalf("model copy does not name the managed output: %#v", result)
	}
	b, err := store.Read(id, 0, 100)
	if err != nil || string(b) != "long model output" {
		t.Fatalf("stored %q: %v", b, err)
	}
}

func TestExecutorReportsLossyOutputWhenStorageFails(t *testing.T) {
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
	if result.Text != "long model output" {
		t.Fatalf("executor altered the record: %#v", result)
	}
	if !strings.Contains(result.ModelText, "unrecoverable") {
		t.Fatalf("model copy does not report the lost output: %#v", result)
	}
}

// Definitions is marshalled into the model's tool guidance on every turn, so a
// field added to Definition costs prompt tokens forever. Presentation is a
// parallel projection precisely to avoid that; this fences the boundary.
func TestDefinitionsCarryNoPresentationDetail(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(testTool{id: "test"}); err != nil {
		t.Fatal(err)
	}
	snapshot := r.Materialize()
	encoded, err := json.Marshal(snapshot.Definitions())
	if err != nil {
		t.Fatal(err)
	}
	var decoded []map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, definition := range decoded {
		if len(definition) != 3 {
			t.Fatalf("definition has %d keys, want exactly id/description/schema: %v", len(definition), definition)
		}
		for _, key := range []string{"id", "description", "schema"} {
			if _, ok := definition[key]; !ok {
				t.Fatalf("definition missing %q: %v", key, definition)
			}
		}
	}
	if entries := snapshot.Presentations(); len(entries) != 1 || entries[0].ID != "test" {
		t.Fatalf("presentations = %#v", entries)
	}
}

func TestSnapshotWithout(t *testing.T) {
	r := NewRegistry()
	for _, id := range []string{"a", "b", "c", "d"} {
		if err := r.Register(testTool{id: id}); err != nil {
			t.Fatal(err)
		}
	}
	s := r.Materialize()

	if len(s.Definitions()) != 4 {
		t.Fatalf("expected 4 definitions, got %d", len(s.Definitions()))
	}

	// Filter out "b" and "d"
	filtered := s.Without([]string{"b", "d"})
	defs := filtered.Definitions()
	if len(defs) != 2 {
		t.Fatalf("expected 2 definitions, got %d", len(defs))
	}
	if defs[0].ID != "a" || defs[1].ID != "c" {
		t.Fatalf("filtered definitions = %#v", defs)
	}

	// Original snapshot is unchanged
	if len(s.Definitions()) != 4 {
		t.Fatal("original snapshot was modified")
	}

	// Empty blacklist returns the same snapshot
	same := s.Without(nil)
	if len(same.Definitions()) != 4 {
		t.Fatalf("empty blacklist returned %d definitions", len(same.Definitions()))
	}

	// Empty slice blacklist returns the same snapshot
	same = s.Without([]string{})
	if len(same.Definitions()) != 4 {
		t.Fatalf("empty slice blacklist returned %d definitions", len(same.Definitions()))
	}

	// Executor rejects blacklisted tools
	executor := Executor{Snapshot: filtered}
	if _, err := executor.Execute(context.Background(), "b", json.RawMessage(`{"value":"x"}`), CallContext{}); err == nil {
		t.Fatal("blacklisted tool was executable")
	}
	if _, err := executor.Execute(context.Background(), "a", json.RawMessage(`{"value":"x"}`), CallContext{}); err != nil {
		t.Fatalf("allowed tool was rejected: %v", err)
	}

	// Blacklisting unknown IDs is a no-op
	filtered = s.Without([]string{"unknown"})
	if len(filtered.Definitions()) != 4 {
		t.Fatalf("unknown blacklist changed the snapshot: %d definitions", len(filtered.Definitions()))
	}
}

func TestModelTextBoundsWithoutTouchingTheRecord(t *testing.T) {
	large := strings.Repeat("x", maxModelTextBytes*2)
	for _, test := range []struct {
		name string
		text string
		want bool
	}{
		{name: "under the limit is verbatim", text: "short output"},
		{name: "over the limit is truncated", text: large, want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := modelText(test.text)
			if truncated := got != test.text; truncated != test.want {
				t.Fatalf("modelText truncated = %t, want %t", truncated, test.want)
			}
			if len(got) > maxModelTextBytes {
				t.Fatalf("model copy has %d bytes, want at most %d", len(got), maxModelTextBytes)
			}
		})
	}
}

// A model copy must stay parseable, so tools encoding JSON bound the oversized
// field before encoding rather than cutting the encoded document.
func TestJSONToolsKeepModelCopyParseable(t *testing.T) {
	large := strings.Repeat("y", maxModelTextBytes*2)
	result := agentResult(AgentTask{SessionID: "session_1", Agent: "reviewer", Status: "completed", Output: large, Error: large})
	if len(result.Text) <= maxModelTextBytes {
		t.Fatalf("record was bounded: %d bytes", len(result.Text))
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(result.ModelText), &decoded); err != nil {
		t.Fatalf("model copy is not valid JSON: %v", err)
	}
	for _, field := range []string{"output", "error"} {
		if value, _ := decoded[field].(string); len(value) > maxModelTextBytes {
			t.Fatalf("%s has %d bytes, want at most %d", field, len(value), maxModelTextBytes)
		}
	}
	if decoded["session_id"] != "session_1" {
		t.Fatalf("model copy lost its identifiers: %v", decoded)
	}
}

type outputTestTool struct {
	BasePresentation
}

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
	return Result{Text: "long model output", ModelText: "long model output"}, nil
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
		t.Fatal("total quota ignored")
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

type chunkedReader struct {
	data  []byte
	chunk int
}

func (r *chunkedReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := min(r.chunk, len(r.data))
	copy(p, r.data[:n])
	r.data = r.data[n:]
	return n, nil
}

func TestOutputStoreStartTruncationKeepsTailAndCountsOmitted(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "outputs")
	s, err := NewOutputStore(OutputConfig{Directory: dir, PreviewBytes: 4, PreviewLines: 2, PerOutput: 10, Total: 1000, Retention: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	input := "0123456789ABCDEFGHIJKLMNOPQRSTUV"
	out, err := s.Store(context.Background(), &chunkedReader{data: []byte(input), chunk: 5})
	if err != nil {
		t.Fatal(err)
	}
	if out.Size != 10 || out.OmittedBytes != 22 {
		t.Fatalf("stored = %#v", out)
	}
	b, err := s.Read(out.ID, 0, 10)
	if err != nil || string(b) != "MNOPQRSTUV" {
		t.Fatalf("stored content %q: %v", b, err)
	}
}

func TestExecutorReportsBytesLostWhenSpilledOutputExceedsPerOutputLimit(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "outputs")
	store, err := NewOutputStore(OutputConfig{Directory: dir, PreviewBytes: 4, PreviewLines: 2, PerOutput: 5, Total: 100, Retention: time.Hour})
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
	if _, ok := result.Metadata["output_id"].(string); !ok {
		t.Fatalf("result does not name the managed output: %#v", result)
	}
	if !strings.Contains(result.ModelText, "first 12 bytes could not be stored") || !strings.Contains(result.ModelText, "read_output id") {
		t.Fatalf("model copy does not report the lost bytes: %#v", result)
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
	rg := NewRgTool(RgConfig{MaxLineBytes: 128})
	rg.command = cliRgCommand{}
	for _, item := range []Tool{NewReadTool(ReadConfig{MaxLines: 20, MaxBytes: 128, MaxEntries: 20}), NewGlobTool(GlobConfig{}), rg} {
		if err := r.Register(item); err != nil {
			t.Fatal(err)
		}
	}
	broker := permission.NewBroker(true, nil)
	call := CallContext{Workspace: w}
	return w, Executor{Snapshot: r.Materialize(), Permissions: broker, MaxOutputBytes: 4096}, call
}

// Approval is reserved for operations the sandbox cannot contain. Every other
// tool plans no permission request at all, so it is never prompted.
func TestOnlySandboxEscapingToolsRequestApproval(t *testing.T) {
	_, ws, _ := workspaceToolHarness(t)
	call := CallContext{Workspace: ws, SessionID: "s"}
	content := []byte("hello\n")
	if err := os.WriteFile(filepath.Join(ws.Root(), "a.txt"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	grantable := t.TempDir()

	for _, test := range []struct {
		name  string
		tool  Tool
		input string
		want  int
	}{
		{name: "read", tool: NewReadTool(ReadConfig{}), input: `{"path":"a.txt"}`},
		{name: "rg", tool: NewRgTool(RgConfig{}), input: `{"pattern":"hello"}`},
		{name: "glob", tool: NewGlobTool(GlobConfig{}), input: `{"pattern":"*.txt"}`},
		{name: "exec_command sandboxed", tool: NewExecCommandTool(nil), input: `{"cmd":"true","shell":"/bin/sh"}`},

		{name: "exec_command unsandboxed", tool: NewExecCommandTool(nil), input: `{"cmd":"true","shell":"/bin/sh","sandbox_permissions":"disable_sandbox","justification":"needs the host"}`, want: 1},
		{name: "set_config", tool: NewSetConfigTool(t.TempDir()), input: `{"key":"model","value":"openai/gpt-4","operation":"set"}`, want: 1},
		{name: "request_write_permission", tool: NewWritePermissionTool(nil), input: `{"path":"` + grantable + `"}`, want: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			planned, err := test.tool.Plan(context.Background(), json.RawMessage(test.input), call)
			if err != nil {
				t.Fatal(err)
			}
			if len(planned.Permissions) != test.want {
				t.Fatalf("permission requests = %d, want %d", len(planned.Permissions), test.want)
			}
			for _, request := range planned.Permissions {
				if request.ToolID != test.tool.ID() {
					t.Fatalf("request tool ID = %q, want %q", request.ToolID, test.tool.ID())
				}
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

func TestReadAndRgExplicitExternalPaths(t *testing.T) {
	_, executor, call := toolHarness(t)
	external := t.TempDir()
	path := filepath.Join(external, "outside.txt")
	if err := os.WriteFile(path, []byte("external match\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	readTool := NewReadTool(ReadConfig{})
	rgTool := NewRgTool(RgConfig{})
	readPlan, err := readTool.Plan(context.Background(), json.RawMessage(fmt.Sprintf(`{"path":%q}`, path)), call)
	if err != nil {
		t.Fatal(err)
	}
	rgPlan, err := rgTool.Plan(context.Background(), json.RawMessage(fmt.Sprintf(`{"pattern":"match","path":%q}`, external)), call)
	if err != nil {
		t.Fatal(err)
	}
	// An explicit external path is still a bounded read, so it is not prompted.
	if len(readPlan.Permissions) != 0 || len(rgPlan.Permissions) != 0 {
		t.Fatalf("external read requested approval: read = %d, rg = %d", len(readPlan.Permissions), len(rgPlan.Permissions))
	}

	read, err := executor.Execute(context.Background(), "read", json.RawMessage(fmt.Sprintf(`{"path":%q}`, path)), call)
	if err != nil {
		t.Fatal(err)
	}
	grep, err := executor.Execute(context.Background(), "rg", json.RawMessage(fmt.Sprintf(`{"pattern":"match","path":%q}`, external)), call)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(read.Text, "1: external match\nsha256: ") || !strings.Contains(grep.Text, "outside.txt:1:external match\n") {
		t.Fatalf("read = %q, grep = %q", read.Text, grep.Text)
	}
}

func TestGlobAndRgDeterministicAndCancellation(t *testing.T) {
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
	grep, err := e.Execute(context.Background(), "rg", json.RawMessage(`{"pattern":"hit"}`), call)
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

func TestRgMatchesAndTruncatesOversizedLines(t *testing.T) {
	w, err := workspace.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	content := "�1234é--needle\nneedle\n"
	if err := os.WriteFile(filepath.Join(w.Root(), "oversized.txt"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	grep := NewRgTool(RgConfig{MaxLineBytes: 8})
	grep.command = cliRgCommand{}
	call := CallContext{Workspace: w}
	plan, err := grep.Plan(context.Background(), json.RawMessage(`{"pattern":"needle","path":"oversized.txt"}`), call)
	if err != nil {
		t.Fatal(err)
	}
	result, err := grep.Execute(context.Background(), plan, call)
	if err != nil {
		t.Fatal(err)
	}
	want := "oversized.txt:1:�1234... [truncated; original length: 17 bytes]\noversized.txt:2:needle\n"
	if result.Text != want || !utf8.ValidString(result.Text) {
		t.Fatalf("grep output = %q, valid UTF-8 = %t, want %q", result.Text, utf8.ValidString(result.Text), want)
	}
	if result.Metadata["matches"] != 2 || result.Metadata["truncated"] != true {
		t.Fatalf("grep metadata = %#v", result.Metadata)
	}
}

func TestRgIncludeFiltersCandidateFiles(t *testing.T) {
	w, e, call := toolHarness(t)
	if err := os.Mkdir(filepath.Join(w.Root(), "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"top.sql":          "session top\n",
		"top.go":           "session go\n",
		"nested/child.sql": "session child\n",
		"nested/child.go":  "session nested go\n",
	} {
		if err := os.WriteFile(filepath.Join(w.Root(), filepath.FromSlash(name)), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	for _, test := range []struct {
		name      string
		input     string
		want      string
		wantFiles int
	}{
		{
			name:      "basename glob applies recursively",
			input:     `{"pattern":"session","include":"*.sql"}`,
			want:      "nested/child.sql:1:session child\ntop.sql:1:session top\n",
			wantFiles: 2,
		},
		{
			name:      "path-aware glob",
			input:     `{"pattern":"session","include":"nested/*.sql"}`,
			want:      "nested/child.sql:1:session child\n",
			wantFiles: 1,
		},
		{
			name:      "include omitted",
			input:     `{"pattern":"session"}`,
			want:      "nested/child.go:1:session nested go\nnested/child.sql:1:session child\ntop.go:1:session go\ntop.sql:1:session top\n",
			wantFiles: 4,
		},
		{
			name:      "explicit file path-aware glob",
			input:     `{"pattern":"session","path":"nested/child.sql","include":"nested/*.sql"}`,
			want:      "nested/child.sql:1:session child\n",
			wantFiles: 1,
		},
		{
			name:      "explicit file excluded",
			input:     `{"pattern":"session","path":"top.go","include":"*.sql"}`,
			wantFiles: 0,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			result, err := e.Execute(context.Background(), "rg", json.RawMessage(test.input), call)
			if err != nil {
				t.Fatal(err)
			}
			if result.Text != test.want || result.Metadata["files"] != test.wantFiles {
				t.Fatalf("grep result = %q, metadata = %#v; want %q and %d files", result.Text, result.Metadata, test.want, test.wantFiles)
			}
		})
	}

	grep := NewRgTool(RgConfig{})
	if !strings.Contains(string(grep.JSONSchema()), `"include"`) {
		t.Fatalf("grep schema does not expose include: %s", grep.JSONSchema())
	}
	if _, err := grep.Plan(context.Background(), json.RawMessage(`{"pattern":"session","include":"../*.sql"}`), call); err == nil {
		t.Fatal("grep accepted a traversing include glob")
	}
}

type fakeRgCommand struct {
	available bool
	called    bool
}

func (f *fakeRgCommand) Available() bool { return f.available }
func (f *fakeRgCommand) Search(context.Context, string, []string, rgInput, RgConfig) (Result, error) {
	f.called = true
	return Result{Text: "cli\n", ModelText: "cli\n"}, nil
}

func TestRgPrefersAvailableCommandAndFallsBackInternally(t *testing.T) {
	w, err := workspace.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.Root(), "file.txt"), []byte("internal match\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	call := CallContext{Workspace: w}

	for _, test := range []struct {
		name      string
		available bool
		want      string
		called    bool
	}{
		{name: "available command", available: true, want: "cli\n", called: true},
		{name: "internal fallback", want: "file.txt:1:internal match\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			command := &fakeRgCommand{available: test.available}
			tool := NewRgTool(RgConfig{})
			tool.command = command
			plan, err := tool.Plan(context.Background(), json.RawMessage(`{"pattern":"match"}`), call)
			if err != nil {
				t.Fatal(err)
			}
			result, err := tool.Execute(context.Background(), plan, call)
			if err != nil {
				t.Fatal(err)
			}
			if result.Text != test.want || command.called != test.called {
				t.Fatalf("result = %q, command called = %t; want %q, %t", result.Text, command.called, test.want, test.called)
			}
		})
	}
}

func TestCliRgCommandSearch(t *testing.T) {
	path, err := exec.LookPath("rg")
	if err != nil {
		t.Skip("rg CLI is unavailable")
	}
	root := t.TempDir()
	first := filepath.Join(root, "a.txt")
	second := filepath.Join(root, "b.txt")
	if err := os.WriteFile(first, []byte("match one\nignore\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("match two and a long suffix\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(t.TempDir(), "ripgrep.conf")
	if err := os.WriteFile(config, []byte("--definitely-invalid-option\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RIPGREP_CONFIG_PATH", config)

	result, err := (cliRgCommand{path: path}).Search(context.Background(), root, []string{first, second}, rgInput{Pattern: "match"}, RgConfig{MaxMatches: 10, MaxLineBytes: 8})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "a.txt:1:match on [... omitted end of long line]\nb.txt:1:match tw [... omitted end of long line]\n" || result.Metadata["matches"] != 2 || result.Metadata["truncated"] != true {
		t.Fatalf("result = %#v", result)
	}
}

func TestRgDefaultLimits(t *testing.T) {
	rg := NewRgTool(RgConfig{})
	if rg.Config.MaxFiles != 100000 {
		t.Fatalf("MaxFiles = %d, want 100000", rg.Config.MaxFiles)
	}
	if rg.Config.MaxVisited != 1000000 {
		t.Fatalf("MaxVisited = %d, want 1000000", rg.Config.MaxVisited)
	}
}

type guidanceTestTool struct {
	testTool
	guidance string
}

func (t guidanceTestTool) SystemPromptGuidance() string { return t.guidance }

func TestSnapshotSystemPromptGuidanceCollectsNonEmpty(t *testing.T) {
	for _, test := range []struct {
		name  string
		tools []Tool
		want  string
	}{
		{
			name:  "only silent tools return empty",
			tools: []Tool{testTool{id: "a"}, testTool{id: "b"}},
			want:  "",
		},
		{
			name:  "single tool with guidance",
			tools: []Tool{testTool{id: "a"}, guidanceTestTool{testTool: testTool{id: "b"}, guidance: "b explains itself"}},
			want:  "b explains itself",
		},
		{
			name: "multiple tools sorted by guidance text",
			tools: []Tool{
				guidanceTestTool{testTool: testTool{id: "z"}, guidance: "zebra guidance"},
				guidanceTestTool{testTool: testTool{id: "a"}, guidance: "alpha guidance"},
			},
			want: "alpha guidance\n\nzebra guidance",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := NewRegistry()
			for _, item := range test.tools {
				if err := r.Register(item); err != nil {
					t.Fatal(err)
				}
			}
			if got := r.Materialize().SystemPromptGuidance(); got != test.want {
				t.Fatalf("SystemPromptGuidance() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestExecCommandSystemPromptGuidanceExplainsSandbox(t *testing.T) {
	guidance := (&ExecCommandTool{}).SystemPromptGuidance()
	if guidance == "" {
		t.Fatal("exec_command guidance should not be empty")
	}
	for _, fragment := range []string{"sandbox", "read-only", "request_write_permission"} {
		if !strings.Contains(guidance, fragment) {
			t.Errorf("guidance missing %q: %s", fragment, guidance)
		}
	}
}

func TestWritePermissionSystemPromptGuidanceExplainsSandbox(t *testing.T) {
	guidance := (&WritePermissionTool{}).SystemPromptGuidance()
	if guidance == "" {
		t.Fatal("request_write_permission guidance should not be empty")
	}
	for _, fragment := range []string{"sandbox", "read-only"} {
		if !strings.Contains(guidance, fragment) {
			t.Errorf("guidance missing %q: %s", fragment, guidance)
		}
	}
}
