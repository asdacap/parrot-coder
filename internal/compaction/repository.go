package compaction

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	v1 "github.com/amirulashraf/parrot-coder/internal/api/v1"
	"github.com/amirulashraf/parrot-coder/internal/event"
	"github.com/amirulashraf/parrot-coder/internal/id"
	"github.com/amirulashraf/parrot-coder/internal/processidentity"
	"github.com/amirulashraf/parrot-coder/internal/protocol"
	"github.com/amirulashraf/parrot-coder/internal/store"
)

type Repository struct {
	sessions *store.Registry
	events   *event.Repository
	owner    processidentity.Identity
	inspect  func(processidentity.Identity, processidentity.Identity) processidentity.Liveness
}

func NewRepository(sessions *store.Registry, events *event.Repository, owner processidentity.Identity) *Repository {
	return &Repository{sessions: sessions, events: events, owner: owner, inspect: processidentity.Inspect}
}

func (r *Repository) Load(ctx context.Context, sessionID string) (State, error) {
	db, err := r.sessions.Session(ctx, sessionID)
	if err != nil {
		return State{}, err
	}
	var state State
	if err := db.SQL().QueryRowContext(ctx, `SELECT id,ordinal,summary_prompt,history_cutoff
		FROM session_compaction_epoch WHERE session_id=? ORDER BY ordinal DESC LIMIT 1`, sessionID).Scan(
		&state.Checkpoint.ID, &state.Checkpoint.Ordinal, &state.Checkpoint.SummaryPrompt, &state.Checkpoint.HistoryCutoff); err != nil {
		return State{}, fmt.Errorf("compaction: load epoch: %w", err)
	}
	rows, err := db.SQL().QueryContext(ctx, `SELECT id,role,content,parts_json,status,usage_json,sequence
		FROM session_message WHERE session_id=? AND sequence>=? ORDER BY sequence`, sessionID, state.Checkpoint.HistoryCutoff)
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
	db, err := r.sessions.Session(ctx, sessionID)
	if err != nil {
		return Record{}, false, err
	}
	row := db.SQL().QueryRowContext(ctx, `SELECT id,attempt_id,session_id,source_epoch_id,target_epoch_id,
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
	if r.owner.HostKey == "" || r.owner.PID <= 0 || r.owner.ProcessKey == "" {
		return Attempt{}, errors.New("compaction: process owner is required")
	}
	attempt.Status = "active"
	payload, _ := json.Marshal(v1.CompactionEvent{
		AttemptID: attempt.ID, Status: "started", SourceEpochID: attempt.SourceEpochID, HistoryCutoff: attempt.HistoryCutoff,
	})
	_, err := r.events.Append(ctx, attempt.SessionID, []event.NewEvent{{Type: v1.EventSessionCompactionStarted, Data: payload}}, func(ctx context.Context, tx *sql.Tx, events []event.Event) error {
		attempt.CreatedAt = events[0].CreatedAt
		_, err := tx.ExecContext(ctx, `INSERT INTO compaction_attempt(
			id,session_id,source_epoch_id,covered_from_sequence,covered_to_sequence,history_cutoff,
			provider_id,model_id,forced,status,created_at,owner_host_key,owner_pid,owner_process_key)
			VALUES(?,?,?,?,?,?,?,?,?,'active',?,?,?,?)`,
			attempt.ID, attempt.SessionID, attempt.SourceEpochID, attempt.CoveredFrom, attempt.CoveredTo,
			attempt.HistoryCutoff, attempt.ProviderID, attempt.ModelID, boolInt(attempt.Forced), formatTime(attempt.CreatedAt),
			r.owner.HostKey, r.owner.PID, r.owner.ProcessKey)
		return err
	})
	if err != nil {
		return Attempt{}, fmt.Errorf("compaction: begin attempt: %w", err)
	}
	return attempt, nil
}

func (r *Repository) Complete(ctx context.Context, attempt Attempt, summary SummaryResult) (Record, error) {
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
	payload, _ := json.Marshal(v1.CompactionEvent{
		AttemptID: attempt.ID, Status: "completed", RecordID: recordID, SourceEpochID: attempt.SourceEpochID,
		TargetEpochID: epochID, HistoryCutoff: attempt.HistoryCutoff,
	})
	var record Record
	_, err = r.events.Append(ctx, attempt.SessionID, []event.NewEvent{
		{Type: v1.EventSessionCompactionCompleted, Data: payload},
		{Type: v1.EventSessionCompactionFinished, Data: payload},
	}, func(ctx context.Context, tx *sql.Tx, events []event.Event) error {
		var status, hostKey, processKey string
		var pid int
		if err := tx.QueryRowContext(ctx, `SELECT status,owner_host_key,owner_pid,owner_process_key FROM compaction_attempt WHERE id=?`, attempt.ID).Scan(&status, &hostKey, &pid, &processKey); err != nil {
			return err
		}
		if status != "active" {
			return errors.New("compaction: attempt is not active")
		}
		if (processidentity.Identity{HostKey: hostKey, PID: pid, ProcessKey: processKey}) != r.owner {
			return errors.New("compaction: attempt belongs to another process")
		}
		var current string
		var ordinal int
		if err := tx.QueryRowContext(ctx, `SELECT id,ordinal FROM session_compaction_epoch WHERE session_id=? ORDER BY ordinal DESC LIMIT 1`, attempt.SessionID).Scan(&current, &ordinal); err != nil {
			return err
		}
		if current != attempt.SourceEpochID {
			return errors.New("compaction: source epoch is no longer current")
		}
		now := events[0].CreatedAt
		compactedSummaryPrompt := composeCompactedSummaryPrompt(summary.Summary, attempt)
		if _, err := tx.ExecContext(ctx, `INSERT INTO session_compaction_epoch(
			id,session_id,ordinal,summary_prompt,history_cutoff,created_at) VALUES(?,?,?,?,?,?)`,
			epochID, attempt.SessionID, ordinal+1, compactedSummaryPrompt, attempt.HistoryCutoff, formatTime(now)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO compaction_record(
			id,attempt_id,session_id,source_epoch_id,target_epoch_id,covered_from_sequence,covered_to_sequence,
			history_cutoff,summary,usage_json,provider_id,model_id,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			recordID, attempt.ID, attempt.SessionID, attempt.SourceEpochID, epochID, attempt.CoveredFrom,
			attempt.CoveredTo, attempt.HistoryCutoff, summary.Summary, usage, attempt.ProviderID, attempt.ModelID, formatTime(now)); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE compaction_attempt SET status='completed',error_text='',finished_at=?
			WHERE id=? AND status='active' AND owner_host_key=? AND owner_pid=? AND owner_process_key=?`,
			formatTime(now), attempt.ID, r.owner.HostKey, r.owner.PID, r.owner.ProcessKey)
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

func (r *Repository) Fail(ctx context.Context, sessionID, attemptID, status, reason string) error {
	if status != "failed" && status != "interrupted" {
		return errors.New("compaction: invalid failure status")
	}
	reason = boundedText(reason, 1024)
	payload, _ := json.Marshal(v1.CompactionEvent{AttemptID: attemptID, Status: status, Error: reason})
	_, err := r.events.Append(ctx, sessionID, []event.NewEvent{{Type: v1.EventSessionCompactionFinished, Data: payload}}, func(ctx context.Context, tx *sql.Tx, events []event.Event) error {
		result, err := tx.ExecContext(ctx, `UPDATE compaction_attempt SET status=?,error_text=?,finished_at=?
			WHERE id=? AND session_id=? AND status='active' AND owner_host_key=? AND owner_pid=? AND owner_process_key=?`,
			status, reason, formatTime(events[0].CreatedAt), attemptID, sessionID,
			r.owner.HostKey, r.owner.PID, r.owner.ProcessKey)
		if err != nil {
			return err
		}
		if changed, _ := result.RowsAffected(); changed != 1 {
			return errors.New("compaction: attempt changed during failure")
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("compaction: fail attempt: %w", err)
	}
	return nil
}

// RepairActive interrupts attempts whose durable owner is known dead. Attempts
// owned by this process, another live process, or an uninspectable foreign host
// remain active.
func (r *Repository) RepairActive(ctx context.Context, sessionID string) error {
	const reason = "process restarted"
	_, err := r.events.AppendBuilt(ctx, sessionID, func(ctx context.Context, tx *sql.Tx, _ int64) ([]event.NewEvent, event.Projector, error) {
		type ownedAttempt struct {
			id    string
			owner processidentity.Identity
		}
		rows, err := tx.QueryContext(ctx, `SELECT id,owner_host_key,owner_pid,owner_process_key
			FROM compaction_attempt WHERE session_id=? AND status='active' ORDER BY created_at,id`, sessionID)
		if err != nil {
			return nil, nil, err
		}
		defer rows.Close()
		var abandoned []ownedAttempt
		for rows.Next() {
			var item ownedAttempt
			if err := rows.Scan(&item.id, &item.owner.HostKey, &item.owner.PID, &item.owner.ProcessKey); err != nil {
				return nil, nil, err
			}
			if r.inspect(r.owner, item.owner) == processidentity.LivenessDead {
				abandoned = append(abandoned, item)
			}
		}
		if err := rows.Err(); err != nil || len(abandoned) == 0 {
			return nil, nil, err
		}
		pending := make([]event.NewEvent, len(abandoned))
		for i, item := range abandoned {
			payload, _ := json.Marshal(v1.CompactionEvent{AttemptID: item.id, Status: "interrupted", Error: reason})
			pending[i] = event.NewEvent{Type: v1.EventSessionCompactionFinished, Data: payload}
		}
		project := func(ctx context.Context, tx *sql.Tx, events []event.Event) error {
			for i, item := range abandoned {
				result, err := tx.ExecContext(ctx, `UPDATE compaction_attempt SET status='interrupted',error_text=?,finished_at=?
					WHERE id=? AND session_id=? AND status='active' AND owner_host_key=? AND owner_pid=? AND owner_process_key=?`,
					reason, formatTime(events[i].CreatedAt), item.id, sessionID,
					item.owner.HostKey, item.owner.PID, item.owner.ProcessKey)
				if err != nil {
					return err
				}
				if changed, _ := result.RowsAffected(); changed != 1 {
					return errors.New("compaction: active attempt changed during repair")
				}
			}
			return nil
		}
		return pending, project, nil
	})
	return err
}

func (r *Repository) Records(ctx context.Context, sessionID string) ([]Record, error) {
	db, err := r.sessions.Session(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	rows, err := db.SQL().QueryContext(ctx, `SELECT id,attempt_id,session_id,source_epoch_id,target_epoch_id,
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
