package session

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

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

func (s *Service) SetSelection(ctx context.Context, sessionID string, selection Selection) error {
	if selection.Agent == "" || selection.Provider == "" || selection.Model == "" {
		return ErrSelectionRequired
	}
	_, err := s.UpdateSelection(ctx, sessionID, SelectionPatch{Agent: selection.Agent, Provider: selection.Provider, Model: selection.Model, Variant: &selection.Variant}, nil)
	return err
}

// UpdateSelection carries omitted values forward and persists the resulting
// complete selection in the same aggregate transaction. The validator runs
// after the current values are read and before an event or projection is made.
func (s *Service) UpdateSelection(ctx context.Context, sessionID string, patch SelectionPatch, validate SelectionValidator) (Session, error) {
	var updated Session
	_, err := s.events.AppendBuilt(ctx, sessionID, func(ctx context.Context, tx *sql.Tx, _ int64) ([]event.NewEvent, event.Projector, error) {
		current, err := scanSession(tx.QueryRowContext(ctx, `
			SELECT id, parent_session_id, project_id, project_root, title, selected_agent, selected_provider, selected_model, selected_variant, created_at, updated_at
			FROM session WHERE id = ?`, sessionID))
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
		data, err := json.Marshal(selection)
		if err != nil {
			return nil, nil, err
		}
		project := func(ctx context.Context, tx *sql.Tx, events []event.Event) error {
			result, err := tx.ExecContext(ctx, `UPDATE session SET selected_agent=?, selected_provider=?, selected_model=?, selected_variant=?, updated_at=? WHERE id=?`, selection.Agent, selection.Provider, selection.Model, selection.Variant, formatTime(events[0].CreatedAt), sessionID)
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
		return Session{}, err
	}
	// The selection is indexed, so the published entry has to follow the commit
	// for LatestSelection on any host to see it.
	if err := s.publish(updated); err != nil {
		return Session{}, err
	}
	return updated, nil
}

func (s *Service) LatestSequence(ctx context.Context, sessionID string) (int64, error) {
	db, err := s.sessions.Session(ctx, sessionID)
	if err != nil {
		return 0, err
	}
	var next int64
	err = db.SQL().QueryRowContext(ctx, `SELECT next_sequence FROM event_sequence WHERE session_id=?`, sessionID).Scan(&next)
	if errors.Is(err, sql.ErrNoRows) {
		return -1, nil
	}
	return next - 1, err
}

func (s *Service) PendingCutoff(ctx context.Context, sessionID string) (int64, error) {
	db, err := s.sessions.Session(ctx, sessionID)
	if err != nil {
		return -1, err
	}
	var cutoff sql.NullInt64
	err = db.SQL().QueryRowContext(ctx, `SELECT MAX(admitted_sequence) FROM session_input WHERE session_id=? AND status='pending'`, sessionID).Scan(&cutoff)
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
	db, err := s.sessions.Session(ctx, sessionID)
	if err != nil {
		return ContextEpoch{}, err
	}
	var item ContextEpoch
	var created string
	err = db.SQL().QueryRowContext(ctx, `SELECT id, session_id, ordinal, baseline, sources_json, history_cutoff, created_at FROM session_context_epoch WHERE session_id=? ORDER BY ordinal DESC LIMIT 1`, sessionID).Scan(&item.ID, &item.SessionID, &item.Ordinal, &item.Baseline, &item.Sources, &item.HistoryCutoff, &created)
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
	payload, _ := json.Marshal(map[string]any{"epoch_id": epochID, "history_cutoff": cutoff, "agents_files": loadedAgentsFiles(nil, sources)})
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
			data, _ := json.Marshal(map[string]any{"message_id": messageID, "content": text, "agents_files": loadedAgentsFiles(current, sources)})
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

// loadedAgentsFiles returns AGENTS.md sources that are present in the next
// snapshot and are new or changed relative to the previous snapshot. Paths are
// explicit context metadata so clients can accurately report global and nested
// project files without parsing human-facing labels.
func loadedAgentsFiles(previous, next json.RawMessage) []string {
	type source struct {
		Available bool            `json:"available"`
		Value     json.RawMessage `json:"value"`
		Path      string          `json:"path"`
	}
	decode := func(raw json.RawMessage) map[string]source {
		values := map[string]source{}
		_ = json.Unmarshal(raw, &values)
		return values
	}
	before, after := decode(previous), decode(next)
	keys := make([]string, 0, len(after))
	for key := range after {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	paths := make([]string, 0)
	for _, key := range keys {
		item := after[key]
		if !strings.HasPrefix(key, "agents:") || !item.Available || len(item.Value) == 0 || item.Path == "" {
			continue
		}
		old, existed := before[key]
		if existed && old.Available && old.Path == item.Path && bytes.Equal(old.Value, item.Value) {
			continue
		}
		paths = append(paths, item.Path)
	}
	return paths
}

func (s *Service) ReplaceContext(ctx context.Context, sessionID, baseline string, sources json.RawMessage, cutoff int64) (ContextEpoch, error) {
	if !json.Valid(sources) || cutoff < 0 {
		return ContextEpoch{}, errors.New("session: invalid replacement context")
	}
	epochID, _ := id.New("ctx")
	payload, _ := json.Marshal(map[string]any{"epoch_id": epochID, "history_cutoff": cutoff, "agents_files": loadedAgentsFiles(nil, sources)})
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
	return s.SettleToolWithOutput(ctx, sessionID, callID, status, result, errorText, "")
}

func (s *Service) SettleToolWithOutput(ctx context.Context, sessionID, callID, status, result, errorText, outputTail string) error {
	if status != "success" && status != "failure" && status != "interrupted" {
		return errors.New("session: invalid terminal tool status")
	}
	return s.transitionTool(ctx, sessionID, callID, status, result, errorText, outputTail)
}

func (s *Service) transitionTool(ctx context.Context, sessionID, callID, status, resultText, errorText string, outputTail ...string) error {
	_, err := s.events.AppendBuilt(ctx, sessionID, func(ctx context.Context, tx *sql.Tx, _ int64) ([]event.NewEvent, event.Projector, error) {
		var name string
		if err := tx.QueryRowContext(ctx, `SELECT name FROM session_tool_call WHERE id=? AND session_id=?`, callID, sessionID).Scan(&name); err != nil {
			return nil, nil, err
		}
		tail := ""
		if len(outputTail) > 0 {
			tail = outputTail[0]
		}
		payload, _ := json.Marshal(map[string]string{"call_id": callID, "tool_name": name, "status": status, "result": resultText, "error": errorText, "output_tail": tail})
		project := func(ctx context.Context, tx *sql.Tx, events []event.Event) error {
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
func (s *Service) RepairActive(ctx context.Context, sessionID string) error {
	data := json.RawMessage(`{"reason":"process restarted"}`)
	_, err := s.events.AppendBuilt(ctx, sessionID, func(ctx context.Context, tx *sql.Tx, _ int64) ([]event.NewEvent, event.Projector, error) {
		var active int
		if err := tx.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM session_message WHERE session_id=? AND status='active')
			+ (SELECT COUNT(*) FROM session_tool_call WHERE session_id=? AND status IN ('pending','running'))
			+ (SELECT COUNT(*) FROM compaction_attempt WHERE session_id=? AND status='active')`, sessionID, sessionID, sessionID).Scan(&active); err != nil {
			return nil, nil, err
		}
		if active == 0 {
			return nil, nil, nil
		}
		project := func(ctx context.Context, tx *sql.Tx, events []event.Event) error {
			if _, err := tx.ExecContext(ctx, `UPDATE session_message SET status='interrupted',error_text='process restarted' WHERE session_id=? AND status='active'`, sessionID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE session_tool_call SET status='interrupted',error_text='process restarted',settled_sequence=?,settled_at=? WHERE session_id=? AND status IN ('pending','running')`, events[0].Sequence, formatTime(events[0].CreatedAt), sessionID); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `UPDATE compaction_attempt SET status='interrupted',error_text='process restarted',finished_at=? WHERE session_id=? AND status='active'`, formatTime(events[0].CreatedAt), sessionID)
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
	db, err := s.sessions.Session(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	rows, err := db.SQL().QueryContext(ctx, `SELECT role,content,parts_json FROM session_message WHERE session_id=? AND sequence>=? AND status IN ('complete','error','interrupted') AND NOT (status='error' AND content='' AND parts_json='[]') ORDER BY sequence`, sessionID, cutoff)
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
	return s.repairToolHistory(ctx, sessionID, cutoff, messages)
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
func (s *Service) repairToolHistory(ctx context.Context, sessionID string, cutoff int64, messages []protocol.Message) ([]protocol.Message, error) {
	db, err := s.sessions.Session(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	states := make(map[string]historyToolState)
	rows, err := db.SQL().QueryContext(ctx, `SELECT id,status,result_text,error_text FROM session_tool_call WHERE session_id=? AND sequence>=?`, sessionID, cutoff)
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
