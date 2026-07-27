package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/amirulashraf/parrot-coder/internal/tool"
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

	release, blocked, err := child.tryAcquireTurnQuota()
	if err != nil || blocked {
		t.Fatalf("first acquisition = %v, %v", blocked, err)
	}
	if _, blocked, err := child.tryAcquireTurnQuota(); err != nil || !blocked {
		t.Fatalf("second acquisition = %v, %v; want blocked", blocked, err)
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
	if _, err := child.acquireTurnQuota(t.Context()); !errors.Is(err, ErrUserSessionClosed) {
		t.Fatalf("blocking acquisition error = %v, want %v", err, ErrUserSessionClosed)
	}
	parentPermit, ok = parent.childTurns.tryAcquire()
	if !ok {
		t.Fatal("failed blocking acquisition did not release parent quota")
	}
	parentPermit.Release()
}

func TestAgentSessionShutdownBlocksRejectsNewTurnsAndHonorsContext(t *testing.T) {
	cancelCalled := make(chan struct{}, 1)
	turn := &turnState{cancel: func() { cancelCalled <- struct{}{} }, status: StatusRunning}
	s := &agentSession{turn: turn}
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
	turn.completion.complete(Status{State: StatusCanceled}, context.Canceled)
	if err := s.Shutdown(t.Context()); err != nil {
		t.Fatalf("idempotent Shutdown error = %v", err)
	}
}

func TestTurnCompletionListeners(t *testing.T) {
	terminal := Status{State: StatusFailed, Error: "failed", Lineage: []string{"root", "child"}}
	runErr := errors.New("execution failed")

	t.Run("before and after completion", func(t *testing.T) {
		var completion turnCompletion
		before, removeBefore := completion.subscribe()
		defer removeBefore()
		second, removeSecond := completion.subscribe()
		defer removeSecond()
		completion.complete(terminal, runErr)
		after, removeAfter := completion.subscribe()
		defer removeAfter()
		for name, listener := range map[string]*turnCompletionListener{
			"before":        before,
			"second before": second,
			"after":         after,
		} {
			result, completed := listener.await(t.Context())
			if !completed || !errors.Is(result.err, runErr) || result.status.State != StatusFailed || result.status.Error != "failed" {
				t.Fatalf("%s result = %#v, %v", name, result, completed)
			}
			result.status.Lineage[0] = "changed"
		}
		stored, ok := completion.Result()
		if !ok || stored.status.Lineage[0] != "root" {
			t.Fatalf("stored result was not cloned: %#v, %v", stored, ok)
		}
	})

	t.Run("canceled listener is removed", func(t *testing.T) {
		var completion turnCompletion
		listener, remove := completion.subscribe()
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if _, completed := listener.await(ctx); completed {
			t.Fatal("canceled listener unexpectedly completed")
		}
		remove()
		completion.complete(terminal, runErr)
		later, removeLater := completion.subscribe()
		defer removeLater()
		if result, completed := later.await(t.Context()); !completed || !errors.Is(result.err, runErr) {
			t.Fatalf("late listener result = %#v, %v", result, completed)
		}
	})

	t.Run("aborted reserved turn", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		turn := &turnState{ctx: ctx, cancel: cancel, status: StatusBlocked}
		s := &agentSession{turn: turn, status: Status{State: StatusBlocked}}
		observer := turnObserver{turn: turn}
		s.abortTurn(turn)
		status, err := observer.Wait(t.Context())
		if err != nil || status.State != StatusCanceled || status.Error != context.Canceled.Error() {
			t.Fatalf("aborted observation = %#v, %v", status, err)
		}
		result, completed := turn.completion.Result()
		if !completed || !errors.Is(result.err, context.Canceled) {
			t.Fatalf("aborted result = %#v, %v", result, completed)
		}
	})
}

func TestNewUserSessionRejectsInvalidChildTurnLimits(t *testing.T) {
	for _, config := range []UserSessionConfig{
		{},
		{MaxConcurrentChildTurns: 1},
		{MaxConcurrentChildTurnsPerParent: 1},
		{MaxConcurrentChildTurns: 1, MaxConcurrentChildTurnsPerParent: 2},
	} {
		if _, err := NewUserSession(t.Context(), nil, nil, nil, config,
			nil, nil, nil, tool.Providers{}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
			nil, nil, nil, nil, nil, nil, nil, nil); err == nil {
			t.Fatalf("NewUserSession(%#v) succeeded", config)
		}
	}
}
