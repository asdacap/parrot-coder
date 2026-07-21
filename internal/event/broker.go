package event

import (
	"encoding/json"
	"sync"

	v1 "github.com/amirulashraf/parrot-coder/internal/api/v1"
	"github.com/amirulashraf/parrot-coder/internal/id"
	"github.com/amirulashraf/parrot-coder/internal/protocol"
)

// Broker carries disposable protocol events. Durable state must be committed
// through Repository before a corresponding live event is published.
type Broker struct {
	mu        sync.Mutex
	next      uint64
	listeners map[string]map[uint64]chan v1.Event
	taskIDFor func(string) string
}

func NewBroker() *Broker {
	return &Broker{listeners: make(map[string]map[uint64]chan v1.Event)}
}

// SetTaskIDFor installs the resolver attributing locally produced session
// events to their main task. It must be set before the first drain publishes.
func (b *Broker) SetTaskIDFor(resolver func(sessionID string) string) {
	b.mu.Lock()
	b.taskIDFor = resolver
	b.mu.Unlock()
}

func (b *Broker) taskID(sessionID string) string {
	b.mu.Lock()
	resolver := b.taskIDFor
	b.mu.Unlock()
	if resolver == nil {
		return ""
	}
	return resolver(sessionID)
}

func (b *Broker) Subscribe(sessionID string, capacity int) (<-chan v1.Event, func()) {
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

// Publish maps canonical runner events into disposable v1 deltas and statuses.
// It implements agent.LivePublisher without coupling the runner to the API.
func (b *Broker) Publish(sessionID string, item protocol.Event) {
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
			return
		}
		eventType, payload = v1.EventMessagePartDelta, v1.MessagePartDelta{MessageID: item.MessageID, PartID: item.PartID, Kind: "tool_input", Delta: item.ToolInput.Delta, ToolCallID: item.ToolInput.ID, ToolName: item.ToolInput.Name}
	case protocol.EventToolCallComplete:
		eventType, payload = v1.EventSessionStatus, v1.SessionStatus{MessageID: item.MessageID, Kind: "tool_call_complete"}
	case protocol.EventUsage:
		if item.Usage == nil {
			return
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
	case protocol.EventRouterMetadata:
		if item.RouterMetadata == nil {
			return
		}
		message := "via " + item.RouterMetadata.ProviderName
		if item.RouterMetadata.Model != "" {
			message += " (" + item.RouterMetadata.Model + ")"
		}
		eventType, payload = v1.EventSessionStatus, v1.SessionStatus{MessageID: item.MessageID, Kind: "router_metadata", Message: message}
	case protocol.EventToolOutputDelta:
		eventType, payload = v1.EventToolOutputDelta, v1.ToolOutputDelta{ToolCallID: item.ToolCallID, Delta: item.Text}
	default:
		return
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	b.PublishEvent(v1.Event{Type: eventType, SessionID: sessionID, TaskID: b.taskID(sessionID), Data: data})
}

// PublishEvent never blocks a producer. Overflow closes only the slow
// subscriber. Callers publish only after the represented state is visible.
func (b *Broker) PublishEvent(event v1.Event) {
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
