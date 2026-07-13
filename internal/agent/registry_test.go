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
	for _, id := range []string{PlanID, ExploreID} {
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
	if got := []string{registry.List()[0].ID, registry.List()[1].ID, registry.List()[2].ID}; !reflect.DeepEqual(got, []string{BuildID, ExploreID, PlanID}) {
		t.Fatalf("profile order = %#v", got)
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
