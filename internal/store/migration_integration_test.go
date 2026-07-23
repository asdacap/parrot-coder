package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestMigration006WithSnapshotDataUpgradesThrough008AndReopens(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "parrot.db")
	migrations, err := loadMigrations(legacyMigrations)
	if err != nil {
		t.Fatal(err)
	}
	if len(migrations) != 8 {
		t.Fatalf("migration count = %d, want 8", len(migrations))
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, `CREATE TABLE _parrot_migration (version INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE, checksum TEXT NOT NULL, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for _, migration := range migrations[:6] {
		if _, err := raw.ExecContext(ctx, migration.sql); err != nil {
			t.Fatalf("apply %s: %v", migration.name, err)
		}
		if _, err := raw.ExecContext(ctx, `INSERT INTO _parrot_migration(version,name,checksum,applied_at) VALUES(?,?,?,?)`, migration.version, migration.name, migration.checksum, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := raw.ExecContext(ctx, `
		INSERT INTO session(id,title,created_at,updated_at) VALUES('session-1','legacy','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z');
		INSERT INTO snapshot_blob(hash,data,size) VALUES('blob-1',X'61',1);
		INSERT INTO snapshot_transaction(id,workspace,session_id,position,created_at) VALUES('transaction-1','/workspace','session-1',1,'2026-01-01T00:00:00Z');
		INSERT INTO snapshot_file(transaction_id,ordinal,path,before_exists,before_mode,before_hash,before_blob_hash,after_exists,after_mode,after_hash,after_blob_hash)
		VALUES('transaction-1',0,'file.txt',1,420,'before','blob-1',1,420,'after','blob-1');
		INSERT INTO snapshot_cursor(workspace,session_id,position) VALUES('/workspace','session-1',1);
	`); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.SQL().QueryRowContext(ctx, `SELECT COUNT(*) FROM _parrot_migration`).Scan(&count); err != nil || count != 8 {
		t.Fatalf("migration count after upgrade = %d, %v", count, err)
	}
	if err := db.SQL().QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name LIKE 'snapshot_%'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("snapshot table count after upgrade = %d, %v", count, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.SQL().QueryRowContext(ctx, `SELECT COUNT(*) FROM compaction_record`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("compaction table after reopen = %d, %v", count, err)
	}
}
