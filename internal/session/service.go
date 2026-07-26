package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	v1 "github.com/amirulashraf/parrot-coder/internal/api/v1"
	"github.com/amirulashraf/parrot-coder/internal/event"
	"github.com/amirulashraf/parrot-coder/internal/id"
	"github.com/amirulashraf/parrot-coder/internal/store"
	petname "github.com/dustinkirkland/golang-petname"
)

var (
	ErrNotFound              = errors.New("session: not found")
	ErrInvalidDelivery       = errors.New("session: delivery must be steer or queue")
	ErrIdempotencyConflict   = errors.New("session: message ID was already admitted with different content or delivery")
	ErrSelectionRequired     = errors.New("session: agent, provider, and model are required")
	ErrParentProjectMismatch = errors.New("session: parent belongs to a different project")
	errAlreadyAdmitted       = errors.New("session: input already admitted")
)

type Delivery string

const (
	DeliverySteer Delivery = "steer"
	DeliveryQueue Delivery = "queue"
)

type AgentSessionDto struct {
	ID              string
	ParentSessionID string
	Name            string
	ProjectID       string
	ProjectRoot     string
	Title           string
	Agent           string
	Provider        string
	Model           string
	Variant         string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

type CreateParams struct {
	ParentSessionID string
	Name            string
	ProjectID       string
	ProjectRoot     string
	Title           string
}

type InteractiveOwner struct {
	WorkingDirectory string
	HostKey          string
	PID              int
}

type ClaimDisposition string

const (
	ClaimExisting  ClaimDisposition = "existing"
	ClaimReclaimed ClaimDisposition = "reclaimed"
	ClaimCreated   ClaimDisposition = "created"
)

type InteractiveClaim struct {
	Session     AgentSessionDto
	Disposition ClaimDisposition
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
	ID           string
	SessionID    string
	Role         string
	Content      string
	Parts        json.RawMessage
	Status       string
	FinishReason string
	Error        string
	Usage        json.RawMessage
	InputID      string
	Sequence     int64
	CreatedAt    time.Time
}

type Service struct {
	sessions *store.Registry
	events   *event.Repository
	pid      int
}

type agentSessionStore struct {
	*Service
	sessionID string
}

func (s *Service) GetSession(sessionID string) AgentSessionStore {
	return &agentSessionStore{Service: s, sessionID: sessionID}
}

func NewService(sessions *store.Registry, events *event.Repository) *Service {
	return &Service{sessions: sessions, events: events, pid: os.Getpid()}
}

func (s *Service) Create(ctx context.Context, params CreateParams) (AgentSessionDto, error) {
	return s.create(ctx, params, Selection{})
}

// CreateSelected persists a new session and its initial execution selection in
// one SQLite statement, so concurrent readers cannot observe a half-configured
// session.
func (s *Service) CreateSelected(ctx context.Context, params CreateParams, selection Selection) (AgentSessionDto, error) {
	if selection.Agent == "" || selection.Provider == "" || selection.Model == "" {
		return AgentSessionDto{}, ErrSelectionRequired
	}
	return s.create(ctx, params, selection)
}

func (s *Service) create(ctx context.Context, params CreateParams, selection Selection) (AgentSessionDto, error) {
	if err := s.validateParent(params); err != nil {
		return AgentSessionDto{}, err
	}
	if params.ParentSessionID == "" && strings.TrimSpace(params.Name) == "" {
		params.Name = petname.Generate(3, "-")
	}
	sessionID, err := id.New("ses")
	if err != nil {
		return AgentSessionDto{}, fmt.Errorf("session: generate ID: %w", err)
	}
	db, err := s.sessions.Create(ctx, sessionID)
	if err != nil {
		return AgentSessionDto{}, err
	}
	epochID, err := id.New("ctx")
	if err != nil {
		_ = s.sessions.Remove(sessionID)
		return AgentSessionDto{}, fmt.Errorf("session: generate compaction epoch ID: %w", err)
	}
	now := time.Now().UTC()
	result := AgentSessionDto{
		ID: sessionID, ParentSessionID: params.ParentSessionID, Name: params.Name, ProjectID: params.ProjectID, ProjectRoot: params.ProjectRoot, Title: params.Title,
		Agent: selection.Agent, Provider: selection.Provider, Model: selection.Model, Variant: selection.Variant,
		CreatedAt: now, UpdatedAt: now,
	}
	err = db.WithImmediate(ctx, func(tx *sql.Tx) error {
		if err := insertSession(ctx, tx, result); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO session_compaction_epoch(id,session_id,ordinal,summary_prompt,history_cutoff,created_at) VALUES(?,?,0,'',0,?)`, epochID, sessionID, formatTime(now))
		return err
	})
	if err != nil {
		// A session whose row was never written would be listed from its
		// directory but fail to open, so remove it rather than leave a shell.
		_ = s.sessions.Remove(sessionID)
		return AgentSessionDto{}, err
	}
	if err := s.publish(result); err != nil {
		_ = s.sessions.Remove(sessionID)
		return AgentSessionDto{}, err
	}
	return result, nil
}

func (s *Service) validateParent(params CreateParams) error {
	if params.ParentSessionID == "" {
		return nil
	}
	// Parent and child have separate databases, so this relationship cannot use
	// a cross-database foreign key or be validated in the child's transaction.
	// The published metadata is the durable cross-session index and can also be
	// read when another host owns the parent database.
	meta, err := store.ReadMeta(s.sessions.State(), params.ParentSessionID)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("session: parent: %w", ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("session: read parent: %w", err)
	}
	if meta.ProjectID != params.ProjectID {
		return ErrParentProjectMismatch
	}
	return nil
}

func insertSession(ctx context.Context, tx *sql.Tx, item AgentSessionDto) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO session(id, parent_session_id, name, project_id, project_root, title, selected_agent, selected_provider, selected_model, selected_variant, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		item.ID, item.ParentSessionID, item.Name, item.ProjectID, item.ProjectRoot, item.Title,
		item.Agent, item.Provider, item.Model, item.Variant,
		formatTime(item.CreatedAt), formatTime(item.UpdatedAt))
	if err != nil {
		return fmt.Errorf("session: create: %w", err)
	}
	return nil
}

// publish republishes a session's index entry. The database stays the source of
// truth; the entry exists so another host can list this session without opening
// a database it cannot lock.
func (s *Service) publish(item AgentSessionDto) error {
	return store.WriteMeta(s.sessions.State(), store.Meta{
		ID:              item.ID,
		ParentSessionID: item.ParentSessionID,
		Name:            item.Name,
		ProjectID:       item.ProjectID,
		ProjectRoot:     item.ProjectRoot,
		Title:           item.Title,
		Agent:           item.Agent,
		Provider:        item.Provider,
		Model:           item.Model,
		Variant:         item.Variant,
		CreatedAt:       formatTime(item.CreatedAt),
		UpdatedAt:       formatTime(item.UpdatedAt),
		HostKey:         s.sessions.HostKey(),
		PID:             s.pid,
	})
}

// republish refreshes the index entry from the session database. Callers use it
// after a commit that changed indexed fields.
func (s *agentSessionStore) republish(ctx context.Context) error {
	item, err := s.Get(ctx)
	if err != nil {
		return err
	}
	return s.publish(item)
}

// ClaimInteractive selects the session associated with a working directory on
// this machine. A live owner is never displaced; an abandoned owner is replaced.
//
// Owner records are kept per host because a working directory is a host-local
// name: the same path on two machines is two different directories on two disks.
// A record written by another host therefore describes something this host
// cannot see, and is neither read nor written here. The previous shared table
// looked owners up by directory alone and reclaimed any record whose host did
// not match, which silently took over a session another machine was still
// running.
func (s *Service) ClaimInteractive(ctx context.Context, owner InteractiveOwner, params CreateParams, selection Selection, forceNew bool, alive func(int) bool) (InteractiveClaim, error) {
	if owner.WorkingDirectory == "" || owner.HostKey == "" || owner.PID <= 0 {
		return InteractiveClaim{}, errors.New("session: working directory, host key, and PID are required")
	}
	if selection.Agent == "" || selection.Provider == "" || selection.Model == "" {
		return InteractiveClaim{}, ErrSelectionRequired
	}
	// A conflict means another process on this host published a claim between
	// the read and the write, so the decision is remade against its record.
	for attempt := 0; attempt < ownerClaimAttempts; attempt++ {
		claim, err := s.claimInteractiveOnce(ctx, owner, params, selection, forceNew, alive)
		if !errors.Is(err, store.ErrOwnerConflict) {
			return claim, err
		}
	}
	return InteractiveClaim{}, errors.New("session: interactive owner kept changing")
}

const ownerClaimAttempts = 5

func (s *Service) claimInteractiveOnce(ctx context.Context, owner InteractiveOwner, params CreateParams, selection Selection, forceNew bool, alive func(int) bool) (InteractiveClaim, error) {
	chain, _, err := store.LoadOwnerChain(s.sessions.State(), owner.HostKey, owner.WorkingDirectory)
	if err != nil {
		return InteractiveClaim{}, err
	}
	current, bound := chain.Current()

	if bound && !forceNew {
		item, err := s.GetSession(current.SessionID).Get(ctx)
		switch {
		case err == nil && current.PID == owner.PID:
			return InteractiveClaim{Session: item, Disposition: ClaimExisting}, nil
		case err == nil && !(alive != nil && alive(current.PID)):
			// The owning process is gone, so its binding is abandoned.
			next := current
			next.PID, next.ClaimedAt = owner.PID, ""
			if err := chain.Claim(next); err != nil {
				return InteractiveClaim{}, err
			}
			if err := store.StampOwner(s.sessions.State(), item.ID, owner.HostKey, owner.PID); err != nil {
				return InteractiveClaim{}, err
			}
			return InteractiveClaim{Session: item, Disposition: ClaimReclaimed}, nil
		case err != nil && !errors.Is(err, ErrNotFound) && !errors.Is(err, store.ErrNoSession):
			return InteractiveClaim{}, err
		}
		// A live owner, or a record pointing at a deleted session: fall through
		// and create a new session rather than displace or resurrect it.
	}

	item, err := s.create(ctx, params, selection)
	if err != nil {
		return InteractiveClaim{}, err
	}
	if err := chain.Claim(store.Owner{
		SessionID:        item.ID,
		WorkingDirectory: owner.WorkingDirectory,
		HostKey:          owner.HostKey,
		PID:              owner.PID,
	}); err != nil {
		_ = s.sessions.Remove(item.ID)
		return InteractiveClaim{}, err
	}
	return InteractiveClaim{Session: item, Disposition: ClaimCreated}, nil
}

func (s *agentSessionStore) Get(ctx context.Context) (AgentSessionDto, error) {
	db, err := s.sessions.Session(ctx, s.sessionID)
	if errors.Is(err, store.ErrNoSession) {
		return AgentSessionDto{}, ErrNotFound
	}
	if err != nil {
		return AgentSessionDto{}, err
	}
	return scanSession(db.SQL().QueryRowContext(ctx, `
		SELECT id, parent_session_id, name, project_id, project_root, title, selected_agent, selected_provider, selected_model, selected_variant, created_at, updated_at
        FROM session WHERE id = ?`, s.sessionID))
}

// List reports every session on every machine sharing this state directory, by
// reading published index entries rather than opening each database. Entries are
// small and few enough that a directory scan is cheaper than the shared table it
// replaces, which had to be written on every message to stay ordered.
func (s *Service) List(ctx context.Context) ([]AgentSessionDto, error) {
	metas, _, err := store.ListMeta(s.sessions.State())
	if err != nil {
		return nil, err
	}
	result := make([]AgentSessionDto, 0, len(metas))
	for _, meta := range metas {
		item, err := sessionFromMeta(meta)
		if err != nil {
			continue
		}
		result = append(result, item)
	}
	return result, nil
}

// LatestSelection returns the most recently used complete selection for a
// project. It is used as an interactive startup preference, not as a config
// override.
func (s *Service) LatestSelection(ctx context.Context, projectID string) (Selection, error) {
	metas, _, err := store.ListMeta(s.sessions.State())
	if err != nil {
		return Selection{}, err
	}
	var best store.Meta
	for _, meta := range metas {
		if meta.ProjectID != projectID || meta.Agent == "" || meta.Provider == "" || meta.Model == "" {
			continue
		}
		if best.ID == "" || meta.UpdatedAt > best.UpdatedAt || (meta.UpdatedAt == best.UpdatedAt && meta.ID > best.ID) {
			best = meta
		}
	}
	if best.ID == "" {
		return Selection{}, ErrNotFound
	}
	return Selection{Agent: best.Agent, Provider: best.Provider, Model: best.Model, Variant: best.Variant}, nil
}

// Delete removes a session directory. The old shared table relied on cascading
// deletes that its own RESTRICT constraints could block; a session now owns its
// file, so deleting it is removing that file.
func (s *agentSessionStore) Delete(ctx context.Context) error {
	err := s.sessions.Remove(s.sessionID)
	if errors.Is(err, store.ErrNoSession) {
		return ErrNotFound
	}
	return err
}

func (s *agentSessionStore) Admit(ctx context.Context, params AdmitParams) (Admission, error) {
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
	payload, err := json.Marshal(v1.SessionInputAdmitted{
		InputID:   inputID,
		MessageID: params.MessageID,
		Content:   params.Content,
		Delivery:  string(params.Delivery),
	})
	if err != nil {
		return Admission{}, fmt.Errorf("session: encode admission event: %w", err)
	}

	var admitted Input
	appended, err := s.events.Append(ctx, s.sessionID,
		[]event.NewEvent{{Type: v1.EventSessionInputAdmitted, Data: payload}},
		func(ctx context.Context, tx *sql.Tx, events []event.Event) error {
			existing, err := getInput(ctx, tx, s.sessionID, params.MessageID)
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
				inputID, s.sessionID, params.MessageID, params.Content, params.Delivery,
				events[0].Sequence, formatTime(now))
			if err != nil {
				return fmt.Errorf("insert input: %w", err)
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE session SET updated_at = ? WHERE id = ?`, formatTime(now), s.sessionID); err != nil {
				return fmt.Errorf("touch session: %w", err)
			}
			admitted = Input{
				ID: inputID, SessionID: s.sessionID, MessageID: params.MessageID,
				Content: params.Content, Delivery: params.Delivery, Status: "pending",
				AdmittedSequence: events[0].Sequence, CreatedAt: now,
			}
			return nil
		})
	if err == nil {
		if len(appended) != 1 {
			return Admission{}, errors.New("session: admission did not append one event")
		}
		if err := s.republish(ctx); err != nil {
			return Admission{}, err
		}
		return Admission{Input: admitted, Created: true}, nil
	}
	if errors.Is(err, errAlreadyAdmitted) {
		existing, loadErr := s.inputByMessageID(ctx, params.MessageID)
		if loadErr != nil {
			return Admission{}, loadErr
		}
		return Admission{Input: existing, Created: false}, nil
	}
	return Admission{}, err
}

