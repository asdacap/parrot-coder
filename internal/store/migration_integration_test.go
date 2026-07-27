package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestSessionNameMigrationBackfillsOnlyStrictLegacySubtaskTitles(t *testing.T) {
	lineages := []struct {
		name      string
		dir       string
		open      func(context.Context, string) (*DB, error)
		wantCount int
	}{
		{name: "legacy", dir: legacyMigrations, open: Open, wantCount: 12},
		{name: "session", dir: sessionMigrations, open: OpenSession, wantCount: 7},
	}
	rows := []struct {
		id, title, agent, want string
	}{
		{"exact", "Subtask inspect [build]", "build", "inspect"},
		{"nested", "Subtask inspect [carefully] [explorer]", "explorer", "inspect [carefully]"},
		{"ordinary", "Inspect the code", "build", ""},
		{"wrong-agent", "Subtask inspect [explorer]", "build", ""},
		{"trailing", "Subtask inspect [build] later", "build", ""},
		{"empty", "Subtask  [build]", "build", ""},
		{"no-agent", "Subtask inspect []", "", ""},
	}

	for _, lineage := range lineages {
		t.Run(lineage.name, func(t *testing.T) {
			migrations, err := loadMigrations(lineage.dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(migrations) != lineage.wantCount {
				t.Fatalf("migration count = %d, want %d", len(migrations), lineage.wantCount)
			}
			for _, row := range rows {
				t.Run(row.id, func(t *testing.T) {
					ctx := context.Background()
					path := filepath.Join(t.TempDir(), "session.db")
					raw, err := sql.Open("sqlite", path)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := raw.ExecContext(ctx, `CREATE TABLE _parrot_migration (version INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE, checksum TEXT NOT NULL, applied_at TEXT NOT NULL)`); err != nil {
						t.Fatal(err)
					}
					for _, migration := range migrations[:len(migrations)-4] {
						if _, err := raw.ExecContext(ctx, migration.sql); err != nil {
							t.Fatalf("apply %s: %v", migration.name, err)
						}
						if _, err := raw.ExecContext(ctx, `INSERT INTO _parrot_migration(version,name,checksum,applied_at) VALUES(?,?,?,?)`, migration.version, migration.name, migration.checksum, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
							t.Fatal(err)
						}
					}
					if _, err := raw.ExecContext(ctx, `INSERT INTO session(id,title,selected_agent,created_at,updated_at) VALUES(?,?,?,?,?)`, row.id, row.title, row.agent, "2026-01-01T00:00:00Z", "2026-01-01T00:00:00Z"); err != nil {
						t.Fatal(err)
					}
					if err := raw.Close(); err != nil {
						t.Fatal(err)
					}

					db, err := lineage.open(ctx, path)
					if err != nil {
						t.Fatal(err)
					}
					defer db.Close()
					var got string
					if err := db.SQL().QueryRowContext(ctx, `SELECT name FROM session WHERE id=?`, row.id).Scan(&got); err != nil {
						t.Fatal(err)
					}
					if got != row.want {
						t.Errorf("name = %q, want %q", got, row.want)
					}
				})
			}
		})
	}
}

func TestCompactionEpochMigrationBackfillsCompletedTargetsAndPreservesReferences(t *testing.T) {
	lineages := []struct {
		name string
		dir  string
		open func(context.Context, string) (*DB, error)
	}{
		{name: "legacy", dir: legacyMigrations, open: Open},
		{name: "session", dir: sessionMigrations, open: OpenSession},
	}
	const wantPrompt = "----- BEGIN COMPACTED SESSION HISTORY -----\nSource epoch: epoch-0\nCovered sequences: 0-3\nHistory cutoff: 4\n\nsummary text\n----- END COMPACTED SESSION HISTORY -----"

	for _, lineage := range lineages {
		t.Run(lineage.name, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "session.db")
			migrations, err := loadMigrations(lineage.dir)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := raw.ExecContext(ctx, `CREATE TABLE _parrot_migration (version INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE, checksum TEXT NOT NULL, applied_at TEXT NOT NULL)`); err != nil {
				t.Fatal(err)
			}
			for _, migration := range migrations[:len(migrations)-3] {
				if _, err := raw.ExecContext(ctx, migration.sql); err != nil {
					t.Fatalf("apply %s: %v", migration.name, err)
				}
				if _, err := raw.ExecContext(ctx, `INSERT INTO _parrot_migration(version,name,checksum,applied_at) VALUES(?,?,?,?)`, migration.version, migration.name, migration.checksum, "2026-01-01T00:00:00Z"); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := raw.ExecContext(ctx, `
				INSERT INTO session(id,title,created_at,updated_at) VALUES('session-1','title','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z');
				INSERT INTO session_context_epoch VALUES('epoch-0','session-1',0,'old baseline','[]',0,'2026-01-01T00:00:00Z');
				INSERT INTO session_context_epoch VALUES('epoch-1','session-1',1,'old baseline','[]',4,'2026-01-01T00:01:00Z');
				INSERT INTO compaction_attempt VALUES('attempt-1','session-1','epoch-0',0,3,4,'provider','model',0,'completed','', '2026-01-01T00:00:30Z','2026-01-01T00:01:00Z');
				INSERT INTO compaction_record VALUES('record-1','attempt-1','session-1','epoch-0','epoch-1',0,3,4,'  summary text  ','{}','provider','model','2026-01-01T00:01:00Z');
			`); err != nil {
				t.Fatal(err)
			}
			if err := raw.Close(); err != nil {
				t.Fatal(err)
			}

			db, err := lineage.open(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			rows, err := db.SQL().QueryContext(ctx, `SELECT id, summary_prompt FROM session_compaction_epoch WHERE session_id='session-1' ORDER BY ordinal`)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			var got [][2]string
			for rows.Next() {
				var row [2]string
				if err := rows.Scan(&row[0], &row[1]); err != nil {
					t.Fatal(err)
				}
				got = append(got, row)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			if len(got) != 2 || got[0] != [2]string{"epoch-0", ""} || got[1] != [2]string{"epoch-1", wantPrompt} {
				t.Fatalf("migrated epochs = %#v", got)
			}
			var attemptID, recordID string
			if err := db.SQL().QueryRowContext(ctx, `SELECT (SELECT id FROM compaction_attempt), (SELECT id FROM compaction_record)`).Scan(&attemptID, &recordID); err != nil {
				t.Fatal(err)
			}
			if attemptID != "attempt-1" || recordID != "record-1" {
				t.Fatalf("preserved IDs = %q, %q", attemptID, recordID)
			}
			var violations int
			if err := db.SQL().QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_foreign_key_check`).Scan(&violations); err != nil || violations != 0 {
				t.Fatalf("foreign key violations = %d, %v", violations, err)
			}
		})
	}
}

func TestCompactionEpochMigrationCreatesInitialEpochForMissingSession(t *testing.T) {
	const baseID = "ctx_migrated_73657373696F6E2D32"
	tests := []struct {
		name, dir string
		open      func(context.Context, string) (*DB, error)
		collision bool
		wantID    string
	}{
		{name: "legacy", dir: legacyMigrations, open: Open, collision: true, wantID: baseID + "_1"},
		{name: "session", dir: sessionMigrations, open: OpenSession, wantID: baseID},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "session.db")
			migrations, err := loadMigrations(test.dir)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := raw.ExecContext(ctx, `CREATE TABLE _parrot_migration (version INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE, checksum TEXT NOT NULL, applied_at TEXT NOT NULL)`); err != nil {
				t.Fatal(err)
			}
			for _, migration := range migrations[:len(migrations)-3] {
				if _, err := raw.ExecContext(ctx, migration.sql); err != nil {
					t.Fatalf("apply %s: %v", migration.name, err)
				}
				if _, err := raw.ExecContext(ctx, `INSERT INTO _parrot_migration(version,name,checksum,applied_at) VALUES(?,?,?,?)`, migration.version, migration.name, migration.checksum, "2026-01-01T00:00:00Z"); err != nil {
					t.Fatal(err)
				}
			}
			if test.collision {
				if _, err := raw.ExecContext(ctx, `
					INSERT INTO session(id,title,created_at,updated_at) VALUES('session-1','existing','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z');
					INSERT INTO session_context_epoch VALUES('`+baseID+`','session-1',0,'old baseline','[]',0,'2026-01-01T00:00:00Z');
				`); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := raw.ExecContext(ctx, `INSERT INTO session(id,title,created_at,updated_at) VALUES('session-2','missing','2026-01-02T00:00:00Z','2026-01-02T00:00:00Z')`); err != nil {
				t.Fatal(err)
			}
			if err := raw.Close(); err != nil {
				t.Fatal(err)
			}

			db, err := test.open(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			var id, prompt, createdAt string
			var ordinal int
			var cutoff int64
			if err := db.SQL().QueryRowContext(ctx, `SELECT id,ordinal,summary_prompt,history_cutoff,created_at FROM session_compaction_epoch WHERE session_id='session-2'`).Scan(&id, &ordinal, &prompt, &cutoff, &createdAt); err != nil {
				t.Fatal(err)
			}
			if id != test.wantID || ordinal != 0 || prompt != "" || cutoff != 0 || createdAt != "2026-01-02T00:00:00Z" {
				t.Fatalf("initial epoch = %q, %d, %q, %d, %q", id, ordinal, prompt, cutoff, createdAt)
			}
		})
	}
}

func TestCanonicalModelMigrationCollapsesSelectionColumns(t *testing.T) {
	lineages := []struct {
		name string
		dir  string
		open func(context.Context, string) (*DB, error)
	}{
		{name: "legacy", dir: legacyMigrations, open: Open},
		{name: "session", dir: sessionMigrations, open: OpenSession},
	}
	rows := []struct {
		id, provider, model, variant, want string
	}{
		{id: "complete", provider: "chatgpt", model: "gpt", variant: "high", want: "chatgpt/gpt/high"},
		{id: "no-variant", provider: "openrouter", model: "anthropic/claude", want: "openrouter/anthropic/claude"},
		{id: "unqualified", model: "local-model", want: "local-model"},
		{id: "empty", provider: "unused", variant: "unused", want: ""},
	}

	for _, lineage := range lineages {
		t.Run(lineage.name, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "session.db")
			migrations, err := loadMigrations(lineage.dir)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := raw.ExecContext(ctx, `CREATE TABLE _parrot_migration (version INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE, checksum TEXT NOT NULL, applied_at TEXT NOT NULL)`); err != nil {
				t.Fatal(err)
			}
			for _, migration := range migrations[:len(migrations)-2] {
				if _, err := raw.ExecContext(ctx, migration.sql); err != nil {
					t.Fatalf("apply %s: %v", migration.name, err)
				}
				if _, err := raw.ExecContext(ctx, `INSERT INTO _parrot_migration(version,name,checksum,applied_at) VALUES(?,?,?,?)`, migration.version, migration.name, migration.checksum, "2026-01-01T00:00:00Z"); err != nil {
					t.Fatal(err)
				}
			}
			testRows := rows
			if lineage.dir == sessionMigrations {
				// A per-session database deliberately enforces a singleton row.
				testRows = rows[:1]
			}
			for _, row := range testRows {
				if _, err := raw.ExecContext(ctx, `INSERT INTO session(id,title,selected_provider,selected_model,selected_variant,created_at,updated_at) VALUES(?,?,?,?,?,'2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`, row.id, row.id, row.provider, row.model, row.variant); err != nil {
					t.Fatal(err)
				}
			}
			if err := raw.Close(); err != nil {
				t.Fatal(err)
			}

			db, err := lineage.open(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			for _, row := range testRows {
				var got string
				if err := db.SQL().QueryRowContext(ctx, `SELECT selected_model FROM session WHERE id=?`, row.id).Scan(&got); err != nil {
					t.Fatal(err)
				}
				if got != row.want {
					t.Errorf("session %s model = %q, want %q", row.id, got, row.want)
				}
			}
			for _, column := range []string{"selected_provider", "selected_variant"} {
				var count int
				if err := db.SQL().QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('session') WHERE name=?`, column).Scan(&count); err != nil {
					t.Fatal(err)
				}
				if count != 0 {
					t.Errorf("column %s remains after migration", column)
				}
			}
		})
	}
}

func TestMigration006WithSnapshotDataUpgradesThrough012AndReopens(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "parrot.db")
	migrations, err := loadMigrations(legacyMigrations)
	if err != nil {
		t.Fatal(err)
	}
	if len(migrations) != 12 {
		t.Fatalf("migration count = %d, want 12", len(migrations))
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
	if err := db.SQL().QueryRowContext(ctx, `SELECT COUNT(*) FROM _parrot_migration`).Scan(&count); err != nil || count != 12 {
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
