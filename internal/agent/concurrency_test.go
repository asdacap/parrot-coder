package agent

import "testing"

func TestChildTurnSemaphoreBoundsCapacityAndPermitReleaseIsIdempotent(t *testing.T) {
	semaphore := newChildTurnSemaphore(2)
	first, ok := semaphore.tryAcquire()
	if !ok {
		t.Fatal("first acquisition was rejected")
	}
	second, ok := semaphore.tryAcquire()
	if !ok {
		t.Fatal("second acquisition was rejected")
	}
	if permit, acquired := semaphore.tryAcquire(); acquired || permit != nil {
		t.Fatalf("acquisition beyond capacity = %#v, %v", permit, acquired)
	}

	first.Release()
	first.Release()
	replacement, ok := semaphore.tryAcquire()
	if !ok {
		t.Fatal("released capacity was not reusable")
	}
	if permit, acquired := semaphore.tryAcquire(); acquired || permit != nil {
		t.Fatalf("idempotent release added capacity = %#v, %v", permit, acquired)
	}

	second.Release()
	replacement.Release()
}

func TestNewUserSessionRejectsInvalidChildTurnLimits(t *testing.T) {
	for _, config := range []UserSessionConfig{
		{},
		{MaxConcurrentChildTurns: 1},
		{MaxConcurrentChildTurnsPerParent: 1},
		{MaxConcurrentChildTurns: 1, MaxConcurrentChildTurnsPerParent: 2},
	} {
		if _, err := NewUserSession(t.Context(), nil, config); err == nil {
			t.Fatalf("NewUserSession(%#v) succeeded", config)
		}
	}
}
