package mode

import (
	"os"
	"strings"
	"testing"

	"github.com/amirulashraf/parrot-coder/internal/security"
)

func TestBuiltinsExposeOnlyForegroundModes(t *testing.T) {
	r, err := NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	items := r.List()
	if len(items) != 3 || items[0].ID() != BuildID || items[1].ID() != PlanID || items[2].ID() != QueryID {
		t.Fatalf("modes = %#v", items)
	}
	if items[0].Profile().ReadOnly || !items[1].Profile().ReadOnly || !items[2].Profile().ReadOnly || items[0].Profile().Status == nil || items[1].Profile().Status == nil || items[2].Profile().Status == nil {
		t.Fatal("unexpected mode policies")
	}
	if _, err := r.Get("explorer"); err == nil {
		t.Fatal("explorer exposed as foreground mode")
	}
}

func TestPlanPrepareTurnCreatesWritableArtifactAndPreservesRevisions(t *testing.T) {
	r, err := NewRegistryWithPlanDirectory(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	plan, err := r.Get(PlanID)
	if err != nil {
		t.Fatal(err)
	}
	baseRule := security.Rule{Path: "/protected", Action: security.ActionDenyWrite}
	plan.(*planMode).profile.SandboxRules = []security.Rule{baseRule}
	prepared, err := r.PrepareTurn(PlanID, "session")
	if err != nil {
		t.Fatal(err)
	}
	profile := prepared.Profile()
	capabilities := prepared.CapabilityRules()
	if len(profile.SandboxRules) != 1 || profile.SandboxRules[0] != baseRule || len(capabilities) != 1 || capabilities[0].Action != security.ActionAllowWrite || !strings.Contains(profile.Prompt, "This mode is read-only except for the designated plan file.") {
		t.Fatalf("plan profile = %#v, capabilities = %#v", profile, capabilities)
	}
	path := capabilities[0].Path
	if info, err := os.Stat(path); err != nil || info.Size() != 0 || info.Mode().Perm() != 0o600 {
		t.Fatalf("initial plan artifact = %#v, %v", info, err)
	}
	const existingPlan = "existing plan"
	if err := os.WriteFile(path, []byte(existingPlan), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
	revised, err := r.PrepareTurn(PlanID, "session")
	if err != nil {
		t.Fatal(err)
	}
	revisedProfile := revised.Profile()
	revisedCapabilities := revised.CapabilityRules()
	if len(revisedProfile.SandboxRules) != 1 || revisedProfile.SandboxRules[0] != baseRule || len(revisedCapabilities) != 1 || revisedCapabilities[0] != (security.Rule{Path: path, Action: security.ActionAllowWrite}) {
		t.Fatalf("revised security layers = base %#v, capabilities %#v", revisedProfile.SandboxRules, revisedCapabilities)
	}
	if info, err := os.Stat(path); err != nil || info.Size() != int64(len(existingPlan)) || info.Mode().Perm() != 0o600 {
		t.Fatalf("revised plan artifact = %#v, %v", info, err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != existingPlan {
		t.Fatalf("revised plan contents = %q, %v", data, err)
	}
}

func TestOnTurnCompleteDeclaresDialogPerMode(t *testing.T) {
	r, err := NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	items := r.List()
	// Build mode declares no turn-complete behavior.
	if result := items[0].OnTurnComplete(); result != (TurnCompleteResult{}) {
		t.Fatalf("build mode turn-complete = %#v, want zero", result)
	}
	// Query mode declares no turn-complete behavior.
	if result := items[2].OnTurnComplete(); result != (TurnCompleteResult{}) {
		t.Fatalf("query mode turn-complete = %#v, want zero", result)
	}
	// Plan mode declares an approval dialog with a transition to build.
	plan := items[1].OnTurnComplete()
	if plan.Dialog == nil {
		t.Fatalf("plan mode has no dialog: %#v", plan)
	}
	if plan.Dialog.Prompt != "Plan complete: " || len(plan.Dialog.Context) != 1 || len(plan.Dialog.Choices) != 2 {
		t.Fatalf("plan dialog = %#v", plan.Dialog)
	}
	if plan.Dialog.Choices[0].Value != "yes" || plan.Dialog.Choices[0].Action.Agent != BuildID || plan.Dialog.Choices[0].Action.Prompt != "Implement the approved plan." {
		t.Fatalf("approve choice = %#v", plan.Dialog.Choices[0])
	}
	if plan.Dialog.Choices[1].Value != "no" || plan.Dialog.Choices[1].Action.Agent != "" || plan.Dialog.Choices[1].Action.Prompt != "" {
		t.Fatalf("decline choice = %#v", plan.Dialog.Choices[1])
	}
	if plan.Dialog.CustomChoice != "feedback" || plan.Dialog.CustomPrompt != "plan feedback: " || plan.Dialog.EmptyMessage != "enter yes, no, or feedback" {
		t.Fatalf("plan dialog custom = %#v", plan.Dialog)
	}
}
