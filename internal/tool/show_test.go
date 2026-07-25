package tool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/amirulashraf/parrot-coder/internal/workspace"
)

type displayRecorder struct{ displays []CodeDisplay }

func (r *displayRecorder) DisplayCode(display CodeDisplay) { r.displays = append(r.displays, display) }

func TestShowDisplaysBoundedRawSource(t *testing.T) {
	root := t.TempDir()
	ws, err := workspace.New(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	recorder := &displayRecorder{}
	item := NewShowTool(ReadConfig{MaxLines: 20, MaxBytes: 1024})
	plan, err := item.Plan(context.Background(), json.RawMessage(`{"path":"main.go","offset":2,"limit":2}`), CallContext{Workspace: ws})
	if err != nil {
		t.Fatal(err)
	}
	result, err := item.Execute(context.Background(), plan, CallContext{Workspace: ws, Displays: recorder})
	if err != nil {
		t.Fatal(err)
	}
	if len(recorder.displays) != 1 || recorder.displays[0] != (CodeDisplay{Source: "\nfunc main() {}\n", Path: "main.go", Language: "go", StartLine: 2}) {
		t.Fatalf("displays = %#v", recorder.displays)
	}
	if strings.Contains(recorder.displays[0].Source, "2:") || strings.Contains(recorder.displays[0].Source, "sha256") {
		t.Fatalf("display contains read formatting: %q", recorder.displays[0].Source)
	}
	if result.Text == "" || result.ModelText == "" {
		t.Fatalf("result = %#v", result)
	}
}

func TestShowRejectsInvalidFilesAndDoesNotPublishFailures(t *testing.T) {
	root := t.TempDir()
	ws, err := workspace.New(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "binary"), []byte{'a', 0, 'b'}, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "huge"), []byte(strings.Repeat("x", 100)), 0o600); err != nil {
		t.Fatal(err)
	}
	item := NewShowTool(ReadConfig{MaxLines: 10, MaxBytes: 32})
	recorder := &displayRecorder{}
	for _, test := range []struct {
		name, input string
	}{
		{name: "directory", input: `{"path":"."}`},
		{name: "binary", input: `{"path":"binary"}`},
		{name: "oversized", input: `{"path":"huge"}`},
		{name: "negative range", input: `{"path":"binary","offset":-1}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			plan, planErr := item.Plan(context.Background(), json.RawMessage(test.input), CallContext{Workspace: ws})
			if planErr == nil {
				_, planErr = item.Execute(context.Background(), plan, CallContext{Workspace: ws, Displays: recorder})
			}
			if planErr == nil {
				t.Fatal("invalid show succeeded")
			}
		})
	}
	if len(recorder.displays) != 0 {
		t.Fatalf("failed show published %#v", recorder.displays)
	}
}
