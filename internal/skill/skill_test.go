package skill

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiscoverPrecedenceLoadAndRender(t *testing.T) {
	root := t.TempDir()
	global := filepath.Join(root, "config")
	project := filepath.Join(root, "project")
	cwd := filepath.Join(project, "a", "b")
	mkdir(t, cwd)
	write(t, filepath.Join(global, "skills", "base", "SKILL.md"), "---\nname: review\ndescription: global\nallowed-tools: [Read, Grep]\n---\nglobal body\n")
	write(t, filepath.Join(project, ".parrot", "skills", "local", "SKILL.md"), "---\nname: review\ndescription: root\n---\nroot body\n")
	write(t, filepath.Join(project, "a", ".parrot", "skills", "deep", "SKILL.md"), "---\nname: review\ndescription: deep\nagent: audit\n---\ndeep body\n")

	registry, err := Discover(Options{GlobalConfig: global, ProjectRoot: project, CWD: cwd})
	if err != nil {
		t.Fatal(err)
	}
	items := registry.List()
	if len(items) != 1 || items[0].Description != "deep" || items[0].Source.Kind != "project" {
		t.Fatalf("unexpected metadata: %#v", items)
	}
	if len(items[0].Provenance) != 3 {
		t.Fatalf("provenance = %#v", items[0].Provenance)
	}
	loaded, err := registry.Load("review")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Prompt != "deep body\n" {
		t.Fatalf("prompt = %q", loaded.Prompt)
	}
	rendered := RenderInstruction([]Skill{{Metadata: Metadata{Name: "z", Source: Source{Path: "z"}}, Prompt: "last"}, loaded})
	if strings.Index(rendered, `name="review"`) > strings.Index(rendered, `name="z"`) {
		t.Fatalf("render is not sorted: %s", rendered)
	}
}

func TestInlineQuotedAllowedTools(t *testing.T) {
	project := t.TempDir()
	write(t, filepath.Join(project, ".parrot", "skills", "tools", "SKILL.md"), "---\nname: tools\ndescription: tools\nallowed-tools: [\"Read\", 'Grep']\n---\nbody")
	registry, err := Discover(Options{ProjectRoot: project, CWD: project})
	if err != nil {
		t.Fatal(err)
	}
	tools := registry.List()[0].AllowedTools
	if len(tools) != 2 || tools[0] != "Read" || tools[1] != "Grep" {
		t.Fatalf("tools = %#v", tools)
	}
}

func TestDiscoverRejectsMalformedAndBounds(t *testing.T) {
	project := t.TempDir()
	write(t, filepath.Join(project, ".parrot", "skills", "bad", "SKILL.md"), "---\nname: good\nname: duplicate\ndescription: bad\n---\nbody")
	_, err := Discover(Options{ProjectRoot: project, CWD: project})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("error = %v", err)
	}

	os.RemoveAll(filepath.Join(project, ".parrot"))
	write(t, filepath.Join(project, ".parrot", "skills", "large", "SKILL.md"), "---\nname: large\ndescription: large\n---\n"+strings.Repeat("x", 50))
	_, err = Discover(Options{ProjectRoot: project, CWD: project, Limits: Limits{MaxFileBytes: 40}})
	if !errors.Is(err, ErrLimit) {
		t.Fatalf("error = %v", err)
	}
}

func TestDiscoveryDoesNotFollowEscapingSymlinks(t *testing.T) {
	project := t.TempDir()
	external := t.TempDir()
	write(t, filepath.Join(external, "SKILL.md"), "---\nname: escaped\ndescription: escaped\n---\nbody")
	mkdir(t, filepath.Join(project, ".parrot", "skills"))
	if err := os.Symlink(external, filepath.Join(project, ".parrot", "skills", "escape")); err != nil {
		t.Fatal(err)
	}
	registry, err := Discover(Options{ProjectRoot: project, CWD: project})
	if err != nil {
		t.Fatal(err)
	}
	if len(registry.List()) != 0 {
		t.Fatalf("followed escaping symlink: %#v", registry.List())
	}
}

func TestDiscoveryRejectsEscapingRootSymlink(t *testing.T) {
	project := t.TempDir()
	external := t.TempDir()
	write(t, filepath.Join(external, "item", "SKILL.md"), "---\nname: escaped\ndescription: escaped\n---\nbody")
	mkdir(t, filepath.Join(project, ".parrot"))
	if err := os.Symlink(external, filepath.Join(project, ".parrot", "skills")); err != nil {
		t.Fatal(err)
	}
	_, err := Discover(Options{ProjectRoot: project, CWD: project})
	if !errors.Is(err, ErrOutsideRoot) {
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
