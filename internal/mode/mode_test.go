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
	for _, toolID := range []string{"monitor", "task_interrupt", "task_list_active"} {
		if !slices.Contains(items[1].Profile().AllowedToolIDs, toolID) || !items[1].Profile().AllowsTool(toolID) {
			t.Fatalf("plan mode does not allow %s", toolID)
		}
	}
	if _, err := r.Get("explorer"); err == nil {
		t.Fatal("explorer exposed as foreground mode")
	}
}
