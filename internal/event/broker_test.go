package event_test

import (
	"encoding/json"
	"testing"

	v1 "github.com/amirulashraf/parrot-coder/internal/api/v1"
	"github.com/amirulashraf/parrot-coder/internal/event"
	"github.com/amirulashraf/parrot-coder/internal/protocol"
)

func TestLiveBrokerAssignsIDAndDropsSlowSubscriber(t *testing.T) {
	broker := event.NewBroker()
	events, closeSubscription := broker.Subscribe("ses_test", 1)
	defer closeSubscription()
	broker.Publish("ses_test", protocol.Event{Type: protocol.EventTextDelta, Text: "one"})
	broker.Publish("ses_test", protocol.Event{Type: protocol.EventTextDelta, Text: "two"})
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
	broker := event.NewBroker()
	events, closeSubscription := broker.Subscribe("ses_test", 1)
	defer closeSubscription()
	broker.Publish("ses_test", protocol.Event{
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
	broker := event.NewBroker()
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

func TestLiveBrokerPreservesReasoningSummaryPartID(t *testing.T) {
	broker := event.NewBroker()
	events, closeSubscription := broker.Subscribe("ses_test", 1)
	defer closeSubscription()

	broker.Publish("ses_test", protocol.Event{
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
