package systemcontext

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func observeSources(t *testing.T, sources []Source) map[string]Observation {
	t.Helper()
	observations := make(map[string]Observation, len(sources))
	for _, source := range sources {
		observation, err := source.Observe(context.Background())
		if err != nil {
			t.Fatalf("observe %s: %v", source.Key(), err)
		}
		observations[source.Key()] = observation
	}
	return observations
}

func TestBuiltinsRenderOnlyConfiguredModelAliasesInNameOrder(t *testing.T) {
	root := t.TempDir()
	sources, err := Builtins(BuiltinOptions{
		AgentPrompt: "prompt", ProjectRoot: root, WorkingDirectory: root,
		ModelAliases: NewModelAliasesSource([]ModelAlias{
			{Name: "xhigh_llm", ModelString: "provider/model/xhigh", Usage: "exceptionally difficult work"},
			{Name: "low_llm", ModelString: "provider/model/low", Usage: "routine work"},
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	observation, ok := observeSources(t, sources)["runtime:model-aliases"]
	if !ok {
		t.Fatal("model alias source is absent")
	}
	low := strings.Index(observation.Text, "- low_llm: provider/model/low — routine work")
	xhigh := strings.Index(observation.Text, "- xhigh_llm: provider/model/xhigh — exceptionally difficult work")
	if low < 0 || xhigh < 0 || low > xhigh || !strings.Contains(observation.Text, "agent_spawn model argument") {
		t.Fatalf("model alias observation = %q", observation.Text)
	}

	sources, err = Builtins(BuiltinOptions{AgentPrompt: "prompt", ProjectRoot: root, WorkingDirectory: root})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := observeSources(t, sources)["runtime:model-aliases"]; ok {
		t.Fatal("empty model aliases unexpectedly produced a source")
	}
}

func TestModelAliasesSourceUpdateChangesSystemContextPrompt(t *testing.T) {
	source := NewModelAliasesSource([]ModelAlias{{Name: "low_llm", Usage: "routine work"}})
	registry, err := NewRegistry(source)
	if err != nil {
		t.Fatal(err)
	}
	before, err := registry.GetSystemContextPrompt(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if before != "" {
		t.Fatalf("prompt before configuration = %q", before)
	}

	source.Set([]ModelAlias{{Name: "low_llm", ModelString: "provider/model/low", Usage: "routine work"}})
	after, err := registry.GetSystemContextPrompt(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(after, "- low_llm: provider/model/low — routine work") {
		t.Fatalf("prompt after configuration = %q", after)
	}
}

func TestCLIUtilitiesSourceListsAvailableUtilities(t *testing.T) {
	observation, err := (CLIUtilitiesSource{Available: []string{"bash", "git"}}).Observe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(observation.Text, "Available CLI utilities: bash, git") {
		t.Fatalf("baseline = %q", observation.Text)
	}
}

func TestCLIUtilitiesSourceReportsNoneAvailable(t *testing.T) {
	observation, err := (CLIUtilitiesSource{}).Observe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if observation.Text != "Available CLI utilities: none" {
		t.Fatalf("baseline = %q", observation.Text)
	}
}

func TestOptionalCLIUtilitiesSourceListsOnlyDetectedUtilities(t *testing.T) {
	observation, err := (OptionalCLIUtilitiesSource{Available: []string{"bat", "python3"}}).Observe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if observation.Text != "Available optional CLI utilities: bat, python3" {
		t.Fatalf("baseline = %q", observation.Text)
	}
}

func TestBuiltinsIncludeCLIUtilitiesSource(t *testing.T) {
	root := t.TempDir()
	sources, err := Builtins(BuiltinOptions{AgentPrompt: "configured agent prompt", ProjectRoot: root, WorkingDirectory: root, AvailableCLIUtilities: []string{"bash", "stat"}, AvailableOptionalCLIUtilities: []string{"bat", "node"}})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := observeSources(t, sources)
	observation, ok := snapshot["runtime:cli-utilities"]
	if !ok || observation.Text != "Available CLI utilities: bash, stat" {
		t.Fatalf("CLI utility observation = %#v", observation)
	}
	if strings.Contains(snapshot["runtime:environment"].Text, "CLI utilities") {
		t.Fatalf("environment source contains CLI utility inventory: %q", snapshot["runtime:environment"].Text)
	}
	optional, ok := snapshot["runtime:optional-cli-utilities"]
	if !ok || optional.Text != "Available optional CLI utilities: bat, node" {
		t.Fatalf("optional CLI utility observation = %#v", optional)
	}
	prompt, ok := snapshot["agent:prompt"]
	if !ok || prompt.Text != "configured agent prompt" {
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
	snapshot := observeSources(t, sources)
	var got []string
	for _, key := range []string{"agents:global", "agents:project-0000", "agents:project-0001", "agents:project-0002"} {
		observation, ok := snapshot[key]
		if !ok || !observation.Available {
			t.Fatalf("missing %s: %#v", key, observation)
		}
		if observation.Path == "" || filepath.Base(observation.Path) != "AGENTS.md" {
			t.Fatalf("%s path = %q", key, observation.Path)
		}
		got = append(got, observation.Text)
	}
	want := []string{"Global AGENTS.md:\nglobal", "AGENTS.md at " + root + ":\nroot", "AGENTS.md at " + filepath.Join(root, "a") + ":\na", "AGENTS.md at " + cwd + ":\nb"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("AGENTS order = %#v, want %#v", got, want)
	}
	for _, observation := range snapshot {
		if strings.HasSuffix(observation.Text, "\noutside") {
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
	observation, err := (SubagentsSource{Available: []Subagent{
		{ID: "explorer", Usage: "investigate"},
		{ID: "review", Usage: "review changes"},
		{ID: "worker", Usage: "implement"},
	}}).Observe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := "Available subagents; delegate according to their configured usage:\n- explorer: investigate\n- review: review changes\n- worker: implement"
	if observation.Text != want {
		t.Fatalf("baseline = %q, want %q", observation.Text, want)
	}
}

func TestSubagentsSourceReportsNoneAvailable(t *testing.T) {
	observation, err := (SubagentsSource{}).Observe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if observation.Text != "Available subagents: none" {
		t.Fatalf("baseline = %q", observation.Text)
	}
}

func TestBuiltinsIncludeSubagentsSource(t *testing.T) {
	root := t.TempDir()
	sources, err := Builtins(BuiltinOptions{
		ProjectRoot:      root,
		WorkingDirectory: root,
		Subagents:        []Subagent{{ID: "explorer", Usage: "investigate"}, {ID: "worker", Usage: "implement"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := observeSources(t, sources)
	observation, ok := snapshot["runtime:subagents"]
	if !ok || observation.Text != "Available subagents; delegate according to their configured usage:\n- explorer: investigate\n- worker: implement" {
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
			snapshot := observeSources(t, sources)
			observation, ok := snapshot["runtime:tool-system-guidance"]
			if test.wantKey {
				if !ok || observation.Text != test.guidance {
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