// PromoteSteers promotes all pending steer inputs admitted through cutoff.
func (s *agentSessionStore) PromoteSteers(ctx context.Context, cutoff int64) ([]Message, error) {
	return s.promote(ctx, DeliverySteer, cutoff)
}

// PromoteNextQueue promotes at most one pending queue input.
func (s *agentSessionStore) PromoteNextQueue(ctx context.Context) ([]Message, error) {
	return s.promote(ctx, DeliveryQueue, -1)
}

// HasPendingInputs reports whether the session has any inputs that have been
// admitted but not yet promoted. The interrupt path uses this to decide
// whether to automatically resume the drain so queued steers are processed
// without the user re-prompting.
func (s *agentSessionStore) HasPendingInputs(ctx context.Context) (bool, error) {
	db, err := s.sessions.Session(ctx, s.sessionID)
	if err != nil {
		return false, err
	}
	var count int
	if err := db.SQL().QueryRowContext(ctx, `
        SELECT COUNT(*) FROM session_input WHERE session_id = ? AND status = 'pending'`, s.sessionID).Scan(&count); err != nil {
		return false, fmt.Errorf("session: count pending inputs: %w", err)
	}
	return count > 0, nil
}

func (s *agentSessionStore) promote(ctx context.Context, delivery Delivery, cutoff int64) ([]Message, error) {
	var promoted []Input
	var messages []Message
	_, err := s.events.AppendBuilt(ctx, s.sessionID, func(ctx context.Context, tx *sql.Tx, next int64) ([]event.NewEvent, event.Projector, error) {
		query := `
            SELECT id, session_id, message_id, content, delivery, status,
                   admitted_sequence, promoted_sequence, created_at, promoted_at
            FROM session_input
            WHERE session_id = ? AND delivery = ? AND status = 'pending'`
		args := []any{s.sessionID, delivery}
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
			data, err := json.Marshal(v1.SessionInputPromoted{
				InputID:   input.ID,
				MessageID: input.MessageID,
			})
			if err != nil {
				return nil, nil, fmt.Errorf("session: encode promotion event: %w", err)
			}
			pending[i] = event.NewEvent{Type: v1.EventSessionInputPromoted, Data: data}
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
					input.MessageID, s.sessionID, input.Content, input.ID,
					eventItem.Sequence, formatTime(eventItem.CreatedAt))
				if err != nil {
					return fmt.Errorf("project user message: %w", err)
				}
				messages[i] = Message{
					ID: input.MessageID, SessionID: s.sessionID, Role: "user", Content: input.Content,
					InputID: input.ID, Sequence: eventItem.Sequence, CreatedAt: eventItem.CreatedAt,
				}
			}
			_, err := tx.ExecContext(ctx, `UPDATE session SET updated_at = ? WHERE id = ?`,
				formatTime(events[len(events)-1].CreatedAt), s.sessionID)
			return err
		}
		return pending, project, nil
	})
	if err != nil {
		return nil, err
	}
	if len(messages) > 0 {
		if err := s.republish(ctx); err != nil {
			return nil, err
		}
	}
	return messages, nil
}

