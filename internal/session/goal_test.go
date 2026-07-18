package session_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	v1 "github.com/amirulashraf/parrot-coder/internal/api/v1"
	"github.com/amirulashraf/parrot-coder/internal/event"
	"github.com/amirulashraf/parrot-coder/internal/protocol"
	"github.com/amirulashraf/parrot-coder/internal/session"
	"github.com/amirulashraf/parrot-coder/internal/store"
)

func TestGoalLifecycleAccountingElapsedTimeAndEvents(t *testing.T) {
	ctx := context.Background()
	registry := store.NewRegistry(t.TempDir(), "host-test")
	t.Cleanup(func() { _ = registry.Close() })
	repository := event.NewRepository(registry)
	sessions := session.NewService(registry, repository)
	created, err := sessions.Create(ctx, session.CreateParams{Title: "goal"})
	if err != nil {
		t.Fatal(err)
	}
	goals := session.NewGoalService(registry, repository)
	budget := int64(10)
	goal, err := goals.Create(ctx, created.ID, "  ship goal support  ", &budget)
	if err != nil {
		t.Fatal(err)
	}
	if goal.Objective != "ship goal support" || goal.Status != session.GoalActive || goal.RemainingTokens() == nil || *goal.RemainingTokens() != 10 {
		t.Fatalf("created goal = %#v", goal)
	}
	if _, err := goals.Create(ctx, created.ID, "replace unfinished", nil); !errors.Is(err, session.ErrGoalExists) {
		t.Fatalf("unfinished replacement error = %v", err)
	}

	db, err := registry.Session(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().Add(-5 * time.Second).Format(time.RFC3339Nano)
	if _, err := db.SQL().ExecContext(ctx, `UPDATE session_goal SET updated_at=? WHERE session_id=?`, old, created.ID); err != nil {
		t.Fatal(err)
	}
	loaded, err := goals.Get(ctx, created.ID)
	if err != nil || loaded.ElapsedSeconds < 4 {
		t.Fatalf("active elapsed goal = %#v, %v", loaded, err)
	}
	paused := session.GoalPaused
	pausedGoal, err := goals.Update(ctx, created.ID, session.GoalMutation{Status: &paused})
	if err != nil || pausedGoal.ElapsedSeconds < 4 {
		t.Fatalf("paused goal = %#v, %v", pausedGoal, err)
	}
	if _, err := db.SQL().ExecContext(ctx, `UPDATE session_goal SET updated_at=? WHERE session_id=?`, old, created.ID); err != nil {
		t.Fatal(err)
	}
	loaded, err = goals.Get(ctx, created.ID)
	if err != nil || loaded.ElapsedSeconds != pausedGoal.ElapsedSeconds {
		t.Fatalf("paused elapsed changed: got %#v, want %d, %v", loaded, pausedGoal.ElapsedSeconds, err)
	}
	active := session.GoalActive
	if _, err := goals.Update(ctx, created.ID, session.GoalMutation{Status: &active}); err != nil {
		t.Fatal(err)
	}
	accounted, err := goals.AccountUsage(ctx, created.ID, protocol.Usage{InputTokens: 12, CachedInputTokens: 5, OutputTokens: 3})
	if err != nil {
		t.Fatal(err)
	}
	if accounted.TokensUsed != 10 || accounted.Status != session.GoalBudgetLimited || accounted.RemainingTokens() == nil || *accounted.RemainingTokens() != 0 {
		t.Fatalf("accounted goal = %#v", accounted)
	}
	if _, err := goals.UpdateAgentStatus(ctx, created.ID, session.GoalActive); err == nil {
		t.Fatal("agent activated a goal")
	}
	complete := session.GoalComplete
	if _, err := goals.Update(ctx, created.ID, session.GoalMutation{Status: &complete}); err != nil {
		t.Fatal(err)
	}
	replacement, err := goals.Create(ctx, created.ID, "replacement", nil)
	if err != nil {
		t.Fatal(err)
	}
	if replacement.ID == goal.ID || replacement.Status != session.GoalActive {
		t.Fatalf("replacement = %#v", replacement)
	}
	if err := goals.Clear(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := goals.Get(ctx, created.ID); !errors.Is(err, session.ErrGoalNotFound) {
		t.Fatalf("Get after clear error = %v", err)
	}

	events, err := repository.List(ctx, created.ID, -1, 100)
	if err != nil {
		t.Fatal(err)
	}
	var updated, cleared bool
	for _, item := range events {
		if item.Type != session.EventGoalUpdated && item.Type != session.EventGoalCleared {
			continue
		}
		var payload v1.Goal
		if err := json.Unmarshal(item.Data, &payload); err != nil || payload.ID == "" || payload.SessionID != created.ID {
			t.Fatalf("goal event %q payload = %#v, %v", item.Type, payload, err)
		}
		updated = updated || item.Type == session.EventGoalUpdated
		cleared = cleared || item.Type == session.EventGoalCleared
	}
	if !updated || !cleared {
		t.Fatalf("goal events missing: updated=%t cleared=%t", updated, cleared)
	}
}

func TestGoalValidationAndMissingSession(t *testing.T) {
	ctx := context.Background()
	registry := store.NewRegistry(t.TempDir(), "host-test")
	t.Cleanup(func() { _ = registry.Close() })
	goals := session.NewGoalService(registry)
	invalidBudget := int64(0)
	if _, err := goals.Create(ctx, "missing", "goal", nil); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("missing session error = %v", err)
	}
	if _, err := goals.Create(ctx, "missing", " ", &invalidBudget); err == nil {
		t.Fatal("invalid goal accepted")
	}
}

func TestMarkUsageLimitedOnlyTransitionsActiveGoal(t *testing.T) {
	ctx := context.Background()
	registry := store.NewRegistry(t.TempDir(), "host-test")
	t.Cleanup(func() { _ = registry.Close() })
	repository := event.NewRepository(registry)
	sessions := session.NewService(registry, repository)
	created, err := sessions.Create(ctx, session.CreateParams{Title: "usage limit"})
	if err != nil {
		t.Fatal(err)
	}
	goals := session.NewGoalService(registry, repository)
	if _, err := goals.Create(ctx, created.ID, "finish", nil); err != nil {
		t.Fatal(err)
	}
	goal, changed, err := goals.MarkUsageLimited(ctx, created.ID)
	if err != nil || !changed || goal.Status != session.GoalUsageLimited {
		t.Fatalf("active transition = %#v, %t, %v", goal, changed, err)
	}
	paused := session.GoalPaused
	if _, err := goals.Update(ctx, created.ID, session.GoalMutation{Status: &paused}); err != nil {
		t.Fatal(err)
	}
	goal, changed, err = goals.MarkUsageLimited(ctx, created.ID)
	if err != nil || changed || goal.Status != session.GoalPaused {
		t.Fatalf("paused transition = %#v, %t, %v", goal, changed, err)
	}
}
