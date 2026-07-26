package session

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	v1 "github.com/amirulashraf/parrot-coder/internal/api/v1"
	"github.com/amirulashraf/parrot-coder/internal/event"
	"github.com/amirulashraf/parrot-coder/internal/id"
	"github.com/amirulashraf/parrot-coder/internal/protocol"
)

type Selection struct {
	Agent    string `json:"agent"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Variant  string `json:"variant,omitempty"`
}

type SelectionPatch struct {
	Agent            string
	Provider         string
	Model            string
	Variant          *string
	FallbackAgent    string
	FallbackProvider string
}

type SelectionValidator func(Selection) error

type AgentSessionStore interface {
	Get(context.Context) (AgentSessionDto, error)
	Delete(context.Context) error
	SetSelection(context.Context, Selection) error
	UpdateSelection(context.Context, SelectionPatch, SelectionValidator) (AgentSessionDto, error)
	StatusPromptPending(context.Context) (bool, error)
	Admit(context.Context, AdmitParams) (Admission, error)
	PromoteSteers(context.Context, int64) ([]Message, error)
	PromoteNextQueue(context.Context) ([]Message, error)
	HasPendingInputs(context.Context) (bool, error)
	ListMessages(context.Context) ([]Message, error)
	AppendMessage(context.Context, protocol.Message) (Message, error)
	AppendMessageIfNoPendingInputs(context.Context, string, protocol.Message) (Message, bool, error)
	AppendStatusPrompt(context.Context, string) (Message, error)
	ListModelHistory(context.Context, int64) ([]protocol.Message, error)
	LatestSequence(context.Context) (int64, error)
	PendingCutoff(context.Context) (int64, error)
	CurrentCompactionEpoch(context.Context) (CompactionEpoch, error)
	StartAssistant(context.Context) (Message, error)
	FinishAssistant(context.Context, string, AssistantFinal) error
	AddToolCall(context.Context, string, protocol.ToolCall) (ToolCall, error)
	StartTool(context.Context, string) error
	SettleTool(context.Context, string, string, string, string) error
	SettleToolWithOutput(context.Context, string, string, string, string, string) error
	RepairActive(context.Context) error
	RecordCompactionRetry(context.Context, string, string) error
}

func (s *agentSessionStore) SetSelection(ctx context.Context, selection Selection) error {
	if selection.Agent == "" || selection.Provider == "" || selection.Model == "" {
		return ErrSelectionRequired
	}
	_, err := s.UpdateSelection(ctx, SelectionPatch{Agent: selection.Agent, Provider: selection.Provider, Model: selection.Model, Variant: &selection.Variant}, nil)
	return err
}

// UpdateSelection carries omitted values forward and persists the resulting
// complete selection in the same aggregate transaction. The validator runs
// after the current values are read and before an event or projection is made.
func (s *agentSessionStore) UpdateSelection(ctx context.Context, patch SelectionPatch, validate SelectionValidator) (AgentSessionDto, error) {
	var updated AgentSessionDto
	_, err := s.events.AppendBuilt(ctx, s.sessionID, func(ctx context.Context, tx *sql.Tx, _ int64) ([]event.NewEvent, event.Projector, error) {
		current, err := scanSession(tx.QueryRowContext(ctx, `
			SELECT id, parent_session_id, name, project_id, project_root, title, selected_agent, selected_provider, selected_model, selected_variant, created_at, updated_at
			FROM session WHERE id = ?`, s.sessionID))
		if err != nil {
			return nil, nil, err
		}
		selection := Selection{Agent: current.Agent, Provider: current.Provider, Model: current.Model, Variant: current.Variant}
		if patch.Agent != "" {
			selection.Agent = patch.Agent
		}
		if patch.Provider != "" {
			selection.Provider = patch.Provider
		}
		if patch.Model != "" {
			selection.Model = patch.Model
		}
		if patch.Variant != nil {
			selection.Variant = *patch.Variant
		}
		if selection.Agent == "" {
			selection.Agent = patch.FallbackAgent
		}
		if selection.Provider == "" {
			selection.Provider = patch.FallbackProvider
		}
		if selection.Agent == "" || selection.Provider == "" || selection.Model == "" {
			return nil, nil, ErrSelectionRequired
		}
		if validate != nil {
			if err := validate(selection); err != nil {
				return nil, nil, err
			}
		}
		data, err := json.Marshal(struct {
			Selection
			ModeChanged bool `json:"mode_changed"`
		}{Selection: selection, ModeChanged: current.Agent != selection.Agent})
		if err != nil {
			return nil, nil, err
		}
		project := func(ctx context.Context, tx *sql.Tx, events []event.Event) error {
			result, err := tx.ExecContext(ctx, `UPDATE session SET selected_agent=?, selected_provider=?, selected_model=?, selected_variant=?, updated_at=? WHERE id=?`, selection.Agent, selection.Provider, selection.Model, selection.Variant, formatTime(events[0].CreatedAt), s.sessionID)
			if err != nil {
				return err
			}
			n, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if n != 1 {
				return ErrNotFound
			}
			updated = current
			updated.Agent, updated.Provider, updated.Model, updated.Variant = selection.Agent, selection.Provider, selection.Model, selection.Variant
			updated.UpdatedAt = events[0].CreatedAt
			return nil
		}
		return []event.NewEvent{{Type: "session.selection.changed", Data: data}}, project, nil
	})
	if err != nil {
		return AgentSessionDto{}, err
	}
	// The selection is indexed, so the published entry has to follow the commit
	// for LatestSelection on any host to see it.
	if err := s.publish(updated); err != nil {
		return AgentSessionDto{}, err
	}
	return updated, nil
}

// StatusPromptPending reports whether the session needs a durable status
// message. A status is needed once for a new session and after each actual mode
// change. The append event is the consumption marker, so an interrupted turn
// does not append the same status again.
func (s *agentSessionStore) StatusPromptPending(ctx context.Context) (bool, error) {
	db, err := s.sessions.Session(ctx, s.sessionID)
	if err != nil {
		return false, err
	}
	var statusCount, changedAfterStatus int
	err = db.SQL().QueryRowContext(ctx, `
		SELECT
			(SELECT COUNT(*) FROM event WHERE session_id=? AND type='session.status_prompt.appended'),
			(SELECT COUNT(*) FROM event
			 WHERE session_id=? AND type='session.selection.changed'
			   AND json_extract(data_json, '$.mode_changed') = 1
			   AND sequence > COALESCE((SELECT MAX(sequence) FROM event WHERE session_id=? AND type='session.status_prompt.appended'), -1))`,
		s.sessionID, s.sessionID, s.sessionID).Scan(&statusCount, &changedAfterStatus)
	if err != nil {
		return false, err
	}
	return statusCount == 0 || changedAfterStatus > 0, nil
}

func (s *agentSessionStore) LatestSequence(ctx context.Context) (int64, error) {
	db, err := s.sessions.Session(ctx, s.sessionID)
	if err != nil {
		return 0, err
	}
	var next int64
	err = db.SQL().QueryRowContext(ctx, `SELECT next_sequence FROM event_sequence WHERE session_id=?`, s.sessionID).Scan(&next)
	if errors.Is(err, sql.ErrNoRows) {
		return -1, nil
	}
	return next - 1, err
}

func (s *agentSessionStore) PendingCutoff(ctx context.Context) (int64, error) {
	db, err := s.sessions.Session(ctx, s.sessionID)
	if err != nil {
		return -1, err
	}
	var cutoff sql.NullInt64
	err = db.SQL().QueryRowContext(ctx, `SELECT MAX(admitted_sequence) FROM session_input WHERE session_id=? AND status='pending'`, s.sessionID).Scan(&cutoff)
	if err != nil || !cutoff.Valid {
		return -1, err
	}
	return cutoff.Int64, nil
}

type CompactionEpoch struct {
	ID            string
	SessionID     string
	Ordinal       int
	SummaryPrompt string
	HistoryCutoff int64
	CreatedAt     time.Time
}

func (s *agentSessionStore) CurrentCompactionEpoch(ctx context.Context) (CompactionEpoch, error) {
	db, err := s.sessions.Session(ctx, s.sessionID)
	if err != nil {
		return CompactionEpoch{}, err
	}
	var item CompactionEpoch
	var created string
	err = db.SQL().QueryRowContext(ctx, `SELECT id, session_id, ordinal, summary_prompt, history_cutoff, created_at FROM session_compaction_epoch WHERE session_id=? ORDER BY ordinal DESC LIMIT 1`, s.sessionID).Scan(&item.ID, &item.SessionID, &item.Ordinal, &item.SummaryPrompt, &item.HistoryCutoff, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return CompactionEpoch{}, ErrNotFound
	}
	if err != nil {
		return CompactionEpoch{}, err
	}
	item.CreatedAt, err = parseTime(created)
	return item, err
}

func (s *agentSessionStore) AppendMessage(ctx context.Context, message protocol.Message) (Message, error) {
	messageID, err := id.New("msg")
	if err != nil {
		return Message{}, err
	}
	parts, err := json.Marshal(message.Content)
	if err != nil {
		return Message{}, err
	}
	content := textContent(message.Content)
	payload, _ := json.Marshal(map[string]any{"message_id": messageID, "role": message.Role})
	var out Message
	_, err = s.events.Append(ctx, s.sessionID, []event.NewEvent{{Type: "session.message.appended", Data: payload}}, func(ctx context.Context, tx *sql.Tx, events []event.Event) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO session_message(id,session_id,role,content,parts_json,status,sequence,created_at) VALUES(?,?,?,?,?,'complete',?,?)`, messageID, s.sessionID, message.Role, content, parts, events[0].Sequence, formatTime(events[0].CreatedAt))
		if err == nil {
			out = Message{ID: messageID, SessionID: s.sessionID, Role: string(message.Role), Content: content, Parts: parts, Status: "complete", Sequence: events[0].Sequence, CreatedAt: events[0].CreatedAt}
		}
		return err
	})
	return out, err
}

