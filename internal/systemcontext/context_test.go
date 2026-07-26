package systemcontext

import (
	"context"
	"errors"
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
	prompt, err := registry.GetSystemContextPrompt(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if prompt != "A\n\nZ" {
		t.Fatalf("prompt = %q", prompt)
	}
}

func TestGetSystemContextPromptOmitsAvailableEmptyObservations(t *testing.T) {
	registry, err := NewRegistry(
		&testSource{key: "test:empty", observation: Observation{Available: true, Text: " \n"}},
		&testSource{key: "test:included", observation: Observation{Available: true, Text: "included"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	prompt, err := registry.GetSystemContextPrompt(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if prompt != "included" {
		t.Fatalf("prompt = %q", prompt)
	}
}

func TestGetSystemContextPromptReportsAllSourceAndNilRegistryErrors(t *testing.T) {
	failing := &testSource{key: "test:failing", err: errors.New("offline")}
	later := &testSource{key: "test:later", observation: Observation{Available: true, Text: "later"}}
	unavailable := &testSource{key: "test:unavailable"}
	registry, _ := NewRegistry(failing, later, unavailable)
	prompt, err := registry.GetSystemContextPrompt(context.Background())
	if err == nil || !strings.Contains(err.Error(), "test:failing: offline") || !strings.Contains(err.Error(), "test:unavailable: source unavailable") {
		t.Fatalf("error = %v", err)
	}
	if prompt != "later" || !failing.observed || !later.observed || !unavailable.observed {
		t.Fatalf("prompt = %q, observed = %t/%t/%t", prompt, failing.observed, later.observed, unavailable.observed)
	}
	var missing *Registry
	if _, err := missing.GetSystemContextPrompt(context.Background()); err == nil {
		t.Fatal("nil registry succeeded")
	}
}
