package project

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestResolveUsesCanonicalPath(t *testing.T) {
	dir := t.TempDir()
	info, err := Resolve(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Root != canonicalPath(dir) {
		t.Fatalf("Root = %q, want %q", info.Root, canonicalPath(dir))
	}
	if info.ID != StableID(dir) {
		t.Fatalf("ID = %q, want %q", info.ID, StableID(dir))
	}
	if len(info.ID) != 64 {
		t.Fatalf("ID length = %d, want 64", len(info.ID))
	}
}

func TestResolveDifferentDirectoriesHaveDifferentIDs(t *testing.T) {
	root := t.TempDir()
	dir1 := filepath.Join(root, "a")
	dir2 := filepath.Join(root, "b")
	if err := os.MkdirAll(dir1, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir2, 0o755); err != nil {
		t.Fatal(err)
	}
	info1, err := Resolve(context.Background(), dir1)
	if err != nil {
		t.Fatal(err)
	}
	info2, err := Resolve(context.Background(), dir2)
	if err != nil {
		t.Fatal(err)
	}
	if info1.ID == info2.ID {
		t.Fatalf("IDs should differ: %q vs %q", info1.ID, info2.ID)
	}
}
