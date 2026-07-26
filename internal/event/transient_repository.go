package event

import (
	"encoding/json"
	"sync"

	v1 "github.com/amirulashraf/parrot-coder/internal/api/v1"
	"github.com/amirulashraf/parrot-coder/internal/id"
	"github.com/amirulashraf/parrot-coder/internal/protocol"
)

// TransientRepository carries disposable protocol events. Durable state must be committed
// through Repository before a corresponding live event is published.
type TransientRepository struct {
	mu        sync.Mutex
	next      uint64
	listeners map[string]map[uint64]chan v1.Event
	taskIDFor func(string) string
}

func NewTransientRepository() *TransientRepository {
	return &TransientRepository{listeners: make(map[string]map[uint64]chan v1.Event)}
}

// SetTaskIDFor installs the resolver attributing locally produced session
// events to their main task. It must be set before the first drain publishes.
func (b *TransientRepository) SetTaskIDFor(resolver func(sessionID string) string) {
	b.mu.Lock()
	b.taskIDFor = resolver
	b.mu.Unlock()
}

func (b *TransientRepository) taskID(sessionID string) string {
	b.mu.Lock()
	resolver := b.taskIDFor
	b.mu.Unlock()
	if resolver == nil {
		return ""
	}
	return resolver(sessionID)
}

func (b *TransientRepository) Subscribe(sessionID string, capacity int) (<-chan v1.Event, func()) {
	if capacity < 1 {
		capacity = 1
	}
	ch := make(chan v1.Event, capacity)
	b.mu.Lock()
	id := b.next
	b.next++
	if b.listeners[sessionID] == nil {
		b.listeners[sessionID] = make(map[uint64]chan v1.Event)
	}
	b.listeners[sessionID][id] = ch
	b.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			if current, ok := b.listeners[sessionID][id]; ok {
				delete(b.listeners[sessionID], id)
				close(current)
			}
			if len(b.listeners[sessionID]) == 0 {
				delete(b.listeners, sessionID)
			}
		})
	}
}

// PublishProtocol maps canonical runner events into disposable v1 deltas and statuses.
func (b *TransientRepository) PublishProtocol(sessionID string, item protocol.Event) {
	event, ok := protocolEvent(sessionID, item)
	if ok {
		event.TaskID = b.taskID(sessionID)
		b.PublishEvent(event)
	}
}

