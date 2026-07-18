package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/amirulashraf/parrot-coder/internal/event"
	"github.com/amirulashraf/parrot-coder/internal/id"
	"github.com/amirulashraf/parrot-coder/internal/protocol"
	"github.com/amirulashraf/parrot-coder/internal/store"
)

type GoalStatus string

const (
	GoalActive        GoalStatus = "active"
	GoalPaused        GoalStatus = "paused"
	GoalBlocked       GoalStatus = "blocked"
	GoalUsageLimited  GoalStatus = "usage_limited"
	GoalBudgetLimited GoalStatus = "budget_limited"
	GoalComplete      GoalStatus = "complete"
)

var (
	ErrGoalExists   = errors.New("session: an unfinished goal already exists")
	ErrGoalNotFound = errors.New("session: goal not found")
)

const (
	EventGoalUpdated = "goal.updated"
	EventGoalCleared = "goal.cleared"
)

type Goal struct {
	ID             string     `json:"id"`
	SessionID      string     `json:"session_id"`
	Objective      string     `json:"objective"`
	Status         GoalStatus `json:"status"`
	TokenBudget    *int64     `json:"token_budget,omitempty"`
	TokensUsed     int64      `json:"tokens_used"`
	ElapsedSeconds int64      `json:"elapsed_seconds"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

func (g Goal) RemainingTokens() *int64 {
	if g.TokenBudget == nil {
		return nil
	}
	remaining := max(*g.TokenBudget-g.TokensUsed, 0)
	return &remaining
}

type GoalMutation struct {
	Objective        *string
	Status           *GoalStatus
	TokenBudget      *int64
	ClearTokenBudget bool
}

type GoalService struct {
	sessions *store.Registry
	events   *event.Repository
}

func NewGoalService(sessions *store.Registry, repositories ...*event.Repository) *GoalService {
	service := &GoalService{sessions: sessions}
	if len(repositories) > 0 {
		service.events = repositories[0]
	}
	return service
}

func (s *GoalService) Get(ctx context.Context, sessionID string) (Goal, error) {
	db, err := s.database(ctx, sessionID)
	if err != nil {
		return Goal{}, err
	}
	goal, err := scanGoal(db.SQL().QueryRowContext(ctx, goalSelect+` WHERE session_id = ?`, sessionID))
	if errors.Is(err, sql.ErrNoRows) {
		return Goal{}, ErrGoalNotFound
	}
	if err != nil {
		return Goal{}, fmt.Errorf("session: get goal: %w", err)
	}
	if goal.Status == GoalActive {
		goal.ElapsedSeconds += max(int64(time.Since(goal.UpdatedAt).Seconds()), 0)
	}
	return goal, nil
}

// Create starts a new active goal. A completed goal may be replaced, but an
// unfinished goal must be updated rather than silently discarded.
func (s *GoalService) Create(ctx context.Context, sessionID, objective string, tokenBudget *int64) (Goal, error) {
	objective = strings.TrimSpace(objective)
	if objective == "" || tokenBudget != nil && *tokenBudget <= 0 {
		return Goal{}, errors.New("session: goal objective and a positive token budget are required")
	}
	goalID, err := id.New("goal")
	if err != nil {
		return Goal{}, fmt.Errorf("session: generate goal ID: %w", err)
	}
	now := time.Now().UTC()
	goal := Goal{ID: goalID, SessionID: sessionID, Objective: objective, Status: GoalActive, TokenBudget: tokenBudget, CreatedAt: now, UpdatedAt: now}
	_, err = s.write(ctx, sessionID, EventGoalUpdated, func(ctx context.Context, tx *sql.Tx) (Goal, error) {
		var status GoalStatus
		err := tx.QueryRowContext(ctx, `SELECT status FROM session_goal WHERE session_id = ?`, sessionID).Scan(&status)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return Goal{}, err
		}
		if err == nil && status != GoalComplete {
			return Goal{}, ErrGoalExists
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM session_goal WHERE session_id = ?`, sessionID); err != nil {
			return Goal{}, err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO session_goal(session_id,id,objective,status,token_budget,tokens_used,elapsed_seconds,created_at,updated_at) VALUES(?,?,?,?,?,0,0,?,?)`, sessionID, goal.ID, goal.Objective, goal.Status, goal.TokenBudget, formatGoalTime(now), formatGoalTime(now))
		return goal, err
	})
	if err != nil {
		return Goal{}, fmt.Errorf("session: create goal: %w", err)
	}
	return goal, nil
}

// Update applies user/system-controlled fields to an existing goal.
func (s *GoalService) Update(ctx context.Context, sessionID string, mutation GoalMutation) (Goal, error) {
	if mutation.Objective == nil && mutation.Status == nil && mutation.TokenBudget == nil && !mutation.ClearTokenBudget {
		return Goal{}, errors.New("session: goal update is empty")
	}
	if mutation.TokenBudget != nil && mutation.ClearTokenBudget {
		return Goal{}, errors.New("session: goal token budget cannot be set and cleared together")
	}
	if mutation.Objective != nil {
		trimmed := strings.TrimSpace(*mutation.Objective)
		if trimmed == "" {
			return Goal{}, errors.New("session: goal objective is required")
		}
		mutation.Objective = &trimmed
	}
	if mutation.Status != nil && !validGoalStatus(*mutation.Status) {
		return Goal{}, errors.New("session: invalid goal status")
	}
	if mutation.TokenBudget != nil && *mutation.TokenBudget <= 0 {
		return Goal{}, errors.New("session: goal token budget must be positive")
	}
	updated, err := s.write(ctx, sessionID, EventGoalUpdated, func(ctx context.Context, tx *sql.Tx) (Goal, error) {
		goal, err := scanGoal(tx.QueryRowContext(ctx, goalSelect+` WHERE session_id = ?`, sessionID))
		if errors.Is(err, sql.ErrNoRows) {
			return Goal{}, ErrGoalNotFound
		}
		if err != nil {
			return Goal{}, err
		}
		wasBudgetLimited := goal.Status == GoalBudgetLimited
		if goal.Status == GoalActive {
			goal.ElapsedSeconds += max(int64(time.Since(goal.UpdatedAt).Seconds()), 0)
		}
		if mutation.Objective != nil {
			goal.Objective = *mutation.Objective
		}
		if mutation.Status != nil {
			goal.Status = *mutation.Status
		}
		if mutation.TokenBudget != nil || mutation.ClearTokenBudget {
			goal.TokenBudget = mutation.TokenBudget
		}
		if goal.Status == GoalActive && goal.TokenBudget != nil && goal.TokensUsed >= *goal.TokenBudget {
			goal.Status = GoalBudgetLimited
		} else if wasBudgetLimited && mutation.Status == nil && (goal.TokenBudget == nil || goal.TokensUsed < *goal.TokenBudget) {
			goal.Status = GoalPaused
		}
		goal.UpdatedAt = time.Now().UTC()
		_, err = tx.ExecContext(ctx, `UPDATE session_goal SET objective=?,status=?,token_budget=?,elapsed_seconds=?,updated_at=? WHERE session_id=?`, goal.Objective, goal.Status, goal.TokenBudget, goal.ElapsedSeconds, formatGoalTime(goal.UpdatedAt), sessionID)
		return goal, err
	})
	if err != nil {
		return Goal{}, fmt.Errorf("session: update goal: %w", err)
	}
	return updated, nil
}

func (s *GoalService) Clear(ctx context.Context, sessionID string) error {
	_, err := s.write(ctx, sessionID, EventGoalCleared, func(ctx context.Context, tx *sql.Tx) (Goal, error) {
		goal, err := scanGoal(tx.QueryRowContext(ctx, goalSelect+` WHERE session_id = ?`, sessionID))
		if errors.Is(err, sql.ErrNoRows) {
			return Goal{}, ErrGoalNotFound
		}
		if err != nil {
			return Goal{}, err
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM session_goal WHERE session_id = ?`, sessionID)
		return goal, err
	})
	if err != nil {
		return fmt.Errorf("session: clear goal: %w", err)
	}
	return nil
}

