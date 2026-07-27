package compaction

import (
	"context"
	"reflect"
	"testing"
	"time"

	v1 "github.com/amirulashraf/parrot-coder/internal/api/v1"
	"github.com/amirulashraf/parrot-coder/internal/event"
	"github.com/amirulashraf/parrot-coder/internal/processidentity"
	"github.com/amirulashraf/parrot-coder/internal/protocol"
)

func TestRepositoryBeginAppendsStartedWithAttempt(t *testing.T) {
	h := newHarness(t, "", 0)
	attempt := repositoryAttempt(t, h, "cmpa_begin")

	got, err := h.repo.Begin(context.Background(), attempt)
	if err != nil {
		t.Fatal(err)
	}
	events := repositoryCompactionEvents(t, h)
	if len(events) != 1 || events[0].Type != v1.EventSessionCompactionStarted {
		t.Fatalf("compaction events = %#v", events)
	}
	wantPayload := v1.CompactionEvent{
		AttemptID: attempt.ID, Status: "started", SourceEpochID: attempt.SourceEpochID, HistoryCutoff: attempt.HistoryCutoff,
	}
	if payload := repositoryCompactionPayload(t, events[0]); !reflect.DeepEqual(payload, wantPayload) {
		t.Fatalf("started payload = %#v, want %#v", payload, wantPayload)
	}

	var status, createdAt, hostKey, processKey string
	var pid int
	if err := h.db.SQL().QueryRow(`SELECT status,created_at,owner_host_key,owner_pid,owner_process_key FROM compaction_attempt WHERE id=?`, attempt.ID).Scan(&status, &createdAt, &hostKey, &pid, &processKey); err != nil {
		t.Fatal(err)
	}
	if status != "active" || (processidentity.Identity{HostKey: hostKey, PID: pid, ProcessKey: processKey}) != h.repo.owner ||
		!got.CreatedAt.Equal(events[0].CreatedAt) || !repositoryTime(t, createdAt).Equal(events[0].CreatedAt) {
		t.Fatalf("attempt status/owner/time = %q/%q/%d/%q/%s, returned=%s, event=%s", status, hostKey, pid, processKey, createdAt, got.CreatedAt, events[0].CreatedAt)
	}
}

func TestRepositoryTerminalTransitionsRequireExactOwner(t *testing.T) {
	for _, transition := range []string{"complete", "fail"} {
		t.Run(transition, func(t *testing.T) {
			h := newHarness(t, "", 0)
			attempt, err := h.repo.Begin(context.Background(), repositoryAttempt(t, h, "cmpa_owner_"+transition))
			if err != nil {
				t.Fatal(err)
			}
			h.repo.owner.ProcessKey = "replacement-process"
			if transition == "complete" {
				_, err = h.repo.Complete(context.Background(), attempt, SummaryResult{Summary: "must not commit"})
			} else {
				err = h.repo.Fail(context.Background(), h.id, attempt.ID, "failed", "must not commit")
			}
			if err == nil {
				t.Fatalf("%s succeeded for replacement owner", transition)
			}
			var status string
			if err := h.db.SQL().QueryRow(`SELECT status FROM compaction_attempt WHERE id=?`, attempt.ID).Scan(&status); err != nil {
				t.Fatal(err)
			}
			if events := repositoryCompactionEvents(t, h); status != "active" || len(events) != 1 {
				t.Fatalf("state after rejected %s = %q, events %#v", transition, status, events)
			}
		})
	}
}

