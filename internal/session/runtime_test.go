package session_test

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/amirulashraf/parrot-coder/internal/event"
	"github.com/amirulashraf/parrot-coder/internal/protocol"
	"github.com/amirulashraf/parrot-coder/internal/session"
	"github.com/amirulashraf/parrot-coder/internal/store"
)

func TestStatusPromptPendingTracksInitialTurnAndModeChanges(t *testing.T) {
	ctx, _, repository, service, sessionID := newService(t)
	if err := service.GetSession(sessionID).SetSelection(ctx, session.Selection{Agent: "build", Provider: "local", Model: "code"}); err != nil {
		t.Fatal(err)
	}
	assertPending := func(want bool) {
		t.Helper()
		got, err := service.GetSession(sessionID).StatusPromptPending(ctx)
		if err != nil || got != want {
			t.Fatalf("StatusPromptPending() = %v, %v; want %v", got, err, want)
		}
	}
	assertPending(true)
	statusMessage, err := service.GetSession(sessionID).AppendStatusPrompt(ctx, "build status")
	if err != nil {
		t.Fatal(err)
	}
	if statusMessage.Role != string(protocol.RoleSystem) || statusMessage.Content != "build status" {
		t.Fatalf("status message = %#v", statusMessage)
	}
	assertPending(false)
	interrupted, err := service.GetSession(sessionID).StartAssistant(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.GetSession(sessionID).FinishAssistant(ctx, interrupted.ID, session.AssistantFinal{Status: "interrupted", Error: "cancelled"}); err != nil {
		t.Fatal(err)
	}
	assertPending(false)
	variant := "high"
	if _, err := service.GetSession(sessionID).UpdateSelection(ctx, session.SelectionPatch{Model: "other", Variant: &variant}, nil); err != nil {
		t.Fatal(err)
	}
	assertPending(false)
	if _, err := service.GetSession(sessionID).UpdateSelection(ctx, session.SelectionPatch{Agent: "plan"}, nil); err != nil {
		t.Fatal(err)
	}
	assertPending(true)
	if _, err := service.GetSession(sessionID).AppendStatusPrompt(ctx, "plan status"); err != nil {
		t.Fatal(err)
	}
	assertPending(false)
	interrupted, err = service.GetSession(sessionID).StartAssistant(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.GetSession(sessionID).FinishAssistant(ctx, interrupted.ID, session.AssistantFinal{Status: "interrupted", Error: "cancelled"}); err != nil {
		t.Fatal(err)
	}
	assertPending(false)

	messages, err := service.GetSession(sessionID).ListModelHistory(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	var statuses []string
	for _, message := range messages {
		if message.Role == protocol.RoleSystem {
			statuses = append(statuses, message.Content[0].Text)
		}
	}
	if want := []string{"build status", "plan status"}; !reflect.DeepEqual(statuses, want) {
		t.Fatalf("status history = %#v, want %#v", statuses, want)
	}

	events, err := repository.List(ctx, sessionID, -1, 100)
	if err != nil {
		t.Fatal(err)
	}
	var changes []bool
	for _, item := range events {
		if item.Type != "session.selection.changed" {
			continue
		}
		var data struct {
			ModeChanged bool `json:"mode_changed"`
		}
		if err := json.Unmarshal(item.Data, &data); err != nil {
			t.Fatal(err)
		}
		changes = append(changes, data.ModeChanged)
	}
	if want := []bool{true, false, true}; !reflect.DeepEqual(changes, want) {
		t.Fatalf("mode_changed values = %v, want %v", changes, want)
	}
}

func TestModelHistoryCutoffIsInclusive(t *testing.T) {
	ctx, _, _, service, sessionID := newService(t)
	for _, text := range []string{"zero", "one", "two"} {
		if _, err := service.GetSession(sessionID).AppendMessage(ctx, protocol.Message{Role: protocol.RoleUser, Content: []protocol.ContentPart{{Type: protocol.ContentText, Text: text}}}); err != nil {
			t.Fatal(err)
		}
	}
	history, err := service.GetSession(sessionID).ListModelHistory(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 || history[0].Content[0].Text != "one" || history[1].Content[0].Text != "two" {
		t.Fatalf("history at cutoff 1 = %#v", history)
	}
	history, err = service.GetSession(sessionID).ListModelHistory(ctx, 3)
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
	if _, err := service.GetSession(sessionID).InitializeContext(ctx, "baseline", sources, 0); err != nil {
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
	if err := service.GetSession(sessionID).ReconcileContext(ctx, "changed", changed); err != nil {
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
	assistant, err := service.GetSession(sessionID).StartAssistant(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.GetSession(sessionID).FinishAssistant(ctx, assistant.ID, session.AssistantFinal{Status: "error", FinishReason: protocol.FinishError, Error: "context overflow"}); err != nil {
		t.Fatal(err)
	}
	history, err := service.GetSession(sessionID).ListModelHistory(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 0 {
		t.Fatalf("history = %#v, want no contentless error turn", history)
	}
}

func TestModelHistoryRepairsInterruptedToolWithoutOutput(t *testing.T) {
	ctx, _, _, service, sessionID := newService(t)
	assistant, err := service.GetSession(sessionID).StartAssistant(ctx)
	if err != nil {
		t.Fatal(err)
	}
	call := protocol.ToolCall{ID: "call", Name: "shell", Input: json.RawMessage(`{}`)}
	if err := service.GetSession(sessionID).FinishAssistant(ctx, assistant.ID, session.AssistantFinal{Status: "complete", FinishReason: protocol.FinishToolCalls, Parts: []protocol.ContentPart{{Type: protocol.ContentToolCall, ToolCall: &call}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.GetSession(sessionID).AddToolCall(ctx, assistant.ID, call); err != nil {
		t.Fatal(err)
	}
	if err := service.GetSession(sessionID).SettleTool(ctx, call.ID, "interrupted", "", "context canceled"); err != nil {
		t.Fatal(err)
	}

	history, err := service.GetSession(sessionID).ListModelHistory(ctx, 0)
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
	assistant, err := service.GetSession(sessionID).StartAssistant(ctx)
	if err != nil {
		t.Fatal(err)
	}
	call := protocol.ToolCall{ID: "orphan", Name: "shell", Input: json.RawMessage(`{}`)}
	if err := service.GetSession(sessionID).FinishAssistant(ctx, assistant.ID, session.AssistantFinal{Status: "interrupted", FinishReason: protocol.FinishError, Parts: []protocol.ContentPart{{Type: protocol.ContentText, Text: "partial"}, {Type: protocol.ContentToolCall, ToolCall: &call}}}); err != nil {
		t.Fatal(err)
	}

	history, err := service.GetSession(sessionID).ListModelHistory(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 || len(history[0].Content) != 1 || history[0].Content[0].Text != "partial" {
		t.Fatalf("history retained orphaned call: %#v", history)
	}
}

func TestRepairActiveAfterReopenSettlesDurableState(t *testing.T) {
	ctx := context.Background()
	state := t.TempDir()
	db := store.NewRegistry(state, "host-test")
	repository := event.NewRepository(db)
	service := session.NewService(db, repository)
	created, err := service.Create(ctx, session.CreateParams{Title: "repair"})
	if err != nil {
		t.Fatal(err)
	}
	assistant, err := service.GetSession(created.ID).StartAssistant(ctx)
	if err != nil {
		t.Fatal(err)
	}
	calls := []protocol.ToolCall{
		{ID: "pending-call", Name: "pending-tool", Input: json.RawMessage(`{}`)},
		{ID: "running-call", Name: "running-tool", Input: json.RawMessage(`{}`)},
	}
	for _, call := range calls {
		if _, err := service.GetSession(created.ID).AddToolCall(ctx, assistant.ID, call); err != nil {
			t.Fatal(err)
		}
	}
	if err := service.GetSession(created.ID).StartTool(ctx, calls[1].ID); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopening the same state directory stands in for a process restart.
	db = store.NewRegistry(state, "host-test")
	t.Cleanup(func() { db.Close() })
	repository = event.NewRepository(db)
	service = session.NewService(db, repository)
	if err := service.GetSession(created.ID).RepairActive(ctx); err != nil {
		t.Fatal(err)
	}
	messages, err := service.GetSession(created.ID).ListMessages(ctx)
	if err != nil || len(messages) != 1 || messages[0].Status != "interrupted" {
		t.Fatalf("repaired messages = %#v, %v", messages, err)
	}
	sessionDB, err := db.Session(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, call := range calls {
		var toolStatus, toolError string
		if err := sessionDB.SQL().QueryRowContext(ctx, `SELECT status,error_text FROM session_tool_call WHERE id=?`, call.ID).Scan(&toolStatus, &toolError); err != nil {
			t.Fatal(err)
		}
		if toolStatus != "interrupted" || toolError != "process restarted" {
			t.Fatalf("tool %s state = %q, %q", call.ID, toolStatus, toolError)
		}
	}
	eventsBefore, _ := repository.List(ctx, created.ID, -1, 100)
	interrupted := make(map[string]map[string]string)
	for _, item := range eventsBefore {
		if item.Type != "session.tool.interrupted" {
			continue
		}
		var payload map[string]string
		if err := json.Unmarshal(item.Data, &payload); err != nil {
			t.Fatalf("decode interrupted tool event: %s, %v", item.Data, err)
		}
		interrupted[payload["call_id"]] = payload
	}
	for _, call := range calls {
		payload := interrupted[call.ID]
		if payload["tool_name"] != call.Name || payload["status"] != "interrupted" || payload["error"] != "process restarted" {
			t.Fatalf("interrupted event for %s = %#v", call.ID, payload)
		}
	}
	if err := service.GetSession(created.ID).RepairActive(ctx); err != nil {
		t.Fatal(err)
	}
	eventsAfter, _ := repository.List(ctx, created.ID, -1, 100)
	if len(eventsAfter) != len(eventsBefore) {
		t.Fatalf("no-op repair appended an event: %d -> %d", len(eventsBefore), len(eventsAfter))
	}
}