// UpdateAgentStatus restricts model-driven status changes to the two statuses
// the goal tools permit.
func (s *GoalService) UpdateAgentStatus(ctx context.Context, sessionID string, status GoalStatus) (Goal, error) {
	if status != GoalComplete && status != GoalBlocked {
		return Goal{}, errors.New("session: agent may only complete or block a goal")
	}
	return s.Update(ctx, sessionID, GoalMutation{Status: &status})
}

// MarkUsageLimited atomically transitions an active goal after structured
// provider quota exhaustion. A concurrent user or agent state change wins.
func (s *GoalService) MarkUsageLimited(ctx context.Context, sessionID string) (Goal, bool, error) {
	var goal Goal
	var transitioned bool
	mutate := func(ctx context.Context, tx *sql.Tx) error {
		var err error
		goal, err = scanGoal(tx.QueryRowContext(ctx, goalSelect+` WHERE session_id = ?`, sessionID))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrGoalNotFound
		}
		if err != nil || goal.Status != GoalActive {
			return err
		}
		goal.ElapsedSeconds += max(int64(time.Since(goal.UpdatedAt).Seconds()), 0)
		goal.Status = GoalUsageLimited
		goal.UpdatedAt = time.Now().UTC()
		_, err = tx.ExecContext(ctx, `UPDATE session_goal SET status=?,elapsed_seconds=?,updated_at=? WHERE session_id=? AND status=?`, goal.Status, goal.ElapsedSeconds, formatGoalTime(goal.UpdatedAt), sessionID, GoalActive)
		transitioned = err == nil
		return err
	}
	if s.events == nil {
		db, err := s.database(ctx, sessionID)
		if err != nil {
			return Goal{}, false, err
		}
		err = db.WithImmediate(ctx, func(tx *sql.Tx) error { return mutate(ctx, tx) })
		return goal, transitioned, err
	}
	_, err := s.events.AppendBuilt(ctx, sessionID, func(ctx context.Context, tx *sql.Tx, _ int64) ([]event.NewEvent, event.Projector, error) {
		if err := mutate(ctx, tx); err != nil || !transitioned {
			return nil, nil, err
		}
		data, err := json.Marshal(struct {
			Goal
			RemainingTokens *int64 `json:"remaining_tokens,omitempty"`
		}{Goal: goal, RemainingTokens: goal.RemainingTokens()})
		if err != nil {
			return nil, nil, err
		}
		return []event.NewEvent{{Type: EventGoalUpdated, Data: data}}, nil, nil
	})
	if errors.Is(err, store.ErrNoSession) {
		err = ErrNotFound
	}
	return goal, transitioned, err
}

