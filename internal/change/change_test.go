package change

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/amirulashraf/parrot-coder/internal/workspace"
)

func testWorkspace(t *testing.T) *workspace.Workspace {
	t.Helper()
	ws, err := workspace.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return ws
}

func TestEditExactBOMCRLFAndStale(t *testing.T) {
	ctx := context.Background()
	ws := testWorkspace(t)
	path := filepath.Join(ws.Root(), "file.txt")
	before := append([]byte{0xef, 0xbb, 0xbf}, []byte("one\r\ntwo\r\n")...)
	if err := os.WriteFile(path, before, 0o640); err != nil {
		t.Fatal(err)
	}
	service := NewService(Config{})
	plan, err := service.PlanEdit(ctx, ws, Edit{Path: "file.txt", ExpectedSHA256: SHA256(before), Old: "two\n", New: "three\n"})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(plan.Mutations[0].After.Data, append([]byte{0xef, 0xbb, 0xbf}, []byte("one\r\nthree\r\n")...)) {
		t.Fatalf("newline/BOM changed: %q", plan.Mutations[0].After.Data)
	}
	if err := service.Commit(ctx, ws, plan); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
	if _, err := service.PlanEdit(ctx, ws, Edit{Path: "file.txt", ExpectedSHA256: SHA256(before), Old: "three", New: "four"}); !errors.Is(err, ErrStale) {
		t.Fatalf("stale hash error = %v", err)
	}
}