// AppendMessageIfNoPendingInputs appends a message only when every admitted
// input has already been promoted. The pending-input check and append share the
// aggregate transaction so an admission cannot slip between them. A repeated
// message ID is accepted idempotently and needs processing only when no later
// assistant response exists.
func (s *agentSessionStore) AppendMessageIfNoPendingInputs(ctx context.Context, messageID string, message protocol.Message) (Message, bool, error) {
	sessionID := s.sessionID
	if messageID == "" {
		return Message{}, false, errors.New("session: message ID is required")
	}
	parts, err := json.Marshal(message.Content)
	if err != nil {
		return Message{}, false, err
	}
	content := textContent(message.Content)
	payload, _ := json.Marshal(map[string]any{"message_id": messageID, "role": message.Role})
	var out Message
	needsProcessing := false
	_, err = s.events.AppendBuilt(ctx, sessionID, func(ctx context.Context, tx *sql.Tx, _ int64) ([]event.NewEvent, event.Projector, error) {
		var created string
		var existingParts []byte
		err := tx.QueryRowContext(ctx, `SELECT role,content,parts_json,status,sequence,created_at FROM session_message WHERE session_id=? AND id=?`, sessionID, messageID).Scan(&out.Role, &out.Content, &existingParts, &out.Status, &out.Sequence, &created)
		if err == nil {
			out.ID, out.SessionID, out.Parts = messageID, sessionID, append(json.RawMessage(nil), existingParts...)
			out.CreatedAt, err = parseTime(created)
			if err != nil {
				return nil, nil, err
			}
			var answered bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM session_message WHERE session_id=? AND role='assistant' AND sequence>?)`, sessionID, out.Sequence).Scan(&answered); err != nil {
				return nil, nil, fmt.Errorf("session: check conditional message response: %w", err)
			}
			needsProcessing = !answered
			return nil, nil, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, nil, fmt.Errorf("session: find conditional message: %w", err)
		}
		var pending int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_input WHERE session_id=? AND status='pending'`, sessionID).Scan(&pending); err != nil {
			return nil, nil, fmt.Errorf("session: count pending inputs: %w", err)
		}
		if pending != 0 {
			return nil, nil, nil
		}
		needsProcessing = true
		project := func(ctx context.Context, tx *sql.Tx, events []event.Event) error {
			_, err := tx.ExecContext(ctx, `INSERT INTO session_message(id,session_id,role,content,parts_json,status,sequence,created_at) VALUES(?,?,?,?,?,'complete',?,?)`, messageID, sessionID, message.Role, content, parts, events[0].Sequence, formatTime(events[0].CreatedAt))
			if err == nil {
				out = Message{ID: messageID, SessionID: sessionID, Role: string(message.Role), Content: content, Parts: parts, Status: "complete", Sequence: events[0].Sequence, CreatedAt: events[0].CreatedAt}
			}
			return err
		}
		return []event.NewEvent{{Type: "session.message.appended", Data: payload}}, project, nil
	})
	if err != nil {
		return Message{}, false, err
	}
	return out, needsProcessing, nil
}

