package store_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/amirulashraf/parrot-coder/internal/store"
	_ "modernc.org/sqlite"
)

func TestOpenMigratesAndReopensRealFile(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "parrot.db")
	db, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	var migrations int
	if err := db.SQL().QueryRowContext(ctx, `SELECT count(*) FROM _parrot_migration`).Scan(&migrations); err != nil {
		t.Fatal(err)
	}
	if migrations == 0 {
		t.Fatal("no migrations were journaled")
	}
	var journalMode, synchronous string
	var foreignKeys int
	if err := db.SQL().QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&journalMode); err != nil {
		t.Fatal(err)
	}
	if err := db.SQL().QueryRowContext(ctx, `PRAGMA synchronous`).Scan(&synchronous); err != nil {
		t.Fatal(err)
	}
	if err := db.SQL().QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		t.Fatal(err)
	}
	if journalMode != "wal" || synchronous != "1" || foreignKeys != 1 {
		t.Fatalf("pragmas: journal=%q synchronous=%q foreign_keys=%d", journalMode, synchronous, foreignKeys)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("database permissions = %o, want 600", got)
	}
	reopened, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	var reopenedMigrations int
	if err := reopened.SQL().QueryRowContext(ctx, `SELECT count(*) FROM _parrot_migration`).Scan(&reopenedMigrations); err != nil {
		t.Fatal(err)
	}
	if reopenedMigrations != migrations {
		t.Fatalf("migration count after reopen = %d, want %d", reopenedMigrations, migrations)
	}
}

func TestOpenRejectsUnknownNonEmptyDatabase(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "other.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, `CREATE TABLE somebody_elses_data (value TEXT)`); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	_, err = store.Open(ctx, path)
	if !errors.Is(err, store.ErrUnknownDatabase) {
		t.Fatalf("Open error = %v, want ErrUnknownDatabase", err)
	}
}

func TestOpenRejectsChangedMigrationJournal(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "parrot.db")
	db, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SQL().ExecContext(ctx, `UPDATE _parrot_migration SET checksum = 'changed' WHERE version = 1`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Open(ctx, path); err == nil {
		t.Fatal("Open succeeded with a changed migration checksum")
	}
}
