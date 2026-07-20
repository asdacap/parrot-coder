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
	// Plan mode no longer enumerates read-only tools: an empty allowlist plus
	// ReadOnly means "every tool which declares itself read-only", so a new
	// read-only tool is available without editing this list.
	if len(items[1].Profile().AllowedToolIDs) != 0 {
		t.Fatalf("plan mode still enumerates tools: %#v", items[1].Profile().AllowedToolIDs)
	}
	for _, toolID := range []string{"monitor", "task_interrupt", "task_list_active"} {
		if !items[1].Profile().AllowsTool(toolID) {
			t.Fatalf("plan mode does not allow %s", toolID)
		}
	}
	if _, err := r.Get("explorer"); err == nil {
		t.Fatal("explorer exposed as foreground mode")
	}
}
