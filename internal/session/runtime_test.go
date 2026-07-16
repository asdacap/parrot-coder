package session_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
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

func TestContextEventsReportLoadedAgentsFiles(t *testing.T) {
	ctx, _, repository, service, sessionID := newService(t)
	sources := json.RawMessage(`{
		"agents:global":{"available":true,"value":"global","path":"/home/test/.config/parrot/AGENTS.md"},
		"agents:project-0000":{"available":true,"value":"root","path":"/work/AGENTS.md"},
		"runtime:date":{"available":true,"value":"2026-07-16"}
	}`)
	if _, err := service.InitializeContext(ctx, sessionID, "baseline", sources, 0); err != nil {
		t.Fatal(err)
	}
	events, err := repository.List(ctx, sessionID, -1, 100)
	if err != nil {
		t.Fatal(err)
	}
	var initialized struct {
		AgentsFiles []string `json:"agents_files"`
	}
	if err := json.Unmarshal(events[len(events)-1].Data, &initialized); err != nil {
		t.Fatal(err)
	}
	want := []string{"/home/test/.config/parrot/AGENTS.md", "/work/AGENTS.md"}
	if !reflect.DeepEqual(initialized.AgentsFiles, want) {
		t.Fatalf("initialized agents_files = %#v, want %#v", initialized.AgentsFiles, want)
	}

	changed := json.RawMessage(`{
		"agents:global":{"available":true,"value":"global","path":"/home/test/.config/parrot/AGENTS.md"},
		"agents:project-0000":{"available":true,"value":"changed","path":"/work/AGENTS.md"},
		"runtime:date":{"available":true,"value":"2026-07-16"}
	}`)
	if err := service.ReconcileContext(ctx, sessionID, "changed", changed); err != nil {
		t.Fatal(err)
	}
	events, err = repository.List(ctx, sessionID, -1, 100)
	if err != nil {
		t.Fatal(err)
	}
	var reconciled struct {
		AgentsFiles []string `json:"agents_files"`
	}
	if err := json.Unmarshal(events[len(events)-1].Data, &reconciled); err != nil {
		t.Fatal(err)
	}
	if want := []string{"/work/AGENTS.md"}; !reflect.DeepEqual(reconciled.AgentsFiles, want) {
		t.Fatalf("changed agents_files = %#v, want %#v", reconciled.AgentsFiles, want)
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

func TestModelHistoryRepairsInterruptedToolWithoutOutput(t *testing.T) {
	ctx, _, _, service, sessionID := newService(t)
	assistant, err := service.StartAssistant(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	call := protocol.ToolCall{ID: "call", Name: "shell", Input: json.RawMessage(`{}`)}
	if err := service.FinishAssistant(ctx, sessionID, assistant.ID, session.AssistantFinal{Status: "complete", FinishReason: protocol.FinishToolCalls, Parts: []protocol.ContentPart{{Type: protocol.ContentToolCall, ToolCall: &call}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.AddToolCall(ctx, sessionID, assistant.ID, call); err != nil {
		t.Fatal(err)
	}
	if err := service.SettleTool(ctx, sessionID, call.ID, "interrupted", "", "context canceled"); err != nil {
		t.Fatal(err)
	}

	history, err := service.ListModelHistory(ctx, sessionID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 || history[0].Content[0].Type != protocol.ContentToolCall || history[1].Content[0].Type != protocol.ContentToolResult {
		t.Fatalf("repaired history = %#v", history)
	}
	result := history[1].Content[0]
	if result.ToolCallID != call.ID || result.Text != "Error: tool execution interrupted: context canceled" {
		t.Fatalf("repaired tool result = %#v", result)
	}
}

func TestModelHistoryDropsUnregisteredToolCall(t *testing.T) {
	ctx, _, _, service, sessionID := newService(t)
	assistant, err := service.StartAssistant(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	call := protocol.ToolCall{ID: "orphan", Name: "shell", Input: json.RawMessage(`{}`)}
	if err := service.FinishAssistant(ctx, sessionID, assistant.ID, session.AssistantFinal{Status: "interrupted", FinishReason: protocol.FinishError, Parts: []protocol.ContentPart{{Type: protocol.ContentText, Text: "partial"}, {Type: protocol.ContentToolCall, ToolCall: &call}}}); err != nil {
		t.Fatal(err)
	}

	history, err := service.ListModelHistory(ctx, sessionID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 || len(history[0].Content) != 1 || history[0].Content[0].Text != "partial" {
		t.Fatalf("history retained orphaned call: %#v", history)
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
