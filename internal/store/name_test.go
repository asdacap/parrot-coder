package store_test

import (
	"errors"
	"testing"

	"github.com/amirulashraf/parrot-coder/internal/store"
)

func TestSessionNameReservationOwnershipAndRelease(t *testing.T) {
	state := t.TempDir()
	alive := func(int) bool { return true }
	reserve := func(sessionID string) error {
		return store.ReserveSessionName(state, "main-agent", sessionID, "host", 101, alive)
	}
	if err := reserve("first"); err != nil {
		t.Fatal(err)
	}
	if err := reserve("second"); !errors.Is(err, store.ErrSessionNameReserved) {
		t.Fatalf("concurrent reserve error = %v", err)
	}
	if err := store.ReleaseSessionName(state, "main-agent", "wrong"); err != nil {
		t.Fatal(err)
	}
	if err := reserve("second"); !errors.Is(err, store.ErrSessionNameReserved) {
		t.Fatalf("reserve after wrong release error = %v", err)
	}
	if err := store.ReleaseSessionName(state, "main-agent", "first"); err != nil {
		t.Fatal(err)
	}
	if err := reserve("second"); err != nil {
		t.Fatal(err)
	}
	if err := store.ReleaseSessionName(state, "main-agent", "first"); err != nil {
		t.Fatal(err)
	}
	if err := reserve("third"); !errors.Is(err, store.ErrSessionNameReserved) {
		t.Fatalf("stale release displaced current owner: %v", err)
	}
}