// AppendStatusPrompt persists rendered runtime status in its model-history
// position and records the marker used to avoid duplicate delivery.
func (s *agentSessionStore) AppendStatusPrompt(ctx context.Context, text string) (Message, error) {
	messageID, err := id.New("msg")
	if err != nil {
		return Message{}, err
	}
	parts, err := json.Marshal([]protocol.ContentPart{{Type: protocol.ContentText, Text: text}})
	if err != nil {
		return Message{}, err
	}
	payload, _ := json.Marshal(map[string]string{"message_id": messageID})
	var out Message
	_, err = s.events.Append(ctx, s.sessionID, []event.NewEvent{{Type: "session.status_prompt.appended", Data: payload}}, func(ctx context.Context, tx *sql.Tx, events []event.Event) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO session_message(id,session_id,role,content,parts_json,status,sequence,created_at) VALUES(?,?,'system',?,?,'complete',?,?)`, messageID, s.sessionID, text, parts, events[0].Sequence, formatTime(events[0].CreatedAt))
		if err == nil {
			out = Message{ID: messageID, SessionID: s.sessionID, Role: string(protocol.RoleSystem), Content: text, Parts: parts, Status: "complete", Sequence: events[0].Sequence, CreatedAt: events[0].CreatedAt}
		}
		return err
	})
	return out, err
}

func (s *agentSessionStore) StartAssistant(ctx context.Context) (Message, error) {
	messageID, _ := id.New("msg")
	payload, _ := json.Marshal(map[string]string{"message_id": messageID})
	var out Message
	_, err := s.events.Append(ctx, s.sessionID, []event.NewEvent{{Type: "session.assistant.started", Data: payload}}, func(ctx context.Context, tx *sql.Tx, events []event.Event) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO session_message(id,session_id,role,content,parts_json,status,sequence,created_at) VALUES(?,?,'assistant','','[]','active',?,?)`, messageID, s.sessionID, events[0].Sequence, formatTime(events[0].CreatedAt))
		out = Message{ID: messageID, SessionID: s.sessionID, Role: "assistant", Status: "active", Sequence: events[0].Sequence, CreatedAt: events[0].CreatedAt}
		return err
	})
	return out, err
}

