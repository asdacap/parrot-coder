package profiles

import (
	"testing"

	"github.com/amirulashraf/parrot-coder/internal/security"
	"github.com/amirulashraf/parrot-coder/internal/status"
)

func TestNewPreservesValuesAndDefensiveCopies(t *testing.T) {
	hardRules := []string{"original rule"}
	allowedTools := []string{"read", "rg"}
	sandboxRules := []security.Rule{{Path: "/workspace", Action: security.ActionAllowWrite}}
	statusProvider := status.Static{ProviderKey: "profile:test", Text: "runtime status"}

	profile := New("test", "test prompt", "test usage", hardRules, allowedTools, 12, 2, true, sandboxRules, statusProvider, true)
	hardRules[0] = "mutated input"
	allowedTools[0] = "mutated_input"
	sandboxRules[0].Path = "/mutated-input"

	gotHardRules := profile.HardRules()
	gotAllowedTools := profile.AllowedTools()
	gotRules := profile.Rules()
	if profile.ID() != "test" || profile.Prompt() != "test prompt" || profile.Usage() != "test usage" || profile.MaxTurns() != 12 || profile.RecursionLimit() != 2 || !profile.IsReadOnly() || !profile.IsUserAgent() || profile.Status() != statusProvider {
		t.Fatalf("profile values = (%q, %q, %d, %d, %t), want constructor values", profile.ID(), profile.Prompt(), profile.MaxTurns(), profile.RecursionLimit(), profile.IsReadOnly())
	}
	if len(gotHardRules) != 1 || gotHardRules[0] != "original rule" {
		t.Fatalf("HardRules() = %v, want [original rule]", gotHardRules)
	}
	if len(gotAllowedTools) != 2 || gotAllowedTools[0] != "read" || gotAllowedTools[1] != "rg" {
		t.Fatalf("AllowedTools() = %v, want [read rg]", gotAllowedTools)
	}
	if len(gotRules) != 1 || gotRules[0].Path != "/workspace" || gotRules[0].Action != security.ActionAllowWrite {
		t.Fatalf("Rules() = %v, want original sandbox rule", gotRules)
	}

	gotHardRules[0] = "mutated result"
	gotAllowedTools[0] = "mutated_result"
	gotRules[0].Path = "/mutated-result"
	if profile.HardRules()[0] != "original rule" || profile.AllowedTools()[0] != "read" || profile.Rules()[0].Path != "/workspace" {
		t.Fatal("slice accessor exposed profile storage")
	}
}

func TestAllowedToolsPreservesOptionalAndEmptySemantics(t *testing.T) {
	unrestricted := New("all", "prompt", "usage", nil, nil, 1, 0, false, nil, nil, false)
	none := New("none", "prompt", "usage", nil, []string{}, 1, 0, false, nil, nil, false)
	if unrestricted.AllowedTools() != nil {
		t.Fatalf("unrestricted AllowedTools() = %#v, want nil", unrestricted.AllowedTools())
	}
	if allowed := none.AllowedTools(); allowed == nil || len(allowed) != 0 {
		t.Fatalf("no-tools AllowedTools() = %#v, want non-nil empty", allowed)
	}
}

func TestStableProfileIDs(t *testing.T) {
	if ExplorerID != "explorer" || ReviewID != "review" || ThinkerID != "thinker" || WorkerID != "worker" {
		t.Fatalf("profile IDs changed: %q %q %q %q", ExplorerID, ReviewID, ThinkerID, WorkerID)
	}
}
