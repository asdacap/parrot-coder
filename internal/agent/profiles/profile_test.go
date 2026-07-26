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

	profile := New("test", "test prompt", hardRules, 12, 2, true, sandboxRules, statusProvider)
	hardRules[0] = "mutated input"
	sandboxRules[0].Path = "/mutated-input"

	gotHardRules := profile.HardRules()
	gotRules := profile.Rules()
	if profile.ID() != "test" || profile.Prompt() != "test prompt" || profile.MaxTurns() != 12 || profile.RecursionLimit() != 2 || !profile.IsReadOnly() {
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

func TestBuiltinProfilesPreserveConfiguration(t *testing.T) {
	tests := []struct {
		name           string
		profile        Profile
		id             string
		maxTurns       int
		recursionLimit int
		readOnly       bool
		statusKey      string
	}{
		{"build", Build(), BuildID, 64, 3, false, "profile:build"},
		{"plan", Plan(), PlanID, 24, 1, true, "profile:plan-agent"},
		{"explorer", Explorer(), ExplorerID, 32, 3, true, "profile:explorer"},
		{"review", Review(), ReviewID, 32, 3, true, "profile:review"},
		{"worker", Worker(), WorkerID, 64, 3, false, "profile:worker"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.profile.ID() != test.id || test.profile.Prompt() == "" || len(test.profile.HardRules()) == 0 {
				t.Fatalf("profile identity/instructions = (%q, %q, %v)", test.profile.ID(), test.profile.Prompt(), test.profile.HardRules())
			}
			if test.profile.MaxTurns() != test.maxTurns || test.profile.RecursionLimit() != test.recursionLimit || test.profile.IsReadOnly() != test.readOnly {
				t.Fatalf("profile limits/security = (%d, %d, %t), want (%d, %d, %t)", test.profile.MaxTurns(), test.profile.RecursionLimit(), test.profile.IsReadOnly(), test.maxTurns, test.recursionLimit, test.readOnly)
			}
			if test.profile.Status() == nil || test.profile.Status().Key() != test.statusKey {
				t.Fatalf("status key = %v, want %q", test.profile.Status(), test.statusKey)
			}
		})
	}
}
