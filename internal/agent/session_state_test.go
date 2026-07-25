package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSessionStateDirectoriesResolvePrepareAndRejectUnsafeIDs(t *testing.T) {
	root := t.TempDir()
	directories, err := NewUserSessionStateDirectories(root)
	if err != nil {
		t.Fatal(err)
	}

	resolved, err := directories.Directory("ses_test")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(root, "session", "ses_test"); resolved.Path() != want || resolved.ScratchPath() != filepath.Join(want, "scratch") {
		t.Fatalf("resolved directory = %q (%q), want %q", resolved.Path(), resolved.ScratchPath(), want)
	}
	if _, err := os.Stat(resolved.Path()); !os.IsNotExist(err) {
		t.Fatalf("Directory provisioned state: %v", err)
	}

	prepared, err := directories.Prepare("ses_test")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{prepared.Path(), prepared.ScratchPath()} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o700 {
			t.Fatalf("%s permissions = %o, want 700", path, info.Mode().Perm())
		}
	}

	if err := directories.Remove("ses_test"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(prepared.Path()); !os.IsNotExist(err) {
		t.Fatalf("Remove left state directory: %v", err)
	}

	for _, id := range []string{"", ".", "..", "../escape", "nested/id", `nested\\id`} {
		if _, err := directories.Prepare(id); err == nil {
			t.Errorf("Prepare(%q) succeeded", id)
		}
	}
	if _, err := NewUserSessionStateDirectories("relative"); err == nil {
		t.Error("relative state root succeeded")
	}
}