func (s *agentSessionStore) ListMessages(ctx context.Context) ([]Message, error) {
	db, err := s.sessions.Session(ctx, s.sessionID)
	if err != nil {
		return nil, err
	}
	rows, err := db.SQL().QueryContext(ctx, `
		SELECT id, session_id, role, content, parts_json, status, finish_reason, error_text,
		       usage_json, COALESCE(input_id, ''), sequence, created_at
        FROM session_message WHERE session_id = ? ORDER BY sequence`, s.sessionID)
	if err != nil {
		return nil, fmt.Errorf("session: list messages: %w", err)
	}
	defer rows.Close()
	var result []Message
	for rows.Next() {
		var item Message
		var createdAt string
		var parts, usage []byte
		if err := rows.Scan(&item.ID, &item.SessionID, &item.Role, &item.Content,
			&parts, &item.Status, &item.FinishReason, &item.Error, &usage,
			&item.InputID, &item.Sequence, &createdAt); err != nil {
			return nil, fmt.Errorf("session: scan message: %w", err)
		}
		item.Parts = append(json.RawMessage(nil), parts...)
		item.Usage = append(json.RawMessage(nil), usage...)
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

func (s *agentSessionStore) inputByMessageID(ctx context.Context, messageID string) (Input, error) {
	db, err := s.sessions.Session(ctx, s.sessionID)
	if err != nil {
		return Input{}, err
	}
	return getInput(ctx, db.SQL(), s.sessionID, messageID)
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

// sessionFromMeta builds a session from its published index entry.
func sessionFromMeta(meta store.Meta) (AgentSessionDto, error) {
	createdAt, err := parseTime(meta.CreatedAt)
	if err != nil {
		return AgentSessionDto{}, err
	}
	updatedAt, err := parseTime(meta.UpdatedAt)
	if err != nil {
		return AgentSessionDto{}, err
	}
	return AgentSessionDto{
		ID: meta.ID, ParentSessionID: meta.ParentSessionID, Name: meta.Name, ProjectID: meta.ProjectID, ProjectRoot: meta.ProjectRoot, Title: meta.Title,
		Agent: meta.Agent, Provider: meta.Provider, Model: meta.Model, Variant: meta.Variant,
		CreatedAt: createdAt, UpdatedAt: updatedAt,
	}, nil
}

func scanSession(row rowScanner) (AgentSessionDto, error) {
	var item AgentSessionDto
	var createdAt, updatedAt string
	if err := row.Scan(&item.ID, &item.ParentSessionID, &item.Name, &item.ProjectID, &item.ProjectRoot, &item.Title, &item.Agent, &item.Provider, &item.Model, &item.Variant, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return AgentSessionDto{}, ErrNotFound
		}
		return AgentSessionDto{}, fmt.Errorf("session: scan: %w", err)
	}
	var err error
	item.CreatedAt, err = parseTime(createdAt)
	if err != nil {
		return AgentSessionDto{}, err
	}
	item.UpdatedAt, err = parseTime(updatedAt)
	if err != nil {
		return AgentSessionDto{}, err
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
