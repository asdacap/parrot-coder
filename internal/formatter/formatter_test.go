package formatter

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFormatterHelperProcess(t *testing.T) {
	if os.Getenv("PARROT_FORMATTER_HELPER") != "1" {
		return
	}
	mode := ""
	for i, arg := range os.Args {
		if arg == "--" && i+1 < len(os.Args) {
			mode = os.Args[i+1]
			break
		}
	}
	switch mode {
	case "stdin":
		data, _ := io.ReadAll(os.Stdin)
		_, _ = os.Stdout.Write(bytes.ToUpper(data))
	case "file":
		path := os.Args[len(os.Args)-1]
		data, _ := os.ReadFile(path)
		_ = os.WriteFile(path, bytes.ToUpper(data), 0o600)
	case "args":
		_, _ = io.WriteString(os.Stdout, os.Args[len(os.Args)-1])
	case "sleep":
		time.Sleep(10 * time.Second)
	case "large":
		_, _ = io.WriteString(os.Stdout, strings.Repeat("x", 1<<20))
	}
	os.Exit(0)
}

func testRegistry(t *testing.T, max int64, timeout time.Duration, definitions ...Formatter) (*Registry, string) {
	t.Helper()
	root := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for i := range definitions {
		definitions[i].Command[0] = executable
	}
	registry, err := NewRegistry(Config{
		Workspace: root, MaxOutputBytes: max, Timeout: timeout,
		Environment: map[string]string{"PARROT_FORMATTER_HELPER": "1"},
	}, definitions...)
	if err != nil {
		t.Fatal(err)
	}
	return registry, root
}

func helperCommand(mode string, rest ...string) []string {
	return append([]string{"", "-test.run=TestFormatterHelperProcess", "--", mode}, rest...)
}

func TestPlanStaleHashProposalAndFileIsolation(t *testing.T) {
	registry, root := testRegistry(t, 1<<20, 5*time.Second,
		Formatter{Name: "stdin", Extensions: []string{".go"}, Command: helperCommand("stdin"), Mode: ModeStdin},
		Formatter{Name: "file", Extensions: []string{".txt"}, Command: helperCommand("file", "{file}"), Mode: ModeFile},
	)
	goPath := filepath.Join(root, "source.go")
	before := []byte("package p\n")
	if err := os.WriteFile(goPath, before, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Plan(goPath, Hash([]byte("stale"))); !errors.Is(err, ErrStale) {
		t.Fatalf("stale plan error = %v", err)
	}
	plan, err := registry.Plan(goPath, Hash(before))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(goPath, []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Format(context.Background(), plan); !errors.Is(err, ErrStale) {
		t.Fatalf("stale format error = %v", err)
	}
	if err := os.WriteFile(goPath, before, 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := registry.Format(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if string(result.Proposed) != "PACKAGE P\n" || !result.Changed || result.Diff == "" {
		t.Fatalf("result = %#v", result)
	}
	assertFile(t, goPath, before)

	textPath := filepath.Join(root, "source.txt")
	if err := os.WriteFile(textPath, []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	filePlan, err := registry.Plan(textPath, Hash([]byte("hello\n")))
	if err != nil {
		t.Fatal(err)
	}
	fileResult, err := registry.Format(context.Background(), filePlan)
	if err != nil || string(fileResult.Proposed) != "HELLO\n" {
		t.Fatalf("file result = %#v, %v", fileResult, err)
	}
	assertFile(t, textPath, []byte("hello\n"))
}

func TestArgvNoShellDeterministicSelectionTimeoutAndCap(t *testing.T) {
	registry, root := testRegistry(t, 256, 5*time.Second,
		Formatter{Name: "z-last", Extensions: []string{"go"}, Command: helperCommand("args", "{file}"), Mode: ModeStdin},
		Formatter{Name: "a-first", Extensions: []string{"go"}, Command: helperCommand("args", "{file}"), Mode: ModeStdin},
		Formatter{Name: "large", Extensions: []string{"big"}, Command: helperCommand("large"), Mode: ModeStdin},
	)
	path := filepath.Join(root, "name; touch injected.go")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan, err := registry.Plan(path, Hash([]byte("x")))
	if err != nil {
		t.Fatal(err)
	}
	if plan.Formatter != "a-first" || plan.Command[len(plan.Command)-1] != plan.Path {
		t.Fatalf("non-deterministic or altered argv: %#v", plan)
	}
	result, err := registry.Format(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if string(result.Proposed) != filepath.Join(os.TempDir(), filepath.Base(string(result.Proposed))) && strings.Contains(string(result.Proposed), "touch injected") == false {
		// The helper receives one literal temporary-file argument; no shell parses the semicolon.
		t.Fatalf("unexpected argv output %q", result.Proposed)
	}
	if _, err := os.Stat(filepath.Join(root, "injected.go")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("filename was interpreted as shell syntax")
	}

	for extension, wanted := range map[string]error{"big": ErrOutputLimit} {
		file := filepath.Join(root, "x."+extension)
		if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		plan, err := registry.Plan(file, Hash([]byte("x")))
		if err != nil {
			t.Fatal(err)
		}
		_, err = registry.Format(context.Background(), plan)
		if !errors.Is(err, wanted) {
			t.Fatalf(".%s error = %v, want %v", extension, err, wanted)
		}
	}
	timeoutRegistry, timeoutRoot := testRegistry(t, 256, 500*time.Millisecond,
		Formatter{Name: "sleep", Extensions: []string{"slow"}, Command: helperCommand("sleep"), Mode: ModeStdin},
	)
	timeoutPath := filepath.Join(timeoutRoot, "x.slow")
	if err := os.WriteFile(timeoutPath, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	timeoutPlan, err := timeoutRegistry.Plan(timeoutPath, Hash([]byte("x")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := timeoutRegistry.Format(context.Background(), timeoutPlan); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout error = %v", err)
	}

	warning := registry.Pipeline(context.Background(), filepath.Join(root, "missing.go"), Hash(nil))
	if warning.Warning == "" {
		t.Fatal("pipeline did not convert failure to warning")
	}
}

func assertFile(t *testing.T, path string, wanted []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, wanted) {
		t.Fatalf("file = %q, %v; want %q", got, err, wanted)
	}
}
