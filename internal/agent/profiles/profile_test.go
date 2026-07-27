package profiles

import (
	"context"
	"testing"

	"github.com/amirulashraf/parrot-coder/internal/security"
	"github.com/amirulashraf/parrot-coder/internal/status"
)

func TestNewPreservesValuesAndDefensiveCopies(t *testing.T) {
	hardRules := []string{"original rule"}
	sandboxRules := []security.Rule{{Path: "/workspace", Action: security.ActionAllowWrite}}
	statusProvider := status.Static{ProviderKey: "profile:test", Text: "test status"}

	profile := New("test", "test prompt", "test usage", hardRules, 12, 2, true, sandboxRules, statusProvider)
	hardRules[0] = "mutated input"
	sandboxRules[0].Path = "/mutated-input"

	gotHardRules := profile.HardRules()
	gotRules := profile.Rules()
	if profile.ID() != "test" || profile.Prompt() != "test prompt" || profile.Usage() != "test usage" || profile.MaxTurns() != 12 || profile.RecursionLimit() != 2 || !profile.IsReadOnly() {
		t.Fatalf("profile values = (%q, %q, %d, %d, %t), want constructor values", profile.ID(), profile.Prompt(), profile.MaxTurns(), profile.RecursionLimit(), profile.IsReadOnly())
	}
	if len(gotHardRules) != 1 || gotHardRules[0] != "original rule" {
		t.Fatalf("HardRules() = %v, want [original rule]", gotHardRules)
	}
	if len(gotRules) != 1 || gotRules[0].Path != "/workspace" || gotRules[0].Action != security.ActionAllowWrite {
		t.Fatalf("Rules() = %v, want original sandbox rule", gotRules)
	}

	gotHardRules[0] = "mutated result"
	gotRules[0].Path = "/mutated-result"
	if profile.HardRules()[0] != "original rule" || profile.Rules()[0].Path != "/workspace" {
		t.Fatal("slice accessor exposed profile storage")
	}

	if profile.Status() != statusProvider {
		t.Fatalf("Status() = %#v, want supplied provider", profile.Status())
	}
	observation, err := profile.Status().Observe(context.Background(), status.Query{})
	if err != nil || !observation.Available || observation.Text != "test status" {
		t.Fatalf("status observation = %#v, %v, want available test status", observation, err)
	}
}

func TestStableProfileIDs(t *testing.T) {
	if ExplorerID != "explorer" || ReviewID != "review" || WorkerID != "worker" {
		t.Fatalf("profile IDs changed: %q %q %q", ExplorerID, ReviewID, WorkerID)
	}
}
