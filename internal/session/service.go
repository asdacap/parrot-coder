package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/amirulashraf/parrot-coder/internal/event"
	"github.com/amirulashraf/parrot-coder/internal/id"
	"github.com/amirulashraf/parrot-coder/internal/store"
)

var (
	ErrNotFound            = errors.New("session: not found")
	ErrInvalidDelivery     = errors.New("session: delivery must be steer or queue")
	ErrIdempotencyConflict = errors.New("session: message ID was already admitted with different content or delivery")
	errAlreadyAdmitted     = errors.New("session: input already admitted")
)

type Delivery string

const (
	DeliverySteer Delivery = "steer"
	DeliveryQueue Delivery = "queue"
)

type Session struct {
	ID        string
	ProjectID string
	Title     string
	CreatedAt time.Time
	UpdatedAt time.Time
}

type CreateParams struct {
	ProjectID string
	Title     string
}

type Input struct {
	ID               string
	SessionID        string
	MessageID        string
	Content          string
	Delivery         Delivery
	Status           string
	AdmittedSequence int64
	PromotedSequence *int64
	CreatedAt        time.Time
	PromotedAt       *time.Time
}

type AdmitParams struct {
	MessageID string
	Content   string
	Delivery  Delivery
}

type Admission struct {
	Input   Input
	Created bool
}

type Message struct {
	ID        string
	SessionID string
	Role      string
	Content   string
	InputID   string
	Sequence  int64
	CreatedAt time.Time
}

type Service struct {
	db     *store.DB
	events *event.Repository
}

func NewService(db *store.DB, events *event.Repository) *Service {
	return &Service{db: db, events: events}
}

func (s *Service) Create(ctx context.Context, params CreateParams) (Session, error) {
	sessionID, err := id.New("ses")
	if err != nil {
		return Session{}, fmt.Errorf("session: generate ID: %w", err)
	}
	now := time.Now().UTC()
	var projectID any
	if params.ProjectID != "" {
		projectID = params.ProjectID
	}
	_, err = s.db.SQL().ExecContext(ctx, `
        INSERT INTO session(id, project_id, title, created_at, updated_at)
        VALUES (?, ?, ?, ?, ?)`,
		sessionID, projectID, params.Title, formatTime(now), formatTime(now))
	if err != nil {
		return Session{}, fmt.Errorf("session: create: %w", err)
	}
	return Session{ID: sessionID, ProjectID: params.ProjectID, Title: params.Title, CreatedAt: now, UpdatedAt: now}, nil
}

func (s *Service) Get(ctx context.Context, sessionID string) (Session, error) {
	return scanSession(s.db.SQL().QueryRowContext(ctx, `
        SELECT id, COALESCE(project_id, ''), title, created_at, updated_at
        FROM session WHERE id = ?`, sessionID))
}

