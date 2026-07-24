package compaction

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/amirulashraf/parrot-coder/internal/event"
	"github.com/amirulashraf/parrot-coder/internal/protocol"
	"github.com/amirulashraf/parrot-coder/internal/provider"
	"github.com/amirulashraf/parrot-coder/internal/session"
	"github.com/amirulashraf/parrot-coder/internal/store"
)

type fakeSummarizer struct {
	request SummaryRequest
	result  SummaryResult
	err     error
	calls   int
}

func (s *fakeSummarizer) Summarize(_ context.Context, request SummaryRequest) (SummaryResult, error) {
	s.calls++
	s.request = request
	return s.result, s.err
}

type fakeContext struct {
	value FullContext
	err   error
}

func (c fakeContext) ObserveFull(context.Context) (FullContext, error) { return c.value, c.err }

type failingCompleteStore struct{ *Repository }

func (s failingCompleteStore) Complete(context.Context, Attempt, SummaryResult, FullContext) (Record, error) {
	return Record{}, errors.New("injected database commit failure")
}

type harness struct {
	db       *store.DB
	sessions *session.Service
	repo     *Repository
	id       string
}

func newHarness(t *testing.T, baseline string, messages int) *harness {
	t.Helper()
	ctx := context.Background()
	db := store.NewRegistry(t.TempDir(), "host-test")
	t.Cleanup(func() { _ = db.Close() })
	events := event.NewRepository(db)
	sessions := session.NewService(db, events)
	created, err := sessions.Create(ctx, session.CreateParams{Title: "compact"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sessions.InitializeContext(ctx, created.ID, baseline, json.RawMessage(`{"test":{"available":true,"value":{"v":1}}}`), 0); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < messages; i++ {
		role := protocol.RoleUser
		if i%2 == 1 {
			role = protocol.RoleAssistant
		}
		text := "message-" + string(rune('a'+i%26))
		if _, err := sessions.AppendMessage(ctx, created.ID, protocol.Message{Role: role, Content: []protocol.ContentPart{{Type: protocol.ContentText, Text: text}}}); err != nil {
			t.Fatal(err)
		}
	}
	sessionDB, err := db.Session(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	return &harness{db: sessionDB, sessions: sessions, repo: NewRepository(db, events), id: created.ID}
}

func TestBudgetTriggerAndDeterministicHeuristic(t *testing.T) {
	if first, second := HeuristicTextTokens("hello, 世界"), HeuristicTextTokens("hello, 世界"); first != second || first <= 0 {
		t.Fatalf("heuristic = %d, %d", first, second)
	}
	estimate := EstimateRequest("system", []Message{
		{Role: protocol.RoleAssistant, Usage: protocol.Usage{OutputTokens: 7}, Content: "measured"},
		{Role: protocol.RoleUser, Content: "heuristic", Parts: []protocol.ContentPart{{Type: protocol.ContentText, Text: "heuristic"}}},
	}, nil)
	if estimate.MeasuredTokens != 7 || estimate.HeuristicTokens == 0 {
		t.Fatalf("mixed estimate = %#v", estimate)
	}
	providerEstimate := EstimateRequest("system", []Message{
		{Role: protocol.RoleAssistant, Usage: protocol.Usage{InputTokens: 100_000, OutputTokens: 10, TotalTokens: 100_010}},
		{Role: protocol.RoleAssistant, Usage: protocol.Usage{InputTokens: 369_000, OutputTokens: 700, TotalTokens: 369_700}},
		{Role: protocol.RoleUser, Content: "suffix"},
	}, nil)
	if providerEstimate.Total() <= 369_000 || providerEstimate.Total() >= 469_000 {
		t.Fatalf("provider context estimate = %#v", providerEstimate)
	}
	h := newHarness(t, "baseline", 6)
	summary := &fakeSummarizer{result: SummaryResult{Summary: "summary"}}
	service, err := NewService(h.repo, summary, fakeContext{value: FullContext{Baseline: "fresh", Sources: json.RawMessage(`{}`)}}, Config{RecentMessages: 2, TriggerFraction: .8})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Compact(context.Background(), Request{SessionID: h.id, ProviderID: "fake", Model: provider.Model{ID: "large", ContextWindow: 10000}})
	if err != nil || result.Status != "skipped" || summary.calls != 0 {
		t.Fatalf("no-trigger result = %#v, calls=%d, err=%v", result, summary.calls, err)
	}
	result, err = service.Compact(context.Background(), Request{SessionID: h.id, ProviderID: "fake", Model: provider.Model{ID: "small", ContextWindow: 24}})
	if err != nil || result.Status != "complete" || summary.calls != 1 {
		t.Fatalf("trigger result = %#v, calls=%d, err=%v", result, summary.calls, err)
	}
}

func TestForcedCompactionRelaxesRecentMessageRetention(t *testing.T) {
	h := newHarness(t, "baseline", 6)
	summary := &fakeSummarizer{result: SummaryResult{Summary: "summary"}}
	service, err := NewService(h.repo, summary, fakeContext{value: FullContext{Baseline: "fresh", Sources: json.RawMessage(`{}`)}}, Config{RecentMessages: 6})
	if err != nil {
		t.Fatal(err)
	}

	result, err := service.Compact(context.Background(), Request{SessionID: h.id, ProviderID: "fake", Model: provider.Model{ID: "model", ContextWindow: 24}})
	if err != nil || result.Status != "skipped" || result.Reason != ErrNoSafeCut.Error() {
		t.Fatalf("proactive result = %#v, err=%v", result, err)
	}
	result, err = service.Compact(context.Background(), Request{SessionID: h.id, ProviderID: "fake", Model: provider.Model{ID: "model", ContextWindow: 24}, Force: true})
	if err != nil || result.Status != "complete" || len(summary.request.Messages) != 5 {
		t.Fatalf("forced result = %#v, summarized=%d, err=%v", result, len(summary.request.Messages), err)
	}
}

func TestForcedCompactionDoesNotRequireModelBudgetMetadata(t *testing.T) {
	h := newHarness(t, "baseline", 6)
	summary := &fakeSummarizer{result: SummaryResult{Summary: "summary"}}
	service, err := NewService(h.repo, summary, fakeContext{value: FullContext{Baseline: "fresh", Sources: json.RawMessage(`{}`)}}, Config{RecentMessages: 2})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Compact(context.Background(), Request{SessionID: h.id, ProviderID: "fake", Model: provider.Model{ID: "unknown"}, Force: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "complete" {
		t.Fatalf("result = %#v", result)
	}
}

func TestSafeCutKeepsToolCallWithAllResults(t *testing.T) {
	call := protocol.ToolCall{ID: "call", Name: "read", Input: json.RawMessage(`{}`)}
	messages := []Message{
		{Sequence: 1, Status: "complete", Role: protocol.RoleUser},
		{Sequence: 2, Status: "complete", Role: protocol.RoleAssistant, Parts: []protocol.ContentPart{{Type: protocol.ContentToolCall, ToolCall: &call}}},
		{Sequence: 3, Status: "complete", Role: protocol.RoleTool, Parts: []protocol.ContentPart{{Type: protocol.ContentToolResult, ToolCallID: "call", Text: "one"}}},
		{Sequence: 4, Status: "complete", Role: protocol.RoleTool, Parts: []protocol.ContentPart{{Type: protocol.ContentToolResult, ToolCallID: "call", Text: "two"}}},
		{Sequence: 5, Status: "complete", Role: protocol.RoleUser},
	}
	cut, ok := SafeCut(messages, 2)
	if !ok || cut != 2 {
		t.Fatalf("safe cut = %d, %t; want tool group retained from 2", cut, ok)
	}
	messages[3].Status = "interrupted"
	cut, ok = SafeCut(messages, 1)
	if !ok || cut > 2 {
		t.Fatalf("uncertain tool group cut = %d, %t", cut, ok)
	}
}

func TestAtomicSuccessPreservesHistoryAndIncludesPriorBaseline(t *testing.T) {
	h := newHarness(t, "PRIOR EPOCH SUMMARY", 8)
	summary := &fakeSummarizer{result: SummaryResult{Summary: "intent and decisions", Usage: protocol.Usage{TotalTokens: 12}}}
	service, _ := NewService(h.repo, summary, fakeContext{value: FullContext{Baseline: "FRESH TYPED CONTEXT", Sources: json.RawMessage(`{"fresh":{"available":true}}`)}}, Config{RecentMessages: 2})
	result, err := service.Compact(context.Background(), Request{SessionID: h.id, ProviderID: "fake", Model: provider.Model{ID: "model", ContextWindow: 100}, Force: true})
	if err != nil || result.Status != "complete" {
		t.Fatalf("compact = %#v, %v", result, err)
	}
	if len(summary.request.Messages) != 6 || !strings.Contains(summary.request.Prompt, "PRIOR EPOCH SUMMARY") || strings.Contains(strings.ToLower(summary.request.Prompt), "include secrets") {
		t.Fatalf("summary request = %#v", summary.request)
	}
	epoch, err := h.sessions.CurrentContextEpoch(context.Background(), h.id)
	if err != nil || epoch.Ordinal != 1 || !strings.Contains(epoch.Baseline, "FRESH TYPED CONTEXT") || !strings.Contains(epoch.Baseline, "intent and decisions") {
		t.Fatalf("epoch = %#v, %v", epoch, err)
	}
	messages, _ := h.sessions.ListMessages(context.Background(), h.id)
	if len(messages) != 8 {
		t.Fatalf("immutable messages changed: %d", len(messages))
	}
	records, err := h.repo.Records(context.Background(), h.id)
	if err != nil || len(records) != 1 || records[0].Usage.TotalTokens != 12 {
		t.Fatalf("records = %#v, %v", records, err)
	}
	var attempts int
	var status string
	if err := h.db.SQL().QueryRow(`SELECT COUNT(*),MAX(status) FROM compaction_attempt WHERE session_id=?`, h.id).Scan(&attempts, &status); err != nil || attempts != 1 || status != "completed" {
		t.Fatalf("attempts = %d %q, %v", attempts, status, err)
	}

	second, err := service.Compact(context.Background(), Request{SessionID: h.id, ProviderID: "fake", Model: provider.Model{ID: "model", ContextWindow: 100}, Force: true})
	if err != nil || second.Status != "skipped" || summary.calls != 1 {
		t.Fatalf("idempotent repeat = %#v calls=%d err=%v", second, summary.calls, err)
	}
	if err := h.sessions.Delete(context.Background(), h.id); err != nil {
		t.Fatalf("delete compacted session: %v", err)
	}
}

func TestFailuresLeaveOldEpochActive(t *testing.T) {
	tests := []struct {
		name       string
		summarizer *fakeSummarizer
		observer   fakeContext
	}{
		{"provider", &fakeSummarizer{err: errors.New("provider failed")}, fakeContext{}},
		{"context", &fakeSummarizer{result: SummaryResult{Summary: "summary"}}, fakeContext{err: errors.New("source unavailable")}},
		{"invalid context", &fakeSummarizer{result: SummaryResult{Summary: "summary"}}, fakeContext{value: FullContext{Sources: json.RawMessage(`no`)}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			h := newHarness(t, "old", 6)
			service, _ := NewService(h.repo, test.summarizer, test.observer, Config{RecentMessages: 2})
			if _, err := service.Compact(context.Background(), Request{SessionID: h.id, ProviderID: "fake", Model: provider.Model{ID: "model", ContextWindow: 100}, Force: true}); err == nil {
				t.Fatal("compaction succeeded")
			}
			epoch, _ := h.sessions.CurrentContextEpoch(context.Background(), h.id)
			if epoch.Ordinal != 0 || epoch.Baseline != "old" {
				t.Fatalf("epoch advanced: %#v", epoch)
			}
			var records int
			_ = h.db.SQL().QueryRow(`SELECT COUNT(*) FROM compaction_record`).Scan(&records)
			if records != 0 {
				t.Fatalf("records = %d", records)
			}
		})
	}
}

func TestCancellationMarksAttemptInterrupted(t *testing.T) {
	h := newHarness(t, "old", 6)
	summary := &fakeSummarizer{err: context.Canceled}
	service, _ := NewService(h.repo, summary, fakeContext{}, Config{RecentMessages: 2})
	_, err := service.Compact(context.Background(), Request{SessionID: h.id, ProviderID: "fake", Model: provider.Model{ID: "model", ContextWindow: 100}, Force: true})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
	var status string
	if err := h.db.SQL().QueryRow(`SELECT status FROM compaction_attempt`).Scan(&status); err != nil || status != "interrupted" {
		t.Fatalf("status = %q, %v", status, err)
	}
}

func TestDatabaseCompletionFailureDoesNotAdvanceEpoch(t *testing.T) {
	h := newHarness(t, "old", 6)
	summary := &fakeSummarizer{result: SummaryResult{Summary: "summary"}}
	service, _ := NewService(failingCompleteStore{h.repo}, summary, fakeContext{value: FullContext{Sources: json.RawMessage(`{}`)}}, Config{RecentMessages: 2})
	if _, err := service.Compact(context.Background(), Request{SessionID: h.id, ProviderID: "fake", Model: provider.Model{ID: "model", ContextWindow: 100}, Force: true}); err == nil {
		t.Fatal("compaction succeeded")
	}
	epoch, _ := h.sessions.CurrentContextEpoch(context.Background(), h.id)
	if epoch.Ordinal != 0 {
		t.Fatalf("epoch advanced: %#v", epoch)
	}
	var status string
	if err := h.db.SQL().QueryRow(`SELECT status FROM compaction_attempt`).Scan(&status); err != nil || status != "failed" {
		t.Fatalf("attempt status = %q, %v", status, err)
	}
}

type captureProvider struct{ request protocol.Request }

func (*captureProvider) ID() string               { return "fake" }
func (*captureProvider) Models() []provider.Model { return []provider.Model{{ID: "model"}} }
func (p *captureProvider) Stream(_ context.Context, request protocol.Request) (provider.Stream, error) {
	p.request = request
	return &testStream{events: []protocol.Event{{Type: protocol.EventTextDelta, Text: "summary"}, {Type: protocol.EventUsage, Usage: &protocol.Usage{TotalTokens: 3}}}}, nil
}

type testStream struct {
	events []protocol.Event
	index  int
}

func (s *testStream) Next(context.Context) (protocol.Event, error) {
	if s.index == len(s.events) {
		return protocol.Event{}, io.EOF
	}
	item := s.events[s.index]
	s.index++
	return item, nil
}
func (*testStream) Close() error { return nil }

type captureResolver struct{ provider.Provider }

func (r captureResolver) Resolve(_, _ string) (provider.Provider, provider.Model, error) {
	return r.Provider, provider.Model{ID: "model"}, nil
}

func TestProviderSummarizerNeverPassesTools(t *testing.T) {
	fake := &captureProvider{}
	summarizer := ProviderSummarizer{Providers: captureResolver{fake}}
	result, err := summarizer.Summarize(context.Background(), SummaryRequest{ProviderID: "fake", ModelID: "model", Prompt: "prompt", Messages: []protocol.Message{{Role: protocol.RoleUser}}})
	if err != nil || result.Summary != "summary" || result.Usage.TotalTokens != 3 {
		t.Fatalf("result = %#v, %v", result, err)
	}
	if fake.request.Tools != nil {
		t.Fatalf("summary tools = %#v", fake.request.Tools)
	}
}

func TestLongHistoryCompactsRequestButKeepsFullHistory(t *testing.T) {
	// Alternating user/assistant messages model 510 complete turns.
	h := newHarness(t, "baseline", 1020)
	summary := &fakeSummarizer{result: SummaryResult{Summary: "bounded"}}
	service, _ := NewService(h.repo, summary, fakeContext{value: FullContext{Sources: json.RawMessage(`{}`)}}, Config{RecentMessages: 10})
	if _, err := service.Compact(context.Background(), Request{SessionID: h.id, ProviderID: "fake", Model: provider.Model{ID: "model", ContextWindow: 100}, Force: true}); err != nil {
		t.Fatal(err)
	}
	epoch, _ := h.sessions.CurrentContextEpoch(context.Background(), h.id)
	history, err := h.sessions.ListModelHistory(context.Background(), h.id, epoch.HistoryCutoff)
	if err != nil || len(history) != 10 {
		t.Fatalf("model history = %d, %v", len(history), err)
	}
	all, err := h.sessions.ListMessages(context.Background(), h.id)
	if err != nil || len(all) != 1020 {
		t.Fatalf("full history = %d, %v", len(all), err)
	}
}
