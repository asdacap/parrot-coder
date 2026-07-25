package status

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/amirulashraf/parrot-coder/internal/queue"
	"github.com/amirulashraf/parrot-coder/internal/task"
)

type activeTaskListerFunc func(string) []task.Active

func (list activeTaskListerFunc) ListActive(sessionID string) []task.Active { return list(sessionID) }

type queueListerFunc func(string) ([]queue.Info, error)

func (list queueListerFunc) List(sessionID string) ([]queue.Info, error) { return list(sessionID) }

type providerFunc struct {
	key string
	fn  func(Query) (Observation, error)
}

func (p providerFunc) Key() string { return p.key }
func (p providerFunc) Observe(_ context.Context, query Query) (Observation, error) {
	return p.fn(query)
}

func TestRegistryComposesDynamicStatus(t *testing.T) {
	query := Query{SessionID: "session", Agent: "plan", Provider: "openai", Model: "gpt", Variant: "high"}
	registry, err := NewRegistry(
		providerFunc{key: "runtime:unavailable", fn: func(Query) (Observation, error) { return Observation{}, nil }},
		providerFunc{key: "runtime:selection", fn: func(got Query) (Observation, error) {
			if got != query {
				t.Fatalf("query = %#v", got)
			}
			return Observation{Available: true, Text: "selection"}, nil
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	text, err := registry.Observe(context.Background(), query, Static{ProviderKey: "profile:plan", Text: "profile"})
	if err != nil {
		t.Fatal(err)
	}
	if text != "profile\n\nselection" {
		t.Fatalf("status = %q", text)
	}

	for _, provider := range []Provider{Static{}, Static{ProviderKey: "invalid"}, Static{ProviderKey: "runtime:selection"}} {
		if err := registry.Register(provider); err == nil {
			t.Fatalf("Register(%#v) succeeded", provider)
		}
	}
	failure, _ := NewRegistry(providerFunc{key: "runtime:failure", fn: func(Query) (Observation, error) { return Observation{}, errors.New("failed") }})
	if _, err := failure.Observe(context.Background(), query, nil); err == nil || !strings.Contains(err.Error(), "runtime:failure") {
		t.Fatalf("error = %v", err)
	}
}

func TestActiveTasksStatus(t *testing.T) {
	startedAt := time.Date(2026, time.July, 23, 12, 34, 56, 0, time.UTC)
	tests := []struct {
		name   string
		active []task.Active
		want   string
	}{
		{name: "none", want: "Active tasks: none"},
		{
			name: "stable concise list",
			active: []task.Active{
				{ID: "task_z", SessionID: "other-session", Kind: task.KindAgent, Status: "pending", StartedAt: startedAt, Agent: "worker", Turn: 3, Depth: 2},
				{ID: "proc_a", SessionID: "other-session", Kind: task.KindShell, Status: "running", StartedAt: startedAt},
			},
			want: "Active tasks:\n- proc_a (shell, running)\n- task_z (agent, pending, agent: worker, turn: 3, depth: 2)",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var gotSession string
			provider := NewActiveTasks(activeTaskListerFunc(func(sessionID string) []task.Active {
				gotSession = sessionID
				return test.active
			}))
			observation, err := provider.Observe(context.Background(), Query{SessionID: "session"})
			if err != nil {
				t.Fatal(err)
			}
			if provider.Key() != "runtime:tasks" {
				t.Fatalf("key = %q", provider.Key())
			}
			if gotSession != "session" {
				t.Fatalf("ListActive session = %q", gotSession)
			}
			if !observation.Available || observation.Text != test.want {
				t.Fatalf("observation = %#v, want text %q", observation, test.want)
			}
		})
	}
}

func TestQueuesStatus(t *testing.T) {
	for _, test := range []struct {
		name  string
		items []queue.Info
		want  string
	}{
		{name: "none", want: "Queues: none"},
		{name: "summaries", items: []queue.Info{{Name: "alpha-beta-gamma", Description: "work\n- forged", Size: 2}, {Name: "one-two-three"}}, want: "Queues:\n- alpha-beta-gamma (2 items, description: \"work\\n- forged\")\n- one-two-three (0 items)"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var sessionID string
			provider := NewQueues(queueListerFunc(func(got string) ([]queue.Info, error) { sessionID = got; return test.items, nil }))
			observation, err := provider.Observe(context.Background(), Query{SessionID: "session"})
			if err != nil || provider.Key() != "runtime:queues" || sessionID != "session" || !observation.Available || observation.Text != test.want {
				t.Fatalf("Observe() = %#v, %v; key=%q session=%q", observation, err, provider.Key(), sessionID)
			}
		})
	}
}

func TestSelectionStatus(t *testing.T) {
	tests := []struct {
		name  string
		query Query
		want  string
	}{
		{
			name:  "root with variant",
			query: Query{Agent: "worker", Provider: "openai", Model: "gpt", Variant: "high"},
			want:  "Active profile: worker\nModel: openai/gpt\nVariant: high",
		},
		{
			name:  "named parent",
			query: Query{ParentSessionID: "ses_parent", ParentSessionName: "main-task", Agent: "worker", Provider: "openai", Model: "gpt"},
			want:  "Active profile: worker\nModel: openai/gpt\nParent session: ses_parent (main-task)",
		},
		{
			name:  "unnamed parent",
			query: Query{ParentSessionID: "ses_parent", ParentSessionName: "  ", Agent: "worker", Provider: "openai", Model: "gpt"},
			want:  "Active profile: worker\nModel: openai/gpt\nParent session: ses_parent",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			observation, err := (Selection{}).Observe(context.Background(), test.query)
			if err != nil {
				t.Fatal(err)
			}
			if observation.Text != test.want {
				t.Fatalf("selection = %q, want %q", observation.Text, test.want)
			}
		})
	}
}
