package change

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/amirulashraf/parrot-coder/internal/workspace"
)

// The workspace every unified diff test starts from.
const unifiedBaseFile = "one\ntwo\nthree\nfour\n"

func TestUnifiedDiffPlanning(t *testing.T) {
	// want maps a requested path to its content after the patch; an empty
	// string means the file must be deleted.
	tests := []struct {
		name  string
		input string
		want  map[string]string
	}{
		{
			name:  "single hunk with context",
			input: "--- a/file\n+++ b/file\n@@ -1,3 +1,3 @@\n one\n-two\n+TWO\n three\n",
			want:  map[string]string{"file": "one\nTWO\nthree\nfour\n"},
		},
		{
			name:  "two hunks in one file",
			input: "--- a/file\n+++ b/file\n@@ -1,2 +1,2 @@\n-one\n+ONE\n two\n@@ -3,2 +3,2 @@\n three\n-four\n+FOUR\n",
			want:  map[string]string{"file": "ONE\ntwo\nthree\nFOUR\n"},
		},
		{
			name:  "git headers and timestamps are ignored",
			input: "diff --git a/file b/file\nindex 1234567..89abcde 100644\n--- a/file\t2026-07-20 10:00:00\n+++ b/file\t2026-07-20 10:01:00\n@@ -2 +2 @@\n-two\n+TWO\n",
			want:  map[string]string{"file": "one\nTWO\nthree\nfour\n"},
		},
		{
			name:  "bare paths without a and b prefixes",
			input: "--- file\n+++ file\n@@ -2 +2 @@\n-two\n+TWO\n",
			want:  map[string]string{"file": "one\nTWO\nthree\nfour\n"},
		},
		{
			name:  "no newline marker is tolerated",
			input: "--- a/file\n+++ b/file\n@@ -4 +4 @@\n-four\n+FOUR\n\\ No newline at end of file\n",
			want:  map[string]string{"file": "one\ntwo\nthree\nFOUR\n"},
		},
		{
			name:  "dev null source creates the file",
			input: "diff --git a/added b/added\nnew file mode 100644\n--- /dev/null\n+++ b/added\n@@ -0,0 +1,2 @@\n+alpha\n+beta\n",
			want:  map[string]string{"added": "alpha\nbeta\n"},
		},
		{
			name:  "dev null target deletes the file",
			input: "diff --git a/file b/file\ndeleted file mode 100644\n--- a/file\n+++ /dev/null\n@@ -1,4 +0,0 @@\n-one\n-two\n-three\n-four\n",
			want:  map[string]string{"file": ""},
		},
		{
			name:  "several files in one patch",
			input: "--- a/file\n+++ b/file\n@@ -1 +1 @@\n-one\n+ONE\n--- /dev/null\n+++ b/added\n@@ -0,0 +1 @@\n+alpha\n",
			want:  map[string]string{"file": "ONE\ntwo\nthree\nfour\n", "added": "alpha\n"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ws := testWorkspace(t)
			if err := os.WriteFile(filepath.Join(ws.Root(), "file"), []byte(unifiedBaseFile), 0o600); err != nil {
				t.Fatal(err)
			}
			plan, err := NewService(Config{}).PlanPatch(context.Background(), ws, tc.input, PatchFormatUnified)
			if err != nil {
				t.Fatal(err)
			}
			if len(plan.Mutations) != len(tc.want) {
				t.Fatalf("mutations = %d, want %d", len(plan.Mutations), len(tc.want))
			}
			for _, mutation := range plan.Mutations {
				want, ok := tc.want[mutation.RequestedPath]
				if !ok {
					t.Fatalf("unexpected mutation for %q", mutation.RequestedPath)
				}
				if want == "" {
					if mutation.After.Exists {
						t.Fatalf("%q survives the deletion", mutation.RequestedPath)
					}
					continue
				}
				if got := string(mutation.After.Data); got != want {
					t.Fatalf("%s = %q, want %q", mutation.RequestedPath, got, want)
				}
			}
		})
	}
}