func (s *Service) List(ctx context.Context) ([]Session, error) {
	rows, err := s.db.SQL().QueryContext(ctx, `
        SELECT id, COALESCE(project_id, ''), title, created_at, updated_at
        FROM session ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, fmt.Errorf("session: list: %w", err)
	}
	defer rows.Close()
	var result []Session
	for rows.Next() {
		item, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("session: list: %w", err)
	}
	return result, nil
}

func (s *Service) Delete(ctx context.Context, sessionID string) error {
	result, err := s.db.SQL().ExecContext(ctx, `DELETE FROM session WHERE id = ?`, sessionID)
	if err != nil {
		return fmt.Errorf("session: delete: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("session: delete result: %w", err)
	}
	if count == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Service) Admit(ctx context.Context, sessionID string, params AdmitParams) (Admission, error) {
	if params.MessageID == "" {
		return Admission{}, errors.New("session: message ID is required")
	}
	if params.Delivery != DeliverySteer && params.Delivery != DeliveryQueue {
		return Admission{}, ErrInvalidDelivery
	}
	inputID, err := id.New("inp")
	if err != nil {
		return Admission{}, fmt.Errorf("session: generate input ID: %w", err)
	}
	payload, err := json.Marshal(struct {
		InputID   string   `json:"input_id"`
		MessageID string   `json:"message_id"`
		Content   string   `json:"content"`
		Delivery  Delivery `json:"delivery"`
	}{inputID, params.MessageID, params.Content, params.Delivery})
	if err != nil {
		return Admission{}, fmt.Errorf("session: encode admission event: %w", err)
	}

	var admitted Input
	appended, err := s.events.Append(ctx, sessionID,
		[]event.NewEvent{{Type: "session.input.admitted", Data: payload}},
		func(ctx context.Context, tx *sql.Tx, events []event.Event) error {
			existing, err := getInput(ctx, tx, sessionID, params.MessageID)
			if err == nil {
				if existing.Content != params.Content || existing.Delivery != params.Delivery {
					return ErrIdempotencyConflict
				}
				return errAlreadyAdmitted
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			now := events[0].CreatedAt
			_, err = tx.ExecContext(ctx, `
                INSERT INTO session_input(
                    id, session_id, message_id, content, delivery, status,
                    admitted_sequence, created_at
                ) VALUES (?, ?, ?, ?, ?, 'pending', ?, ?)`,
				inputID, sessionID, params.MessageID, params.Content, params.Delivery,
				events[0].Sequence, formatTime(now))
			if err != nil {
				return fmt.Errorf("insert input: %w", err)
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE session SET updated_at = ? WHERE id = ?`, formatTime(now), sessionID); err != nil {
				return fmt.Errorf("touch session: %w", err)
			}
			admitted = Input{
				ID: inputID, SessionID: sessionID, MessageID: params.MessageID,
				Content: params.Content, Delivery: params.Delivery, Status: "pending",
				AdmittedSequence: events[0].Sequence, CreatedAt: now,
			}
			return nil
		})
	if err == nil {
		if len(appended) != 1 {
			return Admission{}, errors.New("session: admission did not append one event")
		}
		return Admission{Input: admitted, Created: true}, nil
	}
	if errors.Is(err, errAlreadyAdmitted) {
		existing, loadErr := s.inputByMessageID(ctx, sessionID, params.MessageID)
		if loadErr != nil {
			return Admission{}, loadErr
		}
		return Admission{Input: existing, Created: false}, nil
	}
	return Admission{}, err
}

// PromoteSteers promotes all pending steer inputs admitted through cutoff.
func (s *Service) PromoteSteers(ctx context.Context, sessionID string, cutoff int64) ([]Message, error) {
	return s.promote(ctx, sessionID, DeliverySteer, cutoff)
}

// PromoteNextQueue promotes at most one pending queue input.
func (s *Service) PromoteNextQueue(ctx context.Context, sessionID string) ([]Message, error) {
	return s.promote(ctx, sessionID, DeliveryQueue, -1)
}

func (s *Service) promote(ctx context.Context, sessionID string, delivery Delivery, cutoff int64) ([]Message, error) {
	var promoted []Input
	var messages []Message
	_, err := s.events.AppendBuilt(ctx, sessionID, func(ctx context.Context, tx *sql.Tx, next int64) ([]event.NewEvent, event.Projector, error) {
		query := `
            SELECT id, session_id, message_id, content, delivery, status,
                   admitted_sequence, promoted_sequence, created_at, promoted_at
            FROM session_input
            WHERE session_id = ? AND delivery = ? AND status = 'pending'`
		args := []any{sessionID, delivery}
		if delivery == DeliverySteer {
			query += ` AND admitted_sequence <= ?`
			args = append(args, cutoff)
		}
		query += ` ORDER BY admitted_sequence`
		if delivery == DeliveryQueue {
			query += ` LIMIT 1`
		}
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, nil, fmt.Errorf("session: select inputs for promotion: %w", err)
		}
		for rows.Next() {
			item, err := scanInput(rows)
			if err != nil {
				rows.Close()
				return nil, nil, err
			}
			promoted = append(promoted, item)
		}
		if err := rows.Close(); err != nil {
			return nil, nil, fmt.Errorf("session: close promotion inputs: %w", err)
		}
		if len(promoted) == 0 {
			return nil, nil, nil
		}

		pending := make([]event.NewEvent, len(promoted))
		for i, input := range promoted {
			data, err := json.Marshal(struct {
				InputID   string `json:"input_id"`
				MessageID string `json:"message_id"`
			}{input.ID, input.MessageID})
			if err != nil {
				return nil, nil, fmt.Errorf("session: encode promotion event: %w", err)
			}
			pending[i] = event.NewEvent{Type: "session.input.promoted", Data: data}
		}

		project := func(ctx context.Context, tx *sql.Tx, events []event.Event) error {
			messages = make([]Message, len(promoted))
			for i, input := range promoted {
				eventItem := events[i]
				result, err := tx.ExecContext(ctx, `
                    UPDATE session_input
                    SET status = 'promoted', promoted_sequence = ?, promoted_at = ?
                    WHERE id = ? AND status = 'pending'`,
					eventItem.Sequence, formatTime(eventItem.CreatedAt), input.ID)
				if err != nil {
					return fmt.Errorf("promote input: %w", err)
				}
				changed, err := result.RowsAffected()
				if err != nil || changed != 1 {
					return errors.New("session: input changed during promotion")
				}
				_, err = tx.ExecContext(ctx, `
                    INSERT INTO session_message(id, session_id, role, content, input_id, sequence, created_at)
                    VALUES (?, ?, 'user', ?, ?, ?, ?)`,
					input.MessageID, sessionID, input.Content, input.ID,
					eventItem.Sequence, formatTime(eventItem.CreatedAt))
				if err != nil {
					return fmt.Errorf("project user message: %w", err)
				}
				messages[i] = Message{
					ID: input.MessageID, SessionID: sessionID, Role: "user", Content: input.Content,
					InputID: input.ID, Sequence: eventItem.Sequence, CreatedAt: eventItem.CreatedAt,
				}
			}
			_, err := tx.ExecContext(ctx, `UPDATE session SET updated_at = ? WHERE id = ?`,
				formatTime(events[len(events)-1].CreatedAt), sessionID)
			return err
		}
		return pending, project, nil
	})
	if err != nil {
		return nil, err
	}
	return messages, nil
}

