package agent

import (
	"reflect"
	"testing"

	"github.com/amirulashraf/parrot-coder/internal/tool"
)

func TestBuiltinProfilesEnforceHardToolRestrictions(t *testing.T) {
	registry, err := NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	build, err := registry.Get(BuildID)
	if err != nil {
		t.Fatal(err)
	}
	if !build.AllowsTool("write", false) || !build.AllowsTool("shell", false) || !build.AllowsTool("exec_command", false) || !build.AllowsTool("write_stdin", false) {
		t.Fatal("build agent unexpectedly denied mutation tools")
	}
	for _, id := range []string{PlanID, ExploreID, ExplorerID, ReviewID} {
		profile, err := registry.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		for _, allowed := range []string{"glob", "grep", "read", "read_output"} {
			if !ProfileAllows(profile, tool.Definition{ID: allowed, ReadOnly: true}) {
				t.Fatalf("%s denied %s", id, allowed)
			}
		}
		for _, denied := range []string{"write", "apply_patch", "shell", "exec_command", "write_stdin", "custom_mutation", "unrestricted_shell"} {
			if ProfileAllows(profile, tool.Definition{ID: denied}) {
				t.Fatalf("%s allowed %s", id, denied)
			}
		}
	}
	for _, id := range []string{PlanID, ExploreID, ExplorerID} {
		profile, err := registry.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		for _, allowed := range []string{"monitor", "task_interrupt", "task_list_active"} {
			if !ProfileAllows(profile, tool.Definition{ID: allowed, ReadOnly: true}) {
				t.Fatalf("%s denied %s", id, allowed)
			}
		}
		if !ProfileAllows(profile, tool.Definition{ID: "question", ReadOnly: tool.NewQuestionTool(nil).ReadOnly()}) {
			t.Fatalf("%s denied question", id)
		}
	}
	review, err := registry.Get(ReviewID)
	if err != nil {
		t.Fatal(err)
	}
	for _, allowed := range []string{"agent_spawn", "agent_send", "monitor", "task_interrupt", "task_list_active"} {
		if !review.AllowsTool(allowed, true) {
			t.Fatalf("review agent denied delegation tool %s", allowed)
		}
	}
	if got := []string{registry.List()[0].ID, registry.List()[1].ID, registry.List()[2].ID, registry.List()[3].ID, registry.List()[4].ID}; !reflect.DeepEqual(got, []string{BuildID, ExplorerID, PlanID, ReviewID, WorkerID}) {
		t.Fatalf("profile order = %#v", got)
	}
	worker, err := registry.Get(WorkerID)
	if err != nil {
		t.Fatal(err)
	}
	if worker.ReadOnly || !worker.AllowsTool("apply_patch", false) || !worker.AllowsTool("exec_command", false) {
		t.Fatalf("worker profile = %#v", worker)
	}
}

func TestSubagentsIncludeExplorerWorkerAndDedicatedReviewProfiles(t *testing.T) {
	registry, err := NewRegistry(Subagents()...)
	if err != nil {
		t.Fatal(err)
	}
	explore, err := registry.Get(ExploreID)
	if err != nil || explore.ID != ExplorerID || !explore.ReadOnly {
		t.Fatalf("explore alias = %#v, %v", explore, err)
	}
	review, err := registry.Get(ReviewID)
	if err != nil {
		t.Fatal(err)
	}
	if !review.ReadOnly || !review.AllowsTool("agent_spawn", true) || !review.AllowsTool("read", true) {
		t.Fatalf("review profile = %#v", review)
	}
	if review.Prompt == "" || review.MaxTurns <= 0 {
		t.Fatalf("incomplete review profile = %#v", review)
	}
	for _, test := range []struct {
		id       string
		readOnly bool
	}{
		{id: ExplorerID, readOnly: true},
		{id: WorkerID},
	} {
		profile, profileErr := registry.Get(test.id)
		if profileErr != nil {
			t.Fatal(profileErr)
		}
		if profile.ReadOnly != test.readOnly || profile.Prompt == "" || profile.MaxTurns <= 0 {
			t.Fatalf("%s profile = %#v", test.id, profile)
		}
	}
}

func TestListDoesNotExposeProfileSliceStorage(t *testing.T) {
	registry, err := NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	listed := registry.List()
	for i := range listed {
		if listed[i].ID != ReviewID {
			continue
		}
		listed[i].HardRules[0] = "allow everything"
	}
	review, err := registry.Get(ReviewID)
	if err != nil {
		t.Fatal(err)
	}
	if review.HardRules[0] == "allow everything" {
		t.Fatalf("List mutated registered review profile: %#v", review)
	}
}

func TestReadOnlyProfileCannotExpandHardAllowlist(t *testing.T) {
	registry, err := NewRegistry(Profile{ID: "readonly", Prompt: "read", MaxTurns: 1, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	profile, _ := registry.Get("readonly")
	// Listing a writable tool cannot expand a read-only profile: the refusal
	// comes from the tool declaring itself writable, so a profile cannot opt
	// out of it by naming the tool.
	if ProfileAllows(profile, tool.Definition{ID: "write"}) {
		t.Fatalf("read-only hard restriction was not enforced: %#v", profile)
	}
	if !ProfileAllows(profile, tool.Definition{ID: "read", ReadOnly: true}) {
		t.Fatalf("read-only profile denied a read-only tool: %#v", profile)
	}
}

// The review profile's allowlist was unsorted while AllowsTool binary-searched
// it, which silently denied the review agent git_diff and every lsp_* tool. The
// allowlist is gone and membership is a linear scan, so these are available
// again. This is a deliberate behaviour change, fenced here.
func TestReviewProfileRegainsToolsLostToTheBinarySearchBug(t *testing.T) {
	registry, err := NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	profile, err := registry.Get(ReviewID)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"git_diff", "lsp_diagnostics", "lsp_definition", "lsp_references", "lsp_hover", "lsp_symbols"} {
		if !ProfileAllows(profile, tool.Definition{ID: id, ReadOnly: true}) {
			t.Errorf("review still denied %s", id)
		}
	}
	// Delegation tools are now allowed by the profile because they are
	// read-only tools. The agent_spawn/agent_send tools themselves prevent
	// read-only agents from delegating to writable agents internally.
	for _, id := range []string{"agent_spawn", "agent_send", "review", "monitor", "task_interrupt", "task_list_active"} {
		if !ProfileAllows(profile, tool.Definition{ID: id, ReadOnly: true}) {
			t.Errorf("review profile denied read-only delegation tool %s", id)
		}
	}
}