// AccountUsage charges uncached input plus all output tokens. Crossing a token
// budget atomically moves an active goal to budget_limited.
func (s *GoalService) AccountUsage(ctx context.Context, sessionID string, usage protocol.Usage) (Goal, error) {
	input := max(int64(usage.InputTokens), 0)
	cached := max(int64(usage.CachedInputTokens), 0)
	output := max(int64(usage.OutputTokens), 0)
	delta := min(max(input-cached, 0), maxInt64-output) + output
	updated, err := s.write(ctx, sessionID, EventGoalUpdated, func(ctx context.Context, tx *sql.Tx) (Goal, error) {
		goal, err := scanGoal(tx.QueryRowContext(ctx, goalSelect+` WHERE session_id = ?`, sessionID))
		if errors.Is(err, sql.ErrNoRows) {
			return Goal{}, ErrGoalNotFound
		}
		if err != nil {
			return Goal{}, err
		}
		if goal.Status != GoalActive {
			return goal, nil
		}
		goal.ElapsedSeconds += max(int64(time.Since(goal.UpdatedAt).Seconds()), 0)
		goal.TokensUsed = min(goal.TokensUsed, maxInt64-delta) + delta
		if goal.TokenBudget != nil && goal.TokensUsed >= *goal.TokenBudget {
			goal.Status = GoalBudgetLimited
		}
		goal.UpdatedAt = time.Now().UTC()
		_, err = tx.ExecContext(ctx, `UPDATE session_goal SET tokens_used=?,elapsed_seconds=?,status=?,updated_at=? WHERE session_id=?`, goal.TokensUsed, goal.ElapsedSeconds, goal.Status, formatGoalTime(goal.UpdatedAt), sessionID)
		return goal, err
	})
	if err != nil {
		return Goal{}, err
	}
	return updated, nil
}

