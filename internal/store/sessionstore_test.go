package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// appendEvent mirrors the shape of the event append hot path: an event row plus
// the sequence counter, in one immediate transaction.
func appendEvent(ctx context.Context, tx *sql.Tx, sessionID string, sequence int) error {
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO event(id,session_id,sequence,type,data_json,created_at) VALUES(?,?,?,?,?,?)`,
		fmt.Sprintf("evt_%s_%d", sessionID, sequence), sessionID, sequence, "message.updated", []byte(`{"a":1}`),
		time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO event_sequence(session_id,next_sequence) VALUES(?,?)
		 ON CONFLICT(session_id) DO UPDATE SET next_sequence=excluded.next_sequence`,
		sessionID, sequence+1)
	return err
}

func newSession(t *testing.T, state, id string) *DB {
	t.Helper()
	if err := CreateSessionDir(state, id); err != nil {
		t.Fatal(err)
	}
	db, err := OpenSession(context.Background(), DatabasePath(state, id))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.SQL().ExecContext(context.Background(),
		`INSERT INTO session(id,title,created_at,updated_at) VALUES(?,?,?,?)`, id, "t", now, now); err != nil {
		t.Fatal(err)
	}
	if err := WriteMeta(state, Meta{ID: id, Title: "t", CreatedAt: now, UpdatedAt: now, HostKey: "host-a", PID: 1}); err != nil {
		t.Fatal(err)
	}
	return db
}

// A session database must never produce the memory-mapped -shm file, nor a -wal
// file, because neither is coherent between two hosts sharing a filesystem.
// Asserting on the filesystem rather than on a pragma catches the regression
// whatever its cause: a changed journal mode, a stray Open, or a new dependency
// that opens the file itself.
func TestSessionDatabaseLeavesNoSharedMemoryFiles(t *testing.T) {
	t.Parallel()
	state := t.TempDir()
	ctx := context.Background()
	db := newSession(t, state, "ses_a")

	for i := range 20 {
		if err := db.WithImmediate(ctx, func(tx *sql.Tx) error { return appendEvent(ctx, tx, "ses_a", i) }); err != nil {
			t.Fatal(err)
		}
	}

	var found []string
	err := filepath.WalkDir(state, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.HasSuffix(path, "-shm") || strings.HasSuffix(path, "-wal") {
			found = append(found, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 0 {
		t.Fatalf("session state contains WAL artifacts: %v", found)
	}
}

// Opening another host's session read-only must not create it, must not write
// it, and must not take the write lock that _txlock=immediate applies to every
// transaction on a read-write connection.
func TestOpenReadOnlyNeverWrites(t *testing.T) {
	t.Parallel()
	state := t.TempDir()
	ctx := context.Background()
	db := newSession(t, state, "ses_b")
	if err := db.WithImmediate(ctx, func(tx *sql.Tx) error { return appendEvent(ctx, tx, "ses_b", 0) }); err != nil {
		t.Fatal(err)
	}
	db.Close()

	path := DatabasePath(state, "ses_b")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	read, err := OpenReadOnly(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()

	var count int
	if err := read.SQL().QueryRowContext(ctx, `SELECT COUNT(*) FROM event`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("event count = %d, want 1", count)
	}
	if _, err := read.SQL().ExecContext(ctx, `INSERT INTO event(id,session_id,sequence,type,data_json,created_at) VALUES('x','ses_b',9,'t','{}','now')`); err == nil {
		t.Fatal("write through a read-only connection succeeded")
	}

	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !before.ModTime().Equal(after.ModTime()) || before.Size() != after.Size() {
		t.Fatalf("read-only open modified the database: mtime %v -> %v, size %d -> %d",
			before.ModTime(), after.ModTime(), before.Size(), after.Size())
	}
}

func TestOpenReadOnlyDoesNotCreateMissingDatabase(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "absent.db")
	if _, err := OpenReadOnly(context.Background(), path); err == nil {
		t.Fatal("opening an absent database succeeded")
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("read-only open created %s", path)
	}
}

func TestOpenSessionOwnedRejectsForeignHost(t *testing.T) {
	t.Parallel()
	state := t.TempDir()
	ctx := context.Background()
	newSession(t, state, "ses_c").Close()

	if _, err := OpenSessionOwned(ctx, state, "ses_c", "host-b"); !errors.Is(err, ErrForeignHost) {
		t.Fatalf("error = %v, want ErrForeignHost", err)
	}
	owned, err := OpenSessionOwned(ctx, state, "ses_c", "host-a")
	if err != nil {
		t.Fatal(err)
	}
	owned.Close()
}

func TestListMetaOrdersNewestFirstAndSkipsUnreadable(t *testing.T) {
	t.Parallel()
	state := t.TempDir()
	for _, id := range []string{"ses_1", "ses_2"} {
		newSession(t, state, id).Close()
	}
	// A directory with no index stands in for a session created by a newer
	// binary, or one being created right now by another host.
	if err := CreateSessionDir(state, "ses_broken"); err != nil {
		t.Fatal(err)
	}

	metas, skipped, err := ListMeta(state)
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 2 {
		t.Fatalf("listed %d sessions, want 2", len(metas))
	}
	if len(skipped) != 1 || skipped[0] != "ses_broken" {
		t.Fatalf("skipped = %v, want [ses_broken]", skipped)
	}
	if metas[0].CreatedAt < metas[1].CreatedAt {
		t.Fatalf("sessions not ordered newest first: %v", metas)
	}
}
