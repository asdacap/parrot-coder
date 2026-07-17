package systemcontext

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/amirulashraf/parrot-coder/internal/event"
	"github.com/amirulashraf/parrot-coder/internal/session"
	"github.com/amirulashraf/parrot-coder/internal/store"
)

type testSource struct {
	key         string
	observation Observation
	err         error
}

func (s *testSource) Key() string { return s.key }
func (s *testSource) Observe(context.Context) (Observation, error) {
	return s.observation, s.err
}

func observed(value, baseline, update, removal string) Observation {
	raw, _ := json.Marshal(value)
	return Observation{Available: true, Value: raw, Baseline: baseline, Update: update, Removal: removal}
}

type memoryEpochStore struct {
	epoch           session.ContextEpoch
	initializations int
	reconciliations int
	lastText        string
}

func (s *memoryEpochStore) CurrentContextEpoch(context.Context, string) (session.ContextEpoch, error) {
	if s.epoch.ID == "" {
		return session.ContextEpoch{}, session.ErrNotFound
	}
	return s.epoch, nil
}

func (s *memoryEpochStore) InitializeContext(_ context.Context, sessionID, baseline string, sources json.RawMessage, cutoff int64) (session.ContextEpoch, error) {
	s.initializations++
	s.epoch = session.ContextEpoch{ID: "ctx", SessionID: sessionID, Baseline: baseline, Sources: append(json.RawMessage(nil), sources...), HistoryCutoff: cutoff}
	return s.epoch, nil
}

func (s *memoryEpochStore) ReconcileContext(_ context.Context, _ string, text string, sources json.RawMessage) error {
	s.reconciliations++
	s.lastText = text
	s.epoch.Sources = append(json.RawMessage(nil), sources...)
	return nil
}

func (s *memoryEpochStore) ReplaceContext(_ context.Context, sessionID, baseline string, sources json.RawMessage, cutoff int64) (session.ContextEpoch, error) {
	s.epoch = session.ContextEpoch{ID: "replacement", SessionID: sessionID, Baseline: baseline, Sources: append(json.RawMessage(nil), sources...), HistoryCutoff: cutoff}
	return s.epoch, nil
}

func TestRegistryRejectsDuplicatesAndRendersStableKeyOrder(t *testing.T) {
	z := &testSource{key: "test:z", observation: observed("z", "Z", "Z update", "Z removed")}
	a := &testSource{key: "test:a", observation: observed("a", "A", "A update", "A removed")}
	registry, err := NewRegistry(z, a)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(a); err == nil {
		t.Fatal("duplicate source was accepted")
	}
	store := &memoryEpochStore{}
	epoch, err := (Manager{Registry: registry, Store: store}).Initialize(context.Background(), "session", 7)
	if err != nil {
		t.Fatal(err)
	}
	if epoch.Baseline != "A\n\nZ" {
		t.Fatalf("baseline = %q", epoch.Baseline)
	}
	if strings.Contains(string(epoch.Sources), "baseline") || strings.Contains(string(epoch.Sources), "update") {
		t.Fatalf("snapshot persisted render-only fields: %s", epoch.Sources)
	}
	if !strings.Contains(string(epoch.Sources), `"removal":"A removed"`) {
		t.Fatalf("snapshot omitted removal renderer: %s", epoch.Sources)
	}
}

func TestInitializeRequiresEveryRegisteredSource(t *testing.T) {
	tests := []struct {
		name string
		src  *testSource
	}{
		{"unavailable", &testSource{key: "test:bad", observation: Observation{Available: false}}},
		{"error", &testSource{key: "test:bad", observation: observed("partial", "partial", "", ""), err: errors.New("offline")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry, err := NewRegistry(
				&testSource{key: "test:good", observation: observed("good", "good", "", "")},
				tt.src,
			)
			if err != nil {
				t.Fatal(err)
			}
			store := &memoryEpochStore{}
			if _, err := (Manager{Registry: registry, Store: store}).Initialize(context.Background(), "session", 0); err == nil {
				t.Fatal("Initialize succeeded")
			}
			if store.initializations != 0 || store.epoch.ID != "" {
				t.Fatal("partial baseline was persisted")
			}
		})
	}
}

