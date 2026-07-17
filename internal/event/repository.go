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
	sessions *store.Registry

	mu          sync.Mutex
	nextSubID   uint64
	subscribers map[string]map[uint64]*subscriber
	commits     map[string]*sync.Mutex
}

type subscriber struct {
	events chan Event
	after  int64
}

func NewRepository(sessions *store.Registry) *Repository {
	return &Repository{
		sessions:    sessions,
		subscribers: make(map[string]map[uint64]*subscriber),
		commits:     make(map[string]*sync.Mutex),
	}
}

// commitLock serializes commit and publication for one session. Each session
// owns its database, so sessions no longer contend with each other and a
// subagent's child session commits without waiting behind its parent.
func (r *Repository) commitLock(sessionID string) *sync.Mutex {
	r.mu.Lock()
	defer r.mu.Unlock()
	lock, ok := r.commits[sessionID]
	if !ok {
		lock = &sync.Mutex{}
		r.commits[sessionID] = lock
	}
	return lock
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
	db, err := r.sessions.Session(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	// Keep commit and publication in sequence order. SQLite serializes the
	// commits, but without this lock their goroutines could publish in reverse.
	lock := r.commitLock(sessionID)
	lock.Lock()
	defer lock.Unlock()

	var committed []Event
	err = db.WithImmediate(ctx, func(tx *sql.Tx) error {
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
	// Publication takes the subscriber lock that ReplayAndSubscribe holds across
	// its replay query, which is what keeps the durable/live handoff atomic. The
	// session's commit lock is still held, so publication order still matches
	// commit order for this session.
	r.mu.Lock()
	r.publishLocked(sessionID, committed)
	r.mu.Unlock()
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
	db, err := r.sessions.Session(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	rows, err := db.SQL().QueryContext(ctx, `
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
	return r.subscribeLocked(sessionID, -1, capacity)
}

// ReplayAndSubscribe establishes an atomic durable/live handoff. Publication
// uses the same lock, so every commit after the replay query is either present
// in replay or delivered through the subscription. Sequence filtering removes
// the commit-before-publication duplicate case.
func (r *Repository) ReplayAndSubscribe(ctx context.Context, sessionID string, after int64, capacity int) ([]Event, *Subscription, error) {
	// Resolve the database before taking the subscriber lock. AppendBuilt
	// resolves first and publishes second, so acquiring these two locks in the
	// opposite order here could deadlock against it.
	db, err := r.sessions.Session(ctx, sessionID)
	if err != nil {
		return nil, nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	items, err := r.listAll(ctx, db, sessionID, after)
	if err != nil {
		return nil, nil, err
	}
	cutoff := after
	if len(items) > 0 {
		cutoff = items[len(items)-1].Sequence
	}
	return items, r.subscribeLockedHeld(sessionID, cutoff, capacity), nil
}

func (r *Repository) listAll(ctx context.Context, db *store.DB, sessionID string, after int64) ([]Event, error) {
	rows, err := db.SQL().QueryContext(ctx, `
        SELECT id, session_id, sequence, type, data_json, created_at
        FROM event WHERE session_id = ? AND sequence > ? ORDER BY sequence`, sessionID, after)
	if err != nil {
		return nil, fmt.Errorf("event: replay: %w", err)
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
		return nil, fmt.Errorf("event: replay: %w", err)
	}
	return result, nil
}

func (r *Repository) subscribeLocked(sessionID string, after int64, capacity int) *Subscription {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.subscribeLockedHeld(sessionID, after, capacity)
}

func (r *Repository) subscribeLockedHeld(sessionID string, after int64, capacity int) *Subscription {
	if capacity < 1 {
		capacity = 1
	}
	ch := make(chan Event, capacity)
	id := r.nextSubID
	r.nextSubID++
	if r.subscribers[sessionID] == nil {
		r.subscribers[sessionID] = make(map[uint64]*subscriber)
	}
	r.subscribers[sessionID][id] = &subscriber{events: ch, after: after}

	return &Subscription{Events: ch, close: func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if subscriptions := r.subscribers[sessionID]; subscriptions != nil {
			if current, ok := subscriptions[id]; ok {
				delete(subscriptions, id)
				close(current.events)
			}
			if len(subscriptions) == 0 {
				delete(r.subscribers, sessionID)
			}
		}
	}}
}

func (r *Repository) publishLocked(sessionID string, events []Event) {
	for id, subscriber := range r.subscribers[sessionID] {
		keep := true
		for _, item := range events {
			if item.Sequence <= subscriber.after {
				continue
			}
			select {
			case subscriber.events <- item:
				subscriber.after = item.Sequence
			default:
				close(subscriber.events)
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
