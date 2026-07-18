package command

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiscoverPrecedenceAndExpand(t *testing.T) {
	root := t.TempDir()
	global := filepath.Join(root, "config")
	project := filepath.Join(root, "project")
	cwd := filepath.Join(project, "child")
	mkdir(t, cwd)
	write(t, filepath.Join(global, "commands", "review.md"), "---\ndescription: global\n---\nglobal")
	write(t, filepath.Join(project, ".parrot", "commands", "review.md"), "---\ndescription: project\nagent: audit\nmodel: precise\nsubtask: true\n---\nReview $1 ${2}: $ARGUMENTS\n@note.txt")
	write(t, filepath.Join(project, "note.txt"), "FILE")

	registry, err := Discover(Options{GlobalConfig: global, ProjectRoot: project, CWD: cwd})
	if err != nil {
		t.Fatal(err)
	}
	items := registry.List()
	if len(items) != 1 || items[0].Description != "project" || len(items[0].Provenance) != 2 {
		t.Fatalf("items = %#v", items)
	}
	expansion, err := registry.Expand("review", `one "two words"`)
	if err != nil {
		t.Fatal(err)
	}
	if expansion.Prompt != "Review one two words: one \"two words\"\nFILE" {
		t.Fatalf("prompt = %q", expansion.Prompt)
	}
	if expansion.Agent != "audit" || expansion.Model != "precise" || !expansion.Subtask {
		t.Fatalf("metadata = %#v", expansion)
	}
	if len(expansion.SourceHashes) != 2 {
		t.Fatalf("hashes = %#v", expansion.SourceHashes)
	}
	if got := Builtins(); len(got) != 2 || got[0].Name != "compact" || got[1].Name != "new" {
		t.Fatalf("builtins = %#v", got)
	}
}

func TestRejectsShellAndFileEscape(t *testing.T) {
	project := t.TempDir()
	write(t, filepath.Join(project, ".parrot", "commands", "bad.md"), "---\ndescription: bad\n---\n!`touch owned`")
	_, err := Discover(Options{ProjectRoot: project, CWD: project})
	if !errors.Is(err, ErrShell) {
		t.Fatalf("error = %v", err)
	}

	os.RemoveAll(filepath.Join(project, ".parrot"))
	write(t, filepath.Join(project, ".parrot", "commands", "escape.md"), "---\ndescription: escape\n---\n@../secret")
	registry, err := Discover(Options{ProjectRoot: project, CWD: project})
	if err != nil {
		t.Fatal(err)
	}
	_, err = registry.Expand("escape", "")
	if !errors.Is(err, ErrOutsideRoot) {
		t.Fatalf("error = %v", err)
	}
}

func TestFileSubstitutionDoesNotFollowEscapingSymlink(t *testing.T) {
	project := t.TempDir()
	external := t.TempDir()
	write(t, filepath.Join(external, "secret"), "secret")
	if err := os.Symlink(external, filepath.Join(project, "outside")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(project, ".parrot", "commands", "escape.md"), "---\ndescription: escape\n---\n@outside/secret")
	registry, err := Discover(Options{ProjectRoot: project, CWD: project})
	if err != nil {
		t.Fatal(err)
	}
	_, err = registry.Expand("escape", "")
	if !errors.Is(err, ErrOutsideRoot) {
		t.Fatalf("error = %v", err)
	}
}

func TestDiscoveryRejectsEscapingCommandRootSymlink(t *testing.T) {
	project := t.TempDir()
	external := t.TempDir()
	write(t, filepath.Join(external, "outside.md"), "---\ndescription: outside\n---\nbody")
	mkdir(t, filepath.Join(project, ".parrot"))
	if err := os.Symlink(external, filepath.Join(project, ".parrot", "commands")); err != nil {
		t.Fatal(err)
	}
	_, err := Discover(Options{ProjectRoot: project, CWD: project})
	if !errors.Is(err, ErrOutsideRoot) {
		t.Fatalf("error = %v", err)
	}
}

func TestCommandBoundsAndSourceChanges(t *testing.T) {
	project := t.TempDir()
	path := filepath.Join(project, ".parrot", "commands", "one.md")
	write(t, path, "---\ndescription: one\n---\nbody")
	registry, err := Discover(Options{ProjectRoot: project, CWD: project, Limits: Limits{MaxCommands: 1}})
	if err != nil {
		t.Fatal(err)
	}
	write(t, path, "---\ndescription: changed\n---\nbody")
	_, err = registry.Load("one")
	if !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("error = %v", err)
	}

	write(t, filepath.Join(project, ".parrot", "commands", "two.md"), "---\ndescription: two\n---\n"+strings.Repeat("x", 10))
	_, err = Discover(Options{ProjectRoot: project, CWD: project, Limits: Limits{MaxCommands: 1}})
	if !errors.Is(err, ErrLimit) {
		t.Fatalf("error = %v", err)
	}
}

func mkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	mkdir(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
