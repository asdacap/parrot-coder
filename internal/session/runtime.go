package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/amirulashraf/parrot-coder/internal/event"
	"github.com/amirulashraf/parrot-coder/internal/id"
	"github.com/amirulashraf/parrot-coder/internal/protocol"
)

type Selection struct {
	Agent    string
	Provider string
	Model    string
}

func (s *Service) SetSelection(ctx context.Context, sessionID string, selection Selection) error {
	if selection.Agent == "" || selection.Provider == "" || selection.Model == "" {
		return errors.New("session: agent, provider, and model are required")
	}
	data, _ := json.Marshal(selection)
	_, err := s.events.Append(ctx, sessionID, []event.NewEvent{{Type: "session.selection.changed", Data: data}}, func(ctx context.Context, tx *sql.Tx, events []event.Event) error {
		result, err := tx.ExecContext(ctx, `UPDATE session SET selected_agent=?, selected_provider=?, selected_model=?, updated_at=? WHERE id=?`, selection.Agent, selection.Provider, selection.Model, formatTime(events[0].CreatedAt), sessionID)
		if err != nil {
			return err
		}
		n, _ := result.RowsAffected()
		if n != 1 {
			return ErrNotFound
		}
		return nil
	})
	return err
}

func (s *Service) LatestSequence(ctx context.Context, sessionID string) (int64, error) {
	var next int64
	err := s.db.SQL().QueryRowContext(ctx, `SELECT next_sequence FROM event_sequence WHERE session_id=?`, sessionID).Scan(&next)
	if errors.Is(err, sql.ErrNoRows) {
		return -1, nil
	}
	return next - 1, err
}

func (s *Service) PendingCutoff(ctx context.Context, sessionID string) (int64, error) {
	var cutoff sql.NullInt64
	err := s.db.SQL().QueryRowContext(ctx, `SELECT MAX(admitted_sequence) FROM session_input WHERE session_id=? AND status='pending'`, sessionID).Scan(&cutoff)
	if err != nil || !cutoff.Valid {
		return -1, err
	}
	return cutoff.Int64, nil
}

type ContextEpoch struct {
	ID            string
	SessionID     string
	Ordinal       int
	Baseline      string
	Sources       json.RawMessage
	HistoryCutoff int64
	CreatedAt     time.Time
}

func (s *Service) CurrentContextEpoch(ctx context.Context, sessionID string) (ContextEpoch, error) {
	var item ContextEpoch
	var created string
	err := s.db.SQL().QueryRowContext(ctx, `SELECT id, session_id, ordinal, baseline, sources_json, history_cutoff, created_at FROM session_context_epoch WHERE session_id=? ORDER BY ordinal DESC LIMIT 1`, sessionID).Scan(&item.ID, &item.SessionID, &item.Ordinal, &item.Baseline, &item.Sources, &item.HistoryCutoff, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return ContextEpoch{}, ErrNotFound
	}
	if err != nil {
		return ContextEpoch{}, err
	}
	item.CreatedAt, err = parseTime(created)
	return item, err
}

func (s *Service) InitializeContext(ctx context.Context, sessionID, baseline string, sources json.RawMessage, cutoff int64) (ContextEpoch, error) {
	if !json.Valid(sources) {
		return ContextEpoch{}, errors.New("session: invalid context sources JSON")
	}
	if cutoff < 0 {
		cutoff = 0
	}
	epochID, err := id.New("ctx")
	if err != nil {
		return ContextEpoch{}, err
	}
	payload, _ := json.Marshal(map[string]any{"epoch_id": epochID, "history_cutoff": cutoff})
	var out ContextEpoch
	_, err = s.events.Append(ctx, sessionID, []event.NewEvent{{Type: "session.context.initialized", Data: payload}}, func(ctx context.Context, tx *sql.Tx, events []event.Event) error {
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_context_epoch WHERE session_id=?`, sessionID).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			return errors.New("session: context already initialized")
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO session_context_epoch(id,session_id,ordinal,baseline,sources_json,history_cutoff,created_at) VALUES(?,?,0,?,?,?,?)`, epochID, sessionID, baseline, []byte(sources), cutoff, formatTime(events[0].CreatedAt))
		if err == nil {
			out = ContextEpoch{epochID, sessionID, 0, baseline, append(json.RawMessage(nil), sources...), cutoff, events[0].CreatedAt}
		}
		return err
	})
	return out, err
}

