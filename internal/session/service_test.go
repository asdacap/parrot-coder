package session_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/amirulashraf/parrot-coder/internal/event"
	"github.com/amirulashraf/parrot-coder/internal/session"
	"github.com/amirulashraf/parrot-coder/internal/store"
)

func TestSessionLifecycleSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "parrot.db")
	db, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	service := session.NewService(db, event.NewRepository(db))
	created, err := service.Create(ctx, session.CreateParams{Title: "durable"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	service = session.NewService(db, event.NewRepository(db))
	got, err := service.Get(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "durable" {
		t.Fatalf("title = %q", got.Title)
	}
	listed, err := service.List(ctx)
	if err != nil || len(listed) != 1 {
		t.Fatalf("List = %#v, %v", listed, err)
	}
	if err := service.Delete(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Get(ctx, created.ID); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("Get after delete error = %v", err)
	}
}

func TestAdmitIsExactlyIdempotent(t *testing.T) {
	ctx, _, repository, service, sessionID := newService(t)
	params := session.AdmitParams{MessageID: "msg_1", Content: "first", Delivery: session.DeliverySteer}
	first, err := service.Admit(ctx, sessionID, params)
	if err != nil || !first.Created {
		t.Fatalf("first Admit = %#v, %v", first, err)
	}
	second, err := service.Admit(ctx, sessionID, params)
	if err != nil || second.Created {
		t.Fatalf("second Admit = %#v, %v", second, err)
	}
	if second.Input.ID != first.Input.ID {
		t.Fatalf("idempotent input ID = %q, want %q", second.Input.ID, first.Input.ID)
	}

	conflicts := []session.AdmitParams{
		{MessageID: "msg_1", Content: "changed", Delivery: session.DeliverySteer},
		{MessageID: "msg_1", Content: "first", Delivery: session.DeliveryQueue},
	}
	for _, conflict := range conflicts {
		if _, err := service.Admit(ctx, sessionID, conflict); !errors.Is(err, session.ErrIdempotencyConflict) {
			t.Fatalf("conflicting Admit error = %v", err)
		}
	}
	events, err := repository.List(ctx, sessionID, -1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Sequence != 0 {
		t.Fatalf("admission events = %#v", events)
	}
}

func TestConcurrentAdmissionCreatesOneInput(t *testing.T) {
	ctx, _, repository, service, sessionID := newService(t)
	params := session.AdmitParams{MessageID: "msg_same", Content: "same", Delivery: session.DeliveryQueue}
	const callers = 16
	var wg sync.WaitGroup
	results := make(chan session.Admission, callers)
	errs := make(chan error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := service.Admit(ctx, sessionID, params)
			results <- result
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	created := 0
	for result := range results {
		if result.Created {
			created++
		}
	}
	if created != 1 {
		t.Fatalf("created admissions = %d, want 1", created)
	}
	events, err := repository.List(ctx, sessionID, -1, callers)
	if err != nil || len(events) != 1 {
		t.Fatalf("events = %#v, %v", events, err)
	}
}

func TestPromotionCutoffQueueAndMessageProjection(t *testing.T) {
	ctx, _, repository, service, sessionID := newService(t)
	inputs := []session.AdmitParams{
		{MessageID: "msg_s1", Content: "steer one", Delivery: session.DeliverySteer},
		{MessageID: "msg_q1", Content: "queue one", Delivery: session.DeliveryQueue},
		{MessageID: "msg_s2", Content: "steer two", Delivery: session.DeliverySteer},
		{MessageID: "msg_q2", Content: "queue two", Delivery: session.DeliveryQueue},
	}
	for _, input := range inputs {
		if _, err := service.Admit(ctx, sessionID, input); err != nil {
			t.Fatal(err)
		}
	}

	messages, err := service.PromoteSteers(ctx, sessionID, 0)
	if err != nil || len(messages) != 1 || messages[0].ID != "msg_s1" {
		t.Fatalf("first steer promotion = %#v, %v", messages, err)
	}
	messages, err = service.PromoteSteers(ctx, sessionID, 3)
	if err != nil || len(messages) != 1 || messages[0].ID != "msg_s2" {
		t.Fatalf("second steer promotion = %#v, %v", messages, err)
	}
	for _, want := range []string{"msg_q1", "msg_q2"} {
		messages, err = service.PromoteNextQueue(ctx, sessionID)
		if err != nil || len(messages) != 1 || messages[0].ID != want {
			t.Fatalf("queue promotion = %#v, %v; want %s", messages, err, want)
		}
	}
	messages, err = service.PromoteNextQueue(ctx, sessionID)
	if err != nil || len(messages) != 0 {
		t.Fatalf("empty queue promotion = %#v, %v", messages, err)
	}

	listed, err := service.ListMessages(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	wantOrder := []string{"msg_s1", "msg_s2", "msg_q1", "msg_q2"}
	if len(listed) != len(wantOrder) {
		t.Fatalf("message count = %d", len(listed))
	}
	for i, want := range wantOrder {
		if listed[i].ID != want || listed[i].Role != "user" {
			t.Fatalf("message[%d] = %#v, want %s", i, listed[i], want)
		}
	}
	events, err := repository.List(ctx, sessionID, -1, 20)
	if err != nil {
		t.Fatal(err)
	}
	for i, item := range events {
		if item.Sequence != int64(i) {
			t.Fatalf("event sequence[%d] = %d", i, item.Sequence)
		}
	}
}

func newService(t *testing.T) (context.Context, *store.DB, *event.Repository, *session.Service, string) {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "parrot.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	repository := event.NewRepository(db)
	service := session.NewService(db, repository)
	created, err := service.Create(ctx, session.CreateParams{Title: "test"})
	if err != nil {
		t.Fatal(err)
	}
	return ctx, db, repository, service, created.ID
}
