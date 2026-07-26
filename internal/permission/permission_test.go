package permission

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func request(t *testing.T) Request {
	t.Helper()
	r, err := NewRequest("read", json.RawMessage(`{"path":"/x"}`))
	if err != nil {
		t.Fatal(err)
	}
	r.SessionID, r.Workspace = "s", "/w"
	return r
}

func TestNewRequestRequiresToolID(t *testing.T) {
	if _, err := NewRequest("", nil); err == nil {
		t.Fatal("empty tool ID accepted")
	}
}

// waitForPending blocks until the broker holds want pending requests.
func waitForPending(t *testing.T, b *Broker, want int) []Pending {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for len(b.Pending()) != want && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	items := b.Pending()
	if len(items) != want {
		t.Fatalf("pending = %d, want %d", len(items), want)
	}
	return items
}

func TestBrokerCancellationAndSingleUse(t *testing.T) {
	b := NewBroker(false, nil, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := b.Authorize(ctx, request(t)); done <- err }()
	p := waitForPending(t, b, 1)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	if err := b.ReplyOnce(p[0].ID); err == nil {
		t.Fatal("canceled ID was reusable")
	}
}

func TestNoninteractiveDenies(t *testing.T) {
	b := NewBroker(true, nil, time.Second)
	if d, err := b.Authorize(context.Background(), request(t)); err != nil || d != Deny {
		t.Fatalf("%v %v", d, err)
	}
	if pending := b.Pending(); len(pending) != 0 {
		t.Fatalf("pending = %d, want 0", len(pending))
	}
}

// Each reply settles exactly the request which raised it: nothing is
// remembered, so an identical second request prompts again.
func TestRepliesAreNotRemembered(t *testing.T) {
	for _, test := range []struct {
		name  string
		reply func(*Broker, string) error
		want  Decision
	}{
		{name: "allow", reply: (*Broker).ReplyOnce, want: Allow},
		{name: "deny", reply: (*Broker).Reject, want: Deny},
	} {
		t.Run(test.name, func(t *testing.T) {
			b := NewBroker(false, nil, time.Second)
			for round := range 2 {
				done := make(chan Decision, 1)
				go func() { d, _ := b.Authorize(context.Background(), request(t)); done <- d }()
				items := waitForPending(t, b, 1)
				if err := test.reply(b, items[0].ID); err != nil {
					t.Fatal(err)
				}
				if d := <-done; d != test.want {
					t.Fatalf("round %d decision = %q, want %q", round, d, test.want)
				}
			}
		})
	}
}

func TestRejectWithReasonReturnsToolError(t *testing.T) {
	b := NewBroker(false, nil, time.Second)
	done := make(chan error, 1)
	go func() { _, err := b.Authorize(context.Background(), request(t)); done <- err }()
	items := waitForPending(t, b, 1)
	if err := b.RejectWithReason(items[0].ID, "use the project cache instead"); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil || err.Error() != "use the project cache instead" {
		t.Fatalf("authorization error = %v", err)
	}
}

func TestReplyRejectsUnknownIDAndInvalidDecision(t *testing.T) {
	b := NewBroker(false, nil, time.Second)
	if err := b.ReplyOnce("missing"); err == nil {
		t.Fatal("unknown request ID accepted")
	}
	done := make(chan Decision, 1)
	go func() { d, _ := b.Authorize(context.Background(), request(t)); done <- d }()
	items := waitForPending(t, b, 1)
	if err := b.reply(items[0].ID, Reply{Decision: "maybe"}); err == nil {
		t.Fatal("invalid decision accepted")
	}
	if err := b.Reject(items[0].ID); err != nil {
		t.Fatal(err)
	}
	<-done
}

func TestBrokerTimeoutDeniesCleansUpAndRejectsLateReply(t *testing.T) {
	b := NewBroker(false, nil, 100*time.Millisecond)
	done := make(chan struct {
		decision Decision
		err      error
	}, 1)
	go func() {
		decision, err := b.Authorize(context.Background(), request(t))
		done <- struct {
			decision Decision
			err      error
		}{decision, err}
	}()
	pending := waitForPending(t, b, 1)[0]
	result := <-done
	if result.decision != Deny || !errors.Is(result.err, ErrRequestTimeout) {
		t.Fatalf("Authorize = (%q, %v), want deny request timeout", result.decision, result.err)
	}
	if len(b.Pending()) != 0 {
		t.Fatalf("pending = %#v, want empty", b.Pending())
	}
	if err := b.ReplyOnce(pending.ID); err == nil {
		t.Fatal("timed-out request accepted a late reply")
	}
}