func protocolEvent(sessionID string, item protocol.Event) (v1.Event, bool) {
	var eventType string
	var payload any
	switch item.Type {
	case protocol.EventTextDelta:
		eventType, payload = v1.EventMessagePartDelta, v1.MessagePartDelta{MessageID: item.MessageID, PartID: item.PartID, Kind: "text", Delta: item.Text}
	case protocol.EventReasoningDelta:
		eventType, payload = v1.EventMessagePartDelta, v1.MessagePartDelta{MessageID: item.MessageID, PartID: item.PartID, Kind: "reasoning", Delta: item.Text}
	case protocol.EventReasoningSummaryDelta:
		eventType, payload = v1.EventMessagePartDelta, v1.MessagePartDelta{MessageID: item.MessageID, PartID: item.PartID, Kind: "reasoning_summary", Delta: item.Text}
	case protocol.EventReasoningSummaryDone:
		eventType, payload = v1.EventMessagePartDelta, v1.MessagePartDelta{MessageID: item.MessageID, PartID: item.PartID, Kind: "reasoning_summary", Delta: item.Text, Done: true}
	case protocol.EventToolInputDelta:
		if item.ToolInput == nil {
			return v1.Event{}, false
		}
		eventType, payload = v1.EventMessagePartDelta, v1.MessagePartDelta{MessageID: item.MessageID, PartID: item.PartID, Kind: "tool_input", Delta: item.ToolInput.Delta, ToolCallID: item.ToolInput.ID, ToolName: item.ToolInput.Name}
	case protocol.EventToolCallComplete:
		eventType, payload = v1.EventSessionStatus, v1.SessionStatus{MessageID: item.MessageID, Kind: "tool_call_complete"}
	case protocol.EventUsage:
		if item.Usage == nil {
			return v1.Event{}, false
		}
		eventType, payload = v1.EventSessionStatus, v1.SessionStatus{MessageID: item.MessageID, Kind: "usage", Usage: &v1.Usage{InputTokens: item.Usage.InputTokens, OutputTokens: item.Usage.OutputTokens, TotalTokens: item.Usage.TotalTokens, ReasoningTokens: item.Usage.ReasoningTokens, CachedInputTokens: item.Usage.CachedInputTokens, InputCost: item.Usage.InputCost, OutputCost: item.Usage.OutputCost}}
	case protocol.EventFinish:
		eventType, payload = v1.EventSessionStatus, v1.SessionStatus{MessageID: item.MessageID, Kind: "finish", FinishReason: string(item.FinishReason)}
	case protocol.EventProviderError:
		status := v1.SessionStatus{MessageID: item.MessageID, Kind: "provider_error"}
		if item.ProviderError != nil {
			status.ErrorCode = item.ProviderError.Code
		}
		eventType, payload = v1.EventSessionStatus, status
	case protocol.EventProviderRetry:
		eventType, payload = v1.EventSessionStatus, v1.SessionStatus{MessageID: item.MessageID, Kind: "provider_retry", Message: item.Text}
	case protocol.EventStatusPromptInjected:
		eventType, payload = v1.EventSessionStatus, v1.SessionStatus{Kind: "status_prompt", Message: item.Text}
	case protocol.EventMaxTurnsReached:
		eventType, payload = v1.EventSessionStatus, v1.SessionStatus{Kind: "max_turns_reached", Message: item.Text}
	case protocol.EventRouterMetadata:
		if item.RouterMetadata == nil {
			return v1.Event{}, false
		}
		message := "via " + item.RouterMetadata.ProviderName
		if item.RouterMetadata.Model != "" {
			message += " (" + item.RouterMetadata.Model + ")"
		}
		eventType, payload = v1.EventSessionStatus, v1.SessionStatus{MessageID: item.MessageID, Kind: "router_metadata", Message: message}
	case protocol.EventToolOutputDelta:
		eventType, payload = v1.EventToolOutputDelta, v1.ToolOutputDelta{ToolCallID: item.ToolCallID, Delta: item.Text}
	case protocol.EventCodeDisplay:
		if item.CodeDisplay == nil {
			return v1.Event{}, false
		}
		display := item.CodeDisplay
		eventType, payload = v1.EventCodeDisplay, v1.CodeDisplay{ToolCallID: display.ToolCallID, Source: display.Source, Path: display.Path, Language: display.Language, StartLine: display.StartLine}
	default:
		return v1.Event{}, false
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return v1.Event{}, false
	}
	return v1.Event{Type: eventType, SessionID: sessionID, Data: data}, true
}

// PublishEvent never blocks a producer. Overflow closes only the slow
// subscriber. Callers publish only after the represented state is visible.
func (b *TransientRepository) PublishEvent(event v1.Event) {
	if b == nil || event.SessionID == "" || !v1.KnownEvent(event.Type) || len(event.Data) == 0 || !json.Valid(event.Data) {
		return
	}
	if event.ID == "" {
		event.ID, _ = id.New("evt")
		if event.ID == "" {
			return
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for id, listener := range b.listeners[event.SessionID] {
		select {
		case listener <- event:
		default:
			if disposableToolOutput(event) {
				continue
			}
			if evictDisposableToolOutput(listener) {
				listener <- event
				continue
			}
			select {
			case listener <- event:
				continue
			default:
			}
			close(listener)
			delete(b.listeners[event.SessionID], id)
		}
	}
	if len(b.listeners[event.SessionID]) == 0 {
		delete(b.listeners, event.SessionID)
	}
}

func disposableToolOutput(event v1.Event) bool {
	return event.Type == v1.EventToolOutputDelta
}

func evictDisposableToolOutput(listener chan v1.Event) bool {
	queued := make([]v1.Event, 0, cap(listener))
	for {
		select {
		case item := <-listener:
			queued = append(queued, item)
		default:
			removed := false
			for _, item := range queued {
				if !removed && disposableToolOutput(item) {
					removed = true
					continue
				}
				listener <- item
			}
			return removed
		}
	}
}