func TestReconcileChangeNewUnavailableRemovalAndNoop(t *testing.T) {
	ctx := context.Background()
	a := &testSource{key: "test:a", observation: observed("one", "A baseline", "A update", "A old removal")}
	registry, _ := NewRegistry(a)
	store := &memoryEpochStore{}
	manager := Manager{Registry: registry, Store: store}
	if _, err := manager.Initialize(ctx, "session", 0); err != nil {
		t.Fatal(err)
	}

	if _, err := manager.Reconcile(ctx, "session"); err != nil {
		t.Fatal(err)
	}
	if store.reconciliations != 0 {
		t.Fatal("unchanged snapshot was persisted")
	}

	a.observation = observed("two", "ignored", "A update", "A new removal")
	b := &testSource{key: "test:b", observation: observed("new", "B baseline", "B update", "B removed")}
	registry, _ = NewRegistry(b, a)
	manager.Registry = registry
	if _, err := manager.Reconcile(ctx, "session"); err != nil {
		t.Fatal(err)
	}
	if store.lastText != "A update\n\nB baseline" {
		t.Fatalf("change text = %q", store.lastText)
	}

	before := append(json.RawMessage(nil), store.epoch.Sources...)
	a.observation = Observation{Available: false}
	a.err = errors.New("temporarily unavailable")
	b.observation = observed("newer", "ignored", "B second update", "B removed")
	if epoch, err := manager.Reconcile(ctx, "session"); err == nil || epoch.ID == "" {
		t.Fatalf("unavailable reconcile = %#v, %v", epoch, err)
	}
	var oldSnapshot, nextSnapshot Snapshot
	if err := json.Unmarshal(before, &oldSnapshot); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(store.epoch.Sources, &nextSnapshot); err != nil {
		t.Fatal(err)
	}
	if !jsonEqual(oldSnapshot["test:a"].Value, nextSnapshot["test:a"].Value) {
		t.Fatalf("unavailable source was not stale-retained: %s", store.epoch.Sources)
	}
	if store.lastText != "B second update" {
		t.Fatalf("available source update text = %q", store.lastText)
	}

	registry, _ = NewRegistry(b)
	manager.Registry = registry
	if _, err := manager.Reconcile(ctx, "session"); err != nil {
		t.Fatal(err)
	}
	if store.lastText != "A new removal" {
		t.Fatalf("registry removal text = %q", store.lastText)
	}
	if strings.Contains(string(store.epoch.Sources), "test:a") {
		t.Fatalf("removed source retained: %s", store.epoch.Sources)
	}
}

func TestExplicitRemovalUsesPreviouslyStoredRenderer(t *testing.T) {
	ctx := context.Background()
	source := &testSource{key: "test:file", observation: observed("present", "present", "changed", "previous removal")}
	registry, _ := NewRegistry(source)
	store := &memoryEpochStore{}
	manager := Manager{Registry: registry, Store: store}
	if _, err := manager.Initialize(ctx, "session", 0); err != nil {
		t.Fatal(err)
	}
	source.observation = Observation{Available: true, Removal: "current removal"}
	if _, err := manager.Reconcile(ctx, "session"); err != nil {
		t.Fatal(err)
	}
	if store.lastText != "previous removal" {
		t.Fatalf("removal text = %q", store.lastText)
	}
}

func TestReconcileCommitsMessageAndSnapshotAtomically(t *testing.T) {
	ctx := context.Background()
	db := store.NewRegistry(t.TempDir(), "host-test")
	t.Cleanup(func() { db.Close() })
	repository := event.NewRepository(db)
	sessions := session.NewService(db, repository)
	created, err := sessions.Create(ctx, session.CreateParams{Title: "atomic"})
	if err != nil {
		t.Fatal(err)
	}
	source := &testSource{key: "test:value", observation: observed("one", "one", "two update", "removed")}
	registry, _ := NewRegistry(source)
	manager := Manager{Registry: registry, Store: sessions}
	initial, err := manager.Initialize(ctx, created.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	eventsBefore, _ := repository.List(ctx, created.ID, -1, 100)
	if _, err := manager.Reconcile(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	eventsAfterNoop, _ := repository.List(ctx, created.ID, -1, 100)
	if len(eventsAfterNoop) != len(eventsBefore) {
		t.Fatalf("unchanged snapshot appended an event: %d -> %d", len(eventsBefore), len(eventsAfterNoop))
	}

	sessionDB, err := db.Session(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sessionDB.SQL().ExecContext(ctx, `CREATE TRIGGER reject_context_update BEFORE UPDATE ON session_context_epoch BEGIN SELECT RAISE(ABORT, 'reject'); END`); err != nil {
		t.Fatal(err)
	}
	source.observation = observed("two", "two", "two update", "removed")
	if _, err := manager.Reconcile(ctx, created.ID); err == nil {
		t.Fatal("Reconcile succeeded despite projection failure")
	}
	current, err := sessions.CurrentContextEpoch(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !jsonEqual(current.Sources, initial.Sources) {
		t.Fatalf("snapshot advanced without message: %s", current.Sources)
	}
	messages, _ := sessions.ListMessages(ctx, created.ID)
	eventsAfter, _ := repository.List(ctx, created.ID, -1, 100)
	if len(messages) != 0 || len(eventsAfter) != len(eventsBefore) {
		t.Fatalf("failed transaction leaked message/event: messages=%d events=%d/%d", len(messages), len(eventsAfter), len(eventsBefore))
	}
	if _, err := sessionDB.SQL().ExecContext(ctx, `DROP TRIGGER reject_context_update`); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Reconcile(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	current, _ = sessions.CurrentContextEpoch(ctx, created.ID)
	messages, _ = sessions.ListMessages(ctx, created.ID)
	eventsAfter, _ = repository.List(ctx, created.ID, -1, 100)
	if jsonEqual(current.Sources, initial.Sources) || len(messages) != 1 || messages[0].Content != "two update" || len(eventsAfter) != len(eventsBefore)+1 {
		t.Fatalf("atomic reconciliation missing: snapshot=%s messages=%#v events=%d", current.Sources, messages, len(eventsAfter))
	}
}

func jsonEqual(a, b []byte) bool {
	var av, bv any
	return json.Unmarshal(a, &av) == nil && json.Unmarshal(b, &bv) == nil && strings.TrimSpace(string(a)) == strings.TrimSpace(string(b))
}
