package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestResolveContainmentAndSymlinks(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("symlink semantics")
	}
	root, outside := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("no"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	w, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.ResolveRead("file"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"", "../secret", "escape/secret", "bad\x00path"} {
		if _, err := w.ResolveRead(path); err == nil {
			t.Errorf("ResolveRead(%q) succeeded", path)
		}
	}
	if _, err := w.ResolveCreate("escape/new"); !errors.Is(err, ErrOutsideRoot) {
		t.Fatalf("got %v", err)
	}
}

func TestExternalCapabilityAndPrefixSibling(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "work")
	sibling := filepath.Join(parent, "work-secret")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(sibling, 0o700); err != nil {
		t.Fatal(err)
	}
	w, _ := New(root)
	if _, err := w.ResolveCreate(filepath.Join(sibling, "x")); !errors.Is(err, ErrOutsideRoot) {
		t.Fatalf("prefix sibling allowed: %v", err)
	}
	outsideFile := filepath.Join(sibling, "readable")
	if err := os.WriteFile(outsideFile, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := w.ResolveRead(outsideFile); !errors.Is(err, ErrOutsideRoot) {
		t.Fatalf("external read unexpectedly gained workspace capability: %v", err)
	}
	if resolved, err := w.ResolveReadOnly(outsideFile); err != nil || resolved != outsideFile {
		t.Fatalf("read-only external path = %q, %v", resolved, err)
	}
	if _, err := w.ResolveReadOnlyWithin(w.Root(), outsideFile); !errors.Is(err, ErrOutsideRoot) {
		t.Fatalf("external descendant escaped search root: %v", err)
	}
	if _, err := w.ResolveReadOnly(filepath.Join("..", filepath.Base(sibling), "readable")); !errors.Is(err, ErrOutsideRoot) {
		t.Fatalf("relative read escaped workspace: %v", err)
	}
	capability, err := NewExternalRoot(sibling)
	if err != nil {
		t.Fatal(err)
	}
	w, _ = New(root, capability)
	if _, err := w.ResolveCreate(filepath.Join(capability.Path(), "x")); err != nil {
		t.Fatal(err)
	}
}

func TestResolveCreateSymlinkSwapIsRevalidated(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	dir := filepath.Join(root, "dir")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	w, _ := New(root)
	if _, err := w.ResolveCreate("dir/new"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := w.ResolveCreate("dir/new"); !errors.Is(err, ErrOutsideRoot) {
		t.Fatalf("swap not detected: %v", err)
	}
}
