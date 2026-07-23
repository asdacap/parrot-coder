package systemcontext

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestCLIUtilitiesSourceListsAvailableUtilities(t *testing.T) {
	observation, err := (CLIUtilitiesSource{Available: []string{"bash", "git"}}).Observe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(observation.Baseline, "Available CLI utilities: bash, git") {
		t.Fatalf("baseline = %q", observation.Baseline)
	}
	if string(observation.Value) != `["bash","git"]` {
		t.Fatalf("value = %s", observation.Value)
	}
}

func TestCLIUtilitiesSourceReportsNoneAvailable(t *testing.T) {
	observation, err := (CLIUtilitiesSource{}).Observe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if observation.Baseline != "Available CLI utilities: none" {
		t.Fatalf("baseline = %q", observation.Baseline)
	}
}

func TestOptionalCLIUtilitiesSourceListsOnlyDetectedUtilities(t *testing.T) {
	observation, err := (OptionalCLIUtilitiesSource{Available: []string{"bat", "python3"}}).Observe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if observation.Baseline != "Available optional CLI utilities: bat, python3" {
		t.Fatalf("baseline = %q", observation.Baseline)
	}
	if string(observation.Value) != `["bat","python3"]` {
		t.Fatalf("value = %s", observation.Value)
	}
}

func TestBuiltinsIncludeCLIUtilitiesSource(t *testing.T) {
	root := t.TempDir()
	sources, err := Builtins(BuiltinOptions{AgentPrompt: "configured agent prompt", ProjectRoot: root, WorkingDirectory: root, AvailableCLIUtilities: []string{"bash", "stat"}, AvailableOptionalCLIUtilities: []string{"bat", "node"}})
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
	observation, ok := snapshot["runtime:cli-utilities"]
	if !ok || observation.Baseline != "Available CLI utilities: bash, stat" {
		t.Fatalf("CLI utility observation = %#v", observation)
	}
	if strings.Contains(snapshot["runtime:environment"].Baseline, "CLI utilities") {
		t.Fatalf("environment source contains CLI utility inventory: %q", snapshot["runtime:environment"].Baseline)
	}
	optional, ok := snapshot["runtime:optional-cli-utilities"]
	if !ok || optional.Baseline != "Available optional CLI utilities: bat, node" {
		t.Fatalf("optional CLI utility observation = %#v", optional)
	}
	prompt, ok := snapshot["agent:prompt"]
	if !ok || prompt.Baseline != "configured agent prompt" || string(prompt.Value) != `"configured agent prompt"` {
		t.Fatalf("agent prompt observation = %#v", prompt)
	}
}

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
	loaded, err := ObserveAgentsFiles(context.Background(), sources)
	if err != nil {
		t.Fatal(err)
	}
	wantPaths := []string{filepath.Join(config, "AGENTS.md"), filepath.Join(root, "AGENTS.md"), filepath.Join(root, "a", "AGENTS.md"), filepath.Join(cwd, "AGENTS.md")}
	if !reflect.DeepEqual(loaded, wantPaths) {
		t.Fatalf("loaded AGENTS files = %#v, want %#v", loaded, wantPaths)
	}
}

func TestObserveAgentsFilesReportsNoMissingFiles(t *testing.T) {
	root := t.TempDir()
	sources, err := Builtins(BuiltinOptions{ProjectRoot: root, WorkingDirectory: root})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := ObserveAgentsFiles(context.Background(), sources)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 0 {
		t.Fatalf("loaded AGENTS files = %#v, want none", loaded)
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

func TestSubagentsSourceListsAvailableSubagents(t *testing.T) {
	observation, err := (SubagentsSource{Available: []string{"explorer", "review", "worker"}}).Observe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if observation.Baseline != "Available subagents: explorer, review, worker" {
		t.Fatalf("baseline = %q", observation.Baseline)
	}
	if string(observation.Value) != `["explorer","review","worker"]` {
		t.Fatalf("value = %s", observation.Value)
	}
}

func TestSubagentsSourceReportsNoneAvailable(t *testing.T) {
	observation, err := (SubagentsSource{}).Observe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if observation.Baseline != "Available subagents: none" {
		t.Fatalf("baseline = %q", observation.Baseline)
	}
}

func TestBuiltinsIncludeSubagentsSource(t *testing.T) {
	root := t.TempDir()
	sources, err := Builtins(BuiltinOptions{
		ProjectRoot:      root,
		WorkingDirectory: root,
		Subagents:        []string{"explorer", "worker"},
	})
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
	observation, ok := snapshot["runtime:subagents"]
	if !ok || observation.Baseline != "Available subagents: explorer, worker" {
		t.Fatalf("subagent observation = %#v", observation)
	}
}

func TestBuiltinsIncludeToolSystemGuidanceWhenNonEmpty(t *testing.T) {
	root := t.TempDir()
	for _, test := range []struct {
		name     string
		guidance string
		wantKey  bool
	}{
		{name: "non-empty guidance registers source", guidance: "exec_command runs sandboxed", wantKey: true},
		{name: "empty guidance omits source", guidance: "", wantKey: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			sources, err := Builtins(BuiltinOptions{
				ProjectRoot:        root,
				WorkingDirectory:   root,
				ToolSystemGuidance: test.guidance,
			})
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
			observation, ok := snapshot["runtime:tool-system-guidance"]
			if test.wantKey {
				if !ok || observation.Baseline != test.guidance {
					t.Fatalf("tool-system-guidance = %#v, want %q", observation, test.guidance)
				}
			} else {
				if ok {
					t.Fatalf("tool-system-guidance should be absent, got %#v", observation)
				}
			}
		})
	}
}
