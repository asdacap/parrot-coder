package session_test

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"testing"

	v1 "github.com/amirulashraf/parrot-coder/internal/api/v1"
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

func TestAppendMessageIfNoPendingInputs(t *testing.T) {
	ctx, _, repository, service, sessionID := newService(t)
	store := service.GetSession(sessionID)
	message := protocol.Message{Role: protocol.RoleUser, Content: []protocol.ContentPart{{Type: protocol.ContentText, Text: "conditional"}}}

	appendedMessage, appended, err := store.AppendMessageIfNoPendingInputs(ctx, "notification-message", message)
	if err != nil || !appended {
		t.Fatalf("initial append = %#v, %v, %v; want appended", appendedMessage, appended, err)
	}
	if appendedMessage.Role != string(protocol.RoleUser) || appendedMessage.Content != "conditional" {
		t.Fatalf("appended message = %#v", appendedMessage)
	}
	duplicate, appended, err := store.AppendMessageIfNoPendingInputs(ctx, "notification-message", message)
	if err != nil || !appended || duplicate.ID != appendedMessage.ID || duplicate.Sequence != appendedMessage.Sequence {
		t.Fatalf("idempotent append = %#v, %v, %v", duplicate, appended, err)
	}

	assistant, err := store.StartAssistant(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishAssistant(ctx, assistant.ID, session.AssistantFinal{Status: "complete", Parts: []protocol.ContentPart{{Type: protocol.ContentText, Text: "answered"}}}); err != nil {
		t.Fatal(err)
	}
	if duplicate, process, err := store.AppendMessageIfNoPendingInputs(ctx, "notification-message", message); err != nil || process || duplicate.ID != appendedMessage.ID {
		t.Fatalf("answered idempotent append = %#v, %v, %v", duplicate, process, err)
	}

	if _, err := store.Admit(ctx, session.AdmitParams{MessageID: "pending-message", Content: "pending", Delivery: session.DeliveryQueue}); err != nil {
		t.Fatal(err)
	}
	eventsBefore, err := repository.List(ctx, sessionID, -1, 100)
	if err != nil {
		t.Fatal(err)
	}
	skippedMessage, appended, err := store.AppendMessageIfNoPendingInputs(ctx, "skipped-message", message)
	if err != nil || appended || !reflect.DeepEqual(skippedMessage, session.Message{}) {
		t.Fatalf("append with pending input = %#v, %v, %v; want zero, false, nil", skippedMessage, appended, err)
	}
	eventsAfter, err := repository.List(ctx, sessionID, -1, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(eventsAfter) != len(eventsBefore) {
		t.Fatalf("skipped append added events: %d -> %d", len(eventsBefore), len(eventsAfter))
	}

	messages, err := store.ListMessages(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || messages[0].ID != appendedMessage.ID {
		t.Fatalf("messages after skipped append = %#v", messages)
	}
	if _, err := store.PromoteNextQueue(ctx); err != nil {
		t.Fatal(err)
	}
	if _, appended, err := store.AppendMessageIfNoPendingInputs(ctx, "promoted-message", message); err != nil || !appended {
		t.Fatalf("append after promotion = %v, %v; want true, nil", appended, err)
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

func TestToolLifecycleEventsUseCanonicalPayload(t *testing.T) {
	ctx, _, repository, service, sessionID := newService(t)
	store := service.GetSession(sessionID)
	assistant, err := store.StartAssistant(ctx)
	if err != nil {
		t.Fatal(err)
	}
	calls := []protocol.ToolCall{
		{ID: "success-call", Name: "shell", Input: json.RawMessage(`{"command":"pwd","limit":9007199254740993}`)},
		{ID: "failure-call", Name: "read", Input: json.RawMessage(`{"path":"missing"}`)},
		{ID: "interrupted-call", Name: "agent", Input: json.RawMessage(`{"prompt":"work"}`)},
	}
	for _, call := range calls {
		if _, err := store.AddToolCall(ctx, assistant.ID, call); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.StartTool(ctx, calls[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := store.SettleToolWithOutput(ctx, calls[0].ID, "success", "done", "", "last line"); err != nil {
		t.Fatal(err)
	}
	if err := store.SettleTool(ctx, calls[1].ID, "failure", "partial", "not found"); err != nil {
		t.Fatal(err)
	}
	if err := store.SettleTool(ctx, calls[2].ID, "interrupted", "", "context canceled"); err != nil {
		t.Fatal(err)
	}

	expected := map[string]v1.ToolEvent{
		"session.tool.pending/success-call":     {CallID: "success-call", ToolName: "shell", Input: map[string]any{"command": "pwd", "limit": json.Number("9007199254740993")}, Status: "pending"},
		"session.tool.running/success-call":     {CallID: "success-call", ToolName: "shell", Status: "running", Result: ""},
		"session.tool.success/success-call":     {CallID: "success-call", ToolName: "shell", Status: "success", Result: "done", OutputTail: "last line"},
		"session.tool.pending/failure-call":     {CallID: "failure-call", ToolName: "read", Input: map[string]any{"path": "missing"}, Status: "pending"},
		"session.tool.failure/failure-call":     {CallID: "failure-call", ToolName: "read", Status: "failure", Result: "partial", Error: "not found"},
		"session.tool.pending/interrupted-call": {CallID: "interrupted-call", ToolName: "agent", Input: map[string]any{"prompt": "work"}, Status: "pending"},
		"session.tool.interrupted/interrupted-call": {
			CallID: "interrupted-call", ToolName: "agent", Status: "interrupted", Result: "", Error: "context canceled",
		},
	}
	events, err := repository.List(ctx, sessionID, -1, 100)
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]v1.ToolEvent)
	for _, item := range events {
		switch item.Type {
		case "session.tool.pending", "session.tool.running", "session.tool.success", "session.tool.failure", "session.tool.interrupted":
		default:
			continue
		}
		var payload v1.ToolEvent
		decoder := json.NewDecoder(bytes.NewReader(item.Data))
		decoder.UseNumber()
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&payload); err != nil {
			t.Fatalf("decode %s payload %s: %v", item.Type, item.Data, err)
		}
		seen[item.Type+"/"+payload.CallID] = payload
	}
	if !reflect.DeepEqual(seen, expected) {
		t.Fatalf("tool events = %#v, want %#v", seen, expected)
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
	interrupted := make(map[string]v1.ToolEvent)
	for _, item := range eventsBefore {
		if item.Type != "session.tool.interrupted" {
			continue
		}
		var payload v1.ToolEvent
		decoder := json.NewDecoder(bytes.NewReader(item.Data))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&payload); err != nil {
			t.Fatalf("decode interrupted tool event: %s, %v", item.Data, err)
		}
		interrupted[payload.CallID] = payload
	}
	for _, call := range calls {
		want := v1.ToolEvent{CallID: call.ID, ToolName: call.Name, Status: "interrupted", Error: "process restarted"}
		if payload := interrupted[call.ID]; !reflect.DeepEqual(payload, want) {
			t.Fatalf("interrupted event for %s = %#v, want %#v", call.ID, payload, want)
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
