package change

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
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

// aiderBlock renders one SEARCH/REPLACE block. An empty path repeats the
// previous block's file, and an empty search matches a missing or empty file.
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
	if err := os.WriteFile(filepath.Join(ws.Root(), "empty"), nil, 0o640); err != nil {
		t.Fatal(err)
	}
	emptyPlan, err := service.PlanPatch(ctx, ws, aiderBlock("empty", "", "filled"), PatchFormatAider)
	if err != nil {
		t.Fatal(err)
	}
	if got := emptyPlan.Mutations[0]; string(got.After.Data) != "filled\n" || got.After.Mode.Perm() != 0o640 {
		t.Fatalf("empty file mutation = %#v", got)
	}
	if _, err := service.PlanPatch(ctx, ws, aiderBlock("update", "", "overwritten"), PatchFormatAider); !errors.Is(err, ErrConflict) {
		t.Fatalf("non-empty file error = %v, want ErrConflict", err)
	}
	outside := filepath.Join(t.TempDir(), "external")
	if err := os.WriteFile(outside, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	absolutePatch := aiderBlock(outside, "old", "new")
	if _, err := service.PlanPatch(ctx, ws, absolutePatch, PatchFormatAider); !errors.Is(err, workspace.ErrOutsideRoot) {
		t.Fatalf("ordinary workspace absolute path error = %v", err)
	}
	capability, err := workspace.NewExternalPath(outside)
	if err != nil {
		t.Fatal(err)
	}
	externalPlan, err := service.PlanPatch(ctx, ws.WithExternalPaths(capability), absolutePatch, PatchFormatAider)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Commit(ctx, ws.WithExternalPaths(capability), externalPlan); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(outside); err != nil || string(got) != "new\n" {
		t.Fatalf("external = %q, %v", got, err)
	}

	directory := t.TempDir()
	directoryCapability, err := workspace.NewExternalPath(directory)
	if err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(directory, "new", "file")
	nestedPlan, err := service.PlanPatch(ctx, ws.WithExternalPaths(directoryCapability), aiderBlock(nested, "", "created"), PatchFormatAider)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Commit(ctx, ws.WithExternalPaths(directoryCapability), nestedPlan); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(nested); err != nil || string(got) != "created\n" {
		t.Fatalf("nested external = %q, %v", got, err)
	}

	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("untouched\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	swap := filepath.Join(directory, "swap")
	if err := os.WriteFile(swap, []byte("before\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	swapPlan, err := service.PlanPatch(ctx, ws.WithExternalPaths(directoryCapability), aiderBlock(swap, "before", "after"), PatchFormatAider)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(swap); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, swap); err != nil {
		t.Fatal(err)
	}
	if err := service.Commit(ctx, ws.WithExternalPaths(directoryCapability), swapPlan); !errors.Is(err, ErrStale) {
		t.Fatalf("external symlink swap error = %v, want ErrStale", err)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "untouched\n" {
		t.Fatalf("symlink target = %q, %v", got, err)
	}
}

func TestPatchUpdateLineEndingsPreserveExactBytes(t *testing.T) {
	bom := []byte{0xef, 0xbb, 0xbf}
	tests := []struct {
		name       string
		format     PatchFormat
		before     []byte
		patchLines string
		want       []byte
	}{
		{
			name:       "aider LF patch updates CRLF file with BOM",
			format:     PatchFormatAider,
			before:     append(append([]byte(nil), bom...), []byte("keep\r\nold\r\ntail\r\n")...),
			patchLines: "file\n<<<<<<< SEARCH\nold\n=======\nnew\n>>>>>>> REPLACE\n",
			want:       append(append([]byte(nil), bom...), []byte("keep\r\nnew\r\ntail\r\n")...),
		},
		{
			name:       "aider CRLF patch updates LF file",
			format:     PatchFormatAider,
			before:     []byte("keep\nold\ntail\n"),
			patchLines: "file\r\n<<<<<<< SEARCH\r\nold\r\n=======\r\nnew\r\n>>>>>>> REPLACE\r\n",
			want:       []byte("keep\nnew\ntail\n"),
		},
		{
			name:       "aider preserves mixed untouched terminators and uses matched terminator",
			format:     PatchFormatAider,
			before:     []byte("first\r\nold\rsecond\nlast\r\n"),
			patchLines: "file\n<<<<<<< SEARCH\nold\n=======\nnew\nextra\n>>>>>>> REPLACE\n",
			want:       []byte("first\r\nnew\rextra\rsecond\nlast\r\n"),
		},
		{
			name:       "unified LF patch updates mixed file with BOM",
			format:     PatchFormatUnified,
			before:     append(append([]byte(nil), bom...), []byte("before\r\nold\rafter\n")...),
			patchLines: "--- a/file\n+++ b/file\n@@ -1,3 +1,3 @@\n before\n-old\n+new\n after\n",
			want:       append(append([]byte(nil), bom...), []byte("before\r\nnew\rafter\n")...),
		},
		{
			name:       "unified CR patch updates CRLF file",
			format:     PatchFormatUnified,
			before:     []byte("before\r\nold\r\nafter\r\n"),
			patchLines: "--- a/file\r+++ b/file\r@@ -1,3 +1,3 @@\r before\r-old\r+new\r after\r",
			want:       []byte("before\r\nnew\r\nafter\r\n"),
		},
		{
			name:       "aider preserves unterminated final line",
			format:     PatchFormatAider,
			before:     []byte("before\r\nold"),
			patchLines: "file\n<<<<<<< SEARCH\nold\n=======\nnew\nextra\n>>>>>>> REPLACE\n",
			want:       []byte("before\r\nnew\r\nextra"),
		},
		{
			name:       "unified preserves unterminated final line",
			format:     PatchFormatUnified,
			before:     []byte("before\nold"),
			patchLines: "--- a/file\n+++ b/file\n@@ -2 +2,2 @@\n-old\n+new\n+extra\n",
			want:       []byte("before\nnew\nextra"),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ws := testWorkspace(t)
			if err := os.WriteFile(filepath.Join(ws.Root(), "file"), tc.before, 0o600); err != nil {
				t.Fatal(err)
			}
			plan, err := NewService(Config{}).PlanPatch(context.Background(), ws, tc.patchLines, tc.format)
			if err != nil {
				t.Fatal(err)
			}
			if got := plan.Mutations[0].After.Data; !bytes.Equal(got, tc.want) {
				t.Fatalf("after bytes = %q, want %q", got, tc.want)
			}
		})
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

func TestPlanPatchAggregatesDiscoverableErrors(t *testing.T) {
	tests := []struct {
		name      string
		setup     map[string]string
		patch     string
		wantOrder []string
	}{
		{
			name:  "hunks and operations stay in patch order",
			setup: map[string]string{"first": "present\n", "occupied": "content\n"},
			patch: aiderBlock("first", "missing first", "replacement") +
				aiderBlock("", "missing second", "replacement") +
				aiderBlock("absent", "missing third", "replacement") +
				aiderBlock("occupied", "", "replacement"),
			wantOrder: []string{
				`[1/4] change: update "first": hunk 1:`,
				"missing first",
				`[2/4] change: update "first": hunk 2:`,
				"missing second",
				`[3/4] change: update "absent": source is missing`,
				`[4/4] change: add "occupied":`,
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ws := testWorkspace(t)
			for name, data := range tc.setup {
				if err := os.WriteFile(filepath.Join(ws.Root(), name), []byte(data), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			plan, err := NewService(Config{}).PlanPatch(context.Background(), ws, tc.patch, PatchFormatAider)
			if !errors.Is(err, ErrConflict) || !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("error = %v, want joined ErrConflict and os.ErrNotExist", err)
			}
			if len(plan.Mutations) != 0 || len(plan.Directories) != 0 || plan.Diff != "" {
				t.Fatalf("plan = %#v, want zero Plan", plan)
			}
			message := err.Error()
			if !strings.HasPrefix(message, "patch planning failed with 4 errors:\n\n") || strings.Count(message, "\n\n") != 4 {
				t.Fatalf("error format = %q, want header and four blank-line-separated sections", message)
			}
			var aggregate interface{ Unwrap() []error }
			if !errors.As(err, &aggregate) || len(aggregate.Unwrap()) != 4 {
				t.Fatalf("error = %#v, want four directly unwrapped details", err)
			}
			position := -1
			for _, want := range tc.wantOrder {
				next := strings.Index(message, want)
				if next <= position {
					t.Fatalf("error order = %q, want %q after byte %d", message, want, position)
				}
				position = next
			}
		})
	}
}

func TestPatchPlanningErrorFormat(t *testing.T) {
	first := errors.New("first cause")
	second := errors.New("second cause")
	err := newPatchPlanningError(first, second)
	want := "patch planning failed with 2 errors:\n\n[1/2] first cause\n\n[2/2] second cause"
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err, want)
	}
	var aggregate interface{ Unwrap() []error }
	if !errors.As(err, &aggregate) {
		t.Fatalf("error = %#v, want multi-error unwrapping", err)
	}
	unwrapped := aggregate.Unwrap()
	if len(unwrapped) != 2 || unwrapped[0] != first || unwrapped[1] != second {
		t.Fatalf("unwrapped errors = %#v, want original errors in order", unwrapped)
	}
}

func TestPlanPatchSyntaxAndCancellationRemainFailFast(t *testing.T) {
	ws := testWorkspace(t)
	service := NewService(Config{})
	tests := []struct {
		name    string
		ctx     context.Context
		patch   string
		wantErr error
	}{
		{
			name:    "syntax",
			ctx:     context.Background(),
			patch:   "missing\n<<<<<<< SEARCH\nold\n=======\nnew",
			wantErr: ErrInvalidPatch,
		},
		{
			name: "cancellation",
			ctx: func() context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx
			}(),
			patch:   aiderBlock("missing", "old", "new"),
			wantErr: context.Canceled,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := service.PlanPatch(tc.ctx, ws, tc.patch, PatchFormatAider)
			if !errors.Is(err, tc.wantErr) || errors.Is(err, ErrConflict) {
				t.Fatalf("error = %v, want only %v", err, tc.wantErr)
			}
			if len(plan.Mutations) != 0 || len(plan.Directories) != 0 || plan.Diff != "" {
				t.Fatalf("plan = %#v, want zero Plan", plan)
			}
		})
	}
}

func TestPatchRejectsAmbiguousSearch(t *testing.T) {
	ctx := context.Background()
	service := NewService(Config{})
	tests := []struct {
		name           string
		before         []byte
		blocks         string
		wantErr        bool
		wantMatch      string
		maxErrorLength int
	}{
		{
			name:      "repeated line is ambiguous",
			before:    []byte("target\nmiddle\ntarget\n"),
			blocks:    aiderBlock("", "target", "CHANGED"),
			wantErr:   true,
			wantMatch: "this SEARCH block matched 2 locations; include more surrounding lines:\n<<<<<<< SEARCH\ntarget\n=======",
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
		{
			name:      "missing multiline search reports match",
			before:    []byte("one\ntwo\n"),
			blocks:    aiderBlock("", "missing\nlines", "changed"),
			wantErr:   true,
			wantMatch: "failed to find this SEARCH block:\n<<<<<<< SEARCH\nmissing\nlines\n=======",
		},
		{
			name:           "large missing search is bounded",
			before:         []byte("existing\n"),
			blocks:         aiderBlock("", strings.Repeat("x", (8<<10)+100), "changed"),
			wantErr:        true,
			wantMatch:      "100 bytes omitted",
			maxErrorLength: (8 << 10) + 256,
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
			if tc.wantMatch != "" && !strings.Contains(err.Error(), tc.wantMatch) {
				t.Fatalf("err = %v, want failed match %q", err, tc.wantMatch)
			}
			if tc.maxErrorLength > 0 && len(err.Error()) > tc.maxErrorLength {
				t.Fatalf("error length = %d, want at most %d", len(err.Error()), tc.maxErrorLength)
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

}
