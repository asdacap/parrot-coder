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

func TestEditFullReplaceAndStale(t *testing.T) {
	ctx := context.Background()
	ws := testWorkspace(t)
	path := filepath.Join(ws.Root(), "file.txt")
	before := append([]byte{0xef, 0xbb, 0xbf}, []byte("one\r\ntwo\r\n")...)
	if err := os.WriteFile(path, before, 0o640); err != nil {
		t.Fatal(err)
	}
	service := NewService(Config{})
	replacement := []byte("replaced\nverbatim")
	plan, err := service.PlanEdit(ctx, ws, Edit{Path: "file.txt", ExpectedSHA256: SHA256(before), New: string(replacement)})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(plan.Mutations[0].After.Data, replacement) {
		t.Fatalf("full replace not verbatim: %q", plan.Mutations[0].After.Data)
	}
	if err := service.Commit(ctx, ws, plan); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
	if _, err := service.PlanEdit(ctx, ws, Edit{Path: "file.txt", ExpectedSHA256: SHA256(before), New: "four"}); !errors.Is(err, ErrStale) {
		t.Fatalf("stale hash error = %v", err)
	}
}

func TestEditExplicitCreation(t *testing.T) {
	ctx := context.Background()
	ws := testWorkspace(t)
	service := NewService(Config{})
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

// aiderBlock renders one SEARCH/REPLACE block. An empty path repeats the
// previous block's file, and an empty search section creates the file.
func aiderBlock(path, search, replace string) string {
	block := ""
	if path != "" {
		block = path + "\n"
	}
	block += patchSearchMarker + "\n"
	if search != "" {
		block += search + "\n"
	}
	block += "=======\n"
	if replace != "" {
		block += replace + "\n"
	}
	return block + patchReplaceMarker + "\n"
}

func TestPatchAddUpdateAndStrictRejections(t *testing.T) {
	ctx := context.Background()
	ws := testWorkspace(t)
	for name, data := range map[string]string{"update": "old\n", "second": "one\ntwo\n"} {
		if err := os.WriteFile(filepath.Join(ws.Root(), name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// The final block omits its path line, so it continues editing "second".
	patch := aiderBlock("added", "", "new") +
		aiderBlock("update", "old", "changed") +
		aiderBlock("second", "one", "ONE") +
		aiderBlock("", "two", "TWO")
	service := NewService(Config{})
	plan, err := service.PlanPatch(ctx, ws, patch, PatchFormatAider)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Mutations) != 3 || !strings.Contains(plan.Diff, "+++ b/added") {
		t.Fatalf("plan = %#v", plan)
	}
	if err := service.Commit(ctx, ws, plan); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"added": "new\n", "update": "changed\n", "second": "ONE\nTWO\n"} {
		got, err := os.ReadFile(filepath.Join(ws.Root(), name))
		if err != nil || string(got) != want {
			t.Fatalf("%s = %q, %v", name, got, err)
		}
	}

	malformed := map[string]string{
		"no blocks":            "just prose\n",
		"block without path":   patchSearchMarker + "\na\n=======\nb\n" + patchReplaceMarker,
		"missing divider":      "file\n" + patchSearchMarker + "\na\n" + patchReplaceMarker,
		"missing replace":      "file\n" + patchSearchMarker + "\na\n=======\nb",
		"escaping path":        aiderBlock("../escape", "", "x"),
		"absolute path":        aiderBlock("/etc/passwd", "", "x"),
		"empty creation":       aiderBlock("empty", "", ""),
		"creation then update": aiderBlock("file", "", "x") + aiderBlock("", "x", "y"),
		"update then creation": aiderBlock("file", "x", "y") + aiderBlock("", "", "z"),
		"path nested in path":  aiderBlock("dir", "", "x") + aiderBlock("dir/file", "", "y"),
	}
	for name, input := range malformed {
		if _, err := ParsePatch(input); err == nil {
			t.Errorf("%s accepted: %q", name, input)
		}
	}
	if _, err := service.PlanPatch(ctx, ws, aiderBlock("absent", "x", "y"), PatchFormatAider); err == nil {
		t.Fatal("update of missing file accepted")
	}
}

func TestPatchUpdateSemantics(t *testing.T) {
	ctx := context.Background()
	ws := testWorkspace(t)
	service := NewService(Config{})
	tests := []struct {
		name   string
		before []byte
		blocks string
		want   []byte
	}{
		{
			name:   "later blocks seek forward past earlier matches",
			before: []byte("same\nfirst\nsame\nsecond\n"),
			blocks: aiderBlock("", "same\nfirst", "same\nFIRST") + aiderBlock("", "same\nsecond", "same\nSECOND"),
			want:   []byte("same\nFIRST\nsame\nSECOND\n"),
		},
		{
			name:   "surrounding context disambiguates",
			before: []byte("before\nfunc greet():\n    old()\nafter\n"),
			blocks: aiderBlock("", "func greet():\n    old()", "func greet():\n    new()"),
			want:   []byte("before\nfunc greet():\n    new()\nafter\n"),
		},
		{
			name:   "trailing whitespace fallback",
			before: []byte("value   \n"),
			blocks: aiderBlock("", "value", "changed"),
			want:   []byte("changed\n"),
		},
		{
			name:   "preserve BOM and CRLF",
			before: append([]byte{0xef, 0xbb, 0xbf}, []byte("old\r\n")...),
			blocks: aiderBlock("", "old", "new"),
			want:   append([]byte{0xef, 0xbb, 0xbf}, []byte("new\r\n")...),
		},
		{
			name:   "replacement may delete lines",
			before: []byte("one\ntwo\nthree\n"),
			blocks: aiderBlock("", "one\ntwo", "only"),
			want:   []byte("only\nthree\n"),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			name := strings.ReplaceAll(tc.name, " ", "_")
			if err := os.WriteFile(filepath.Join(ws.Root(), name), tc.before, 0o600); err != nil {
				t.Fatal(err)
			}
			plan, err := service.PlanPatch(ctx, ws, name+"\n"+tc.blocks, PatchFormatAider)
			if err != nil {
				t.Fatal(err)
			}
			if got := plan.Mutations[0].After.Data; !bytes.Equal(got, tc.want) {
				t.Fatalf("after = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPatchRejectsAmbiguousSearch(t *testing.T) {
	ctx := context.Background()
	service := NewService(Config{})
	tests := []struct {
		name    string
		before  []byte
		blocks  string
		wantErr bool
	}{
		{
			name:    "repeated line is ambiguous",
			before:  []byte("target\nmiddle\ntarget\n"),
			blocks:  aiderBlock("", "target", "CHANGED"),
			wantErr: true,
		},
		{
			name:    "surrounding lines disambiguate",
			before:  []byte("target\nmiddle\ntarget\n"),
			blocks:  aiderBlock("", "target\nmiddle", "CHANGED\nmiddle"),
			wantErr: false,
		},
		{
			// The exact pass matches once, so the looser trimming passes that
			// would also match the indented copy never run.
			name:    "exact match wins before looser passes widen",
			before:  []byte("value\n    value\n"),
			blocks:  aiderBlock("", "value", "changed"),
			wantErr: false,
		},
		{
			// Neither copy matches exactly, and both match once trimmed.
			name:    "ambiguity found by a looser pass still errors",
			before:  []byte("  value\n    value\n"),
			blocks:  aiderBlock("", "value", "changed"),
			wantErr: true,
		},
		{
			// Each block starts seeking past the previous one, so identical
			// edits at known-distinct sites stay unambiguous.
			name:    "sequential blocks walk forward",
			before:  []byte("dup\ndup\n"),
			blocks:  aiderBlock("", "dup\ndup", "first\nsecond"),
			wantErr: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ws := testWorkspace(t)
			if err := os.WriteFile(filepath.Join(ws.Root(), "file"), tc.before, 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := service.PlanPatch(ctx, ws, "file\n"+tc.blocks, PatchFormatAider)
			if tc.wantErr && !errors.Is(err, ErrConflict) {
				t.Fatalf("err = %v, want ErrConflict", err)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("err = %v, want success", err)
			}
		})
	}
}

func TestPatchSyntaxTolerance(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "CRLF line endings",
			input: strings.ReplaceAll(aiderBlock("file", "old", "new"), "\n", "\r\n"),
			want:  "new\n",
		},
		{
			name:  "fenced code block around the edit",
			input: "file\n```go\n" + aiderBlock("", "old", "new") + "```\n",
			want:  "new\n",
		},
		{
			name:  "blank lines between path and markers",
			input: "file\n\n" + aiderBlock("", "old", "new") + "\n",
			want:  "new\n",
		},
		{
			name:  "over-long divider",
			input: "file\n" + patchSearchMarker + "\nold\n==========\nnew\n" + patchReplaceMarker,
			want:  "new\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ws := testWorkspace(t)
			if err := os.WriteFile(filepath.Join(ws.Root(), "file"), []byte("old\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			plan, err := NewService(Config{}).PlanPatch(context.Background(), ws, tc.input, PatchFormatAider)
			if err != nil {
				t.Fatal(err)
			}
			if got := string(plan.Mutations[0].After.Data); got != tc.want {
				t.Fatalf("after = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPatchCreationOverwritesExistingFile(t *testing.T) {
	ctx := context.Background()
	ws := testWorkspace(t)
	if err := os.WriteFile(filepath.Join(ws.Root(), "added"), []byte("old add\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	service := NewService(Config{})
	plan, err := service.PlanPatch(ctx, ws, aiderBlock("added", "", "new add"), PatchFormatAider)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Commit(ctx, ws, plan); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(ws.Root(), "added"))
	if err != nil || string(data) != "new add\n" {
		t.Fatalf("added = %q, %v", data, err)
	}
}

func TestPatchCommitFailureRemovesCreatedParentDirectories(t *testing.T) {
	ctx := context.Background()
	ws := testWorkspace(t)
	planner := NewService(Config{})
	patch := aiderBlock("deep/nested/a.txt", "", "first") + aiderBlock("deep/nested/b.txt", "", "second")
	plan, err := planner.PlanPatch(ctx, ws, patch, PatchFormatAider)
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
	patch := aiderBlock("a", "old", "new") + aiderBlock("b", "old", "new")
	planner := NewService(Config{})
	plan, err := planner.PlanPatch(ctx, ws, patch, PatchFormatAider)
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
		linkPlan, err := planner.PlanEdit(ctx, ws, Edit{Path: "link", ExpectedSHA256: SHA256([]byte("safe")), New: "changed"})
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