func (s *GoalService) Active(ctx context.Context, sessionID string) (Goal, bool, error) {
	goal, err := s.Get(ctx, sessionID)
	if errors.Is(err, ErrGoalNotFound) {
		return Goal{}, false, nil
	}
	return goal, err == nil && goal.Status == GoalActive, err
}

func (s *GoalService) database(ctx context.Context, sessionID string) (*store.DB, error) {
	if s == nil || s.sessions == nil {
		return nil, errors.New("session: goal service is not configured")
	}
	db, err := s.sessions.Session(ctx, sessionID)
	if errors.Is(err, store.ErrNoSession) {
		return nil, ErrNotFound
	}
	return db, err
}

func (s *GoalService) write(ctx context.Context, sessionID, eventType string, mutate func(context.Context, *sql.Tx) (Goal, error)) (Goal, error) {
	var goal Goal
	if s.events != nil {
		_, err := s.events.AppendBuilt(ctx, sessionID, func(ctx context.Context, tx *sql.Tx, _ int64) ([]event.NewEvent, event.Projector, error) {
			var err error
			goal, err = mutate(ctx, tx)
			if err != nil {
				return nil, nil, err
			}
			data, err := json.Marshal(struct {
				Goal
				RemainingTokens *int64 `json:"remaining_tokens,omitempty"`
			}{Goal: goal, RemainingTokens: goal.RemainingTokens()})
			if err != nil {
				return nil, nil, err
			}
			return []event.NewEvent{{Type: eventType, Data: data}}, nil, nil
		})
		if errors.Is(err, store.ErrNoSession) {
			err = ErrNotFound
		}
		return goal, err
	}
	db, err := s.database(ctx, sessionID)
	if err != nil {
		return Goal{}, err
	}
	err = db.WithImmediate(ctx, func(tx *sql.Tx) error {
		var err error
		goal, err = mutate(ctx, tx)
		return err
	})
	return goal, err
}

const goalSelect = `SELECT id,session_id,objective,status,token_budget,tokens_used,elapsed_seconds,created_at,updated_at FROM session_goal`

type goalScanner interface{ Scan(...any) error }

func scanGoal(row goalScanner) (Goal, error) {
	var goal Goal
	var budget sql.NullInt64
	var created, updated string
	if err := row.Scan(&goal.ID, &goal.SessionID, &goal.Objective, &goal.Status, &budget, &goal.TokensUsed, &goal.ElapsedSeconds, &created, &updated); err != nil {
		return Goal{}, err
	}
	if budget.Valid {
		goal.TokenBudget = &budget.Int64
	}
	var err error
	if goal.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
		return Goal{}, err
	}
	if goal.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated); err != nil {
		return Goal{}, err
	}
	return goal, nil
}

func validGoalStatus(status GoalStatus) bool {
	switch status {
	case GoalActive, GoalPaused, GoalBlocked, GoalUsageLimited, GoalBudgetLimited, GoalComplete:
		return true
	default:
		return false
	}
}

func formatGoalTime(value time.Time) string { return value.UTC().Format(time.RFC3339Nano) }

const maxInt64 = int64(^uint64(0) >> 1)
