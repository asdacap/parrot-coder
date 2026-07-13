package event_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/amirulashraf/parrot-coder/internal/event"
	"github.com/amirulashraf/parrot-coder/internal/store"
)

func TestAppendRollsBackEventSequenceAndProjection(t *testing.T) {
	ctx := context.Background()
	db, repository, sessionID := newRepository(t)
	subscription := repository.Subscribe(sessionID, 1)
	defer subscription.Close()

	projectorErr := errors.New("projection failed")
	_, err := repository.Append(ctx, sessionID,
		[]event.NewEvent{{Type: "test.failed", Data: json.RawMessage(`{"ok":true}`)}},
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
		[]event.NewEvent{{Type: "test.committed", Data: json.RawMessage(`{}`)}}, nil)
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
				Type: "test.concurrent", Data: json.RawMessage(fmt.Sprintf(`{"worker":%d}`, i)),
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

func TestSlowSubscriberIsDisconnected(t *testing.T) {
	ctx := context.Background()
	_, repository, sessionID := newRepository(t)
	subscription := repository.Subscribe(sessionID, 1)
	defer subscription.Close()
	if _, err := repository.Append(ctx, sessionID, []event.NewEvent{
		{Type: "test.one", Data: json.RawMessage(`{}`)},
		{Type: "test.two", Data: json.RawMessage(`{}`)},
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

func newRepository(t *testing.T) (*store.DB, *event.Repository, string) {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "parrot.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	sessionID := "ses_test"
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.SQL().ExecContext(ctx, `
        INSERT INTO session(id, title, created_at, updated_at) VALUES (?, '', ?, ?)`, sessionID, now, now); err != nil {
		t.Fatal(err)
	}
	return db, event.NewRepository(db), sessionID
}
