package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const migrationTable = "_parrot_migration"

var ErrUnknownDatabase = errors.New("store: non-empty database is not a parrot database")

//go:embed migrations/*.sql
var migrationFiles embed.FS

// DB is an opened, migrated Parrot SQLite database.
type DB struct {
	sql *sql.DB
}

// Open opens a private SQLite file and applies all known migrations.
func Open(ctx context.Context, path string) (*DB, error) {
	if path == "" {
		return nil, errors.New("store: database path is empty")
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("store: resolve database path: %w", err)
	}
	file, err := os.OpenFile(abs, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("store: create database: %w", err)
	}
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("store: close database file: %w", err)
	}
	if err := os.Chmod(abs, 0o600); err != nil {
		return nil, fmt.Errorf("store: restrict database permissions: %w", err)
	}

	u := &url.URL{Scheme: "file", Path: filepath.ToSlash(abs)}
	q := u.Query()
	q.Set("mode", "rwc")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "foreign_keys(ON)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Set("_txlock", "immediate")
	u.RawQuery = q.Encode()

	sqldb, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, fmt.Errorf("store: open sqlite: %w", err)
	}
	sqldb.SetMaxOpenConns(8)
	sqldb.SetMaxIdleConns(8)
	db := &DB{sql: sqldb}
	if err := db.initialize(ctx); err != nil {
		sqldb.Close()
		return nil, err
	}
	return db, nil
}

func (db *DB) initialize(ctx context.Context) error {
	if err := db.sql.PingContext(ctx); err != nil {
		return fmt.Errorf("store: connect: %w", err)
	}

	known, empty, err := databaseIdentity(ctx, db.sql)
	if err != nil {
		return err
	}
	if !known && !empty {
		return ErrUnknownDatabase
	}
	if empty {
		if _, err := db.sql.ExecContext(ctx, `CREATE TABLE _parrot_migration (
            version INTEGER PRIMARY KEY,
            name TEXT NOT NULL UNIQUE,
            checksum TEXT NOT NULL,
            applied_at TEXT NOT NULL
        )`); err != nil {
			return fmt.Errorf("store: create migration journal: %w", err)
		}
	}
	return db.migrate(ctx)
}

func databaseIdentity(ctx context.Context, db *sql.DB) (known, empty bool, err error) {
	rows, err := db.QueryContext(ctx, `
        SELECT name FROM sqlite_master
        WHERE type IN ('table', 'index', 'view', 'trigger')
          AND name NOT LIKE 'sqlite_%'
        ORDER BY name`)
	if err != nil {
		return false, false, fmt.Errorf("store: inspect database: %w", err)
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return false, false, fmt.Errorf("store: inspect database object: %w", err)
		}
		count++
		if name == migrationTable {
			known = true
		}
	}
	if err := rows.Err(); err != nil {
		return false, false, fmt.Errorf("store: inspect database: %w", err)
	}
	return known, count == 0, nil
}

type migration struct {
	version  int
	name     string
	checksum string
	sql      string
}

func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		return nil, fmt.Errorf("store: read migrations: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	result := make([]migration, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		prefix, _, ok := strings.Cut(entry.Name(), "_")
		if !ok {
			return nil, fmt.Errorf("store: invalid migration name %q", entry.Name())
		}
		version, err := strconv.Atoi(prefix)
		if err != nil || version <= 0 {
			return nil, fmt.Errorf("store: invalid migration version in %q", entry.Name())
		}
		body, err := migrationFiles.ReadFile("migrations/" + entry.Name())
		if err != nil {
			return nil, fmt.Errorf("store: read migration %q: %w", entry.Name(), err)
		}
		sum := sha256.Sum256(body)
		result = append(result, migration{
			version: version, name: entry.Name(), checksum: fmt.Sprintf("%x", sum), sql: string(body),
		})
	}
	for i, migration := range result {
		if migration.version != i+1 {
			return nil, fmt.Errorf("store: migrations must be contiguous from version 1")
		}
	}
	return result, nil
}

func (db *DB) migrate(ctx context.Context) error {
	migrations, err := loadMigrations()
	if err != nil {
		return err
	}

	rows, err := db.sql.QueryContext(ctx, `SELECT version, name, checksum FROM _parrot_migration ORDER BY version`)
	if err != nil {
		return fmt.Errorf("store: read migration journal: %w", err)
	}
	applied := 0
	for rows.Next() {
		var version int
		var name, checksum string
		if err := rows.Scan(&version, &name, &checksum); err != nil {
			rows.Close()
			return fmt.Errorf("store: read migration journal: %w", err)
		}
		if version != applied+1 || version > len(migrations) {
			rows.Close()
			return fmt.Errorf("store: unknown migration version %d", version)
		}
		migration := migrations[version-1]
		if name != migration.name || checksum != migration.checksum {
			rows.Close()
			return fmt.Errorf("store: migration %d checksum or name mismatch", version)
		}
		applied++
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("store: read migration journal: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("store: close migration journal: %w", err)
	}

	for _, migration := range migrations[applied:] {
		err := db.WithImmediate(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, migration.sql); err != nil {
				return fmt.Errorf("apply %s: %w", migration.name, err)
			}
			_, err := tx.ExecContext(ctx,
				`INSERT INTO _parrot_migration(version, name, checksum, applied_at) VALUES (?, ?, ?, ?)`,
				migration.version, migration.name, migration.checksum, time.Now().UTC().Format(time.RFC3339Nano))
			return err
		})
		if err != nil {
			return fmt.Errorf("store: migrate: %w", err)
		}
	}
	return nil
}

// SQL exposes the connection pool for read queries and package-level repositories.
func (db *DB) SQL() *sql.DB { return db.sql }

// WithImmediate runs fn in a transaction that acquires SQLite's write lock at begin.
func (db *DB) WithImmediate(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin transaction: %w", err)
	}
	if err := fn(tx); err != nil {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			return errors.Join(err, fmt.Errorf("store: rollback: %w", rollbackErr))
		}
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	return nil
}

func (db *DB) Close() error { return db.sql.Close() }
