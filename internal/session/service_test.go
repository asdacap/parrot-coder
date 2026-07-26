package session_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	v1 "github.com/amirulashraf/parrot-coder/internal/api/v1"
	"github.com/amirulashraf/parrot-coder/internal/event"
	"github.com/amirulashraf/parrot-coder/internal/session"
	"github.com/amirulashraf/parrot-coder/internal/store"
)

func TestSessionLifecycleSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	state := t.TempDir()
	db := store.NewRegistry(state, "host-test")
	service := session.NewService(db, event.NewRepository(db))
	created, err := service.Create(ctx, session.CreateParams{Name: "inspect", Title: "durable"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopening the same state directory stands in for a process restart.
	db = store.NewRegistry(state, "host-test")
	defer db.Close()
	service = session.NewService(db, event.NewRepository(db))
	got, err := service.GetSession(created.ID).Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "durable" || got.Name != "inspect" {
		t.Fatalf("session = %#v", got)
	}
	listed, err := service.List(ctx)
	if err != nil || len(listed) != 1 || listed[0].Name != "inspect" {
		t.Fatalf("List = %#v, %v", listed, err)
	}
	if err := service.GetSession(created.ID).Delete(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := service.GetSession(created.ID).Get(ctx); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("Get after delete error = %v", err)
	}
}

func TestBoundSessionsKeepOperationsIsolated(t *testing.T) {
	ctx := context.Background()
	registry := store.NewRegistry(t.TempDir(), "host-test")
	defer registry.Close()
	service := session.NewService(registry, event.NewRepository(registry))
	first, err := service.Create(ctx, session.CreateParams{Name: "first"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Create(ctx, session.CreateParams{Name: "second"})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		bound session.AgentSessionStore
		id    string
		name  string
		text  string
		msgID string
	}{
		{service.GetSession(first.ID), first.ID, "first", "first prompt", "msg_first"},
		{service.GetSession(second.ID), second.ID, "second", "second prompt", "msg_second"},
	} {
		admission, err := test.bound.Admit(ctx, session.AdmitParams{MessageID: test.msgID, Content: test.text, Delivery: session.DeliverySteer})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := test.bound.PromoteSteers(ctx, admission.Input.AdmittedSequence); err != nil {
			t.Fatal(err)
		}
		got, err := test.bound.Get(ctx)
		if err != nil {
			t.Fatal(err)
		}
		messages, err := test.bound.ListMessages(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if got.ID != test.id || got.Name != test.name || len(messages) != 1 || messages[0].SessionID != test.id || messages[0].Content != test.text {
			t.Fatalf("bound session = %#v, messages = %#v", got, messages)
		}
	}
}

func TestRootSessionNamesAreGeneratedWithoutOverridingExplicitOrChildNames(t *testing.T) {
	ctx := context.Background()
	registry := store.NewRegistry(t.TempDir(), "host-test")
	defer registry.Close()
	service := session.NewService(registry, event.NewRepository(registry))

	generated, err := service.Create(ctx, session.CreateParams{ProjectID: "project", Title: "generated"})
	if err != nil {
		t.Fatal(err)
	}
	explicit, err := service.Create(ctx, session.CreateParams{Name: "main-task", Title: "explicit"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := service.Create(ctx, session.CreateParams{ParentSessionID: generated.ID, ProjectID: "project", Title: "child"})
	if err != nil {
		t.Fatal(err)
	}

	loaded, err := service.GetSession(generated.ID).Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if generated.Name == "" || loaded.Name != generated.Name || explicit.Name != "main-task" || child.Name != "" {
		t.Fatalf("session names: generated=%q loaded=%q explicit=%q child=%q", generated.Name, loaded.Name, explicit.Name, child.Name)
	}
}

func TestParentSessionPersistsAndRequiresSameProject(t *testing.T) {
	ctx := context.Background()
	state := t.TempDir()
	registry := store.NewRegistry(state, "host-test")
	service := session.NewService(registry, event.NewRepository(registry))
	parent, err := service.Create(ctx, session.CreateParams{ProjectID: "project", Title: "parent"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := service.Create(ctx, session.CreateParams{ParentSessionID: parent.ID, ProjectID: "project", Title: "child"})
	if err != nil || child.ParentSessionID != parent.ID {
		t.Fatalf("Create child = %#v, %v", child, err)
	}
	if _, err := service.Create(ctx, session.CreateParams{ParentSessionID: "ses_missing", ProjectID: "project"}); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("missing parent error = %v", err)
	}
	if _, err := service.Create(ctx, session.CreateParams{ParentSessionID: parent.ID, ProjectID: "other"}); !errors.Is(err, session.ErrParentProjectMismatch) {
		t.Fatalf("different-project parent error = %v", err)
	}
	if err := registry.Close(); err != nil {
		t.Fatal(err)
	}

	registry = store.NewRegistry(state, "host-test")
	defer registry.Close()
	service = session.NewService(registry, event.NewRepository(registry))
	loaded, err := service.GetSession(child.ID).Get(ctx)
	if err != nil || loaded.ParentSessionID != parent.ID {
		t.Fatalf("reopened child = %#v, %v", loaded, err)
	}
	listed, err := service.List(ctx)
	if err != nil || len(listed) != 2 {
		t.Fatalf("List = %#v, %v", listed, err)
	}
	for _, item := range listed {
		if item.ID == child.ID && item.ParentSessionID != parent.ID {
			t.Fatalf("listed child = %#v", item)
		}
	}
}

func TestCreateSelectedPersistsCompleteInitialSelection(t *testing.T) {
	ctx, _, _, service, _ := newService(t)
	created, err := service.CreateSelected(ctx, session.CreateParams{Title: "selected"}, session.Selection{Agent: "build", Model: "local/code"})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := service.GetSession(created.ID).Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Agent != "build" || loaded.Model != "local/code" {
		t.Fatalf("selection = %#v", loaded)
	}
}

func TestLatestSelectionUsesCurrentProject(t *testing.T) {
	// Projects are no longer rows: project.StableID is a pure function of the
	// repository identity, so a session records its project and every host
	// recomputes the same value.
	ctx, _, _, service, _ := newService(t)
	if _, err := service.CreateSelected(ctx, session.CreateParams{ProjectID: "other", Title: "other"}, session.Selection{Agent: "build", Model: "other/newer"}); err != nil {
		t.Fatal(err)
	}
	want := session.Selection{Agent: "plan", Model: "local/code"}
	if _, err := service.CreateSelected(ctx, session.CreateParams{ProjectID: "project", Title: "selected"}, want); err != nil {
		t.Fatal(err)
	}
	got, err := service.LatestSelection(ctx, "project")
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("LatestSelection() = %#v, want %#v", got, want)
	}
	if _, err := service.LatestSelection(ctx, "missing"); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("missing LatestSelection() error = %v", err)
	}
}

func TestConcurrentPartialSelectionCarriesForwardCurrentValues(t *testing.T) {
	ctx, _, _, service, _ := newService(t)
	created, err := service.CreateSelected(ctx, session.CreateParams{Title: "selected"}, session.Selection{Agent: "build", Model: "local/code"})
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, patch := range []session.SelectionPatch{{Agent: "plan"}, {Model: "reasoning"}} {
		wg.Add(1)
		go func(patch session.SelectionPatch) {
			defer wg.Done()
			<-start
			_, err := service.GetSession(created.ID).UpdateSelection(ctx, patch, nil)
			errs <- err
		}(patch)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	loaded, err := service.GetSession(created.ID).Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Agent != "plan" || loaded.Model != "reasoning" {
		t.Fatalf("selection = %#v", loaded)
	}
}

func TestAdmitIsExactlyIdempotent(t *testing.T) {
	ctx, _, repository, service, sessionID := newService(t)
	params := session.AdmitParams{MessageID: "msg_1", Content: "first", Delivery: session.DeliverySteer}
	first, err := service.GetSession(sessionID).Admit(ctx, params)
	if err != nil || !first.Created {
		t.Fatalf("first Admit = %#v, %v", first, err)
	}
	second, err := service.GetSession(sessionID).Admit(ctx, params)
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
		if _, err := service.GetSession(sessionID).Admit(ctx, conflict); !errors.Is(err, session.ErrIdempotencyConflict) {
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

func TestSessionInputEventPayloads(t *testing.T) {
	ctx, _, repository, service, sessionID := newService(t)
	admission, err := service.GetSession(sessionID).Admit(ctx, session.AdmitParams{
		MessageID: "msg_payload",
		Content:   "payload content",
		Delivery:  session.DeliveryQueue,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.GetSession(sessionID).PromoteNextQueue(ctx); err != nil {
		t.Fatal(err)
	}

	events, err := repository.List(ctx, sessionID, -1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("event count = %d, want 2", len(events))
	}

	admitted, err := v1.DecodeEventData(v1.Event{Type: events[0].Type, Data: events[0].Data})
	if err != nil {
		t.Fatal(err)
	}
	wantAdmitted := &v1.SessionInputAdmitted{
		InputID:   admission.Input.ID,
		MessageID: "msg_payload",
		Content:   "payload content",
		Delivery:  "queue",
	}
	if admittedPayload, ok := admitted.(*v1.SessionInputAdmitted); !ok || *admittedPayload != *wantAdmitted {
		t.Fatalf("admitted payload = %#v, want %#v", admitted, wantAdmitted)
	}

	promoted, err := v1.DecodeEventData(v1.Event{Type: events[1].Type, Data: events[1].Data})
	if err != nil {
		t.Fatal(err)
	}
	wantPromoted := &v1.SessionInputPromoted{InputID: admission.Input.ID, MessageID: "msg_payload"}
	if promotedPayload, ok := promoted.(*v1.SessionInputPromoted); !ok || *promotedPayload != *wantPromoted {
		t.Fatalf("promoted payload = %#v, want %#v", promoted, wantPromoted)
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
			result, err := service.GetSession(sessionID).Admit(ctx, params)
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
		if _, err := service.GetSession(sessionID).Admit(ctx, input); err != nil {
			t.Fatal(err)
		}
	}

	messages, err := service.GetSession(sessionID).PromoteSteers(ctx, 0)
	if err != nil || len(messages) != 1 || messages[0].ID != "msg_s1" {
		t.Fatalf("first steer promotion = %#v, %v", messages, err)
	}
	messages, err = service.GetSession(sessionID).PromoteSteers(ctx, 3)
	if err != nil || len(messages) != 1 || messages[0].ID != "msg_s2" {
		t.Fatalf("second steer promotion = %#v, %v", messages, err)
	}
	for _, want := range []string{"msg_q1", "msg_q2"} {
		messages, err = service.GetSession(sessionID).PromoteNextQueue(ctx)
		if err != nil || len(messages) != 1 || messages[0].ID != want {
			t.Fatalf("queue promotion = %#v, %v; want %s", messages, err, want)
		}
	}
	messages, err = service.GetSession(sessionID).PromoteNextQueue(ctx)
	if err != nil || len(messages) != 0 {
		t.Fatalf("empty queue promotion = %#v, %v", messages, err)
	}

	listed, err := service.GetSession(sessionID).ListMessages(ctx)
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

func TestTodosOrderedReplacementValidationAndConcurrency(t *testing.T) {
	ctx, db, repository, _, sessionID := newService(t)
	service := session.NewTodoService(db, repository)
	items, err := service.Replace(ctx, sessionID, []session.Todo{
		{Content: "second", Status: session.TodoInProgress, Priority: session.TodoHigh},
		{Content: "first", Status: session.TodoPending, Priority: session.TodoLow},
	})
	if err != nil || len(items) != 2 || items[0].Position != 0 || items[1].Position != 1 {
		t.Fatalf("replace = %#v, %v", items, err)
	}
	listed, err := service.List(ctx, sessionID)
	if err != nil || listed[0].Content != "second" || listed[1].Content != "first" {
		t.Fatalf("list = %#v, %v", listed, err)
	}
	events, err := repository.List(ctx, sessionID, -1, 20)
	if err != nil || len(events) == 0 || events[len(events)-1].Type != session.EventTodoUpdated {
		t.Fatalf("todo event = %#v, %v", events, err)
	}
	if _, err := service.Replace(ctx, sessionID, []session.Todo{{Content: "bad", Status: "unknown", Priority: session.TodoLow}}); err == nil {
		t.Fatal("invalid status accepted")
	}
	if got, _ := service.List(ctx, sessionID); len(got) != 2 {
		t.Fatal("invalid replacement changed existing todos")
	}

	const writers = 8
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := service.Replace(ctx, sessionID, []session.Todo{{Content: "concurrent", Status: session.TodoCompleted, Priority: session.TodoMedium}})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	listed, err = service.List(ctx, sessionID)
	if err != nil || len(listed) != 1 || listed[0].Position != 0 {
		t.Fatalf("concurrent list = %#v, %v", listed, err)
	}
}

func TestTodoListIsEmptyArrayAndWhitespaceContentIsRejected(t *testing.T) {
	ctx, db, _, _, sessionID := newService(t)
	service := session.NewTodoService(db)
	items, err := service.List(ctx, sessionID)
	if err != nil || items == nil || len(items) != 0 {
		t.Fatalf("empty list = %#v, %v", items, err)
	}
	if _, err := service.Replace(ctx, sessionID, []session.Todo{{Content: "  ", Status: session.TodoPending, Priority: session.TodoLow}}); err == nil {
		t.Fatal("whitespace-only content accepted")
	}
}

func newService(t *testing.T) (context.Context, *store.Registry, *event.Repository, *session.Service, string) {
	t.Helper()
	ctx := context.Background()
	db := store.NewRegistry(t.TempDir(), "host-test")
	t.Cleanup(func() { db.Close() })
	repository := event.NewRepository(db)
	service := session.NewService(db, repository)
	created, err := service.Create(ctx, session.CreateParams{Title: "test"})
	if err != nil {
		t.Fatal(err)
	}
	return ctx, db, repository, service, created.ID
}

func TestInteractiveClaimLifecycle(t *testing.T) {
	ctx := context.Background()
	db := store.NewRegistry(t.TempDir(), "host-test")
	defer db.Close()
	service := session.NewService(db, event.NewRepository(db))
	selection := session.Selection{Agent: "build", Model: "local/code"}
	owner := session.InteractiveOwner{WorkingDirectory: "/workspace", HostKey: "host", PID: 101}

	first, err := service.ClaimInteractive(ctx, owner, session.CreateParams{Title: "first"}, selection, false, func(int) bool { return false })
	if err != nil || first.Disposition != session.ClaimCreated {
		t.Fatalf("first claim = %#v, %v", first, err)
	}
	retry, err := service.ClaimInteractive(ctx, owner, session.CreateParams{}, selection, false, func(int) bool { return true })
	if err != nil || retry.Session.ID != first.Session.ID || retry.Disposition != session.ClaimExisting {
		t.Fatalf("retry = %#v, %v", retry, err)
	}

	reclaimed, err := service.ClaimInteractive(ctx, session.InteractiveOwner{WorkingDirectory: "/workspace", HostKey: "host", PID: 202}, session.CreateParams{}, selection, false, func(pid int) bool { return pid != 101 })
	if err != nil || reclaimed.Session.ID != first.Session.ID || reclaimed.Disposition != session.ClaimReclaimed {
		t.Fatalf("reclaimed = %#v, %v", reclaimed, err)
	}

	cleared, err := service.ClaimInteractive(ctx, session.InteractiveOwner{WorkingDirectory: "/workspace", HostKey: "host", PID: 202}, session.CreateParams{Title: "fresh"}, selection, true, func(int) bool { return true })
	if err != nil || cleared.Session.ID == first.Session.ID || cleared.Disposition != session.ClaimCreated {
		t.Fatalf("clear = %#v, %v", cleared, err)
	}
	items, err := service.List(ctx)
	if err != nil || len(items) != 2 {
		t.Fatalf("sessions = %#v, %v", items, err)
	}
}

func TestInteractiveClaimDoesNotStealLiveOwner(t *testing.T) {
	ctx := context.Background()
	db := store.NewRegistry(t.TempDir(), "host-test")
	defer db.Close()
	service := session.NewService(db, event.NewRepository(db))
	selection := session.Selection{Agent: "build", Model: "local/code"}
	first, err := service.ClaimInteractive(ctx, session.InteractiveOwner{WorkingDirectory: "/workspace", HostKey: "host", PID: 101}, session.CreateParams{}, selection, false, func(int) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.ClaimInteractive(ctx, session.InteractiveOwner{WorkingDirectory: "/workspace", HostKey: "host", PID: 202}, session.CreateParams{}, selection, false, func(int) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if second.Session.ID == first.Session.ID || second.Disposition != session.ClaimCreated {
		t.Fatalf("live owner was stolen: first=%#v second=%#v", first, second)
	}
}
