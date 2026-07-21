package mode

import (
	"testing"
)

func TestBuiltinsExposeOnlyForegroundModes(t *testing.T) {
	r, err := NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	items := r.List()
	if len(items) != 2 || items[0].ID() != BuildID || items[1].ID() != PlanID {
		t.Fatalf("modes = %#v", items)
	}
	if items[0].Profile().ReadOnly || !items[1].Profile().ReadOnly {
		t.Fatal("unexpected mode policies")
	}
	for _, toolID := range []string{"monitor", "task_interrupt", "task_list_active"} {
		if !items[1].Profile().AllowsTool(toolID, true) {
			t.Fatalf("plan mode does not allow %s", toolID)
		}
	}
	if _, err := r.Get("explorer"); err == nil {
		t.Fatal("explorer exposed as foreground mode")
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
