package status

import (
	"context"
	"errors"
	"strings"
	"testing"
)

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

func TestSelectionStatus(t *testing.T) {
	observation, err := (Selection{}).Observe(context.Background(), Query{Agent: "worker", Provider: "openai", Model: "gpt", Variant: "high"})
	if err != nil {
		t.Fatal(err)
	}
	if observation.Text != "Active profile: worker\nModel: openai/gpt\nVariant: high" {
		t.Fatalf("selection = %q", observation.Text)
	}
}