type AssistantFinal struct {
	Parts        []protocol.ContentPart
	Usage        protocol.Usage
	FinishReason protocol.FinishReason
	Error        string
	Status       string
}

func (s *agentSessionStore) FinishAssistant(ctx context.Context, messageID string, final AssistantFinal) error {
	if final.Status == "" {
		final.Status = "complete"
	}
	parts, _ := json.Marshal(final.Parts)
	usage, _ := json.Marshal(final.Usage)
	payload, _ := json.Marshal(map[string]any{"message_id": messageID, "status": final.Status, "finish_reason": final.FinishReason, "error": final.Error})
	_, err := s.events.Append(ctx, s.sessionID, []event.NewEvent{{Type: "session.assistant." + final.Status, Data: payload}}, func(ctx context.Context, tx *sql.Tx, events []event.Event) error {
		result, err := tx.ExecContext(ctx, `UPDATE session_message SET content=?,parts_json=?,status=?,finish_reason=?,error_text=?,usage_json=? WHERE id=? AND session_id=? AND status='active'`, textContent(final.Parts), parts, final.Status, final.FinishReason, final.Error, usage, messageID, s.sessionID)
		if err != nil {
			return err
		}
		n, _ := result.RowsAffected()
		if n != 1 {
			return errors.New("session: assistant is not active")
		}
		return nil
	})
	return err
}

