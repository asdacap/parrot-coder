package event

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/amirulashraf/parrot-coder/internal/id"
	"github.com/amirulashraf/parrot-coder/internal/store"
)

var ErrInvalidEvent = errors.New("event: type and valid JSON data are required")

type Event struct {
	ID        string          `json:"id"`
	SessionID string          `json:"session_id"`
	Sequence  int64           `json:"sequence"`
	Type      string          `json:"type"`
	Data      json.RawMessage `json:"data"`
	CreatedAt time.Time       `json:"created_at"`
}

type NewEvent struct {
	Type string
	Data json.RawMessage
}

// Projector updates query tables in the same transaction as appended events.
type Projector func(context.Context, *sql.Tx, []Event) error

// Builder runs after the immediate transaction has started. nextSequence is the
// sequence that will be assigned to its first returned event.
type Builder func(context.Context, *sql.Tx, int64) ([]NewEvent, Projector, error)

type Repository struct {
	db *store.DB

	mu          sync.Mutex
	nextSubID   uint64
	subscribers map[string]map[uint64]chan Event
}

func NewRepository(db *store.DB) *Repository {
	return &Repository{db: db, subscribers: make(map[string]map[uint64]chan Event)}
}

func (r *Repository) Append(ctx context.Context, sessionID string, pending []NewEvent, projector Projector) ([]Event, error) {
	return r.AppendBuilt(ctx, sessionID, func(context.Context, *sql.Tx, int64) ([]NewEvent, Projector, error) {
		return pending, projector, nil
	})
}

// AppendBuilt appends a contiguous aggregate batch and commits its projection
// before publishing anything to process-local subscribers.
func (r *Repository) AppendBuilt(ctx context.Context, sessionID string, build Builder) ([]Event, error) {
	if sessionID == "" || build == nil {
		return nil, errors.New("event: session ID and builder are required")
	}

	var committed []Event
	err := r.db.WithImmediate(ctx, func(tx *sql.Tx) error {
		next, err := nextSequence(ctx, tx, sessionID)
		if err != nil {
			return err
		}
		pending, projector, err := build(ctx, tx, next)
		if err != nil {
			return err
		}

		now := time.Now().UTC()
		committed = make([]Event, len(pending))
		for i, candidate := range pending {
			if candidate.Type == "" || len(candidate.Data) == 0 || !json.Valid(candidate.Data) {
				return ErrInvalidEvent
			}
			eventID, err := id.New("evt")
			if err != nil {
				return fmt.Errorf("event: generate ID: %w", err)
			}
			item := Event{
				ID:        eventID,
				SessionID: sessionID,
				Sequence:  next + int64(i),
				Type:      candidate.Type,
				Data:      append(json.RawMessage(nil), candidate.Data...),
				CreatedAt: now,
			}
			if _, err := tx.ExecContext(ctx, `
                INSERT INTO event(id, session_id, sequence, type, data_json, created_at)
                VALUES (?, ?, ?, ?, ?, ?)`,
				item.ID, item.SessionID, item.Sequence, item.Type, []byte(item.Data), formatTime(item.CreatedAt)); err != nil {
				return fmt.Errorf("event: insert sequence %d: %w", item.Sequence, err)
			}
			committed[i] = item
		}
		if len(committed) > 0 {
			if _, err := tx.ExecContext(ctx,
				`UPDATE event_sequence SET next_sequence = ? WHERE session_id = ?`,
				next+int64(len(committed)), sessionID); err != nil {
				return fmt.Errorf("event: advance sequence: %w", err)
			}
		}
		if projector != nil {
			if err := projector(ctx, tx, committed); err != nil {
				return fmt.Errorf("event: project: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	r.publish(sessionID, committed)
	return committed, nil
}

func nextSequence(ctx context.Context, tx *sql.Tx, sessionID string) (int64, error) {
	var next int64
	err := tx.QueryRowContext(ctx,
		`SELECT next_sequence FROM event_sequence WHERE session_id = ?`, sessionID).Scan(&next)
	if err == nil {
		return next, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("event: read sequence: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO event_sequence(session_id, next_sequence) VALUES (?, 0)`, sessionID); err != nil {
		return 0, fmt.Errorf("event: initialize sequence: %w", err)
	}
	return 0, nil
}

func (r *Repository) List(ctx context.Context, sessionID string, after int64, limit int) ([]Event, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := r.db.SQL().QueryContext(ctx, `
        SELECT id, session_id, sequence, type, data_json, created_at
        FROM event
        WHERE session_id = ? AND sequence > ?
        ORDER BY sequence
        LIMIT ?`, sessionID, after, limit)
	if err != nil {
		return nil, fmt.Errorf("event: list: %w", err)
	}
	defer rows.Close()

	var result []Event
	for rows.Next() {
		item, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("event: list: %w", err)
	}
	return result, nil
}

type rowScanner interface {
	Scan(...any) error
}

func scanEvent(row rowScanner) (Event, error) {
	var item Event
	var data []byte
	var createdAt string
	if err := row.Scan(&item.ID, &item.SessionID, &item.Sequence, &item.Type, &data, &createdAt); err != nil {
		return Event{}, fmt.Errorf("event: scan: %w", err)
	}
	parsed, err := time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return Event{}, fmt.Errorf("event: parse creation time: %w", err)
	}
	item.Data = append(json.RawMessage(nil), data...)
	item.CreatedAt = parsed
	return item, nil
}

type Subscription struct {
	Events <-chan Event
	close  func()
	once   sync.Once
}

func (s *Subscription) Close() {
	if s != nil {
		s.once.Do(s.close)
	}
}

// Subscribe registers a bounded process-local queue. A subscriber that cannot
// keep up is closed instead of delaying event commits or other subscribers.
func (r *Repository) Subscribe(sessionID string, capacity int) *Subscription {
	if capacity < 1 {
		capacity = 1
	}
	ch := make(chan Event, capacity)
	r.mu.Lock()
	id := r.nextSubID
	r.nextSubID++
	if r.subscribers[sessionID] == nil {
		r.subscribers[sessionID] = make(map[uint64]chan Event)
	}
	r.subscribers[sessionID][id] = ch
	r.mu.Unlock()

	return &Subscription{Events: ch, close: func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if subscriptions := r.subscribers[sessionID]; subscriptions != nil {
			if current, ok := subscriptions[id]; ok {
				delete(subscriptions, id)
				close(current)
			}
			if len(subscriptions) == 0 {
				delete(r.subscribers, sessionID)
			}
		}
	}}
}

func (r *Repository) publish(sessionID string, events []Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, subscriber := range r.subscribers[sessionID] {
		keep := true
		for _, item := range events {
			select {
			case subscriber <- item:
			default:
				close(subscriber)
				delete(r.subscribers[sessionID], id)
				keep = false
			}
			if !keep {
				break
			}
		}
	}
	if len(r.subscribers[sessionID]) == 0 {
		delete(r.subscribers, sessionID)
	}
}

func formatTime(value time.Time) string { return value.UTC().Format(time.RFC3339Nano) }
