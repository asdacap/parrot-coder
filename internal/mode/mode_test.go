package mode

import "testing"

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
	if _, err := r.Get("explore"); err == nil {
		t.Fatal("explore exposed as foreground mode")
	}
}
