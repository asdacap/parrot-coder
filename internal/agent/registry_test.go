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
	if !build.AllowsTool("write") || !build.AllowsTool("shell") {
		t.Fatal("build agent unexpectedly denied mutation tools")
	}
	for _, id := range []string{PlanID, ExploreID, ReviewID} {
		profile, err := registry.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		for _, allowed := range []string{"glob", "grep", "read", "read_output"} {
			if !profile.AllowsTool(allowed) {
				t.Fatalf("%s denied %s", id, allowed)
			}
		}
		for _, denied := range []string{"write", "apply_patch", "shell", "custom_mutation"} {
			if profile.AllowsTool(denied) {
				t.Fatalf("%s allowed %s", id, denied)
			}
		}
	}
	review, err := registry.Get(ReviewID)
	if err != nil {
		t.Fatal(err)
	}
	for _, denied := range []string{"task", "task_status", "task_cancel"} {
		if review.AllowsTool(denied) {
			t.Fatalf("review agent allowed nested delegation tool %s", denied)
		}
	}
	if got := []string{registry.List()[0].ID, registry.List()[1].ID, registry.List()[2].ID, registry.List()[3].ID}; !reflect.DeepEqual(got, []string{BuildID, ExploreID, PlanID, ReviewID}) {
		t.Fatalf("profile order = %#v", got)
	}
}

func TestSubagentsIncludeDedicatedReviewProfile(t *testing.T) {
	registry, err := NewRegistry(Subagents()...)
	if err != nil {
		t.Fatal(err)
	}
	review, err := registry.Get(ReviewID)
	if err != nil {
		t.Fatal(err)
	}
	if !review.ReadOnly || review.AllowsTool("task") || !review.AllowsTool("read") {
		t.Fatalf("review profile = %#v", review)
	}
	if review.Prompt == "" || review.MaxTurns <= 0 {
		t.Fatalf("incomplete review profile = %#v", review)
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
		listed[i].AllowedToolIDs[0] = "task"
		listed[i].HardRules[0] = "allow everything"
	}
	review, err := registry.Get(ReviewID)
	if err != nil {
		t.Fatal(err)
	}
	if review.AllowsTool("task") || review.HardRules[0] == "allow everything" {
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