// ReconcileContext appends one combined system message and advances the source
// snapshot in the same transaction. An unchanged snapshot is a durable no-op.
func (s *Service) ReconcileContext(ctx context.Context, sessionID, text string, sources json.RawMessage) error {
	if !json.Valid(sources) {
		return errors.New("session: invalid context sources JSON")
	}
	_, err := s.events.AppendBuilt(ctx, sessionID, func(ctx context.Context, tx *sql.Tx, _ int64) ([]event.NewEvent, event.Projector, error) {
		var epochID string
		var current []byte
		if err := tx.QueryRowContext(ctx, `SELECT id,sources_json FROM session_context_epoch WHERE session_id=? ORDER BY ordinal DESC LIMIT 1`, sessionID).Scan(&epochID, &current); err != nil {
			return nil, nil, err
		}
		if string(current) == string(sources) {
			return nil, nil, nil
		}
		messageID := ""
		pending := event.NewEvent{Type: "session.context.observed", Data: json.RawMessage(`{}`)}
		if text != "" {
			generated, err := id.New("msg")
			if err != nil {
				return nil, nil, err
			}
			messageID = generated
			data, _ := json.Marshal(map[string]string{"message_id": messageID, "content": text})
			pending = event.NewEvent{Type: "session.context.changed", Data: data}
		}
		project := func(ctx context.Context, tx *sql.Tx, events []event.Event) error {
			if text != "" {
				parts, _ := json.Marshal([]protocol.ContentPart{{Type: protocol.ContentText, Text: text}})
				if _, err := tx.ExecContext(ctx, `INSERT INTO session_message(id,session_id,role,content,parts_json,status,sequence,created_at) VALUES(?,?,'system',?,?,'complete',?,?)`, messageID, sessionID, text, parts, events[0].Sequence, formatTime(events[0].CreatedAt)); err != nil {
					return err
				}
			}
			_, err := tx.ExecContext(ctx, `UPDATE session_context_epoch SET sources_json=? WHERE id=?`, []byte(sources), epochID)
			return err
		}
		return []event.NewEvent{pending}, project, nil
	})
	return err
}

func (s *Service) ReplaceContext(ctx context.Context, sessionID, baseline string, sources json.RawMessage, cutoff int64) (ContextEpoch, error) {
	if !json.Valid(sources) || cutoff < 0 {
		return ContextEpoch{}, errors.New("session: invalid replacement context")
	}
	epochID, _ := id.New("ctx")
	payload, _ := json.Marshal(map[string]any{"epoch_id": epochID, "history_cutoff": cutoff})
	var out ContextEpoch
	_, err := s.events.Append(ctx, sessionID, []event.NewEvent{{Type: "session.context.replaced", Data: payload}}, func(ctx context.Context, tx *sql.Tx, events []event.Event) error {
		var ordinal int
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(ordinal),-1)+1 FROM session_context_epoch WHERE session_id=?`, sessionID).Scan(&ordinal); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO session_context_epoch(id,session_id,ordinal,baseline,sources_json,history_cutoff,created_at) VALUES(?,?,?,?,?,?,?)`, epochID, sessionID, ordinal, baseline, []byte(sources), cutoff, formatTime(events[0].CreatedAt))
		if err == nil {
			out = ContextEpoch{epochID, sessionID, ordinal, baseline, append(json.RawMessage(nil), sources...), cutoff, events[0].CreatedAt}
		}
		return err
	})
	return out, err
}