func TestRepositoryRepairActiveFiltersOwnersAndIsIdempotent(t *testing.T) {
	h := newHarness(t, "", 0)
	ctx := context.Background()
	epoch, err := h.sessions.GetSession(h.id).CurrentCompactionEpoch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	insert := func(id string, owner processidentity.Identity) {
		t.Helper()
		_, err := h.db.SQL().ExecContext(ctx, `INSERT INTO compaction_attempt(
			id,session_id,source_epoch_id,covered_from_sequence,covered_to_sequence,history_cutoff,
			provider_id,model_id,forced,status,created_at,owner_host_key,owner_pid,owner_process_key)
			VALUES(?,?,?,?,?,?,?,?,?,'active',?,?,?,?)`, id, h.id, epoch.ID, 0, 0, 1,
			"provider", "model", 1, "2026-01-01T00:00:00Z", owner.HostKey, owner.PID, owner.ProcessKey)
		if err != nil {
			t.Fatal(err)
		}
	}
	insert("legacy", processidentity.Identity{})
	insert("dead", processidentity.Identity{HostKey: "test-host", PID: 101, ProcessKey: "dead"})
	insert("live", h.repo.owner)
	insert("unknown", processidentity.Identity{HostKey: "foreign", PID: 10, ProcessKey: "unknown"})
	h.repo.inspect = func(local, owner processidentity.Identity) processidentity.Liveness {
		switch {
		case owner == (processidentity.Identity{}), owner.ProcessKey == "dead":
			return processidentity.LivenessDead
		case owner == local:
			return processidentity.LivenessAlive
		default:
			return processidentity.LivenessUnknown
		}
	}

	if err := h.repo.RepairActive(ctx, h.id); err != nil {
		t.Fatal(err)
	}
	events := repositoryCompactionEvents(t, h)
	if len(events) != 2 {
		t.Fatalf("repair events = %#v", events)
	}
	finished := make(map[string]event.Event)
	for _, item := range events {
		payload := repositoryCompactionPayload(t, item)
		if payload.Status != "interrupted" || payload.Error != "process restarted" {
			t.Fatalf("repair payload = %#v", payload)
		}
		finished[payload.AttemptID] = item
	}
	for _, id := range []string{"legacy", "dead", "live", "unknown"} {
		var status, finishedAt string
		if err := h.db.SQL().QueryRow(`SELECT status,COALESCE(finished_at,'') FROM compaction_attempt WHERE id=?`, id).Scan(&status, &finishedAt); err != nil {
			t.Fatal(err)
		}
		if item, repaired := finished[id]; repaired {
			if status != "interrupted" || !repositoryTime(t, finishedAt).Equal(item.CreatedAt) {
				t.Fatalf("repaired %s = %q/%q, event %s", id, status, finishedAt, item.CreatedAt)
			}
		} else if status != "active" || finishedAt != "" {
			t.Fatalf("preserved %s = %q/%q", id, status, finishedAt)
		}
	}
	if err := h.repo.RepairActive(ctx, h.id); err != nil {
		t.Fatal(err)
	}
	if after := repositoryCompactionEvents(t, h); len(after) != len(events) {
		t.Fatalf("idempotent repair appended events: %d -> %d", len(events), len(after))
	}
}

