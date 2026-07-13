package question

import (
	"context"
	"errors"
	"testing"
	"time"
)

func testRequest() Request {
	return Request{SessionID: "session", Questions: []Question{
		{ID: "single", Prompt: "Pick", Options: []Option{{ID: "a", Label: "A"}, {ID: "b", Label: "B"}}, Custom: true},
		{ID: "multi", Prompt: "Pick many", Options: []Option{{ID: "x", Label: "X"}, {ID: "y", Label: "Y"}}, Multiple: true},
	}}
}

func waitPending(t *testing.T, broker *Broker) Pending {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if pending := broker.Pending(); len(pending) == 1 {
			return pending[0]
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("question did not become pending")
	return Pending{}
}

func TestChoicesAndSingleReply(t *testing.T) {
	broker := NewBroker(nil)
	done := make(chan error, 1)
	go func() {
		response, err := broker.Ask(context.Background(), testRequest())
		if err == nil && (len(response.Answers) != 2 || response.Answers[0].Custom != "other") {
			err = errors.New("unexpected response")
		}
		done <- err
	}()
	pending := waitPending(t, broker)
	response := Response{Answers: []Answer{{QuestionID: "single", Custom: "other"}, {QuestionID: "multi", OptionIDs: []string{"x", "y"}}}}
	if err := broker.Reply(pending.ID, response); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := broker.Reply(pending.ID, response); err == nil {
		t.Fatal("second reply accepted")
	}
}

func TestRejectAndContextCancellation(t *testing.T) {
	broker := NewBroker(nil)
	done := make(chan error, 1)
	go func() { _, err := broker.Ask(context.Background(), testRequest()); done <- err }()
	pending := waitPending(t, broker)
	if err := broker.Reject(pending.ID); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, ErrRejected) {
		t.Fatalf("reject error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _, err := broker.Ask(ctx, testRequest()); done <- err }()
	pending = waitPending(t, broker)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error = %v", err)
	}
	if err := broker.Reject(pending.ID); err == nil {
		t.Fatal("cancelled request remained pending")
	}
}

func TestInvalidChoiceDoesNotSettle(t *testing.T) {
	broker := NewBroker(nil)
	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _, err := broker.Ask(ctx, testRequest()); done <- err }()
	pending := waitPending(t, broker)
	if err := broker.Reply(pending.ID, Response{Answers: []Answer{{QuestionID: "single", OptionIDs: []string{"unknown"}}, {QuestionID: "multi", OptionIDs: []string{"x"}}}}); err == nil {
		t.Fatal("unknown choice accepted")
	}
	if len(broker.Pending()) != 1 {
		t.Fatal("invalid reply settled request")
	}
}