type ToolCall struct {
	ID, SessionID, MessageID, Name, Status, Result, Error string
	Input                                                 json.RawMessage
	Sequence                                              int64
}

func (s *agentSessionStore) AddToolCall(ctx context.Context, messageID string, call protocol.ToolCall) (ToolCall, error) {
	if call.ID == "" || call.Name == "" || !json.Valid(call.Input) {
		return ToolCall{}, errors.New("session: invalid tool call")
	}
	var input map[string]any
	decoder := json.NewDecoder(bytes.NewReader(call.Input))
	decoder.UseNumber()
	if err := decoder.Decode(&input); err != nil || input == nil {
		return ToolCall{}, errors.New("session: invalid tool call")
	}
	payload, _ := json.Marshal(v1.ToolEvent{CallID: call.ID, ToolName: call.Name, Input: input, Status: "pending"})
	var out ToolCall
	_, err := s.events.Append(ctx, s.sessionID, []event.NewEvent{{Type: "session.tool.pending", Data: payload}}, func(ctx context.Context, tx *sql.Tx, events []event.Event) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO session_tool_call(id,session_id,message_id,name,input_json,status,sequence,created_at) VALUES(?,?,?,?,?,'pending',?,?)`, call.ID, s.sessionID, messageID, call.Name, []byte(call.Input), events[0].Sequence, formatTime(events[0].CreatedAt))
		out = ToolCall{ID: call.ID, SessionID: s.sessionID, MessageID: messageID, Name: call.Name, Input: append(json.RawMessage(nil), call.Input...), Status: "pending", Sequence: events[0].Sequence}
		return err
	})
	return out, err
}

func (s *agentSessionStore) StartTool(ctx context.Context, callID string) error {
	return s.transitionTool(ctx, callID, "running", "", "")
}

func (s *agentSessionStore) SettleTool(ctx context.Context, callID, status, result, errorText string) error {
	return s.SettleToolWithOutput(ctx, callID, status, result, errorText, "")
}

func (s *agentSessionStore) SettleToolWithOutput(ctx context.Context, callID, status, result, errorText, outputTail string) error {
	if status != "success" && status != "failure" && status != "interrupted" {
		return errors.New("session: invalid terminal tool status")
	}
	return s.transitionTool(ctx, callID, status, result, errorText, outputTail)
}

func (s *agentSessionStore) transitionTool(ctx context.Context, callID, status, resultText, errorText string, outputTail ...string) error {
	_, err := s.events.AppendBuilt(ctx, s.sessionID, func(ctx context.Context, tx *sql.Tx, _ int64) ([]event.NewEvent, event.Projector, error) {
		var name string
		if err := tx.QueryRowContext(ctx, `SELECT name FROM session_tool_call WHERE id=? AND session_id=?`, callID, s.sessionID).Scan(&name); err != nil {
			return nil, nil, err
		}
		tail := ""
		if len(outputTail) > 0 {
			tail = outputTail[0]
		}
		payload, _ := json.Marshal(v1.ToolEvent{CallID: callID, ToolName: name, Status: status, Result: resultText, Error: errorText, OutputTail: tail})
		project := func(ctx context.Context, tx *sql.Tx, events []event.Event) error {
			query := `UPDATE session_tool_call SET status=? WHERE id=? AND session_id=? AND status='pending'`
			args := []any{status, callID, s.sessionID}
			if status != "running" {
				query = `UPDATE session_tool_call SET status=?,result_text=?,error_text=?,settled_sequence=?,settled_at=? WHERE id=? AND session_id=? AND status IN ('pending','running')`
				args = []any{status, resultText, errorText, events[0].Sequence, formatTime(events[0].CreatedAt), callID, s.sessionID}
			}
			res, err := tx.ExecContext(ctx, query, args...)
			if err != nil {
				return err
			}
			n, _ := res.RowsAffected()
			if n != 1 {
				return errors.New("session: tool call is not active")
			}
			return nil
		}
		return []event.NewEvent{{Type: "session.tool." + status, Data: payload}}, project, nil
	})
	return err
}

// RepairActive settles work this session left in flight when its process died.
//
// Repair is per session because that is the scope in which "abandoned" is
// knowable: these rows are active only if the process running this session
// stopped. Compaction attempts are repaired here too, in the same transaction.
// They were previously swept across every session in a shared database, which
// interrupted compactions that other live processes, on other machines, were
// still running.
func (s *agentSessionStore) RepairActive(ctx context.Context) error {
	const reason = "process restarted"
	data := json.RawMessage(`{"reason":"process restarted"}`)
	_, err := s.events.AppendBuilt(ctx, s.sessionID, func(ctx context.Context, tx *sql.Tx, _ int64) ([]event.NewEvent, event.Projector, error) {
		type activeTool struct{ id, name string }
		var tools []activeTool
		rows, err := tx.QueryContext(ctx, `SELECT id,name FROM session_tool_call WHERE session_id=? AND status IN ('pending','running') ORDER BY sequence`, s.sessionID)
		if err != nil {
			return nil, nil, err
		}
		for rows.Next() {
			var item activeTool
			if err := rows.Scan(&item.id, &item.name); err != nil {
				rows.Close()
				return nil, nil, err
			}
			tools = append(tools, item)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, nil, err
		}
		if err := rows.Close(); err != nil {
			return nil, nil, err
		}
		var otherActive int
		if err := tx.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM session_message WHERE session_id=? AND status='active')
			+ (SELECT COUNT(*) FROM compaction_attempt WHERE session_id=? AND status='active')`, s.sessionID, s.sessionID).Scan(&otherActive); err != nil {
			return nil, nil, err
		}
		if len(tools) == 0 && otherActive == 0 {
			return nil, nil, nil
		}

		pending := make([]event.NewEvent, 0, len(tools)+1)
		for _, item := range tools {
			payload, _ := json.Marshal(v1.ToolEvent{CallID: item.id, ToolName: item.name, Status: "interrupted", Error: reason})
			pending = append(pending, event.NewEvent{Type: "session.tool.interrupted", Data: payload})
		}
		pending = append(pending, event.NewEvent{Type: "session.runtime.repaired", Data: data})
		project := func(ctx context.Context, tx *sql.Tx, events []event.Event) error {
			if _, err := tx.ExecContext(ctx, `UPDATE session_message SET status='interrupted',error_text=? WHERE session_id=? AND status='active'`, reason, s.sessionID); err != nil {
				return err
			}
			for i, item := range tools {
				result, err := tx.ExecContext(ctx, `UPDATE session_tool_call SET status='interrupted',error_text=?,settled_sequence=?,settled_at=? WHERE id=? AND session_id=? AND status IN ('pending','running')`, reason, events[i].Sequence, formatTime(events[i].CreatedAt), item.id, s.sessionID)
				if err != nil {
					return err
				}
				if affected, _ := result.RowsAffected(); affected != 1 {
					return errors.New("session: active tool changed during repair")
				}
			}
			repaired := events[len(events)-1]
			_, err := tx.ExecContext(ctx, `UPDATE compaction_attempt SET status='interrupted',error_text=?,finished_at=? WHERE session_id=? AND status='active'`, reason, formatTime(repaired.CreatedAt), s.sessionID)
			return err
		}
		return pending, project, nil
	})
	return err
}

