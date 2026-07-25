package tool

import (
	"testing"

	"github.com/amirulashraf/parrot-coder/internal/skill"
	statusinfo "github.com/amirulashraf/parrot-coder/internal/status"
	"github.com/amirulashraf/parrot-coder/internal/subagent"
	"github.com/amirulashraf/parrot-coder/internal/webfetch"
)

func TestRegisterBuiltinsDefinitions(t *testing.T) {
	skills, err := skill.Discover(skill.Options{ProjectRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	registry := NewRegistry()
	statuses, err := statusinfo.NewRegistry(statusinfo.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	services := BuiltinServices{
		Skills:   skills,
		WebFetch: webfetch.New(webfetch.Config{}),
		Children: managerAgentChildren{manager: subagent.NewManager(nil, subagent.Config{})},
		Agents:   func(string) (bool, error) { return true, nil },
		Status:   statuses,
	}
	if err := RegisterBuiltins(registry, services); err != nil {
		t.Fatal(err)
	}
	definitions := registry.Definitions()
	want := []string{
		"agent_send", "agent_spawn", "apply_patch", "create_goal", "exec_command", "get_goal", "git_diff", "glob", "question",
		"read", "read_output", "request_write_permission", "rg", "set_config", "skill", "status", "task_interrupt", "task_list_active", "todoread", "todowrite", "update_goal", "wait_agent", "wait_process", "web_fetch", "write_stdin",
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
