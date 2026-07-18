package agent

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type drainerFunc func(context.Context, string) error

func (f drainerFunc) Drain(ctx context.Context, sessionID string) error { return f(ctx, sessionID) }

type continuationDrainer struct {
	drains   atomic.Int32
	prepared atomic.Int32
}

func (d *continuationDrainer) Drain(context.Context, string) error {
	d.drains.Add(1)
	return nil
}

func (d *continuationDrainer) PrepareContinuation(context.Context, string) (bool, error) {
	return d.prepared.Add(1) <= 2, nil
}

type lifecycleObserverFunc func(string, error)

func (f lifecycleObserverFunc) LifecycleComplete(sessionID string, err error) {
	f(sessionID, err)
}

func TestCoordinatorConcurrentResumeJoinsOneDrain(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	coordinator := NewCoordinator(drainerFunc(func(context.Context, string) error {
		if calls.Add(1) == 1 {
			close(entered)
		}
		<-release
		return nil
	}))

	const waiters = 32
	results := make(chan error, waiters)
	go func() { results <- coordinator.Resume(context.Background(), "same") }()
	<-entered
	for i := 1; i < waiters; i++ {
		go func() { results <- coordinator.Resume(context.Background(), "same") }()
	}
	waitFor(t, func() bool { return coordinator.Status("same") == StatusRunning })
	time.Sleep(10 * time.Millisecond)
	close(release)
	for i := 0; i < waiters; i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("Drain calls = %d, want 1", got)
	}
}

func TestCoordinatorDrainsPreparedContinuationsInOneLifecycle(t *testing.T) {
	drainer := &continuationDrainer{}
	completed := make(chan error, 1)
	coordinator := NewCoordinator(drainer, lifecycleObserverFunc(func(_ string, err error) { completed <- err }))
	if err := coordinator.Resume(context.Background(), "session"); err != nil {
		t.Fatal(err)
	}
	if drainer.drains.Load() != 3 || drainer.prepared.Load() != 3 {
		t.Fatalf("drains = %d, continuation checks = %d", drainer.drains.Load(), drainer.prepared.Load())
	}
	select {
	case err := <-completed:
		if err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatal("lifecycle completion was not reported")
	}
}

func TestCoordinatorCoalescesWakeAndHandlesSettlementRace(t *testing.T) {
	entered := make(chan int, 4)
	releases := []chan struct{}{make(chan struct{}), make(chan struct{}), make(chan struct{})}
	completed := make(chan error, 2)
	var calls atomic.Int32
	coordinator := NewCoordinator(drainerFunc(func(ctx context.Context, _ string) error {
		call := int(calls.Add(1))
		entered <- call
		select {
		case <-releases[call-1]:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}), lifecycleObserverFunc(func(_ string, err error) { completed <- err }))
	coordinator.Wake("session")
	if call := <-entered; call != 1 {
		t.Fatalf("first call = %d", call)
	}
	for i := 0; i < 100; i++ {
		coordinator.Wake("session")
	}
	close(releases[0])
	if call := <-entered; call != 2 {
		t.Fatalf("coalesced call = %d", call)
	}
	select {
	case err := <-completed:
		t.Fatalf("lifecycle completed between coalesced drains: %v", err)
	default:
	}
	close(releases[1])
	waitFor(t, func() bool { return coordinator.Status("session") == StatusIdle })
	select {
	case err := <-completed:
		if err != nil {
			t.Fatalf("lifecycle error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("lifecycle completion was not reported")
	}
	select {
	case err := <-completed:
		t.Fatalf("duplicate lifecycle completion: %v", err)
	default:
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("coalesced Drain calls = %d", got)
	}
}

func TestCoordinatorInterruptWaitsForCleanupAndWakeAfterCancelRestarts(t *testing.T) {
	canceled := make(chan struct{})
	cleanup := make(chan struct{})
	restarted := make(chan struct{})
	finishRestart := make(chan struct{})
	completed := make(chan error, 2)
	var calls atomic.Int32
	coordinator := NewCoordinator(drainerFunc(func(ctx context.Context, _ string) error {
		if calls.Add(1) == 1 {
			<-ctx.Done()
			close(canceled)
			<-cleanup
			return ctx.Err()
		}
		close(restarted)
		<-finishRestart
		return nil
	}), lifecycleObserverFunc(func(_ string, err error) { completed <- err }))
	coordinator.Wake("session")
	waitFor(t, func() bool { return coordinator.Status("session") == StatusRunning })
	interruptDone := make(chan error, 1)
	go func() { interruptDone <- coordinator.Interrupt(context.Background(), "session") }()
	<-canceled
	coordinator.Wake("session")
	select {
	case err := <-interruptDone:
		t.Fatalf("Interrupt returned before cleanup: %v", err)
	default:
	}
	close(cleanup)
	if err := <-interruptDone; err != nil {
		t.Fatal(err)
	}
	select {
	case <-restarted:
	case <-time.After(time.Second):
		t.Fatal("wake during settlement was lost")
	}
	select {
	case err := <-completed:
		t.Fatalf("lifecycle completed before restarted drain: %v", err)
	default:
	}
	close(finishRestart)
	waitFor(t, func() bool { return coordinator.Status("session") == StatusIdle })
	select {
	case err := <-completed:
		if err != nil {
			t.Fatalf("restarted lifecycle error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("restarted lifecycle completion was not reported")
	}
	select {
	case err := <-completed:
		t.Fatalf("duplicate restarted lifecycle completion: %v", err)
	default:
	}
}

func TestCoordinatorRunsDifferentSessionsConcurrently(t *testing.T) {
	started := make(chan string, 2)
	release := make(chan struct{})
	coordinator := NewCoordinator(drainerFunc(func(_ context.Context, sessionID string) error {
		started <- sessionID
		<-release
		return nil
	}))
	coordinator.Wake("a")
	coordinator.Wake("b")
	seen := map[string]bool{}
	for len(seen) < 2 {
		select {
		case id := <-started:
			seen[id] = true
		case <-time.After(time.Second):
			t.Fatal("sessions did not drain concurrently")
		}
	}
	close(release)
	waitFor(t, func() bool { return len(coordinator.Active()) == 0 })
}

func TestCoordinatorRaceSafeStress(t *testing.T) {
	coordinator := NewCoordinator(drainerFunc(func(ctx context.Context, _ string) error {
		select {
		case <-time.After(time.Microsecond):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}))
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := string(rune('a' + i%8))
			coordinator.Wake(id)
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			if i%5 == 0 {
				_ = coordinator.Interrupt(ctx, id)
			} else {
				err := coordinator.Resume(ctx, id)
				if err != nil && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
					t.Errorf("Resume: %v", err)
				}
			}
		}(i)
	}
	wg.Wait()
	for _, active := range coordinator.Active() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_ = coordinator.Interrupt(ctx, active.SessionID)
		cancel()
	}
	waitFor(t, func() bool { return len(coordinator.Active()) == 0 })
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition was not met")
		}
		time.Sleep(time.Millisecond)
	}
}