func (s *agentSessionStore) RecordCompactionRetry(ctx context.Context, providerCode, recordID string) error {
	data, _ := json.Marshal(map[string]string{"provider_code": providerCode, "compaction_record_id": recordID})
	_, err := s.events.Append(ctx, s.sessionID, []event.NewEvent{{Type: "session.compaction.retry", Data: data}}, nil)
	return err
}

func (s *agentSessionStore) ListModelHistory(ctx context.Context, cutoff int64) ([]protocol.Message, error) {
	db, err := s.sessions.Session(ctx, s.sessionID)
	if err != nil {
		return nil, err
	}
	rows, err := db.SQL().QueryContext(ctx, `SELECT role,content,parts_json FROM session_message WHERE session_id=? AND sequence>=? AND status IN ('complete','error','interrupted') AND NOT (status='error' AND content='' AND parts_json='[]') ORDER BY sequence`, s.sessionID, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var messages []protocol.Message
	for rows.Next() {
		var role, content string
		var raw []byte
		if err := rows.Scan(&role, &content, &raw); err != nil {
			return nil, err
		}
		var parts []protocol.ContentPart
		if err := json.Unmarshal(raw, &parts); err != nil {
			return nil, fmt.Errorf("session: decode message parts: %w", err)
		}
		if len(parts) == 0 && content != "" {
			parts = []protocol.ContentPart{{Type: protocol.ContentText, Text: content}}
		}
		// Terminal error and interruption rows remain durable audit records, but
		// a contentless assistant projection is not a valid model-history turn.
		if len(parts) == 0 {
			continue
		}
		messages = append(messages, protocol.Message{Role: protocol.Role(role), Content: parts})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return s.repairToolHistory(ctx, cutoff, messages)
}

type historyToolState struct {
	status string
	result string
	err    string
}

// repairToolHistory enforces the provider invariant that every function call
// in history has a corresponding output. Normally the runner appends that
// output directly. This fallback also repairs sessions created before that
// behavior existed, and calls interrupted by a process crash between settling
// the tool and appending its result message.
func (s *agentSessionStore) repairToolHistory(ctx context.Context, cutoff int64, messages []protocol.Message) ([]protocol.Message, error) {
	db, err := s.sessions.Session(ctx, s.sessionID)
	if err != nil {
		return nil, err
	}
	states := make(map[string]historyToolState)
	rows, err := db.SQL().QueryContext(ctx, `SELECT id,status,result_text,error_text FROM session_tool_call WHERE session_id=? AND sequence>=?`, s.sessionID, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var state historyToolState
		if err := rows.Scan(&id, &state.status, &state.result, &state.err); err != nil {
			return nil, err
		}
		states[id] = state
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	outputs := make(map[string]bool)
	for _, message := range messages {
		for _, part := range message.Content {
			if part.Type == protocol.ContentToolResult && part.ToolCallID != "" {
				outputs[part.ToolCallID] = true
			}
		}
	}

	repaired := make([]protocol.Message, 0, len(messages))
	for _, message := range messages {
		parts := make([]protocol.ContentPart, 0, len(message.Content))
		var missing []protocol.Message
		for _, part := range message.Content {
			if part.Type != protocol.ContentToolCall || part.ToolCall == nil {
				parts = append(parts, part)
				continue
			}
			state, known := states[part.ToolCall.ID]
			if outputs[part.ToolCall.ID] {
				parts = append(parts, part)
				continue
			}
			if !known || state.status == "pending" || state.status == "running" {
				// The call was never made executable, so retaining it would create
				// an unanswerable function call in the provider request.
				continue
			}
			parts = append(parts, part)
			text := state.result
			if state.status != "success" {
				text = "Error: tool execution " + state.status
				if state.err != "" {
					text += ": " + state.err
				}
			}
			missing = append(missing, protocol.Message{Role: protocol.RoleTool, Content: []protocol.ContentPart{{Type: protocol.ContentToolResult, Text: text, ToolCallID: part.ToolCall.ID}}})
		}
		if len(parts) > 0 {
			message.Content = parts
			repaired = append(repaired, message)
		}
		repaired = append(repaired, missing...)
	}
	return repaired, nil
}

func textContent(parts []protocol.ContentPart) string {
	var text string
	for _, part := range parts {
		if part.Type == protocol.ContentText {
			text += part.Text
		}
	}
	return text
}