func (s *Service) AppendMessage(ctx context.Context, sessionID string, message protocol.Message) (Message, error) {
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
	_, err = s.events.Append(ctx, sessionID, []event.NewEvent{{Type: "session.message.appended", Data: payload}}, func(ctx context.Context, tx *sql.Tx, events []event.Event) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO session_message(id,session_id,role,content,parts_json,status,sequence,created_at) VALUES(?,?,?,?,?,'complete',?,?)`, messageID, sessionID, message.Role, content, parts, events[0].Sequence, formatTime(events[0].CreatedAt))
		if err == nil {
			out = Message{ID: messageID, SessionID: sessionID, Role: string(message.Role), Content: content, Parts: parts, Status: "complete", Sequence: events[0].Sequence, CreatedAt: events[0].CreatedAt}
		}
		return err
	})
	return out, err
}

func (s *Service) StartAssistant(ctx context.Context, sessionID string) (Message, error) {
	messageID, _ := id.New("msg")
	payload, _ := json.Marshal(map[string]string{"message_id": messageID})
	var out Message
	_, err := s.events.Append(ctx, sessionID, []event.NewEvent{{Type: "session.assistant.started", Data: payload}}, func(ctx context.Context, tx *sql.Tx, events []event.Event) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO session_message(id,session_id,role,content,parts_json,status,sequence,created_at) VALUES(?,?,'assistant','','[]','active',?,?)`, messageID, sessionID, events[0].Sequence, formatTime(events[0].CreatedAt))
		out = Message{ID: messageID, SessionID: sessionID, Role: "assistant", Status: "active", Sequence: events[0].Sequence, CreatedAt: events[0].CreatedAt}
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

func (s *Service) FinishAssistant(ctx context.Context, sessionID, messageID string, final AssistantFinal) error {
	if final.Status == "" {
		final.Status = "complete"
	}
	parts, _ := json.Marshal(final.Parts)
	usage, _ := json.Marshal(final.Usage)
	payload, _ := json.Marshal(map[string]any{"message_id": messageID, "status": final.Status, "finish_reason": final.FinishReason, "error": final.Error})
	_, err := s.events.Append(ctx, sessionID, []event.NewEvent{{Type: "session.assistant." + final.Status, Data: payload}}, func(ctx context.Context, tx *sql.Tx, events []event.Event) error {
		result, err := tx.ExecContext(ctx, `UPDATE session_message SET content=?,parts_json=?,status=?,finish_reason=?,error_text=?,usage_json=? WHERE id=? AND session_id=? AND status='active'`, textContent(final.Parts), parts, final.Status, final.FinishReason, final.Error, usage, messageID, sessionID)
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

func (s *Service) AddToolCall(ctx context.Context, sessionID, messageID string, call protocol.ToolCall) (ToolCall, error) {
	if call.ID == "" || call.Name == "" || !json.Valid(call.Input) {
		return ToolCall{}, errors.New("session: invalid tool call")
	}
	payload, _ := json.Marshal(call)
	var out ToolCall
	_, err := s.events.Append(ctx, sessionID, []event.NewEvent{{Type: "session.tool.pending", Data: payload}}, func(ctx context.Context, tx *sql.Tx, events []event.Event) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO session_tool_call(id,session_id,message_id,name,input_json,status,sequence,created_at) VALUES(?,?,?,?,?,'pending',?,?)`, call.ID, sessionID, messageID, call.Name, []byte(call.Input), events[0].Sequence, formatTime(events[0].CreatedAt))
		out = ToolCall{ID: call.ID, SessionID: sessionID, MessageID: messageID, Name: call.Name, Input: append(json.RawMessage(nil), call.Input...), Status: "pending", Sequence: events[0].Sequence}
		return err
	})
	return out, err
}

func (s *Service) StartTool(ctx context.Context, sessionID, callID string) error {
	return s.transitionTool(ctx, sessionID, callID, "running", "", "")
}

func (s *Service) SettleTool(ctx context.Context, sessionID, callID, status, result, errorText string) error {
	if status != "success" && status != "failure" && status != "interrupted" {
		return errors.New("session: invalid terminal tool status")
	}
	return s.transitionTool(ctx, sessionID, callID, status, result, errorText)
}

func (s *Service) transitionTool(ctx context.Context, sessionID, callID, status, resultText, errorText string) error {
	payload, _ := json.Marshal(map[string]string{"call_id": callID, "status": status, "result": resultText, "error": errorText})
	_, err := s.events.Append(ctx, sessionID, []event.NewEvent{{Type: "session.tool." + status, Data: payload}}, func(ctx context.Context, tx *sql.Tx, events []event.Event) error {
		query := `UPDATE session_tool_call SET status=? WHERE id=? AND session_id=? AND status='pending'`
		args := []any{status, callID, sessionID}
		if status != "running" {
			query = `UPDATE session_tool_call SET status=?,result_text=?,error_text=?,settled_sequence=?,settled_at=? WHERE id=? AND session_id=? AND status IN ('pending','running')`
			args = []any{status, resultText, errorText, events[0].Sequence, formatTime(events[0].CreatedAt), callID, sessionID}
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
	})
	return err
}

func (s *Service) RepairActive(ctx context.Context, sessionID string) error {
	data := json.RawMessage(`{"reason":"process restarted"}`)
	_, err := s.events.AppendBuilt(ctx, sessionID, func(ctx context.Context, tx *sql.Tx, _ int64) ([]event.NewEvent, event.Projector, error) {
		var active int
		if err := tx.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM session_message WHERE session_id=? AND status='active') + (SELECT COUNT(*) FROM session_tool_call WHERE session_id=? AND status IN ('pending','running'))`, sessionID, sessionID).Scan(&active); err != nil {
			return nil, nil, err
		}
		if active == 0 {
			return nil, nil, nil
		}
		project := func(ctx context.Context, tx *sql.Tx, events []event.Event) error {
			if _, err := tx.ExecContext(ctx, `UPDATE session_message SET status='interrupted',error_text='process restarted' WHERE session_id=? AND status='active'`, sessionID); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `UPDATE session_tool_call SET status='interrupted',error_text='process restarted',settled_sequence=?,settled_at=? WHERE session_id=? AND status IN ('pending','running')`, events[0].Sequence, formatTime(events[0].CreatedAt), sessionID)
			return err
		}
		return []event.NewEvent{{Type: "session.runtime.repaired", Data: data}}, project, nil
	})
	return err
}

func (s *Service) RecordCompactionRetry(ctx context.Context, sessionID, providerCode, recordID string) error {
	data, _ := json.Marshal(map[string]string{"provider_code": providerCode, "compaction_record_id": recordID})
	_, err := s.events.Append(ctx, sessionID, []event.NewEvent{{Type: "session.compaction.retry", Data: data}}, nil)
	return err
}

func (s *Service) ListModelHistory(ctx context.Context, sessionID string, cutoff int64) ([]protocol.Message, error) {
	rows, err := s.db.SQL().QueryContext(ctx, `SELECT role,content,parts_json FROM session_message WHERE session_id=? AND sequence>=? AND status IN ('complete','error','interrupted') AND NOT (status='error' AND content='' AND parts_json='[]') ORDER BY sequence`, sessionID, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []protocol.Message
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
		result = append(result, protocol.Message{Role: protocol.Role(role), Content: parts})
	}
	return result, rows.Err()
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
