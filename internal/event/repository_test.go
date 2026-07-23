package event_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/amirulashraf/parrot-coder/internal/event"
	"github.com/amirulashraf/parrot-coder/internal/store"
)

// testEventType stands in for any durable event. Append accepts only manifested
// types, and none of the sequencing tests below depend on which one they use.
const testEventType = "session.message.appended"

func TestAppendRejectsEventTheStreamCannotCarry(t *testing.T) {
	ctx := context.Background()
	_, repository, sessionID := newRepository(t)
	for _, test := range []struct {
		name    string
		pending event.NewEvent
	}{
		{name: "unmanifested type", pending: event.NewEvent{Type: "session.invented.appended", Data: json.RawMessage(`{}`)}},
		{name: "empty type", pending: event.NewEvent{Data: json.RawMessage(`{}`)}},
		{name: "empty data", pending: event.NewEvent{Type: testEventType}},
		{name: "malformed data", pending: event.NewEvent{Type: testEventType, Data: json.RawMessage(`{`)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := repository.Append(ctx, sessionID, []event.NewEvent{test.pending}, nil); !errors.Is(err, event.ErrInvalidEvent) {
				t.Fatalf("Append error = %v, want ErrInvalidEvent", err)
			}
		})
	}
	items, err := repository.List(ctx, sessionID, -1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("committed events = %d, want 0", len(items))
	}
}

