package agent

import (
	"context"
	"errors"
	"testing"
	"time"
)

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

func TestWorkerQuotaRespectsShutdownAndGlobalAndParentRelease(t *testing.T) {
	user := &userSession{childTurns: newChildTurnSemaphore(1)}
	parent := &agentSession{childTurns: newChildTurnSemaphore(1)}
	child := &agentSession{user: user, parent: parent}

	release, err := child.tryAcquireWorkerQuota()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := child.tryAcquireWorkerQuota(); !errors.Is(err, ErrChildConcurrency) {
		t.Fatalf("second acquisition error = %v, want %v", err, ErrChildConcurrency)
	}
	release()
	release()
	global, err := user.TryAcquireWorkerQuota()
	if err != nil {
		t.Fatalf("global quota was not released: %v", err)
	}
	parentPermit, ok := parent.childTurns.tryAcquire()
	if !ok {
		t.Fatal("parent quota was not released")
	}
	global()
	parentPermit.Release()

	user.quotaMu.Lock()
	user.closed = true
	user.quotaMu.Unlock()
	if _, err := user.TryAcquireWorkerQuota(); !errors.Is(err, ErrUserSessionClosed) {
		t.Fatalf("closed acquisition error = %v, want %v", err, ErrUserSessionClosed)
	}
}

func TestAgentSessionShutdownBlocksRejectsNewTurnsAndHonorsContext(t *testing.T) {
	done := make(chan struct{})
	cancelCalled := make(chan struct{}, 1)
	s := &agentSession{drain: &drainState{done: done, cancel: func() { cancelCalled <- struct{}{} }, status: StatusRunning}}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if err := s.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown error = %v, want deadline", err)
	}
	select {
	case <-cancelCalled:
	default:
		t.Fatal("Shutdown did not cancel the active turn")
	}
	if _, err := s.Send(t.Context(), "msg_new", "new"); !errors.Is(err, ErrUserSessionClosed) {
		t.Fatalf("admit after shutdown error = %v, want %v", err, ErrUserSessionClosed)
	}
	close(done)
	if err := s.Shutdown(t.Context()); err != nil {
		t.Fatalf("idempotent Shutdown error = %v", err)
	}
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