func (s *Service) ListMessages(ctx context.Context, sessionID string) ([]Message, error) {
	rows, err := s.db.SQL().QueryContext(ctx, `
        SELECT id, session_id, role, content, COALESCE(input_id, ''), sequence, created_at
        FROM session_message WHERE session_id = ? ORDER BY sequence`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("session: list messages: %w", err)
	}
	defer rows.Close()
	var result []Message
	for rows.Next() {
		var item Message
		var createdAt string
		if err := rows.Scan(&item.ID, &item.SessionID, &item.Role, &item.Content,
			&item.InputID, &item.Sequence, &createdAt); err != nil {
			return nil, fmt.Errorf("session: scan message: %w", err)
		}
		item.CreatedAt, err = parseTime(createdAt)
		if err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("session: list messages: %w", err)
	}
	return result, nil
}

func (s *Service) inputByMessageID(ctx context.Context, sessionID, messageID string) (Input, error) {
	return getInput(ctx, s.db.SQL(), sessionID, messageID)
}

type queryRower interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func getInput(ctx context.Context, db queryRower, sessionID, messageID string) (Input, error) {
	return scanInput(db.QueryRowContext(ctx, `
        SELECT id, session_id, message_id, content, delivery, status,
               admitted_sequence, promoted_sequence, created_at, promoted_at
        FROM session_input WHERE session_id = ? AND message_id = ?`, sessionID, messageID))
}

type rowScanner interface {
	Scan(...any) error
}

func scanSession(row rowScanner) (Session, error) {
	var item Session
	var createdAt, updatedAt string
	if err := row.Scan(&item.ID, &item.ProjectID, &item.Title, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Session{}, ErrNotFound
		}
		return Session{}, fmt.Errorf("session: scan: %w", err)
	}
	var err error
	item.CreatedAt, err = parseTime(createdAt)
	if err != nil {
		return Session{}, err
	}
	item.UpdatedAt, err = parseTime(updatedAt)
	if err != nil {
		return Session{}, err
	}
	return item, nil
}

func scanInput(row rowScanner) (Input, error) {
	var item Input
	var delivery string
	var promotedSequence sql.NullInt64
	var createdAt string
	var promotedAt sql.NullString
	if err := row.Scan(&item.ID, &item.SessionID, &item.MessageID, &item.Content, &delivery,
		&item.Status, &item.AdmittedSequence, &promotedSequence, &createdAt, &promotedAt); err != nil {
		return Input{}, err
	}
	item.Delivery = Delivery(delivery)
	var err error
	item.CreatedAt, err = parseTime(createdAt)
	if err != nil {
		return Input{}, err
	}
	if promotedSequence.Valid {
		item.PromotedSequence = &promotedSequence.Int64
	}
	if promotedAt.Valid {
		parsed, err := parseTime(promotedAt.String)
		if err != nil {
			return Input{}, err
		}
		item.PromotedAt = &parsed
	}
	return item, nil
}

func formatTime(value time.Time) string { return value.UTC().Format(time.RFC3339Nano) }

func parseTime(value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("session: parse time: %w", err)
	}
	return parsed, nil
}