func TestRepositoryCompleteAppendsLifecycleAndProjectsAtomically(t *testing.T) {
	h := newHarness(t, "old summary", 0)
	attempt, err := h.repo.Begin(context.Background(), repositoryAttempt(t, h, "cmpa_complete"))
	if err != nil {
		t.Fatal(err)
	}
	summary := SummaryResult{Summary: "new summary", Usage: protocol.Usage{InputTokens: 7, OutputTokens: 3, TotalTokens: 10}}

	record, err := h.repo.Complete(context.Background(), attempt, summary)
	if err != nil {
		t.Fatal(err)
	}
	events := repositoryCompactionEvents(t, h)
	if len(events) != 3 || events[1].Type != v1.EventSessionCompactionCompleted || events[2].Type != v1.EventSessionCompactionFinished {
		t.Fatalf("compaction events = %#v", events)
	}
	wantPayload := v1.CompactionEvent{
		AttemptID: attempt.ID, Status: "completed", RecordID: record.ID, SourceEpochID: attempt.SourceEpochID,
		TargetEpochID: record.TargetEpochID, HistoryCutoff: attempt.HistoryCutoff,
	}
	for _, item := range events[1:] {
		if payload := repositoryCompactionPayload(t, item); !reflect.DeepEqual(payload, wantPayload) {
			t.Fatalf("%s payload = %#v, want %#v", item.Type, payload, wantPayload)
		}
	}

	var status, finishedAt, recordID, recordCreatedAt, epochID, epochCreatedAt string
	var ordinal int
	err = h.db.SQL().QueryRow(`SELECT attempt.status,attempt.finished_at,record.id,record.created_at,epoch.id,epoch.ordinal,epoch.created_at
		FROM compaction_attempt AS attempt
		JOIN compaction_record AS record ON record.attempt_id=attempt.id
		JOIN session_compaction_epoch AS epoch ON epoch.id=record.target_epoch_id
		WHERE attempt.id=?`, attempt.ID).Scan(
		&status, &finishedAt, &recordID, &recordCreatedAt, &epochID, &ordinal, &epochCreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	completedAt := events[1].CreatedAt
	if status != "completed" || recordID != record.ID || epochID != record.TargetEpochID || ordinal != 1 ||
		!events[2].CreatedAt.Equal(completedAt) || !record.CreatedAt.Equal(completedAt) ||
		!repositoryTime(t, finishedAt).Equal(completedAt) || !repositoryTime(t, recordCreatedAt).Equal(completedAt) ||
		!repositoryTime(t, epochCreatedAt).Equal(completedAt) {
		t.Fatalf("projection = status %q, record %q, epoch %q/%d; record/event times %s/%s", status, recordID, epochID, ordinal, record.CreatedAt, completedAt)
	}
}

func TestRepositoryFailAppendsTypedFinishedWithAttemptTimestamp(t *testing.T) {
	for _, status := range []string{"failed", "interrupted"} {
		t.Run(status, func(t *testing.T) {
			h := newHarness(t, "", 0)
			attempt, err := h.repo.Begin(context.Background(), repositoryAttempt(t, h, "cmpa_"+status))
			if err != nil {
				t.Fatal(err)
			}
			if err := h.repo.Fail(context.Background(), h.id, attempt.ID, status, "provider stopped"); err != nil {
				t.Fatal(err)
			}

			events := repositoryCompactionEvents(t, h)
			if len(events) != 2 || events[1].Type != v1.EventSessionCompactionFinished {
				t.Fatalf("compaction events = %#v", events)
			}
			wantPayload := v1.CompactionEvent{AttemptID: attempt.ID, Status: status, Error: "provider stopped"}
			if payload := repositoryCompactionPayload(t, events[1]); !reflect.DeepEqual(payload, wantPayload) {
				t.Fatalf("finished payload = %#v, want %#v", payload, wantPayload)
			}
			var gotStatus, reason, finishedAt string
			if err := h.db.SQL().QueryRow(`SELECT status,error_text,finished_at FROM compaction_attempt WHERE id=?`, attempt.ID).Scan(&gotStatus, &reason, &finishedAt); err != nil {
				t.Fatal(err)
			}
			if gotStatus != status || reason != "provider stopped" || !repositoryTime(t, finishedAt).Equal(events[1].CreatedAt) {
				t.Fatalf("attempt = status %q, reason %q, finished %q; event=%s", gotStatus, reason, finishedAt, events[1].CreatedAt)
			}
		})
	}
}

func TestRepositoryLifecycleProjectionFailureRollsBackDurableEvents(t *testing.T) {
	t.Run("begin", func(t *testing.T) {
		h := newHarness(t, "", 0)
		attempt := repositoryAttempt(t, h, "cmpa_bad_begin")
		attempt.HistoryCutoff = attempt.CoveredTo // Violates the projection table constraint.
		if _, err := h.repo.Begin(context.Background(), attempt); err == nil {
			t.Fatal("Begin succeeded")
		}
		var attempts int
		if err := h.db.SQL().QueryRow(`SELECT COUNT(*) FROM compaction_attempt WHERE id=?`, attempt.ID).Scan(&attempts); err != nil {
			t.Fatal(err)
		}
		if events := repositoryCompactionEvents(t, h); attempts != 0 || len(events) != 0 {
			t.Fatalf("attempts/events after rollback = %d/%d", attempts, len(events))
		}
	})

	t.Run("complete", func(t *testing.T) {
		h := newHarness(t, "", 0)
		attempt, err := h.repo.Begin(context.Background(), repositoryAttempt(t, h, "cmpa_bad_complete"))
		if err != nil {
			t.Fatal(err)
		}
		attempt.SourceEpochID = "ctx_no_longer_current"
		if _, err := h.repo.Complete(context.Background(), attempt, SummaryResult{Summary: "must roll back"}); err == nil {
			t.Fatal("Complete succeeded")
		}
		var status string
		var records, epochs int
		if err := h.db.SQL().QueryRow(`SELECT status FROM compaction_attempt WHERE id=?`, attempt.ID).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if err := h.db.SQL().QueryRow(`SELECT COUNT(*) FROM compaction_record`).Scan(&records); err != nil {
			t.Fatal(err)
		}
		if err := h.db.SQL().QueryRow(`SELECT COUNT(*) FROM session_compaction_epoch`).Scan(&epochs); err != nil {
			t.Fatal(err)
		}
		if events := repositoryCompactionEvents(t, h); status != "active" || records != 0 || epochs != 1 || len(events) != 1 {
			t.Fatalf("projection/events after rollback = status %q, records %d, epochs %d, events %d", status, records, epochs, len(events))
		}
	})

	t.Run("fail", func(t *testing.T) {
		h := newHarness(t, "", 0)
		attempt, err := h.repo.Begin(context.Background(), repositoryAttempt(t, h, "cmpa_bad_fail"))
		if err != nil {
			t.Fatal(err)
		}
		if err := h.repo.Fail(context.Background(), h.id, attempt.ID, "failed", "first"); err != nil {
			t.Fatal(err)
		}
		if err := h.repo.Fail(context.Background(), h.id, attempt.ID, "interrupted", "must roll back"); err == nil {
			t.Fatal("second Fail succeeded")
		}
		var status, reason string
		if err := h.db.SQL().QueryRow(`SELECT status,error_text FROM compaction_attempt WHERE id=?`, attempt.ID).Scan(&status, &reason); err != nil {
			t.Fatal(err)
		}
		if events := repositoryCompactionEvents(t, h); status != "failed" || reason != "first" || len(events) != 2 {
			t.Fatalf("attempt/events after rollback = %q/%q, events %d", status, reason, len(events))
		}
	})
}

func repositoryAttempt(t *testing.T, h *harness, attemptID string) Attempt {
	t.Helper()
	epoch, err := h.sessions.GetSession(h.id).CurrentCompactionEpoch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return Attempt{
		ID: attemptID, SessionID: h.id, SourceEpochID: epoch.ID, CoveredFrom: 0, CoveredTo: 0,
		HistoryCutoff: 1, ProviderID: "provider", ModelID: "model", Forced: true,
	}
}

func repositoryCompactionEvents(t *testing.T, h *harness) []event.Event {
	t.Helper()
	items, err := h.repo.events.List(context.Background(), h.id, -1, 1000)
	if err != nil {
		t.Fatal(err)
	}
	result := make([]event.Event, 0, len(items))
	for _, item := range items {
		switch item.Type {
		case v1.EventSessionCompactionStarted, v1.EventSessionCompactionCompleted, v1.EventSessionCompactionFinished:
			result = append(result, item)
		}
	}
	return result
}

func repositoryCompactionPayload(t *testing.T, item event.Event) v1.CompactionEvent {
	t.Helper()
	decoded, err := v1.DecodeEventData(v1.Event{Type: item.Type, Data: item.Data})
	if err != nil {
		t.Fatal(err)
	}
	payload, ok := decoded.(*v1.CompactionEvent)
	if !ok {
		t.Fatalf("%s payload type = %T", item.Type, decoded)
	}
	return *payload
}

func repositoryTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}