func TestUnifiedDiffAbsolutePathRequiresCapability(t *testing.T) {
	ws := testWorkspace(t)
	external := filepath.Join(t.TempDir(), "external")
	if err := os.WriteFile(external, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	input := "--- " + external + "\n+++ " + external + "\n@@ -1 +1 @@\n-old\n+new\n"
	service := NewService(Config{})
	if _, err := service.PlanPatch(context.Background(), ws, input, PatchFormatUnified); !errors.Is(err, workspace.ErrOutsideRoot) {
		t.Fatalf("ordinary workspace absolute path error = %v", err)
	}
	capability, err := workspace.NewExternalPath(external)
	if err != nil {
		t.Fatal(err)
	}
	scoped := ws.WithExternalPaths(capability)
	plan, err := service.PlanPatch(context.Background(), scoped, input, PatchFormatUnified)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Commit(context.Background(), scoped, plan); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(external); err != nil || string(got) != "new\n" {
		t.Fatalf("external = %q, %v", got, err)
	}
}

func TestUnifiedDiffRejections(t *testing.T) {
	malformed := map[string]string{
		"no file headers":        "just prose\n",
		"missing target header":  "--- a/file\n@@ -1 +1 @@\n-one\n+ONE\n",
		"rename":                 "diff --git a/file b/moved\nrename from file\nrename to moved\n",
		"copy":                   "diff --git a/file b/copied\ncopy from file\ncopy to copied\n",
		"binary patch":           "diff --git a/file b/file\nGIT binary patch\n",
		"mismatched paths":       "--- a/file\n+++ b/other\n@@ -1 +1 @@\n-one\n+ONE\n",
		"unprefixed hunk line":   "--- a/file\n+++ b/file\n@@ -1,2 +1,2 @@\n one\nrogue\n",
		"malformed hunk header":  "--- a/file\n+++ b/file\n@@ nonsense @@\n-one\n+ONE\n",
		"miscounted hunk header": "--- a/file\n+++ b/file\n@@ -1,99 +1,99 @@\n one\n-two\n+TWO\n",
		"hunk without lines":     "--- a/file\n+++ b/file\n@@ -0,0 +0,0 @@\n",
		"escaping path":          "--- a/../escape\n+++ b/../escape\n@@ -1 +1 @@\n-one\n+ONE\n",
		"quoted path":            "--- \"a/sp ace\"\n+++ \"b/sp ace\"\n@@ -1 +1 @@\n-one\n+ONE\n",
		"empty creation":         "--- /dev/null\n+++ b/added\n",
		"creation with context":  "--- /dev/null\n+++ b/added\n@@ -1,2 +1,2 @@\n one\n+alpha\n",
		"duplicate file":         "--- a/file\n+++ b/file\n@@ -1 +1 @@\n-one\n+ONE\n--- a/file\n+++ b/file\n@@ -2 +2 @@\n-two\n+TWO\n",
		"nested paths":           "--- /dev/null\n+++ b/dir\n@@ -0,0 +1 @@\n+x\n--- /dev/null\n+++ b/dir/file\n@@ -0,0 +1 @@\n+y\n",
		"NUL byte":               "--- a/file\n+++ b/file\n@@ -1 +1 @@\n-one\n+O\x00E\n",
	}
	for name, input := range malformed {
		if _, err := ParseUnifiedDiff(input); !errors.Is(err, ErrInvalidPatch) {
			t.Errorf("%s: err = %v, want ErrInvalidPatch", name, err)
		}
	}
	if _, err := NewService(Config{}).PlanPatch(context.Background(), testWorkspace(t), "x", PatchFormat("bogus")); !errors.Is(err, ErrInvalidPatch) {
		t.Errorf("unknown format: err = %v, want ErrInvalidPatch", err)
	}
}

func TestUnifiedDiffDeleteCommits(t *testing.T) {
	ctx := context.Background()
	ws := testWorkspace(t)
	if err := os.WriteFile(filepath.Join(ws.Root(), "file"), []byte(unifiedBaseFile), 0o600); err != nil {
		t.Fatal(err)
	}
	service := NewService(Config{})
	plan, err := service.PlanPatch(ctx, ws, "--- a/file\n+++ /dev/null\n@@ -1,4 +0,0 @@\n-one\n-two\n-three\n-four\n", PatchFormatUnified)
	if err != nil {
		t.Fatal(err)
	}
	if mutation := plan.Mutations[0]; !mutation.Before.Exists || mutation.After.Exists {
		t.Fatalf("mutation = %#v", mutation)
	}
	if err := service.Commit(ctx, ws, plan); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(ws.Root(), "file")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file survived the deletion: %v", err)
	}
}

func FuzzParseUnifiedDiff(f *testing.F) {
	for _, seed := range []string{
		"--- a/file\n+++ b/file\n@@ -1,3 +1,3 @@\n one\n-two\n+TWO\n three\n",
		"--- /dev/null\n+++ b/added\n@@ -0,0 +1 @@\n+alpha\n",
		"--- a/file\n+++ /dev/null\n@@ -1 +0,0 @@\n-one\n",
		"diff --git a/file b/moved\nrename from file\nrename to moved\n",
		"--- a/file\n+++ b/file\n@@ -1,99 +1,99 @@\n one\n",
		"not a patch",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		_, _ = ParseUnifiedDiff(input)
	})
}
