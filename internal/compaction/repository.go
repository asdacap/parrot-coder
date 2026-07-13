package compaction

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/amirulashraf/parrot-coder/internal/event"
	"github.com/amirulashraf/parrot-coder/internal/id"
	"github.com/amirulashraf/parrot-coder/internal/protocol"
	"github.com/amirulashraf/parrot-coder/internal/store"
)

type Repository struct {
	db     *store.DB
	events *event.Repository
}

func NewRepository(db *store.DB, events *event.Repository) *Repository {
	return &Repository{db: db, events: events}
}

func (r *Repository) Load(ctx context.Context, sessionID string) (State, error) {
	var state State
	if err := r.db.SQL().QueryRowContext(ctx, `SELECT id,ordinal,baseline,sources_json,history_cutoff
		FROM session_context_epoch WHERE session_id=? ORDER BY ordinal DESC LIMIT 1`, sessionID).Scan(
		&state.Epoch.ID, &state.Epoch.Ordinal, &state.Epoch.Baseline, &state.Epoch.Sources, &state.Epoch.HistoryCutoff); err != nil {
		return State{}, fmt.Errorf("compaction: load epoch: %w", err)
	}
	rows, err := r.db.SQL().QueryContext(ctx, `SELECT id,role,content,parts_json,status,usage_json,sequence
		FROM session_message WHERE session_id=? AND sequence>=? ORDER BY sequence`, sessionID, state.Epoch.HistoryCutoff)
	if err != nil {
		return State{}, fmt.Errorf("compaction: load messages: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var item Message
		var role string
		var parts, usage []byte
		if err := rows.Scan(&item.ID, &role, &item.Content, &parts, &item.Status, &usage, &item.Sequence); err != nil {
			return State{}, err
		}
		item.Role = protocol.Role(role)
		if err := json.Unmarshal(parts, &item.Parts); err != nil {
			return State{}, fmt.Errorf("compaction: decode message %s parts: %w", item.ID, err)
		}
		if err := json.Unmarshal(usage, &item.Usage); err != nil {
			return State{}, fmt.Errorf("compaction: decode message %s usage: %w", item.ID, err)
		}
		state.Messages = append(state.Messages, item)
	}
	if err := rows.Err(); err != nil {
		return State{}, err
	}
	return state, nil
}

func (r *Repository) Completed(ctx context.Context, sessionID, epochID string, from, to int64) (Record, bool, error) {
	row := r.db.SQL().QueryRowContext(ctx, `SELECT id,attempt_id,session_id,source_epoch_id,target_epoch_id,
		covered_from_sequence,covered_to_sequence,history_cutoff,summary,usage_json,provider_id,model_id,created_at
		FROM compaction_record WHERE session_id=? AND source_epoch_id=? AND covered_from_sequence=? AND covered_to_sequence=?`,
		sessionID, epochID, from, to)
	record, err := scanRecord(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Record{}, false, nil
	}
	return record, err == nil, err
}

func (r *Repository) Begin(ctx context.Context, attempt Attempt) (Attempt, error) {
	if attempt.ID == "" {
		var err error
		attempt.ID, err = id.New("cmpa")
		if err != nil {
			return Attempt{}, err
		}
	}
	attempt.Status = "active"
	attempt.CreatedAt = time.Now().UTC()
	_, err := r.db.SQL().ExecContext(ctx, `INSERT INTO compaction_attempt(
		id,session_id,source_epoch_id,covered_from_sequence,covered_to_sequence,history_cutoff,
		provider_id,model_id,forced,status,created_at) VALUES(?,?,?,?,?,?,?,?,?,'active',?)`,
		attempt.ID, attempt.SessionID, attempt.SourceEpochID, attempt.CoveredFrom, attempt.CoveredTo,
		attempt.HistoryCutoff, attempt.ProviderID, attempt.ModelID, boolInt(attempt.Forced), formatTime(attempt.CreatedAt))
	if err != nil {
		return Attempt{}, fmt.Errorf("compaction: begin attempt: %w", err)
	}
	return attempt, nil
}

func (r *Repository) Complete(ctx context.Context, attempt Attempt, summary SummaryResult, fresh FullContext) (Record, error) {
	if !json.Valid(fresh.Sources) {
		return Record{}, errors.New("compaction: invalid context sources")
	}
	recordID, err := id.New("cmpr")
	if err != nil {
		return Record{}, err
	}
	epochID, err := id.New("ctx")
	if err != nil {
		return Record{}, err
	}
	usage, err := json.Marshal(summary.Usage)
	if err != nil {
		return Record{}, err
	}
	payload, _ := json.Marshal(map[string]any{"attempt_id": attempt.ID, "record_id": recordID, "source_epoch_id": attempt.SourceEpochID, "target_epoch_id": epochID, "history_cutoff": attempt.HistoryCutoff})
	var record Record
	_, err = r.events.Append(ctx, attempt.SessionID, []event.NewEvent{{Type: "session.compaction.completed", Data: payload}}, func(ctx context.Context, tx *sql.Tx, events []event.Event) error {
		var status string
		if err := tx.QueryRowContext(ctx, `SELECT status FROM compaction_attempt WHERE id=?`, attempt.ID).Scan(&status); err != nil {
			return err
		}
		if status != "active" {
			return errors.New("compaction: attempt is not active")
		}
		var current string
		var ordinal int
		if err := tx.QueryRowContext(ctx, `SELECT id,ordinal FROM session_context_epoch WHERE session_id=? ORDER BY ordinal DESC LIMIT 1`, attempt.SessionID).Scan(&current, &ordinal); err != nil {
			return err
		}
		if current != attempt.SourceEpochID {
			return errors.New("compaction: source epoch is no longer current")
		}
		now := events[0].CreatedAt
		if _, err := tx.ExecContext(ctx, `INSERT INTO session_context_epoch(
			id,session_id,ordinal,baseline,sources_json,history_cutoff,created_at) VALUES(?,?,?,?,?,?,?)`,
			epochID, attempt.SessionID, ordinal+1, fresh.Baseline, []byte(fresh.Sources), attempt.HistoryCutoff, formatTime(now)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO compaction_record(
			id,attempt_id,session_id,source_epoch_id,target_epoch_id,covered_from_sequence,covered_to_sequence,
			history_cutoff,summary,usage_json,provider_id,model_id,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			recordID, attempt.ID, attempt.SessionID, attempt.SourceEpochID, epochID, attempt.CoveredFrom,
			attempt.CoveredTo, attempt.HistoryCutoff, summary.Summary, usage, attempt.ProviderID, attempt.ModelID, formatTime(now)); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE compaction_attempt SET status='completed',error_text='',finished_at=? WHERE id=? AND status='active'`, formatTime(now), attempt.ID)
		if err != nil {
			return err
		}
		changed, _ := result.RowsAffected()
		if changed != 1 {
			return errors.New("compaction: attempt changed during completion")
		}
		record = Record{ID: recordID, AttemptID: attempt.ID, SessionID: attempt.SessionID, SourceEpochID: attempt.SourceEpochID, TargetEpochID: epochID, CoveredFrom: attempt.CoveredFrom, CoveredTo: attempt.CoveredTo, HistoryCutoff: attempt.HistoryCutoff, Summary: summary.Summary, Usage: summary.Usage, ProviderID: attempt.ProviderID, ModelID: attempt.ModelID, CreatedAt: now}
		return nil
	})
	if err != nil {
		return Record{}, fmt.Errorf("compaction: complete: %w", err)
	}
	return record, nil
}

func (r *Repository) Fail(ctx context.Context, attemptID, status, reason string) error {
	if status != "failed" && status != "interrupted" {
		return errors.New("compaction: invalid failure status")
	}
	reason = boundedText(reason, 1024)
	_, err := r.db.SQL().ExecContext(ctx, `UPDATE compaction_attempt SET status=?,error_text=?,finished_at=? WHERE id=? AND status='active'`, status, reason, formatTime(time.Now().UTC()), attemptID)
	return err
}

// InterruptAbandoned marks a bounded batch of attempts left active by an
// earlier process. It never changes completed records or context epochs.
func (r *Repository) InterruptAbandoned(ctx context.Context, limit int, reason string) (int64, error) {
	if limit <= 0 {
		limit = 100
	}
	result, err := r.db.SQL().ExecContext(ctx, `UPDATE compaction_attempt SET status='interrupted',error_text=?,finished_at=?
		WHERE id IN (SELECT id FROM compaction_attempt WHERE status='active' ORDER BY created_at LIMIT ?)`, boundedText(reason, 1024), formatTime(time.Now().UTC()), limit)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func (r *Repository) Records(ctx context.Context, sessionID string) ([]Record, error) {
	rows, err := r.db.SQL().QueryContext(ctx, `SELECT id,attempt_id,session_id,source_epoch_id,target_epoch_id,
		covered_from_sequence,covered_to_sequence,history_cutoff,summary,usage_json,provider_id,model_id,created_at
		FROM compaction_record WHERE session_id=? ORDER BY created_at,id`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []Record
	for rows.Next() {
		record, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

type scanner interface{ Scan(...any) error }

func scanRecord(row scanner) (Record, error) {
	var record Record
	var usage []byte
	var created string
	if err := row.Scan(&record.ID, &record.AttemptID, &record.SessionID, &record.SourceEpochID, &record.TargetEpochID,
		&record.CoveredFrom, &record.CoveredTo, &record.HistoryCutoff, &record.Summary, &usage,
		&record.ProviderID, &record.ModelID, &created); err != nil {
		return Record{}, err
	}
	if err := json.Unmarshal(usage, &record.Usage); err != nil {
		return Record{}, err
	}
	parsed, err := time.Parse(time.RFC3339Nano, created)
	record.CreatedAt = parsed
	return record, err
}

func boundedText(value string, max int) string {
	if len(value) <= max {
		return value
	}
	value = value[:max]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func formatTime(value time.Time) string { return value.UTC().Format(time.RFC3339Nano) }