func TestEditMatchRulesAndExplicitCreation(t *testing.T) {
	ctx := context.Background()
	ws := testWorkspace(t)
	data := []byte("same same")
	if err := os.WriteFile(filepath.Join(ws.Root(), "file"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	service := NewService(Config{})
	base := Edit{Path: "file", ExpectedSHA256: SHA256(data), Old: "same", New: "new"}
	if _, err := service.PlanEdit(ctx, ws, base); !errors.Is(err, ErrConflict) {
		t.Fatalf("multiple match error = %v", err)
	}
	base.Old = "missing"
	if _, err := service.PlanEdit(ctx, ws, base); !errors.Is(err, ErrConflict) {
		t.Fatalf("zero match error = %v", err)
	}
	base.Old, base.ReplaceAll = "same", true
	plan, err := service.PlanEdit(ctx, ws, base)
	if err != nil || string(plan.Mutations[0].After.Data) != "new new" {
		t.Fatalf("replace all = %#v, %v", plan, err)
	}
	if _, err := service.PlanEdit(ctx, ws, Edit{Path: "new", New: "content"}); err == nil {
		t.Fatal("implicit creation accepted")
	}
	created, err := service.PlanEdit(ctx, ws, Edit{Path: "new", New: "content", Create: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Commit(ctx, ws, created); err != nil {
		t.Fatal(err)
	}
}

func TestPatchAllOperationsAndStrictRejections(t *testing.T) {
	ctx := context.Background()
	ws := testWorkspace(t)
	for name, data := range map[string]string{"update": "old\n", "delete": "gone\n", "move": "move\n"} {
		if err := os.WriteFile(filepath.Join(ws.Root(), name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	patch := "*** Begin Patch\n" +
		"*** Add File: added\n+new\n" +
		"*** Update File: update\n@@\n-old\n+changed\n" +
		"*** Delete File: delete\n" +
		"*** Update File: move\n*** Move to: moved\n@@\n-move\n+moved\n" +
		"*** End Patch\n"
	service := NewService(Config{})
	plan, err := service.PlanPatch(ctx, ws, patch)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Mutations) != 5 || !strings.Contains(plan.Diff, "+++ b/added") {
		t.Fatalf("plan = %#v", plan)
	}
	if err := service.Commit(ctx, ws, plan); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"added": "new\n", "update": "changed\n", "moved": "moved\n"} {
		got, err := os.ReadFile(filepath.Join(ws.Root(), name))
		if err != nil || string(got) != want {
			t.Fatalf("%s = %q, %v", name, got, err)
		}
	}
	for _, name := range []string{"delete", "move"} {
		if _, err := os.Lstat(filepath.Join(ws.Root(), name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s still exists", name)
		}
	}

	malformed := []string{
		"prose\n*** Begin Patch\n*** End Patch",
		"*** Begin Patch\n*** Add File: ../escape\n+x\n*** End Patch",
		"*** Begin Patch\n*** Add File: x\nnot-prefixed\n*** End Patch",
		"*** Begin Patch\n*** Delete File: absent\ntext\n*** End Patch",
		"*** Begin Patch\n*** Update File: a\n*** Move to: b\n*** Update File: b\n*** Move to: a\n*** End Patch",
		"*** Begin Patch\n*** Add File: dir\n+x\n*** Add File: dir/file\n+y\n*** End Patch",
		"*** Begin Patch\n*** Move File: a -> b\n*** Move to: c\n*** Add File: d\n+x\n*** End Patch",
	}
	for _, input := range malformed {
		if _, err := ParsePatch(input); err == nil {
			t.Errorf("malformed patch accepted: %q", input)
		}
	}
	if _, err := service.PlanPatch(ctx, ws, "*** Begin Patch\n*** Delete File: absent\n*** End Patch"); err == nil {
		t.Fatal("missing delete accepted")
	}
}

func TestPatchEndOfFileAnchorsDuplicateAndNormalizesFinalNewline(t *testing.T) {
	ctx := context.Background()
	ws := testWorkspace(t)
	data := []byte("same\r\nsame")
	if err := os.WriteFile(filepath.Join(ws.Root(), "file"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	patch := "*** Begin Patch\n*** Update File: file\n@@\n-same\n+last\n*** End of File\n*** End Patch"
	service := NewService(Config{})
	plan, err := service.PlanPatch(ctx, ws, patch)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(plan.Mutations[0].After.Data); got != "same\r\nlast\r\n" {
		t.Fatalf("after = %q", got)
	}
}

func TestPatchOpenCodeUpdateSemantics(t *testing.T) {
	ctx := context.Background()
	ws := testWorkspace(t)
	service := NewService(Config{})
	tests := []struct {
		name   string
		before []byte
		patch  string
		want   []byte
	}{
		{
			name:   "pure addition to empty file",
			before: nil,
			patch:  "@@\n+First line",
			want:   []byte("First line\n"),
		},
		{
			name:   "multiple chunks seek forward",
			before: []byte("same\nfirst\nsame\nsecond\n"),
			patch:  "@@\n same\n-first\n+FIRST\n@@\n same\n-second\n+SECOND",
			want:   []byte("same\nFIRST\nsame\nSECOND\n"),
		},
		{
			name:   "hunk header context",
			before: []byte("before\nfunc greet():\n    old()\nafter\n"),
			patch:  "@@ func greet():\n-    old()\n+    new()",
			want:   []byte("before\nfunc greet():\n    new()\nafter\n"),
		},
		{
			name:   "trailing whitespace fallback",
			before: []byte("value   \n"),
			patch:  "@@\n-value\n+changed",
			want:   []byte("changed\n"),
		},
		{
			name:   "preserve BOM and CRLF",
			before: append([]byte{0xef, 0xbb, 0xbf}, []byte("old\r\n")...),
			patch:  "@@\n-old\n+new",
			want:   append([]byte{0xef, 0xbb, 0xbf}, []byte("new\r\n")...),
		},
		{
			name:   "sort pure addition before earlier replacement",
			before: []byte("one\ntwo\nthree\n"),
			patch:  "@@\n+last\n@@\n-one\n-two\n-three\n+first",
			want:   []byte("first\nlast\n"),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(ws.Root(), strings.ReplaceAll(tc.name, " ", "_"))
			if err := os.WriteFile(path, tc.before, 0o600); err != nil {
				t.Fatal(err)
			}
			input := "*** Begin Patch\n*** Update File: " + filepath.Base(path) + "\n" + tc.patch + "\n*** End Patch"
			plan, err := service.PlanPatch(ctx, ws, input)
			if err != nil {
				t.Fatal(err)
			}
			if got := plan.Mutations[0].After.Data; !bytes.Equal(got, tc.want) {
				t.Fatalf("after = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPatchRejectsEmptyAddedFile(t *testing.T) {
	if _, err := ParsePatch("*** Begin Patch\n*** Add File: empty\n*** End Patch"); err == nil {
		t.Fatal("empty add hunk accepted")
	}
}

func TestPatchCodexSyntaxCompatibility(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "CRLF and marker whitespace",
			input: " \r\n*** Begin Patch \r\n*** Update File: file\r\n@@\r\n-old\r\n+new\r\n*** End Patch\r\n ",
			want:  "new\n",
		},
		{
			name:  "legacy heredoc wrapper",
			input: "<<'EOF'\n*** Begin Patch\n*** Update File: file\n@@\n-old\n+new\n*** End Patch\nEOF\n",
			want:  "new\n",
		},
		{
			name:  "headerless update",
			input: "*** Begin Patch\n*** Update File: file\n-old\n+new\n*** End Patch",
			want:  "new\n",
		},
		{
			name:  "unprefixed blank context line",
			input: "*** Begin Patch\n*** Update File: file\n@@\n old\n\n tail\n-old\n+new\n*** End Patch",
			want:  "old\n\ntail\nnew\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ws := testWorkspace(t)
			if err := os.WriteFile(filepath.Join(ws.Root(), "file"), []byte("old\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if tc.name == "unprefixed blank context line" {
				if err := os.WriteFile(filepath.Join(ws.Root(), "file"), []byte("old\n\ntail\nold\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			plan, err := NewService(Config{}).PlanPatch(context.Background(), ws, tc.input)
			if err != nil {
				t.Fatal(err)
			}
			if got := string(plan.Mutations[0].After.Data); got != tc.want {
				t.Fatalf("after = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPatchCodexOverwriteSemantics(t *testing.T) {
	ctx := context.Background()
	ws := testWorkspace(t)
	for path, data := range map[string]string{"added": "old add\n", "source": "source\n", "destination": "old destination\n"} {
		if err := os.WriteFile(filepath.Join(ws.Root(), path), []byte(data), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	service := NewService(Config{})
	plan, err := service.PlanPatch(ctx, ws, "*** Begin Patch\n*** Add File: added\n+new add\n*** Update File: source\n*** Move to: destination\n@@\n-source\n+moved\n*** End Patch")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Commit(ctx, ws, plan); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{"added": "new add\n", "destination": "moved\n"} {
		data, err := os.ReadFile(filepath.Join(ws.Root(), path))
		if err != nil || string(data) != want {
			t.Fatalf("%s = %q, %v", path, data, err)
		}
	}
}

func TestPatchAcceptsMoveFileAliasWithUpdateHunks(t *testing.T) {
	ctx := context.Background()
	ws := testWorkspace(t)
	if err := os.MkdirAll(filepath.Join(ws.Root(), "src", "renderer", "utils"), 0o700); err != nil {
		t.Fatal(err)
	}
	oldPath := filepath.Join(ws.Root(), "src", "renderer", "utils", "workspaceFavourites.ts")
	if err := os.WriteFile(oldPath, []byte("import type { Workspace } from '../types'\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	patch := "*** Begin Patch\n" +
		"*** Move File: src/renderer/utils/workspaceFavourites.ts -> src/shared/workspaceFavourites.ts\n" +
		"@@\n" +
		"-import type { Workspace } from '../types'\n" +
		"+import type { Workspace } from './workspaceFile'\n" +
		"*** End Patch"
	service := NewService(Config{})
	plan, err := service.PlanPatch(ctx, ws, patch)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Mutations) != 2 {
		t.Fatalf("mutations = %#v", plan.Mutations)
	}
	if err := service.Commit(ctx, ws, plan); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(oldPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("move source still exists: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(ws.Root(), "src", "shared", "workspaceFavourites.ts"))
	if err != nil || string(data) != "import type { Workspace } from './workspaceFile'\n" {
		t.Fatalf("move destination = %q, %v", data, err)
	}
}

func TestPatchCommitFailureRemovesCreatedParentDirectories(t *testing.T) {
	ctx := context.Background()
	ws := testWorkspace(t)
	planner := NewService(Config{})
	patch := "*** Begin Patch\n" +
		"*** Add File: deep/nested/a.txt\n+first\n" +
		"*** Add File: deep/nested/b.txt\n+second\n" +
		"*** End Patch"
	plan, err := planner.PlanPatch(ctx, ws, patch)
	if err != nil {
		t.Fatal(err)
	}
	failing := NewService(Config{InjectFailure: func(int, string) error { return errors.New("injected") }})
	if err := failing.Commit(ctx, ws, plan); err == nil {
		t.Fatal("injected failure succeeded")
	}
	if _, err := os.Lstat(filepath.Join(ws.Root(), "deep")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("created directories survived failed commit: %v", err)
	}
}

func TestCommitRollbackAndSymlinkSwap(t *testing.T) {
	ctx := context.Background()
	ws := testWorkspace(t)
	for _, name := range []string{"a", "b"} {
		if err := os.WriteFile(filepath.Join(ws.Root(), name), []byte("old\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	patch := "*** Begin Patch\n*** Update File: a\n@@\n-old\n+new\n*** Update File: b\n@@\n-old\n+new\n*** End Patch"
	planner := NewService(Config{})
	plan, err := planner.PlanPatch(ctx, ws, patch)
	if err != nil {
		t.Fatal(err)
	}
	failing := NewService(Config{InjectFailure: func(index int, _ string) error {
		if index == 2 {
			return errors.New("injected")
		}
		return nil
	}})
	if err := failing.Commit(ctx, ws, plan); err == nil {
		t.Fatal("injected failure succeeded")
	}
	for _, name := range []string{"a", "b"} {
		data, _ := os.ReadFile(filepath.Join(ws.Root(), name))
		if string(data) != "old\n" {
			t.Fatalf("%s not rolled back: %q", name, data)
		}
	}

	if runtime.GOOS == "darwin" || runtime.GOOS == "linux" {
		inside := filepath.Join(ws.Root(), "inside")
		link := filepath.Join(ws.Root(), "link")
		outside := filepath.Join(t.TempDir(), "outside")
		_ = os.WriteFile(inside, []byte("safe"), 0o600)
		_ = os.WriteFile(outside, []byte("secret"), 0o600)
		if err := os.Symlink("inside", link); err != nil {
			t.Fatal(err)
		}
		linkPlan, err := planner.PlanEdit(ctx, ws, Edit{Path: "link", ExpectedSHA256: SHA256([]byte("safe")), Old: "safe", New: "changed"})
		if err != nil {
			t.Fatal(err)
		}
		_ = os.Remove(link)
		if err := os.Symlink(outside, link); err != nil {
			t.Fatal(err)
		}
		if err := planner.Commit(ctx, ws, linkPlan); !errors.Is(err, ErrStale) {
			t.Fatalf("symlink swap error = %v", err)
		}
		data, _ := os.ReadFile(outside)
		if string(data) != "secret" {
			t.Fatal("outside file mutated")
		}
	}
}
