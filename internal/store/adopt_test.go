package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// seedLegacy builds a database in the pre-split shape: one file, every session
// in it, opened in WAL as the previous version left it.
func seedLegacy(t *testing.T, state string, sessionIDs ...string) string {
	t.Helper()
	path := filepath.Join(state, "parrot.db")
	legacy, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := legacy.SQL().ExecContext(ctx,
		`INSERT INTO project(id,root_path,created_at) VALUES('prj_1','/w',?)`, now); err != nil {
		t.Fatal(err)
	}
	for i, id := range sessionIDs {
		if _, err := legacy.SQL().ExecContext(ctx, `
			INSERT INTO session(id,project_id,title,selected_agent,selected_provider,selected_model,selected_variant,created_at,updated_at)
			VALUES(?,'prj_1',?,'build','p','m','',?,?)`, id, "title-"+id, now, now); err != nil {
			t.Fatal(err)
		}
		if _, err := legacy.SQL().ExecContext(ctx,
			`INSERT INTO event_sequence(session_id,next_sequence) VALUES(?,1)`, id); err != nil {
			t.Fatal(err)
		}
		if _, err := legacy.SQL().ExecContext(ctx, `
			INSERT INTO event(id,session_id,sequence,type,data_json,created_at) VALUES(?,?,0,'session.created',?,?)`,
			"evt_"+id, id, []byte(`{"n":`+string(rune('0'+i))+`}`), now); err != nil {
			t.Fatal(err)
		}
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAdoptLegacySplitsSessionsAndSetsFileAside(t *testing.T) {
	t.Parallel()
	state := t.TempDir()
	ctx := context.Background()
	path := seedLegacy(t, state, "ses_1", "ses_2")

	if err := AdoptLegacy(ctx, state, path); err != nil {
		t.Fatal(err)
	}

	metas, skipped, err := ListMeta(state)
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 2 || len(skipped) != 0 {
		t.Fatalf("adopted %d sessions (skipped %v), want 2", len(metas), skipped)
	}
	for _, meta := range metas {
		if meta.ProjectID != "prj_1" || meta.ProjectRoot != "/w" {
			t.Fatalf("session %s lost its project: %+v", meta.ID, meta)
		}
		// Adoption makes a session visible; it must not claim it for this host.
		if meta.HostKey != "" {
			t.Fatalf("adoption stamped a host key on %s", meta.ID)
		}
		db, err := OpenSession(ctx, DatabasePath(state, meta.ID))
		if err != nil {
			t.Fatal(err)
		}
		var events, next int
		if err := db.SQL().QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM event), (SELECT next_sequence FROM event_sequence)`).Scan(&events, &next); err != nil {
			t.Fatal(err)
		}
		db.Close()
		if events != 1 || next != 1 {
			t.Fatalf("session %s carried %d events, next=%d", meta.ID, events, next)
		}
	}

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("legacy database is still in place")
	}
	// The legacy file is renamed, never deleted: a failed adoption must be
	// recoverable and inspectable.
	entries, err := os.ReadDir(state)
	if err != nil {
		t.Fatal(err)
	}
	var aside bool
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "parrot.db.migrated-") {
			aside = true
		}
		if strings.HasSuffix(entry.Name(), "-wal") || strings.HasSuffix(entry.Name(), "-shm") {
			t.Fatalf("adoption left %s behind", entry.Name())
		}
	}
	if !aside {
		t.Fatal("legacy database was removed rather than set aside")
	}
}

func TestAdoptLegacyIsIdempotentAndSkipsWhenSessionsExist(t *testing.T) {
	t.Parallel()
	state := t.TempDir()
	ctx := context.Background()
	path := seedLegacy(t, state, "ses_1")

	if err := AdoptLegacy(ctx, state, path); err != nil {
		t.Fatal(err)
	}
	// A second run has no legacy file and must not fail.
	if err := AdoptLegacy(ctx, state, path); err != nil {
		t.Fatal(err)
	}
	// A legacy file reappearing next to existing sessions is left alone rather
	// than merged into them.
	seedLegacy(t, state, "ses_9")
	if err := AdoptLegacy(ctx, state, path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("adoption consumed a legacy file despite existing sessions")
	}
	metas, _, err := ListMeta(state)
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 1 {
		t.Fatalf("listed %d sessions, want the 1 already adopted", len(metas))
	}
}

func TestAdoptLegacyLockStopsConcurrentAdoption(t *testing.T) {
	t.Parallel()
	state := t.TempDir()
	path := seedLegacy(t, state, "ses_1")

	// A lock held by another process means adoption is already running there.
	lock, err := os.OpenFile(filepath.Join(state, adoptLockKey), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	lock.Close()

	if err := AdoptLegacy(context.Background(), state, path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("adoption proceeded while another process held the lock")
	}
	metas, _, err := ListMeta(state)
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 0 {
		t.Fatalf("adopted %d sessions while locked out, want 0", len(metas))
	}
}

var _ = sql.ErrNoRows
