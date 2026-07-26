package event_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	v1 "github.com/amirulashraf/parrot-coder/internal/api/v1"
	"github.com/amirulashraf/parrot-coder/internal/event"
	"github.com/amirulashraf/parrot-coder/internal/protocol"
)

type fakeHierarchy map[string]string

var _ event.EventBroker = (*event.Broker)(nil)

func (f fakeHierarchy) ChildRelation(sessionID string) (string, bool) {
	parent, ok := f[sessionID]
	return parent, ok
}

func TestBrokerPublishesPredefinedEventsSynchronously(t *testing.T) {
	broker := event.NewBroker(nil, nil)
	var got event.BrokerEvent
	stopped := false
	broker.SetEventHandler(func(item event.BrokerEvent) func() {
		got = item
		return func() { stopped = true }
	})

	stop := broker.Publish(event.BrokerEvent{Name: event.TurnWorking, Payload: "payload"})
	if got.Name != event.TurnWorking || got.Payload != "payload" {
		t.Fatalf("event = %#v", got)
	}
	stop()
	if !stopped {
		t.Fatal("publication cleanup was not invoked")
	}

	for _, name := range []event.Name{event.TurnStarted, event.TurnWorking, event.TurnProgress, event.TurnFinished, event.TurnCompleted} {
		if !name.Valid() {
			t.Fatalf("predefined event name %q is invalid", name)
		}
	}
	got = event.BrokerEvent{}
	broker.Publish(event.BrokerEvent{Name: "unknown"})()
	if got.Name != "" {
		t.Fatalf("unknown event was published: %#v", got)
	}
	noopStop := (event.NoopBroker{}).Publish(event.BrokerEvent{Name: event.TurnStarted})
	noopStop()
}

func TestBrokerProjectsDescendantsToEveryAncestor(t *testing.T) {
	hierarchy := fakeHierarchy{
		"child":      "parent",
		"grandchild": "child",
	}
	broker := event.NewBroker(nil, nil, hierarchy)
	parent, closeParent := broker.Subscribe("parent", 2)
	defer closeParent()
	child, closeChild := broker.Subscribe("child", 2)
	defer closeChild()
	data, _ := json.Marshal(v1.SessionStatus{Kind: "running"})

	broker.PublishEvent(v1.Event{Type: v1.EventSessionStatus, SessionID: "grandchild", Data: data})
	for name, item := range map[string]v1.Event{"parent": <-parent, "child": <-child} {
		if item.SessionID != "grandchild" || item.Sequence != nil || item.CreatedAt != nil {
			t.Fatalf("%s projection = %#v", name, item)
		}
	}
}

