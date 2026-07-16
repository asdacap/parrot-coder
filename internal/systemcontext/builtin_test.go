package systemcontext

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestBuiltinsDiscoverAgentsFromGlobalAndRootToWorkingDirectory(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "project")
	cwd := filepath.Join(root, "a", "b")
	config := filepath.Join(parent, "config")
	for _, dir := range []string{cwd, config} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		filepath.Join(parent, "AGENTS.md"):    "outside",
		filepath.Join(config, "AGENTS.md"):    "global",
		filepath.Join(root, "AGENTS.md"):      "root",
		filepath.Join(root, "a", "AGENTS.md"): "a",
		filepath.Join(cwd, "AGENTS.md"):       "b",
	}
	for path, content := range files {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	sources, err := Builtins(BuiltinOptions{AgentPrompt: "agent", ConfigDir: config, ProjectRoot: root, WorkingDirectory: cwd})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := NewRegistry(sources...)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := registry.Observe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, key := range []string{"agents:global", "agents:project-0000", "agents:project-0001", "agents:project-0002"} {
		observation, ok := snapshot[key]
		if !ok || !observation.Available {
			t.Fatalf("missing %s: %#v", key, observation)
		}
		if observation.Path == "" || filepath.Base(observation.Path) != "AGENTS.md" {
			t.Fatalf("%s path = %q", key, observation.Path)
		}
		got = append(got, string(observation.Value))
	}
	want := []string{`"global"`, `"root"`, `"a"`, `"b"`}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("AGENTS order = %#v, want %#v", got, want)
	}
	for _, observation := range snapshot {
		if string(observation.Value) == `"outside"` {
			t.Fatal("AGENTS discovery escaped the project root")
		}
	}
}

func TestBuiltinsRejectWorkingDirectoryOutsideCanonicalRoot(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	outside := filepath.Join(parent, "outside")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Builtins(BuiltinOptions{ProjectRoot: root, WorkingDirectory: outside}); err == nil {
		t.Fatal("outside working directory was accepted")
	}
	link := filepath.Join(root, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Builtins(BuiltinOptions{ProjectRoot: root, WorkingDirectory: link}); err == nil {
		t.Fatal("symlink escape was accepted")
	}
}

func TestFileSourceRejectsOversizedInput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "AGENTS.md")
	if err := os.WriteFile(path, []byte("12345"), 0o600); err != nil {
		t.Fatal(err)
	}
	source := FileSource{SourceKey: "agents:test", Path: path, Label: "test", MaxBytes: 4}
	if _, err := source.Observe(context.Background()); err == nil {
		t.Fatal("oversized context file was accepted")
	}
}
