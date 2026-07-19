package mode

import (
	"slices"
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
	if !slices.Contains(items[1].Profile().AllowedToolIDs, "monitor") || !items[1].Profile().AllowsTool("monitor") {
		t.Fatal("plan mode does not allow process monitoring")
	}
	if _, err := r.Get("explorer"); err == nil {
		t.Fatal("explorer exposed as foreground mode")
	}
}
