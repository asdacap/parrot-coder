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
	r, err := NewRequest("read", json.RawMessage(`{"b":2,"a":1}`), []Resource{{Kind: "file", Identifier: "/x", Operation: "read"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.SessionID, r.Workspace = "s", "/w"
	return r
}

func TestCanonicalHashAndTamper(t *testing.T) {
	a := request(t)
	b, _ := NewRequest("read", json.RawMessage(`{ "a": 1, "b": 2 }`), a.Resources, nil)
	if a.OperationHash != b.OperationHash {
		t.Fatal("equivalent JSON produced different hashes")
	}
	a.CanonicalInput = json.RawMessage(`{"a":3}`)
	if a.Verify() == nil {
		t.Fatal("tampered request verified")
	}
	a = request(t)
	a.Description = "different request description"
	if a.Verify() == nil {
		t.Fatal("tampered request description verified")
	}
}

func TestBrokerCancellationAndSingleUse(t *testing.T) {
	b := NewBroker(Policy{Default: Ask}, false, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := b.Authorize(ctx, request(t)); done <- err }()
	deadline := time.Now().Add(time.Second)
	for len(b.Pending()) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	p := b.Pending()
	if len(p) != 1 {
		t.Fatal("request did not become pending")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	if err := b.ReplyOnce(p[0].ID); err == nil {
		t.Fatal("canceled ID was reusable")
	}
}

func TestHardDenyDominatesRememberedGrant(t *testing.T) {
	hard := false
	policy := Policy{Default: Ask, Rules: []Rule{{Match: func(Request) bool { return hard }, Decision: Deny, HardDeny: true}}}
	b := NewBroker(policy, false, nil)
	r := request(t)
	done := make(chan Decision, 1)
	go func() { d, _ := b.Authorize(context.Background(), r); done <- d }()
	for len(b.Pending()) == 0 {
		time.Sleep(time.Millisecond)
	}
	if err := b.ReplySession(b.Pending()[0].ID); err != nil {
		t.Fatal(err)
	}
	if d := <-done; d != Allow {
		t.Fatal(d)
	}
	hard = true
	if d, err := b.Authorize(context.Background(), r); err != nil || d != Deny {
		t.Fatalf("%v %v", d, err)
	}
}

func TestNoninteractiveAskDenies(t *testing.T) {
	b := NewBroker(Policy{Default: Ask}, true, nil)
	if d, err := b.Authorize(context.Background(), request(t)); err != nil || d != Deny {
		t.Fatalf("%v %v", d, err)
	}
}

func TestEnableYoloAllowsSessionAndSettlesPending(t *testing.T) {
	b := NewBroker(Policy{Default: Ask}, false, nil)
	first := request(t)
	second := request(t)
	second.CanonicalInput = json.RawMessage(`{"a":1,"b":3}`)
	second.OperationHash, _ = Hash(second)
	results := make(chan Decision, 2)
	go func() { decision, _ := b.Authorize(context.Background(), first); results <- decision }()
	go func() { decision, _ := b.Authorize(context.Background(), second); results <- decision }()
	deadline := time.Now().Add(time.Second)
	for len(b.Pending()) != 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	items := b.Pending()
	if len(items) != 2 {
		t.Fatalf("pending = %d, want 2", len(items))
	}
	if err := b.EnableYolo(items[0].ID); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if decision := <-results; decision != Allow {
			t.Fatalf("pending decision = %q", decision)
		}
	}
	if pending := b.Pending(); len(pending) != 0 {
		t.Fatalf("pending after YOLO = %d", len(pending))
	}

	b.policy.Rules = []Rule{{Match: func(Request) bool { return true }, Decision: Deny, HardDeny: true}}
	if decision, err := b.Authorize(context.Background(), request(t)); err != nil || decision != Allow {
		t.Fatalf("YOLO session decision = %q, err = %v", decision, err)
	}
	other := request(t)
	other.SessionID = "other"
	if decision, err := b.Authorize(context.Background(), other); err != nil || decision != Deny {
		t.Fatalf("other session decision = %q, err = %v", decision, err)
	}
}
