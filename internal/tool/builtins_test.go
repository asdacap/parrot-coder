package tool

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/amirulashraf/parrot-coder/internal/skill"
	statusinfo "github.com/amirulashraf/parrot-coder/internal/status"
	"github.com/amirulashraf/parrot-coder/internal/webfetch"
)

type recordingErrorAdvisor struct{ advice ErrorAdvice }

func (a *recordingErrorAdvisor) Advise(_ context.Context, err error, advice ErrorAdvice) error {
	a.advice = advice
	return err
}

type builtinTestAgentSession struct{}

func (builtinTestAgentSession) SessionID() string { return "session" }
func (builtinTestAgentSession) CreateAgent(context.Context, string, string, string, string, string) (ChildAgent, error) {
	return nil, errors.New("not implemented")
}
func (builtinTestAgentSession) ResolveAgent(string) (ResolvedAgent, error) {
	return ResolvedAgent{}, errors.New("not implemented")
}

func TestBuiltinProvidersDefinitions(t *testing.T) {
	skills, err := skill.Discover(skill.Options{ProjectRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	statuses, err := statusinfo.NewRegistry(statusinfo.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	services := BuiltinServices{
		Skills:   skills,
		WebFetch: webfetch.New(webfetch.Config{}),
		Agents:   func(string) (bool, error) { return true, nil },
		Status:   statuses,
	}
	providers, err := BuiltinProviders(services)
	if err != nil {
		t.Fatal(err)
	}
	state := builtinTestAgentSession{}
	first, err := providers.Materialize(state)
	if err != nil {
		t.Fatal(err)
	}
	second, err := providers.Materialize(state)
	if err != nil {
		t.Fatal(err)
	}
	for id, item := range first.tools {
		if item == second.tools[id] {
			t.Fatalf("tool %q was reused across sessions", id)
		}
	}
	for _, id := range []string{"set_config", "request_write_permission"} {
		got := ChoicesFor(first.tools[id])
		want := unwrapTool(first.tools[id]).(PermissionChoicer).PermissionChoices()
		if !reflect.DeepEqual(got, want) || reflect.DeepEqual(got, DefaultPermissionChoices()) {
			t.Fatalf("%s permission choices = %#v, want %#v", id, got, want)
		}
	}
	for _, id := range []string{"read", "show", "rg", "apply_patch"} {
		raw := json.RawMessage(`{"path":"missing"}`)
		if id == "apply_patch" {
			raw = json.RawMessage(`{"patchText":"missing\n<<<<<<< SEARCH\na\n=======\nb\n>>>>>>> REPLACE"}`)
		}
		advisor := &recordingErrorAdvisor{}
		original := errors.New("failed")
		if got := (Executor{ErrorAdvisor: advisor}).advise(context.Background(), original, raw, first.tools[id]); got != original {
			t.Fatalf("%s advice changed error: %v", id, got)
		}
		if len(advisor.advice.Paths) == 0 || advisor.advice.Paths[0].Path != "missing" {
			t.Fatalf("%s error advice = %#v", id, advisor.advice)
		}
	}
	definitions := providers.Definitions()
	want := []string{
		"agent_send", "agent_spawn", "apply_patch", "create_goal", "exec_command", "get_goal", "git_diff", "glob", "question",
		"read", "request_write_permission", "rg", "set_config", "show", "skill", "status", "task_interrupt", "task_list_active", "todoread", "todowrite", "update_goal", "wait_agent", "wait_process", "web_fetch", "write_stdin",
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
