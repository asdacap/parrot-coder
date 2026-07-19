package tool

import (
	"context"
	"testing"

	"github.com/amirulashraf/parrot-coder/internal/event"
	"github.com/amirulashraf/parrot-coder/internal/session"
	"github.com/amirulashraf/parrot-coder/internal/store"
)

func TestGetGoalWithoutGoal(t *testing.T) {
	ctx := context.Background()
	registry := store.NewRegistry(t.TempDir(), "host-test")
	t.Cleanup(func() { _ = registry.Close() })
	repository := event.NewRepository(registry)
	sessions := session.NewService(registry, repository)
	created, err := sessions.Create(ctx, session.CreateParams{Title: "no goal"})
	if err != nil {
		t.Fatal(err)
	}

	result, err := NewGetGoalTool(session.NewGoalService(registry, repository)).Execute(ctx, Plan{}, CallContext{SessionID: created.ID})
	if err != nil || result.Text != "no goal set" {
		t.Fatalf("result = %#v, error = %v", result, err)
	}
}
