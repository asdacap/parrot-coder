package tool

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/amirulashraf/parrot-coder/internal/queue"
	"github.com/amirulashraf/parrot-coder/internal/skill"
	statusinfo "github.com/amirulashraf/parrot-coder/internal/status"
	"github.com/amirulashraf/parrot-coder/internal/webfetch"
)

type recordingErrorAdvisor struct{ advice ErrorAdvice }

func (a *recordingErrorAdvisor) Advise(_ context.Context, err error, advice ErrorAdvice) error {
	a.advice = advice
	return err
}

type builtinTestAgentSession struct {
	id       string
	subagent bool
	queues   QueueService
}

func (s builtinTestAgentSession) SessionID() string {
	if s.id != "" {
		return s.id
	}
	return "session"
}
func (builtinTestAgentSession) SessionName() string    { return "test-session" }
func (s builtinTestAgentSession) IsSubagent() bool     { return s.subagent }
func (s builtinTestAgentSession) Queues() QueueService { return s.queues }
func (*agentTestSession) Queues() QueueService         { return nil }
func (builtinTestAgentSession) CreateAgent(context.Context, string, string, string, string, string) (ChildAgent, error) {
	return nil, errors.New("not implemented")
}
func (builtinTestAgentSession) ResolveAgent(string) (ResolvedAgent, error) {
	return ResolvedAgent{}, errors.New("not implemented")
}

func TestQueueToolsHonorCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, kind := range []string{"queue_create", "queue_info", "queue_listen", "queue_push", "queue_take"} {
		t.Run(kind, func(t *testing.T) {
			store, err := queue.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			item := &QueueTool{Kind: kind, Store: store}
			if _, err := item.Plan(ctx, json.RawMessage(`{"name":"work-now","item":"x"}`), CallContext{SessionID: "session"}); !errors.Is(err, context.Canceled) {
				t.Fatalf("Plan() error = %v, want context canceled", err)
			}
		})
	}
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
	firstManager, err := queue.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	secondManager, err := queue.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	firstState := builtinTestAgentSession{id: "first", queues: firstManager}
	secondState := builtinTestAgentSession{id: "second", queues: secondManager}
	first, err := providers.Materialize(firstState)
	if err != nil {
		t.Fatal(err)
	}
	second, err := providers.Materialize(secondState)
	if err != nil {
		t.Fatal(err)
	}
	child, err := providers.Materialize(builtinTestAgentSession{id: "child", subagent: true, queues: firstManager})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := child.tools["question"]; ok {
		t.Fatal("question tool was materialized for a subagent")
	}
	if _, ok := child.tools["read"]; !ok {
		t.Fatal("unrestricted tool was not materialized for a subagent")
	}
	for _, definition := range child.Definitions() {
		if definition.ID == "question" {
			t.Fatal("question tool was advertised to a subagent")
		}
	}
	for id, item := range first.tools {
		if item == second.tools[id] {
			t.Fatalf("tool %q was reused across sessions", id)
		}
	}
	for _, id := range []string{"queue_create", "queue_info", "queue_listen", "queue_push", "queue_take"} {
		firstQueue := first.tools[id].(*QueueTool)
		secondQueue := second.tools[id].(*QueueTool)
		childQueue := child.tools[id].(*QueueTool)
		if firstQueue.Store != firstManager || secondQueue.Store != secondManager || firstQueue.Store == secondQueue.Store {
			t.Fatalf("%s did not bind its tool session's queue manager", id)
		}
		if childQueue.Store != firstQueue.Store {
			t.Fatalf("%s did not preserve the shared parent/child queue manager", id)
		}
	}
	if _, err := firstManager.Create("first-bound-queues", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := secondManager.Create("second-bound-queues", ""); err != nil {
		t.Fatal(err)
	}
	statusText := func(snapshot Snapshot) string {
		t.Helper()
		item := snapshot.tools["status"].(*StatusTool)
		text, err := item.Registry.Observe(context.Background(), statusinfo.Query{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return text
	}
	firstStatus, secondStatus, childStatus := statusText(first), statusText(second), statusText(child)
	if !strings.Contains(firstStatus, "first-bound-queues") || strings.Contains(firstStatus, "second-bound-queues") ||
		!strings.Contains(secondStatus, "second-bound-queues") || strings.Contains(secondStatus, "first-bound-queues") {
		t.Fatalf("status providers were not bound to their tool sessions: first=%q second=%q", firstStatus, secondStatus)
	}
	if childStatus != firstStatus {
		t.Fatalf("shared parent/child manager status differs: parent=%q child=%q", firstStatus, childStatus)
	}
	for _, id := range []string{"set_config", "request_write_permission"} {
		got := ChoicesFor(first.tools[id])
		want := unwrapTool(first.tools[id]).(PermissionChoicer).PermissionChoices()
		if !reflect.DeepEqual(got, want) || reflect.DeepEqual(got, DefaultPermissionChoices()) {
			t.Fatalf("%s permission choices = %#v, want %#v", id, got, want)
		}
	}
	for _, id := range []string{"read", "show_to_user", "rg", "apply_patch"} {
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
		"queue_create", "queue_info", "queue_listen", "queue_push", "queue_take", "read", "request_write_permission", "rg", "set_config", "show_to_user", "skill", "status", "task_interrupt", "task_list_active", "todoread", "todowrite", "update_goal", "wait_agent", "wait_process", "web_fetch", "write_stdin",
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
