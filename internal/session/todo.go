package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/amirulashraf/parrot-coder/internal/event"
	"github.com/amirulashraf/parrot-coder/internal/id"
	"github.com/amirulashraf/parrot-coder/internal/store"
)

type TodoStatus string

const (
	TodoPending    TodoStatus = "pending"
	TodoInProgress TodoStatus = "in_progress"
	TodoCompleted  TodoStatus = "completed"
	TodoCancelled  TodoStatus = "cancelled"
)

type TodoPriority string

const (
	TodoHigh   TodoPriority = "high"
	TodoMedium TodoPriority = "medium"
	TodoLow    TodoPriority = "low"
)

type Todo struct {
	ID       string       `json:"id"`
	Content  string       `json:"content"`
	Status   TodoStatus   `json:"status"`
	Priority TodoPriority `json:"priority"`
	Position int          `json:"position"`
}

const EventTodoUpdated = "todo.updated"

type TodoService struct {
	db     *store.DB
	events *event.Repository
}

func NewTodoService(db *store.DB, repositories ...*event.Repository) *TodoService {
	service := &TodoService{db: db}
	if len(repositories) > 0 {
		service.events = repositories[0]
	}
	return service
}

func (s *TodoService) List(ctx context.Context, sessionID string) ([]Todo, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("session: todo service is not configured")
	}
	rows, err := s.db.SQL().QueryContext(ctx, `
		SELECT id, content, status, priority, position
		FROM session_todo WHERE session_id = ? ORDER BY position`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("session: list todos: %w", err)
	}
	defer rows.Close()
	todos := make([]Todo, 0)
	for rows.Next() {
		var item Todo
		if err := rows.Scan(&item.ID, &item.Content, &item.Status, &item.Priority, &item.Position); err != nil {
			return nil, fmt.Errorf("session: scan todo: %w", err)
		}
		todos = append(todos, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("session: list todos: %w", err)
	}
	return todos, nil
}

// Replace atomically replaces the complete ordered todo list. Empty IDs are
// generated before the transaction; supplied IDs remain stable across updates.
func (s *TodoService) Replace(ctx context.Context, sessionID string, todos []Todo) ([]Todo, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("session: todo service is not configured")
	}
	items := append([]Todo(nil), todos...)
	seen := make(map[string]struct{}, len(items))
	for i := range items {
		items[i].Content = strings.TrimSpace(items[i].Content)
		if items[i].Content == "" || !validTodoStatus(items[i].Status) || !validTodoPriority(items[i].Priority) {
			return nil, fmt.Errorf("session: invalid todo at position %d", i)
		}
		if items[i].ID == "" {
			generated, err := id.New("todo")
			if err != nil {
				return nil, fmt.Errorf("session: generate todo ID: %w", err)
			}
			items[i].ID = generated
		}
		if _, duplicate := seen[items[i].ID]; duplicate {
			return nil, fmt.Errorf("session: duplicate todo ID %q", items[i].ID)
		}
		seen[items[i].ID] = struct{}{}
		items[i].Position = i
	}
	project := func(ctx context.Context, tx *sql.Tx, _ []event.Event) error {
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM session WHERE id = ?`, sessionID).Scan(&exists); err != nil {
			return err
		}
		if exists != 1 {
			return ErrNotFound
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM session_todo WHERE session_id = ?`, sessionID); err != nil {
			return fmt.Errorf("delete old todos: %w", err)
		}
		for _, item := range items {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO session_todo(session_id, id, position, content, status, priority)
				VALUES (?, ?, ?, ?, ?, ?)`, sessionID, item.ID, item.Position, item.Content, item.Status, item.Priority); err != nil {
				return fmt.Errorf("insert todo: %w", err)
			}
		}
		return nil
	}
	var err error
	if s.events != nil {
		data, marshalErr := json.Marshal(struct {
			Todos []Todo `json:"todos"`
		}{Todos: items})
		if marshalErr != nil {
			return nil, fmt.Errorf("session: marshal todo update: %w", marshalErr)
		}
		_, err = s.events.Append(ctx, sessionID, []event.NewEvent{{Type: EventTodoUpdated, Data: data}}, project)
	} else {
		err = s.db.WithImmediate(ctx, func(tx *sql.Tx) error { return project(ctx, tx, nil) })
	}
	if err != nil {
		return nil, fmt.Errorf("session: replace todos: %w", err)
	}
	return items, nil
}

func validTodoStatus(status TodoStatus) bool {
	return status == TodoPending || status == TodoInProgress || status == TodoCompleted || status == TodoCancelled
}

func validTodoPriority(priority TodoPriority) bool {
	return priority == TodoHigh || priority == TodoMedium || priority == TodoLow
}
