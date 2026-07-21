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
	if build.ReadOnly {
		t.Fatal("build agent unexpectedly read-only")
	}
	for _, id := range []string{PlanID, ExploreID, ExplorerID, ReviewID} {
		profile, err := registry.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if !profile.ReadOnly {
			t.Fatalf("%s expected to be read-only", id)
		}
	}
	for _, id := range []string{PlanID, ExploreID, ExplorerID} {
		profile, err := registry.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if !profile.ReadOnly {
			t.Fatalf("%s expected to be read-only", id)
		}
	}
	review, err := registry.Get(ReviewID)
	if err != nil {
		t.Fatal(err)
	}
	if !review.ReadOnly {
		t.Fatal("review agent expected to be read-only")
	}
	if got := []string{registry.List()[0].ID, registry.List()[1].ID, registry.List()[2].ID, registry.List()[3].ID, registry.List()[4].ID}; !reflect.DeepEqual(got, []string{BuildID, ExplorerID, PlanID, ReviewID, WorkerID}) {
		t.Fatalf("profile order = %#v", got)
	}
	worker, err := registry.Get(WorkerID)
	if err != nil {
		t.Fatal(err)
	}
	if worker.ReadOnly {
		t.Fatalf("worker profile unexpectedly read-only: %#v", worker)
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
	if !review.ReadOnly {
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

func TestReadOnlyProfileIsReadOnly(t *testing.T) {
	registry, err := NewRegistry(Profile{ID: "readonly", Prompt: "read", MaxTurns: 1, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	profile, _ := registry.Get("readonly")
	if !profile.ReadOnly {
		t.Fatalf("profile was not read-only: %#v", profile)
	}
}

// The review profile allows all read-only tools consistently. This verifies
// that the profile correctly identifies itself as read-only.
func TestReviewProfileIsReadOnly(t *testing.T) {
	registry, err := NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	profile, err := registry.Get(ReviewID)
	if err != nil {
		t.Fatal(err)
	}
	if !profile.ReadOnly {
		t.Errorf("review profile is not read-only")
	}
}
