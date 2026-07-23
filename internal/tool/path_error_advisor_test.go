package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/amirulashraf/parrot-coder/internal/change"
	"github.com/amirulashraf/parrot-coder/internal/workspace"
)

type pathContentSearcherFunc func(context.Context, string, []string) (string, bool, error)

func (f pathContentSearcherFunc) Search(ctx context.Context, root string, queries []string) (string, bool, error) {
	return f(ctx, root, queries)
}

func TestPathErrorAdvisorAddsExactBasenameAndContentHints(t *testing.T) {
	ws := pathErrorAdvisorWorkspace(t, map[string]string{
		"pkg/config.go":        "package config\n",
		"pkg/config.go.backup": "package backup\n",
		"pkg/config_test.go":   "package config\n",
	})
	var searchedRoot string
	var searchedQueries []string
	searcher := pathContentSearcherFunc(func(_ context.Context, root string, queries []string) (string, bool, error) {
		searchedRoot = root
		searchedQueries = append([]string(nil), queries...)
		return "docs/guide.md:5:load config.go\ncmd/main.go:7:config reader\n", false, nil
	})
	original := &os.PathError{Op: "open", Path: filepath.Join(ws.Root(), "old", "config.go"), Err: os.ErrNotExist}

	got := NewPathErrorAdvisor(ws.Root(), searcher).Advise(context.Background(), original, ErrorAdvice{Paths: []ErrorAdvicePath{{Path: "old/config.go"}}})
	want := original.Error() + `

Advisor:
Files named "config.go" exist at:
- pkg/config.go
"config.go" (or its filename stem) appears at:
- cmd/main.go:7:config reader
- docs/guide.md:5:load config.go`
	if got.Error() != want {
		t.Fatalf("advised error:\n%s\nwant:\n%s", got, want)
	}
	if !errors.Is(got, os.ErrNotExist) {
		t.Fatalf("errors.Is(%v, os.ErrNotExist) = false", got)
	}
	var pathError *os.PathError
	if !errors.As(got, &pathError) || pathError != original {
		t.Fatalf("errors.As returned %#v, want original %#v", pathError, original)
	}
	if searchedRoot != ws.Root() || !reflect.DeepEqual(searchedQueries, []string{"config.go", "config"}) {
		t.Fatalf("content search = (%q, %#v), want (%q, %#v)", searchedRoot, searchedQueries, ws.Root(), []string{"config.go", "config"})
	}
	for _, nearMatch := range []string{"pkg/config.go.backup", "pkg/config_test.go"} {
		if strings.Contains(got.Error(), nearMatch) {
			t.Fatalf("error contains non-exact basename hint %q: %s", nearMatch, got)
		}
	}
}

func TestPathErrorAdvisorLeavesIrrelevantErrorsUnchanged(t *testing.T) {
	searchCalls := 0
	advisor := NewPathErrorAdvisor(t.TempDir(), pathContentSearcherFunc(func(context.Context, string, []string) (string, bool, error) {
		searchCalls++
		return "unexpected", false, nil
	}))
	tests := []struct {
		name string
		err  error
	}{
		{name: "nil", err: nil},
		{name: "non-path not-exist", err: fmt.Errorf("wrapped: %w", os.ErrNotExist)},
		{name: "path permission", err: &os.PathError{Op: "open", Path: "config.go", Err: os.ErrPermission}},
		{name: "ordinary error", err: errors.New("ordinary failure")},
		{name: "joined unrelated path error", err: errors.Join(
			&os.PathError{Op: "open", Path: "config.go", Err: os.ErrPermission},
			os.ErrNotExist,
		)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := advisor.Advise(context.Background(), test.err, ErrorAdvice{Paths: []ErrorAdvicePath{{Path: "config.go"}}}); got != test.err {
				t.Fatalf("Advise(%v) = %v, want the original error unchanged", test.err, got)
			}
		})
	}
	if searchCalls != 0 {
		t.Fatalf("content search called %d times for irrelevant errors", searchCalls)
	}
}

func TestPathErrorAdvisorIntegratesWithWorkspacePathTools(t *testing.T) {
	ws := pathErrorAdvisorWorkspace(t, map[string]string{
		"pkg/config.go": "package config\n",
	})
	patchInput, err := json.Marshal(map[string]string{
		"patchText": "old/config.go\n<<<<<<< SEARCH\npackage config\n=======\npackage updated\n>>>>>>> REPLACE\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name  string
		tool  Tool
		input json.RawMessage
	}{
		{name: "read", tool: NewReadTool(ReadConfig{}), input: json.RawMessage(`{"path":"old/config.go"}`)},
		{name: "grep", tool: NewGrepTool(GrepConfig{}), input: json.RawMessage(`{"pattern":"package","path":"old/config.go"}`)},
		{name: "apply patch", tool: NewApplyPatchTool(change.NewService(change.Config{})), input: patchInput},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := pathErrorAdvisorExecute(t, test.tool, test.input, CallContext{Workspace: ws}, NewPathErrorAdvisor(ws.Root(), nil))
			if !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("error identity = %v, want os.ErrNotExist", err)
			}
			if !strings.Contains(err.Error(), "Files named \"config.go\" exist at:\n- pkg/config.go") {
				t.Fatalf("error lacks exact-basename advice: %s", err)
			}
		})
	}
}

func TestPathErrorAdvisorSuggestsOnlyExactPatchContent(t *testing.T) {
	ws := pathErrorAdvisorWorkspace(t, map[string]string{
		"pkg/renamed.go": "const first = 1\nconst second = 2\n",
		"pkg/decoy.go":   "const first = 1\nconst unrelated = true\nconst second = 2\n",
	})
	input, err := json.Marshal(map[string]string{
		"patchText": "obsolete.go\n<<<<<<< SEARCH\nconst first = 1\nconst second = 2\n=======\nreplacement\n>>>>>>> REPLACE\n",
	})
	if err != nil {
		t.Fatal(err)
	}

	err = pathErrorAdvisorExecute(t, NewApplyPatchTool(change.NewService(change.Config{})), input, CallContext{Workspace: ws}, NewPathErrorAdvisor(ws.Root(), nil))
	if !strings.Contains(err.Error(), "Files containing the exact requested text:\n- pkg/renamed.go") {
		t.Fatalf("error lacks exact content advice: %s", err)
	}
	if strings.Contains(err.Error(), "pkg/decoy.go") {
		t.Fatalf("error contains noncontiguous content match: %s", err)
	}
}

func pathErrorAdvisorWorkspace(t *testing.T, files map[string]string) *workspace.Workspace {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ws, err := workspace.New(root)
	if err != nil {
		t.Fatal(err)
	}
	return ws
}

func pathErrorAdvisorExecute(t *testing.T, target Tool, input json.RawMessage, call CallContext, advisor ErrorAdvisor) error {
	t.Helper()
	registry := NewRegistry()
	if err := registry.Register(target); err != nil {
		t.Fatal(err)
	}
	_, err := (Executor{Snapshot: registry.Materialize(), ErrorAdvisor: advisor}).Execute(context.Background(), target.ID(), input, call)
	if err == nil {
		t.Fatal("tool unexpectedly succeeded")
	}
	return err
}
