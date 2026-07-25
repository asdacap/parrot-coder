package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

const adoptLockKey = ".adopt.lock"

// legacyTables lists the per-session tables copied out of the shared database,
// in an order that satisfies their foreign keys.
var legacyTables = []struct {
	name    string
	columns string
}{
	{"event_sequence", "session_id,next_sequence"},
	{"event", "id,session_id,sequence,type,data_json,created_at"},
	{"session_input", "id,session_id,message_id,content,delivery,status,admitted_sequence,promoted_sequence,created_at,promoted_at"},
	{"session_message", "id,session_id,role,content,input_id,sequence,created_at,parts_json,status,finish_reason,error_text,usage_json"},
	{"session_context_epoch", "id,session_id,ordinal,baseline,sources_json,history_cutoff,created_at"},
	{"session_tool_call", "id,session_id,message_id,name,input_json,status,result_text,error_text,sequence,settled_sequence,created_at,settled_at"},
	{"session_todo", "session_id,id,position,content,status,priority"},
	{"compaction_attempt", "id,session_id,source_epoch_id,covered_from_sequence,covered_to_sequence,history_cutoff,provider_id,model_id,forced,status,error_text,created_at,finished_at"},
	{"compaction_record", "id,attempt_id,session_id,source_epoch_id,target_epoch_id,covered_from_sequence,covered_to_sequence,history_cutoff,summary,usage_json,provider_id,model_id,created_at"},
}

// AdoptLegacy splits a pre-split shared database into one database per session.
//
// It is a one-shot upgrade and does nothing once sessions exist or the legacy
// file is gone. The legacy file is renamed aside rather than deleted, so a
// failed adoption can be retried or inspected.
//
// The whole history lives in one file with no record of which machine created
// which session, so there is nothing to divide it on: the first host to upgrade
// adopts every session. Other hosts can list them, and the ownership stamp stops
// them from opening one this host is running.
func AdoptLegacy(ctx context.Context, state, legacyPath string) error {
	if _, err := os.Stat(legacyPath); errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if entries, err := os.ReadDir(SessionsRoot(state)); err == nil && len(entries) > 0 {
		return nil
	}

	// O_EXCL create is atomic on the filesystems Parrot supports, including
	// NFSv3 and later, so a second process starting at the same moment skips
	// adoption instead of duplicating it.
	lock, err := os.OpenFile(filepath.Join(state, adoptLockKey), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("store: acquire adoption lock: %w", err)
	}
	lock.Close()
	defer os.Remove(filepath.Join(state, adoptLockKey))

	legacy, err := Open(ctx, legacyPath)
	if err != nil {
		return fmt.Errorf("store: open legacy database: %w", err)
	}
	// Fold any write-ahead log back into the file before reading it, so a
	// checkpoint left by the previous version is not lost with its -wal.
	if _, err := legacy.SQL().ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		legacy.Close()
		return fmt.Errorf("store: checkpoint legacy database: %w", err)
	}
	ids, err := legacySessionIDs(ctx, legacy)
	if err != nil {
		legacy.Close()
		return err
	}
	for _, sessionID := range ids {
		if err := adoptSession(ctx, legacy, state, sessionID); err != nil {
			legacy.Close()
			return fmt.Errorf("store: adopt session %s: %w", sessionID, err)
		}
	}
	if err := legacy.Close(); err != nil {
		return err
	}

	aside := legacyPath + ".migrated-" + time.Now().UTC().Format("20060102T150405Z")
	if err := os.Rename(legacyPath, aside); err != nil {
		return fmt.Errorf("store: set legacy database aside: %w", err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.Remove(legacyPath + suffix); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("store: remove %s: %w", legacyPath+suffix, err)
		}
	}
	return nil
}

func legacySessionIDs(ctx context.Context, legacy *DB) ([]string, error) {
	rows, err := legacy.SQL().QueryContext(ctx, `SELECT id FROM session ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("store: list legacy sessions: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func adoptSession(ctx context.Context, legacy *DB, state, sessionID string) error {
	if err := CreateSessionDir(state, sessionID); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	db, err := OpenSession(ctx, DatabasePath(state, sessionID))
	if err != nil {
		return err
	}
	defer db.Close()

	var meta Meta
	err = legacy.SQL().QueryRowContext(ctx, `
		SELECT id, name, COALESCE(parent_session_id,''), COALESCE(project_id,''), title, selected_agent, selected_provider, selected_model, selected_variant, created_at, updated_at
		FROM session WHERE id=?`, sessionID).Scan(&meta.ID, &meta.Name, &meta.ParentSessionID, &meta.ProjectID, &meta.Title,
		&meta.Agent, &meta.Provider, &meta.Model, &meta.Variant, &meta.CreatedAt, &meta.UpdatedAt)
	if err != nil {
		return err
	}
	var projectRoot string
	_ = legacy.SQL().QueryRowContext(ctx, `SELECT root_path FROM project WHERE id=?`, meta.ProjectID).Scan(&projectRoot)
	meta.ProjectRoot = projectRoot

	err = db.WithImmediate(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO session(id,name,parent_session_id,project_id,project_root,title,selected_agent,selected_provider,selected_model,selected_variant,created_at,updated_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, meta.ID, meta.Name, meta.ParentSessionID, meta.ProjectID, meta.ProjectRoot, meta.Title,
			meta.Agent, meta.Provider, meta.Model, meta.Variant, meta.CreatedAt, meta.UpdatedAt); err != nil {
			return err
		}
		for _, table := range legacyTables {
			if err := copyTable(ctx, legacy, tx, table.name, table.columns, sessionID); err != nil {
				return fmt.Errorf("copy %s: %w", table.name, err)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	// The index entry carries no host key: adoption does not claim the session
	// for this machine, it only makes it visible.
	return WriteMeta(state, meta)
}

func copyTable(ctx context.Context, legacy *DB, tx *sql.Tx, table, columns, sessionID string) error {
	rows, err := legacy.SQL().QueryContext(ctx,
		fmt.Sprintf(`SELECT %s FROM %s WHERE session_id=?`, columns, table), sessionID)
	if err != nil {
		return err
	}
	defer rows.Close()

	names, err := rows.Columns()
	if err != nil {
		return err
	}
	placeholders := ""
	for i := range names {
		if i > 0 {
			placeholders += ","
		}
		placeholders += "?"
	}
	insert := fmt.Sprintf(`INSERT INTO %s(%s) VALUES(%s)`, table, columns, placeholders)

	for rows.Next() {
		values := make([]any, len(names))
		targets := make([]any, len(names))
		for i := range values {
			targets[i] = &values[i]
		}
		if err := rows.Scan(targets...); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, insert, values...); err != nil {
			return err
		}
	}
	return rows.Err()
}
