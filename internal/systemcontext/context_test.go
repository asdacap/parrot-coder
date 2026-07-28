package systemcontext

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

type testSource struct {
	key         string
	observation Observation
	err         error
	observed    bool
}

func (s *testSource) Key() string { return s.key }
func (s *testSource) Observe(context.Context) (Observation, error) {
	s.observed = true
	return s.observation, s.err
}

func TestRegistryRejectsInvalidAndDuplicateSourcesAndRendersStableKeyOrder(t *testing.T) {
	z := &testSource{key: "test:z", observation: Observation{Available: true, Text: "Z"}}
	a := &testSource{key: "test:a", observation: Observation{Available: true, Text: "A"}}
	registry, err := NewRegistry(z, a)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(a); err == nil {
		t.Fatal("duplicate source was accepted")
	}
	for _, source := range []Source{nil, &testSource{key: "invalid"}} {
		if err := registry.Register(source); err == nil {
			t.Fatalf("invalid source %#v was accepted", source)
		}
	}
	prompt, err := registry.GetSystemPrompt(context.Background(), ModelSelection{})
	if err != nil {
		t.Fatal(err)
	}
	if prompt != "A\n\nZ" {
		t.Fatalf("prompt = %q", prompt)
	}
}

func TestRegistryObservationReportsAgentsFilesFromSameSnapshot(t *testing.T) {
	agent := &testSource{key: "agents:project", observation: Observation{Available: true, Text: "instructions", Path: "/work/AGENTS.md"}}
	registry, err := NewRegistry(agent, &testSource{key: "runtime:test", observation: Observation{Available: true, Text: "runtime", Path: "/not/agents"}})
	if err != nil {
		t.Fatal(err)
	}
	observation, err := registry.ObserveSystemPrompt(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := observation.Text, "instructions\n\nruntime"; got != want {
		t.Fatalf("Text = %q, want %q", got, want)
	}
	if got, want := observation.AgentsFiles, []string{"/work/AGENTS.md"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("AgentsFiles = %#v, want %#v", got, want)
	}
	if !agent.observed {
		t.Fatal("AGENTS.md source was not observed")
	}
}

func TestGetSystemPromptOmitsAvailableEmptyObservations(t *testing.T) {
	registry, err := NewRegistry(
		&testSource{key: "test:empty", observation: Observation{Available: true, Text: " \n"}},
		&testSource{key: "test:included", observation: Observation{Available: true, Text: "included"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	prompt, err := registry.GetSystemPrompt(context.Background(), ModelSelection{})
	if err != nil {
		t.Fatal(err)
	}
	if prompt != "included" {
		t.Fatalf("prompt = %q", prompt)
	}
}

func TestGetSystemPromptReportsAllSourceAndNilRegistryErrors(t *testing.T) {
	failing := &testSource{key: "test:failing", err: errors.New("offline")}
	later := &testSource{key: "test:later", observation: Observation{Available: true, Text: "later"}}
	unavailable := &testSource{key: "test:unavailable"}
	registry, _ := NewRegistry(failing, later, unavailable)
	prompt, err := registry.GetSystemPrompt(context.Background(), ModelSelection{})
	if err == nil || !strings.Contains(err.Error(), "test:failing: offline") || !strings.Contains(err.Error(), "test:unavailable: source unavailable") {
		t.Fatalf("error = %v", err)
	}
	if prompt != "later" || !failing.observed || !later.observed || !unavailable.observed {
		t.Fatalf("prompt = %q, observed = %t/%t/%t", prompt, failing.observed, later.observed, unavailable.observed)
	}
	var missing *Registry
	if _, err := missing.GetSystemPrompt(context.Background(), ModelSelection{}); err == nil {
		t.Fatal("nil registry succeeded")
	}
}
