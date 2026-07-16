package tool

import (
	"testing"

	"github.com/amirulashraf/parrot-coder/internal/skill"
	"github.com/amirulashraf/parrot-coder/internal/subagent"
	"github.com/amirulashraf/parrot-coder/internal/webfetch"
)

func TestRegisterBuiltinsDefinitions(t *testing.T) {
	skills, err := skill.Discover(skill.Options{ProjectRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	registry := NewRegistry()
	services := BuiltinServices{
		Skills:    skills,
		WebFetch:  webfetch.New(webfetch.Config{}),
		Subagents: subagent.NewManager(nil, subagent.Config{}),
		Agents:    func(string) (bool, error) { return true, nil },
	}
	if err := RegisterBuiltins(registry, services); err != nil {
		t.Fatal(err)
	}
	definitions := registry.Definitions()
	want := []string{
		"apply_patch", "edit", "git_diff", "glob", "grep", "question",
		"read", "read_output", "request_write_permission", "review", "shell", "skill", "task",
		"task_cancel", "task_status", "todoread", "todowrite", "unrestricted_shell", "web_fetch",
	}
	if len(definitions) != len(want) {
		t.Fatalf("definitions = %#v", definitions)
	}
	for i := range want {
		if definitions[i].ID != want[i] {
			t.Fatalf("definition[%d] = %q, want %q", i, definitions[i].ID, want[i])
		}
	}
}
