package agent

import (
	"reflect"
	"testing"
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
	if !build.AllowsTool("write") || !build.AllowsTool("shell") || !build.AllowsTool("exec_command") || !build.AllowsTool("write_stdin") {
		t.Fatal("build agent unexpectedly denied mutation tools")
	}
	for _, id := range []string{PlanID, ExploreID, ExplorerID, ReviewID} {
		profile, err := registry.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		for _, allowed := range []string{"glob", "grep", "read", "read_output"} {
			if !profile.AllowsTool(allowed) {
				t.Fatalf("%s denied %s", id, allowed)
			}
		}
		for _, denied := range []string{"write", "apply_patch", "shell", "exec_command", "write_stdin", "custom_mutation"} {
			if profile.AllowsTool(denied) {
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
			if !profile.AllowsTool(allowed) {
				t.Fatalf("%s denied %s", id, allowed)
			}
		}
	}
	review, err := registry.Get(ReviewID)
	if err != nil {
		t.Fatal(err)
	}
	for _, denied := range []string{"agent_spawn", "agent_send", "monitor", "task_interrupt", "task_list_active"} {
		if review.AllowsTool(denied) {
			t.Fatalf("review agent allowed nested delegation tool %s", denied)
		}
	}
	if got := []string{registry.List()[0].ID, registry.List()[1].ID, registry.List()[2].ID, registry.List()[3].ID, registry.List()[4].ID}; !reflect.DeepEqual(got, []string{BuildID, ExplorerID, PlanID, ReviewID, WorkerID}) {
		t.Fatalf("profile order = %#v", got)
	}
	worker, err := registry.Get(WorkerID)
	if err != nil {
		t.Fatal(err)
	}
	if worker.ReadOnly || !worker.AllowsTool("apply_patch") || !worker.AllowsTool("exec_command") {
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
	if !review.ReadOnly || review.AllowsTool("agent_spawn") || !review.AllowsTool("read") {
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
		listed[i].AllowedToolIDs[0] = "agent_spawn"
		listed[i].HardRules[0] = "allow everything"
	}
	review, err := registry.Get(ReviewID)
	if err != nil {
		t.Fatal(err)
	}
	if review.AllowsTool("agent_spawn") || review.HardRules[0] == "allow everything" {
		t.Fatalf("List mutated registered review profile: %#v", review)
	}
}

func TestReadOnlyProfileCannotExpandHardAllowlist(t *testing.T) {
	registry, err := NewRegistry(Profile{ID: "readonly", Prompt: "read", AllowedToolIDs: []string{"write", "read"}, MaxTurns: 1, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	profile, _ := registry.Get("readonly")
	if profile.AllowsTool("write") || !profile.AllowsTool("read") {
		t.Fatalf("read-only hard restriction was not enforced: %#v", profile)
	}
}