func TestBrokerObserveSessionProjectsDurableEvents(t *testing.T) {
	_, repository, childSessionID := newRepository(t)
	hierarchy := fakeHierarchy{childSessionID: "parent"}
	broker := event.NewBroker(repository, nil, hierarchy)
	parent, closeParent := broker.Subscribe("parent", 1)
	defer closeParent()
	broker.ObserveSession(childSessionID)
	broker.ObserveSession(childSessionID)

	data := json.RawMessage(`{"kind":"running"}`)
	appended, err := repository.Append(context.Background(), childSessionID, []event.NewEvent{{Type: v1.EventSessionStatus, Data: data}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case item := <-parent:
		if item.ID == "" || item.SessionID != childSessionID || item.Type != v1.EventSessionStatus || item.Sequence != nil || item.CreatedAt != nil || string(item.Data) != string(data) {
			t.Fatalf("durable projection = %#v", item)
		}
	case <-time.After(time.Second):
		t.Fatalf("did not receive projection of durable event %q", appended[0].ID)
	}
	select {
	case item := <-parent:
		t.Fatalf("duplicate durable projection = %#v", item)
	default:
	}
}

func TestLiveBrokerAssignsIDAndDropsSlowSubscriber(t *testing.T) {
	broker := event.NewBroker(nil, nil, nil)
	events, closeSubscription := broker.Subscribe("ses_test", 1)
	defer closeSubscription()
	broker.PublishProtocol("ses_test", protocol.Event{Type: protocol.EventTextDelta, Text: "one"})
	broker.PublishProtocol("ses_test", protocol.Event{Type: protocol.EventTextDelta, Text: "two"})
	first, ok := <-events
	if !ok || first.ID == "" {
		t.Fatalf("first event = %#v, %v", first, ok)
	}
	payload, err := v1.DecodeEventData(first)
	if err != nil || payload.(*v1.MessagePartDelta).Delta != "one" {
		t.Fatalf("payload = %#v, %v", payload, err)
	}
	if _, ok := <-events; ok {
		t.Fatal("slow subscriber remained connected")
	}
}

func TestLiveBrokerPreservesMessagePartID(t *testing.T) {
	broker := event.NewBroker(nil, nil, nil)
	events, closeSubscription := broker.Subscribe("ses_test", 1)
	defer closeSubscription()
	broker.PublishProtocol("ses_test", protocol.Event{
		Type: protocol.EventReasoningSummaryDelta, MessageID: "msg_test", PartID: "reasoning:1", Text: "Checking tests",
	})

	item := <-events
	payload, err := v1.DecodeEventData(item)
	if err != nil {
		t.Fatal(err)
	}
	delta := payload.(*v1.MessagePartDelta)
	if delta.MessageID != "msg_test" || delta.PartID != "reasoning:1" || delta.Kind != "reasoning_summary" {
		t.Fatalf("payload = %#v", delta)
	}
}

func TestLiveBrokerRejectsUnknownOrInvalidEvents(t *testing.T) {
	broker := event.NewBroker(nil, nil, nil)
	events, closeSubscription := broker.Subscribe("ses_test", 2)
	defer closeSubscription()
	broker.PublishEvent(v1.Event{Type: "unknown", SessionID: "ses_test", Data: json.RawMessage(`{}`)})
	broker.PublishEvent(v1.Event{Type: v1.EventSessionStatus, SessionID: "ses_test", Data: json.RawMessage(`{`)})
	select {
	case item := <-events:
		t.Fatalf("received invalid event %#v", item)
	default:
	}
}

func TestLiveBrokerMapsRunnerNotices(t *testing.T) {
	for _, test := range []struct {
		name, kind, message string
		eventType           protocol.EventType
	}{
		{name: "provider retry", eventType: protocol.EventProviderRetry, kind: "provider_retry", message: "provider fake is overloaded; retrying in 2s (attempt 1)"},
		{name: "status prompt", eventType: protocol.EventStatusPromptInjected, kind: "status_prompt", message: "Status prompt injected"},
		{name: "max turns", eventType: protocol.EventMaxTurnsReached, kind: "max_turns_reached", message: "Maximum turn limit reached (64); producing final response"},
	} {
		t.Run(test.name, func(t *testing.T) {
			broker := event.NewBroker(nil, nil, nil)
			events, closeSubscription := broker.Subscribe("ses_test", 1)
			defer closeSubscription()
			broker.PublishProtocol("ses_test", protocol.Event{Type: test.eventType, Text: test.message})
			item := <-events
			if item.Type != v1.EventSessionStatus {
				t.Fatalf("type = %q", item.Type)
			}
			payload, err := v1.DecodeEventData(item)
			if err != nil {
				t.Fatal(err)
			}
			status := payload.(*v1.SessionStatus)
			if status.Kind != test.kind || status.Message != test.message {
				t.Fatalf("status = %#v", status)
			}
		})
	}
}

func TestLiveBrokerPreservesReasoningSummaryPartID(t *testing.T) {
	broker := event.NewBroker(nil, nil, nil)
	events, closeSubscription := broker.Subscribe("ses_test", 1)
	defer closeSubscription()

	broker.PublishProtocol("ses_test", protocol.Event{
		Type:      protocol.EventReasoningSummaryDelta,
		MessageID: "msg_test",
		PartID:    "reasoning_item:2",
		Text:      "Checking tests",
	})

	item := <-events
	payload, err := v1.DecodeEventData(item)
	if err != nil {
		t.Fatal(err)
	}
	delta := payload.(*v1.MessagePartDelta)
	if delta.MessageID != "msg_test" || delta.PartID != "reasoning_item:2" || delta.Delta != "Checking tests" {
		t.Fatalf("payload = %#v", delta)
	}
}

func TestLiveBrokerPublishesReasoningSummaryDone(t *testing.T) {
	broker := event.NewBroker(nil, nil, nil)
	events, closeSubscription := broker.Subscribe("ses_test", 1)
	defer closeSubscription()

	broker.PublishProtocol("ses_test", protocol.Event{
		Type: protocol.EventReasoningSummaryDone, MessageID: "msg_test", PartID: "reasoning_item:2", Text: "Checking tests",
	})

	item := <-events
	payload, err := v1.DecodeEventData(item)
	if err != nil {
		t.Fatal(err)
	}
	done := payload.(*v1.MessagePartDelta)
	if done.MessageID != "msg_test" || done.PartID != "reasoning_item:2" || done.Kind != "reasoning_summary" || !done.Done || done.Delta != "Checking tests" {
		t.Fatalf("payload = %#v", done)
	}
}

func TestLiveBrokerPublishesToolOutput(t *testing.T) {
	broker := event.NewBroker(nil, nil, nil)
	events, closeSubscription := broker.Subscribe("ses_test", 1)
	defer closeSubscription()
	broker.PublishProtocol("ses_test", protocol.Event{Type: protocol.EventToolOutputDelta, ToolCallID: "call_test", Text: "line\n"})

	payload, err := v1.DecodeEventData(<-events)
	if err != nil {
		t.Fatal(err)
	}
	output := payload.(*v1.ToolOutputDelta)
	if output.ToolCallID != "call_test" || output.Delta != "line\n" {
		t.Fatalf("payload = %#v", output)
	}
}

func TestLiveBrokerPublishesAndProjectsCodeDisplay(t *testing.T) {
	broker := event.NewBroker(nil, nil, fakeHierarchy{"child": "parent"})
	child, closeChild := broker.Subscribe("child", 1)
	defer closeChild()
	parent, closeParent := broker.Subscribe("parent", 1)
	defer closeParent()
	broker.PublishProtocol("child", protocol.Event{Type: protocol.EventCodeDisplay, CodeDisplay: &protocol.CodeDisplay{
		ToolCallID: "call", Source: "package main\n", Path: "main.go", Language: "go", StartLine: 2,
	}})
	for _, item := range []v1.Event{<-child, <-parent} {
		payload, err := v1.DecodeEventData(item)
		if err != nil {
			t.Fatal(err)
		}
		display := payload.(*v1.CodeDisplay)
		if item.Type != v1.EventCodeDisplay || display.ToolCallID != "call" || display.Source != "package main\n" || display.StartLine != 2 {
			t.Fatalf("code display = %#v, payload = %#v", item, display)
		}
	}
}

func TestLiveBrokerDropsToolOutputWithoutClosingSlowSubscriber(t *testing.T) {
	broker := event.NewBroker(nil, nil, nil)
	events, closeSubscription := broker.Subscribe("ses_test", 1)
	defer closeSubscription()
	broker.PublishProtocol("ses_test", protocol.Event{Type: protocol.EventToolOutputDelta, ToolCallID: "call", Text: "first"})
	broker.PublishProtocol("ses_test", protocol.Event{Type: protocol.EventToolOutputDelta, ToolCallID: "call", Text: "dropped"})
	broker.PublishProtocol("ses_test", protocol.Event{Type: protocol.EventFinish})
	if next, ok := <-events; !ok || next.Type != v1.EventSessionStatus {
		t.Fatalf("subscriber was closed after output overflow: %#v, open = %v", next, ok)
	}
}
