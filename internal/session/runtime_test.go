package session_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/amirulashraf/parrot-coder/internal/event"
	"github.com/amirulashraf/parrot-coder/internal/protocol"
	"github.com/amirulashraf/parrot-coder/internal/session"
	"github.com/amirulashraf/parrot-coder/internal/store"
)

func TestModelHistoryCutoffIsInclusive(t *testing.T) {
	ctx, _, _, service, sessionID := newService(t)
	for _, text := range []string{"zero", "one", "two"} {
		if _, err := service.AppendMessage(ctx, sessionID, protocol.Message{Role: protocol.RoleUser, Content: []protocol.ContentPart{{Type: protocol.ContentText, Text: text}}}); err != nil {
			t.Fatal(err)
		}
	}
	history, err := service.ListModelHistory(ctx, sessionID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 || history[0].Content[0].Text != "one" || history[1].Content[0].Text != "two" {
		t.Fatalf("history at cutoff 1 = %#v", history)
	}
	history, err = service.ListModelHistory(ctx, sessionID, 3)
	if err != nil || len(history) != 0 {
		t.Fatalf("history after end = %#v, %v", history, err)
	}
}

func TestModelHistoryOmitsContentlessTerminalAssistant(t *testing.T) {
	ctx, _, _, service, sessionID := newService(t)
	assistant, err := service.StartAssistant(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.FinishAssistant(ctx, sessionID, assistant.ID, session.AssistantFinal{Status: "error", FinishReason: protocol.FinishError, Error: "context overflow"}); err != nil {
		t.Fatal(err)
	}
	history, err := service.ListModelHistory(ctx, sessionID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 0 {
		t.Fatalf("history = %#v, want no contentless error turn", history)
	}
}

func TestRepairActiveAfterReopenSettlesDurableState(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "repair.db")
	db, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	repository := event.NewRepository(db)
	service := session.NewService(db, repository)
	created, err := service.Create(ctx, session.CreateParams{Title: "repair"})
	if err != nil {
		t.Fatal(err)
	}
	assistant, err := service.StartAssistant(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	call := protocol.ToolCall{ID: "call", Name: "tool", Input: json.RawMessage(`{}`)}
	if _, err := service.AddToolCall(ctx, created.ID, assistant.ID, call); err != nil {
		t.Fatal(err)
	}
	if err := service.StartTool(ctx, created.ID, call.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	repository = event.NewRepository(db)
	service = session.NewService(db, repository)
	if err := service.RepairActive(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	messages, err := service.ListMessages(ctx, created.ID)
	if err != nil || len(messages) != 1 || messages[0].Status != "interrupted" {
		t.Fatalf("repaired messages = %#v, %v", messages, err)
	}
	var toolStatus, toolError string
	if err := db.SQL().QueryRowContext(ctx, `SELECT status,error_text FROM session_tool_call WHERE id='call'`).Scan(&toolStatus, &toolError); err != nil {
		t.Fatal(err)
	}
	if toolStatus != "interrupted" || toolError != "process restarted" {
		t.Fatalf("tool state = %q, %q", toolStatus, toolError)
	}
	eventsBefore, _ := repository.List(ctx, created.ID, -1, 100)
	for _, item := range eventsBefore {
		if item.Type != "session.tool.running" {
			continue
		}
		var payload map[string]string
		if err := json.Unmarshal(item.Data, &payload); err != nil || payload["tool_name"] != "tool" {
			t.Fatalf("running tool event lost its name: %s, %v", item.Data, err)
		}
	}
	if err := service.RepairActive(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	eventsAfter, _ := repository.List(ctx, created.ID, -1, 100)
	if len(eventsAfter) != len(eventsBefore) {
		t.Fatalf("no-op repair appended an event: %d -> %d", len(eventsBefore), len(eventsAfter))
	}
}
