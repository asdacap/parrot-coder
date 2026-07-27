package agent

import (
	"reflect"
	"testing"

	"github.com/amirulashraf/parrot-coder/internal/status"
)

const (
	BuildID = "build"
	PlanID  = "plan"
)

type mutableProfile struct{ Profile }

func testProfiles() []Profile {
	return []Profile{
		NewProfile(BuildID, "build", []string{"rule"}, 64, 3, false, nil, nil),
		NewProfile(PlanID, "plan", []string{"rule"}, 24, 1, true, nil, nil),
		NewProfile(ExplorerID, "explorer", []string{"rule"}, 32, 3, true, nil, status.Static{ProviderKey: "profile:explorer"}),
		NewProfile(ReviewID, "review", []string{"rule"}, 32, 3, true, nil, status.Static{ProviderKey: "profile:review"}),
		NewProfile(WorkerID, "worker", []string{"rule"}, 64, 3, false, nil, status.Static{ProviderKey: "profile:worker"}),
	}
}

func testSubagentProfiles() []Profile {
	profiles := testProfiles()
	return profiles[2:]
}

func TestBuiltinProfilesEnforceHardToolRestrictions(t *testing.T) {
	registry, err := NewRegistry(testProfiles()...)
	if err != nil {
		t.Fatal(err)
	}
	build, err := registry.Get(BuildID)
	if err != nil {
		t.Fatal(err)
	}
	if build.IsReadOnly() {
		t.Fatal("build agent unexpectedly read-only")
	}
	for _, id := range []string{PlanID, ExploreID, ExplorerID, ReviewID} {
		profile, err := registry.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if !profile.IsReadOnly() {
			t.Fatalf("%s expected to be read-only", id)
		}
	}
	for _, id := range []string{PlanID, ExploreID, ExplorerID} {
		profile, err := registry.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if !profile.IsReadOnly() {
			t.Fatalf("%s expected to be read-only", id)
		}
	}
	review, err := registry.Get(ReviewID)
	if err != nil {
		t.Fatal(err)
	}
	if !review.IsReadOnly() {
		t.Fatal("review agent expected to be read-only")
	}
	if got := []string{registry.List()[0].ID(), registry.List()[1].ID(), registry.List()[2].ID(), registry.List()[3].ID(), registry.List()[4].ID()}; !reflect.DeepEqual(got, []string{BuildID, ExplorerID, PlanID, ReviewID, WorkerID}) {
		t.Fatalf("profile order = %#v", got)
	}
	worker, err := registry.Get(WorkerID)
	if err != nil {
		t.Fatal(err)
	}
	if worker.IsReadOnly() {
		t.Fatalf("worker profile unexpectedly read-only: %#v", worker)
	}
}

func TestSubagentsIncludeExplorerWorkerAndDedicatedReviewProfiles(t *testing.T) {
	registry, err := NewRegistry(testSubagentProfiles()...)
	if err != nil {
		t.Fatal(err)
	}
	explore, err := registry.Get(ExploreID)
	if err != nil || explore.ID() != ExplorerID || !explore.IsReadOnly() {
		t.Fatalf("explore alias = %#v, %v", explore, err)
	}
	review, err := registry.Get(ReviewID)
	if err != nil {
		t.Fatal(err)
	}
	if !review.IsReadOnly() {
		t.Fatalf("review profile = %#v", review)
	}
	if review.Prompt() == "" || review.MaxTurns() <= 0 {
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
		if profile.IsReadOnly() != test.readOnly || profile.Prompt() == "" || profile.MaxTurns() <= 0 || profile.Status() == nil {
			t.Fatalf("%s profile = %#v", test.id, profile)
		}
	}
}

func TestListDoesNotExposeProfileSliceStorage(t *testing.T) {
	registry, err := NewRegistry(testProfiles()...)
	if err != nil {
		t.Fatal(err)
	}
	for _, profile := range registry.List() {
		if profile.ID() == ReviewID {
			profile.HardRules()[0] = "allow everything"
		}
	}
	review, err := registry.Get(ReviewID)
	if err != nil {
		t.Fatal(err)
	}
	if review.HardRules()[0] == "allow everything" {
		t.Fatalf("List mutated registered review profile: %#v", review)
	}
}

func TestRegistrySnapshotsProfilesAndRejectsTypedNil(t *testing.T) {
	source := &mutableProfile{Profile: NewProfile("original", "prompt", []string{"rule"}, 2, 1, true, nil, nil)}
	registry, err := NewRegistry(source)
	if err != nil {
		t.Fatal(err)
	}
	source.Profile = NewProfile("mutated", "changed", nil, 99, 99, false, nil, nil)
	stored, err := registry.Get("original")
	if err != nil {
		t.Fatal(err)
	}
	if stored.ID() != "original" || stored.Prompt() != "prompt" || stored.MaxTurns() != 2 || stored.RecursionLimit() != 1 || !stored.IsReadOnly() || !reflect.DeepEqual(stored.HardRules(), []string{"rule"}) {
		t.Fatalf("registered profile changed through source: %#v", stored)
	}

	var typedNil *mutableProfile
	if _, err = NewRegistry(typedNil); err == nil {
		t.Fatal("typed-nil profile was accepted")
	}
}

func TestReadOnlyProfileIsReadOnly(t *testing.T) {
	registry, err := NewRegistry(NewProfile("readonly", "read", nil, 1, 0, true, nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	profile, _ := registry.Get("readonly")
	if !profile.IsReadOnly() {
		t.Fatalf("profile was not read-only: %#v", profile)
	}
}

// The review profile allows all read-only tools consistently. This verifies
// that the profile correctly identifies itself as read-only.
func TestReviewProfileIsReadOnly(t *testing.T) {
	registry, err := NewRegistry(testProfiles()...)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := registry.Get(ReviewID)
	if err != nil {
		t.Fatal(err)
	}
	if !profile.IsReadOnly() {
		t.Errorf("review profile is not read-only")
	}
}