func TestAppendRollsBackEventSequenceAndProjection(t *testing.T) {
	ctx := context.Background()
	db, repository, sessionID := newRepository(t)
	subscription := repository.Subscribe(sessionID, 1)
	defer subscription.Close()

	projectorErr := errors.New("projection failed")
	_, err := repository.Append(ctx, sessionID,
		[]event.NewEvent{{Type: testEventType, Data: json.RawMessage(`{"ok":true}`)}},
		func(ctx context.Context, tx *sql.Tx, _ []event.Event) error {
			if _, err := tx.ExecContext(ctx, `UPDATE session SET title = 'not committed' WHERE id = ?`, sessionID); err != nil {
				return err
			}
			return projectorErr
		})
	if !errors.Is(err, projectorErr) {
		t.Fatalf("Append error = %v, want projector error", err)
	}
	select {
	case item := <-subscription.Events:
		t.Fatalf("received rolled-back event: %#v", item)
	default:
	}

	items, err := repository.List(ctx, sessionID, -1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("events after rollback = %d, want 0", len(items))
	}
	var title string
	if err := db.SQL().QueryRowContext(ctx, `SELECT title FROM session WHERE id = ?`, sessionID).Scan(&title); err != nil {
		t.Fatal(err)
	}
	if title != "" {
		t.Fatalf("projected title after rollback = %q", title)
	}
	appended, err := repository.Append(ctx, sessionID,
		[]event.NewEvent{{Type: testEventType, Data: json.RawMessage(`{}`)}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if appended[0].Sequence != 0 {
		t.Fatalf("sequence after rollback = %d, want 0", appended[0].Sequence)
	}
	select {
	case item := <-subscription.Events:
		if item.ID != appended[0].ID {
			t.Fatalf("subscriber event ID = %q, want %q", item.ID, appended[0].ID)
		}
	case <-time.After(time.Second):
		t.Fatal("subscriber did not receive committed event")
	}
}

func TestConcurrentAppendIsContiguous(t *testing.T) {
	ctx := context.Background()
	_, repository, sessionID := newRepository(t)
	const count = 32
	var wg sync.WaitGroup
	errs := make(chan error, count)
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := repository.Append(ctx, sessionID, []event.NewEvent{{
				Type: testEventType, Data: json.RawMessage(fmt.Sprintf(`{"worker":%d}`, i)),
			}}, nil)
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	items, err := repository.List(ctx, sessionID, -1, count+1)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != count {
		t.Fatalf("event count = %d, want %d", len(items), count)
	}
	ids := make(map[string]bool, count)
	sequences := make([]int, 0, count)
	for _, item := range items {
		if ids[item.ID] {
			t.Fatalf("duplicate event ID %q", item.ID)
		}
		ids[item.ID] = true
		sequences = append(sequences, int(item.Sequence))
	}
	sort.Ints(sequences)
	for i, sequence := range sequences {
		if sequence != i {
			t.Fatalf("sequence[%d] = %d", i, sequence)
		}
	}
}

func TestConcurrentAppendPublishesInSequenceOrder(t *testing.T) {
	ctx := context.Background()
	_, repository, sessionID := newRepository(t)
	const count = 64
	subscription := repository.Subscribe(sessionID, count)
	defer subscription.Close()
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := repository.Append(ctx, sessionID, []event.NewEvent{{Type: testEventType, Data: json.RawMessage(`{}`)}}, nil); err != nil {
				t.Errorf("Append: %v", err)
			}
		}()
	}
	wg.Wait()
	for want := int64(0); want < count; want++ {
		select {
		case item, ok := <-subscription.Events:
			if !ok {
				t.Fatalf("subscription closed before sequence %d", want)
			}
			if item.Sequence != want {
				t.Fatalf("sequence = %d, want %d", item.Sequence, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for sequence %d", want)
		}
	}
}

func TestSlowSubscriberIsDisconnected(t *testing.T) {
	ctx := context.Background()
	_, repository, sessionID := newRepository(t)
	subscription := repository.Subscribe(sessionID, 1)
	defer subscription.Close()
	if _, err := repository.Append(ctx, sessionID, []event.NewEvent{
		{Type: testEventType, Data: json.RawMessage(`{}`)},
		{Type: testEventType, Data: json.RawMessage(`{}`)},
	}, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := <-subscription.Events; !ok {
		return
	}
	if _, ok := <-subscription.Events; ok {
		t.Fatal("slow subscriber remained open")
	}
}

func TestReplayAndSubscribeHasNoGapOrDuplicate(t *testing.T) {
	ctx := context.Background()
	_, repository, sessionID := newRepository(t)
	const count = 100

	start := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		<-start
		for i := 0; i < count; i++ {
			_, err := repository.Append(ctx, sessionID, []event.NewEvent{{Type: testEventType, Data: json.RawMessage(`{}`)}}, nil)
			if err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	close(start)
	replay, subscription, err := repository.ReplayAndSubscribe(ctx, sessionID, -1, count+1)
	if err != nil {
		t.Fatal(err)
	}
	defer subscription.Close()

	seen := make(map[int64]bool, count)
	for _, item := range replay {
		if seen[item.Sequence] {
			t.Fatalf("duplicate replay sequence %d", item.Sequence)
		}
		seen[item.Sequence] = true
	}
	deadline := time.After(5 * time.Second)
	for len(seen) < count {
		select {
		case item, ok := <-subscription.Events:
			if !ok {
				t.Fatalf("subscription closed after %d events", len(seen))
			}
			if seen[item.Sequence] {
				t.Fatalf("duplicate live sequence %d", item.Sequence)
			}
			seen[item.Sequence] = true
		case <-deadline:
			t.Fatalf("received %d of %d events", len(seen), count)
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	for i := int64(0); i < count; i++ {
		if !seen[i] {
			t.Fatalf("missing sequence %d", i)
		}
	}
}

func newRepository(t *testing.T) (*store.DB, *event.Repository, string) {
	t.Helper()
	ctx := context.Background()
	sessions := store.NewRegistry(t.TempDir(), "host-test")
	t.Cleanup(func() { sessions.Close() })
	sessionID := "ses_test"
	db, err := sessions.Create(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.SQL().ExecContext(ctx, `
        INSERT INTO session(id, title, created_at, updated_at) VALUES (?, '', ?, ?)`, sessionID, now, now); err != nil {
		t.Fatal(err)
	}
	return db, event.NewRepository(sessions), sessionID
}
