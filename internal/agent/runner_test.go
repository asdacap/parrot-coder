package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amirulashraf/parrot-coder/internal/compaction"
	"github.com/amirulashraf/parrot-coder/internal/event"
	"github.com/amirulashraf/parrot-coder/internal/protocol"
	"github.com/amirulashraf/parrot-coder/internal/provider"
	"github.com/amirulashraf/parrot-coder/internal/session"
	"github.com/amirulashraf/parrot-coder/internal/store"
	"github.com/amirulashraf/parrot-coder/internal/systemcontext"
	"github.com/amirulashraf/parrot-coder/internal/tool"
)

type fakeProvider struct {
	mu          sync.Mutex
	requests    []protocol.Request
	stream      func(int, context.Context, protocol.Request) (provider.Stream, error)
	inputPrice  float64
	outputPrice float64
}

func (*fakeProvider) ID() string { return "fake" }
func (p *fakeProvider) Models() []provider.Model {
	return []provider.Model{{ID: "model", InputPrice: p.inputPrice, OutputPrice: p.outputPrice, Capabilities: provider.Capabilities{Tools: true}}}
}
func (p *fakeProvider) Stream(ctx context.Context, request protocol.Request) (provider.Stream, error) {
	p.mu.Lock()
	index := len(p.requests)
	p.requests = append(p.requests, request)
	p.mu.Unlock()
	return p.stream(index, ctx, request)
}
func (p *fakeProvider) Requests() []protocol.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]protocol.Request(nil), p.requests...)
}

type sliceStream struct {
	events []protocol.Event
	index  int
}

func (s *sliceStream) Next(context.Context) (protocol.Event, error) {
	if s.index == len(s.events) {
		return protocol.Event{}, io.EOF
	}
	event := s.events[s.index]
	s.index++
	return event, nil
}
func (*sliceStream) Close() error { return nil }

func events(items ...protocol.Event) provider.Stream { return &sliceStream{events: items} }

// recordingPublisher captures everything the runner puts on the live stream.
type recordingPublisher struct {
	mu     sync.Mutex
	events []protocol.Event
}

func (p *recordingPublisher) Publish(_ string, item protocol.Event) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if item.Usage != nil {
		copied := *item.Usage
		item.Usage = &copied
	}
	p.events = append(p.events, item)
}

// The live usage event is the only carrier of cost to clients, so it must be
// priced before publication rather than afterwards on a copy the runner keeps.
func TestRunnerPublishesPricedUsage(t *testing.T) {
	fake := &fakeProvider{inputPrice: 0.001, outputPrice: 0.002, stream: func(_ int, _ context.Context, _ protocol.Request) (provider.Stream, error) {
		return events(
			protocol.Event{Type: protocol.EventTextDelta, Text: "done"},
			protocol.Event{Type: protocol.EventUsage, Usage: &protocol.Usage{InputTokens: 100, OutputTokens: 50, TotalTokens: 150}},
			protocol.Event{Type: protocol.EventFinish, FinishReason: protocol.FinishStop},
		), nil
	}}
	live := &recordingPublisher{}
	h := newRunnerHarness(t, fake, nil)
	h.runner.config.Live = live
	h.admit(t, "user", "work", session.DeliverySteer)
	if err := h.runner.Drain(context.Background(), h.sessionID); err != nil {
		t.Fatal(err)
	}
	var published *protocol.Usage
	for _, item := range live.events {
		if item.Type == protocol.EventUsage {
			published = item.Usage
		}
	}
	if published == nil {
		t.Fatal("no usage event published")
	}
	if published.InputCost != 0.1 || published.OutputCost != 0.1 {
		t.Fatalf("published usage costs = %v/%v, want 0.1/0.1", published.InputCost, published.OutputCost)
	}
}

func TestReasoningSummaryAccumulatorPreservesPartOrder(t *testing.T) {
	var summary reasoningSummaryAccumulator
	summary.Write("reasoning:0", "First")
	summary.Write("reasoning:1", "Second item")
	summary.Write("reasoning:0", " item")
	summary.Set("reasoning:1", "Final second item")
	if got, want := summary.String(), "First itemFinal second item"; got != want {
		t.Fatalf("summary = %q, want %q", got, want)
	}
	if summary.Len() != len("First itemFinal second item") {
		t.Fatalf("summary length = %d", summary.Len())
	}
}

type blockingStream struct {
	started chan struct{}
	release <-chan struct{}
	events  *sliceStream
	once    sync.Once
}

func (s *blockingStream) Next(ctx context.Context) (protocol.Event, error) {
	s.once.Do(func() { close(s.started) })
	select {
	case <-s.release:
		return s.events.Next(ctx)
	case <-ctx.Done():
		return protocol.Event{}, ctx.Err()
	}
}
func (*blockingStream) Close() error { return nil }

type callThenBlockingStream struct {
	call    protocol.ToolCall
	started chan struct{}
	emitted bool
}

func (s *callThenBlockingStream) Next(ctx context.Context) (protocol.Event, error) {
	if !s.emitted {
		s.emitted = true
		return protocol.Event{Type: protocol.EventToolCallComplete, ToolCall: &s.call}, nil
	}
	close(s.started)
	<-ctx.Done()
	return protocol.Event{}, ctx.Err()
}

func (*callThenBlockingStream) Close() error { return nil }

type fakeTool struct {
	tool.BasePresentation
	tool.WritableTool
	id      string
	execute func(context.Context) (tool.Result, error)
}

func (t *fakeTool) ID() string                                      { return t.id }
func (t *fakeTool) Description() string                             { return t.id }
func (t *fakeTool) DescribeRequest(json.RawMessage) (string, error) { return t.id, nil }
func (t *fakeTool) JSONSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","additionalProperties":true}`)
}
func (t *fakeTool) Plan(_ context.Context, raw json.RawMessage, _ tool.CallContext) (tool.Plan, error) {
	return tool.NewPlan(t.id, raw, nil, nil, nil)
}
func (t *fakeTool) Execute(ctx context.Context, _ tool.Plan, _ tool.CallContext) (tool.Result, error) {
	if t.execute == nil {
		return tool.Result{Text: "ok", ModelText: "ok"}, nil
	}
	return t.execute(ctx)
}

type runnerHarness struct {
	db         *store.DB
	sessions   *session.Service
	goals      *session.GoalService
	repository *event.Repository
	sessionID  string
	runner     *Runner
}

type fakeCompactor struct {
	requests []compaction.Request
	compact  func(compaction.Request) (compaction.Result, error)
}

func (c *fakeCompactor) Compact(_ context.Context, request compaction.Request) (compaction.Result, error) {
	c.requests = append(c.requests, request)
	if c.compact != nil {
		return c.compact(request)
	}
	return compaction.Result{Status: "skipped"}, nil
}

func newRunnerHarness(t *testing.T, fake *fakeProvider, profiles []Profile, tools ...tool.Tool) *runnerHarness {
	t.Helper()
	ctx := context.Background()
	db := store.NewRegistry(t.TempDir(), "host-test")
	t.Cleanup(func() { db.Close() })
	repository := event.NewRepository(db)
	sessions := session.NewService(db, repository)
	goals := session.NewGoalService(db, repository)
	created, err := sessions.Create(ctx, session.CreateParams{Title: "runner"})
	if err != nil {
		t.Fatal(err)
	}
	agents, err := NewRegistry(profiles...)
	if err != nil {
		t.Fatal(err)
	}
	profile := BuildID
	if len(profiles) > 0 {
		profile = profiles[0].ID
	}
	if err := sessions.SetSelection(ctx, created.ID, session.Selection{Agent: profile, Provider: "fake", Model: "model"}); err != nil {
		t.Fatal(err)
	}
	providers, err := NewProviderRegistry(fake)
	if err != nil {
		t.Fatal(err)
	}
	toolRegistry := tool.NewRegistry()
	for _, item := range tools {
		if err := toolRegistry.Register(item); err != nil {
			t.Fatal(err)
		}
	}
	snapshot := toolRegistry.Materialize()
	contextRegistry, _ := systemcontext.NewRegistry(systemcontext.StaticSource{SourceKey: "agent:context", Text: "baseline"})
	runner, err := NewRunner(RunnerConfig{
		Sessions:           sessions,
		Contexts:           systemcontext.Manager{Registry: contextRegistry, Store: sessions},
		Agents:             agents,
		Providers:          providers,
		ToolSnapshot:       func() tool.Snapshot { return snapshot },
		ToolExecutor:       func(snapshot tool.Snapshot) tool.Executor { return tool.Executor{Snapshot: snapshot} },
		Goals:              goals,
		MaxConcurrentTools: 2,
		CleanupTimeout:     time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	sessionDB, err := db.Session(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	return &runnerHarness{db: sessionDB, sessions: sessions, goals: goals, repository: repository, sessionID: created.ID, runner: runner}
}

func TestRunnerMarksActiveGoalOnStructuredUsageExhaustionOnly(t *testing.T) {
	tests := []struct {
		name       string
		stream     func(context.Context) (provider.Stream, error)
		wantStatus session.GoalStatus
	}{
		{"http quota", func(context.Context) (provider.Stream, error) {
			return nil, &provider.HTTPError{StatusCode: 429, Code: "insufficient_quota"}
		}, session.GoalUsageLimited},
		{"stream quota", func(context.Context) (provider.Stream, error) {
			return events(protocol.Event{Type: protocol.EventProviderError, ProviderError: &protocol.ProviderError{Type: "usage_limit_reached", Message: "limited"}}), nil
		}, session.GoalUsageLimited},
		{"transient rate limit", func(context.Context) (provider.Stream, error) {
			return nil, &provider.HTTPError{StatusCode: 429, Code: "rate_limit_exceeded"}
		}, session.GoalActive},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := &fakeProvider{stream: func(_ int, ctx context.Context, _ protocol.Request) (provider.Stream, error) { return test.stream(ctx) }}
			h := newRunnerHarness(t, fake, nil)
			if _, err := h.goals.Create(context.Background(), h.sessionID, "continue", nil); err != nil {
				t.Fatal(err)
			}
			h.admit(t, "user", "work", session.DeliverySteer)
			if err := h.runner.Drain(context.Background(), h.sessionID); err == nil {
				t.Fatal("Drain succeeded")
			}
			goal, err := h.goals.Get(context.Background(), h.sessionID)
			if err != nil || goal.Status != test.wantStatus {
				t.Fatalf("goal = %#v, %v; want %s", goal, err, test.wantStatus)
			}
		})
	}
}

func (h *runnerHarness) admit(t *testing.T, id, content string, delivery session.Delivery) {
	t.Helper()
	if _, err := h.sessions.Admit(context.Background(), h.sessionID, session.AdmitParams{MessageID: id, Content: content, Delivery: delivery}); err != nil {
		t.Fatal(err)
	}
}

func TestRunnerPersistsStreamedFinalText(t *testing.T) {
	fake := &fakeProvider{stream: func(_ int, _ context.Context, _ protocol.Request) (provider.Stream, error) {
		return events(
			protocol.Event{Type: protocol.EventTextDelta, Text: "hello "},
			protocol.Event{Type: protocol.EventTextDelta, Text: "world"},
			protocol.Event{Type: protocol.EventUsage, Usage: &protocol.Usage{TotalTokens: 3}},
			protocol.Event{Type: protocol.EventFinish, FinishReason: protocol.FinishStop},
		), nil
	}}
	h := newRunnerHarness(t, fake, nil)
	h.admit(t, "user", "question", session.DeliverySteer)
	if err := h.runner.Drain(context.Background(), h.sessionID); err != nil {
		t.Fatal(err)
	}
	messages, err := h.sessions.ListMessages(context.Background(), h.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	last := messages[len(messages)-1]
	if last.Role != "assistant" || last.Content != "hello world" || last.Status != "complete" || last.FinishReason != string(protocol.FinishStop) {
		t.Fatalf("assistant = %#v", last)
	}
	var usage protocol.Usage
	if err := json.Unmarshal(last.Usage, &usage); err != nil || usage.TotalTokens != 3 {
		t.Fatalf("usage = %#v, %v", usage, err)
	}
}

func TestRunnerPersistsToolBeforeSideEffectAndContinuesAfterSettlement(t *testing.T) {
	var h *runnerHarness
	executed := make(chan struct{}, 1)
	item := &fakeTool{id: "mutate", execute: func(context.Context) (tool.Result, error) {
		var status string
		if err := h.db.SQL().QueryRow(`SELECT status FROM session_tool_call WHERE session_id=? AND id='call-1'`, h.sessionID).Scan(&status); err != nil {
			return tool.Result{}, err
		}
		if status != "running" {
			return tool.Result{}, errors.New("tool call was not durably running")
		}
		executed <- struct{}{}
		return tool.Result{Text: "tool output", ModelText: "tool output"}, nil
	}}
	fake := &fakeProvider{stream: func(index int, _ context.Context, request protocol.Request) (provider.Stream, error) {
		if index == 0 {
			call := protocol.ToolCall{ID: "call-1", Name: "mutate", Input: json.RawMessage(`{}`)}
			return events(protocol.Event{Type: protocol.EventToolCallComplete, ToolCall: &call}, protocol.Event{Type: protocol.EventFinish, FinishReason: protocol.FinishToolCalls}), nil
		}
		if len(request.Messages) == 0 || request.Messages[len(request.Messages)-1].Role != protocol.RoleTool {
			return nil, errors.New("continuation started before tool result")
		}
		return events(protocol.Event{Type: protocol.EventTextDelta, Text: "final"}, protocol.Event{Type: protocol.EventFinish, FinishReason: protocol.FinishStop}), nil
	}}
	h = newRunnerHarness(t, fake, nil, item)
	h.admit(t, "user", "run tool", session.DeliverySteer)
	if err := h.runner.Drain(context.Background(), h.sessionID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-executed:
	default:
		t.Fatal("tool did not execute")
	}
	status, result := toolState(t, h, "call-1")
	if status != "success" || result != "tool output" {
		t.Fatalf("tool state = %q, %q", status, result)
	}
	if len(fake.Requests()) != 2 {
		t.Fatalf("provider turns = %d", len(fake.Requests()))
	}
}

func TestRunnerBoundsConcurrentToolsAndSettlesAllBeforeContinuation(t *testing.T) {
	gate := make(chan struct{})
	started := make(chan struct{}, 5)
	var active, maximum atomic.Int32
	item := &fakeTool{id: "parallel", execute: func(context.Context) (tool.Result, error) {
		current := active.Add(1)
		for {
			old := maximum.Load()
			if current <= old || maximum.CompareAndSwap(old, current) {
				break
			}
		}
		started <- struct{}{}
		<-gate
		active.Add(-1)
		return tool.Result{Text: "ok", ModelText: "ok"}, nil
	}}
	fake := &fakeProvider{stream: func(index int, _ context.Context, request protocol.Request) (provider.Stream, error) {
		if index == 0 {
			stream := &sliceStream{}
			for i := 0; i < 5; i++ {
				call := protocol.ToolCall{ID: "call-" + string(rune('a'+i)), Name: "parallel", Input: json.RawMessage(`{}`)}
				stream.events = append(stream.events, protocol.Event{Type: protocol.EventToolCallComplete, ToolCall: &call})
			}
			stream.events = append(stream.events, protocol.Event{Type: protocol.EventFinish, FinishReason: protocol.FinishToolCalls})
			return stream, nil
		}
		for _, message := range request.Messages {
			if message.Role == protocol.RoleTool {
				continue
			}
		}
		return events(protocol.Event{Type: protocol.EventFinish, FinishReason: protocol.FinishStop}), nil
	}}
	h := newRunnerHarness(t, fake, nil, item)
	h.admit(t, "user", "parallel", session.DeliverySteer)
	done := make(chan error, 1)
	go func() { done <- h.runner.Drain(context.Background(), h.sessionID) }()
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("tools did not start")
		}
	}
	select {
	case <-started:
		t.Fatal("more than two tools ran concurrently")
	case <-time.After(20 * time.Millisecond):
	}
	close(gate)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if maximum.Load() != 2 {
		t.Fatalf("maximum concurrency = %d", maximum.Load())
	}
	for i := 0; i < 5; i++ {
		status, _ := toolState(t, h, "call-"+string(rune('a'+i)))
		if status != "success" {
			t.Fatalf("tool %d status = %s", i, status)
		}
	}
}

func TestRunnerSteerDuringBlockedTurnPromotesOnNextTurnOnly(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	item := &fakeTool{id: "read", execute: nil}
	fake := &fakeProvider{stream: func(index int, _ context.Context, _ protocol.Request) (provider.Stream, error) {
		if index == 0 {
			call := protocol.ToolCall{ID: "call", Name: "read", Input: json.RawMessage(`{}`)}
			return &blockingStream{started: started, release: release, events: &sliceStream{events: []protocol.Event{{Type: protocol.EventToolCallComplete, ToolCall: &call}, {Type: protocol.EventFinish, FinishReason: protocol.FinishToolCalls}}}}, nil
		}
		return events(protocol.Event{Type: protocol.EventFinish, FinishReason: protocol.FinishStop}), nil
	}}
	h := newRunnerHarness(t, fake, nil, item)
	h.admit(t, "initial", "initial", session.DeliverySteer)
	done := make(chan error, 1)
	go func() { done <- h.runner.Drain(context.Background(), h.sessionID) }()
	<-started
	h.admit(t, "steer", "late steer", session.DeliverySteer)
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	requests := fake.Requests()
	if len(requests) != 2 || containsText(requests[0].Messages, "late steer") || !containsText(requests[1].Messages, "late steer") {
		t.Fatalf("steer request placement = %#v", requests)
	}
}

func TestRunnerQueueWaitsUntilCurrentContinuationIsIdle(t *testing.T) {
	fake := &fakeProvider{stream: func(_ int, _ context.Context, _ protocol.Request) (provider.Stream, error) {
		return events(protocol.Event{Type: protocol.EventFinish, FinishReason: protocol.FinishStop}), nil
	}}
	h := newRunnerHarness(t, fake, nil)
	h.admit(t, "initial", "initial", session.DeliverySteer)
	h.admit(t, "queued", "queued", session.DeliveryQueue)
	if err := h.runner.Drain(context.Background(), h.sessionID); err != nil {
		t.Fatal(err)
	}
	requests := fake.Requests()
	if len(requests) != 2 || containsText(requests[0].Messages, "queued") || !containsText(requests[1].Messages, "queued") {
		t.Fatalf("queue request placement = %#v", requests)
	}
}

func TestRunnerCancellationSettlesAssistantAndTools(t *testing.T) {
	t.Run("assistant", func(t *testing.T) {
		started := make(chan struct{})
		release := make(chan struct{})
		fake := &fakeProvider{stream: func(_ int, _ context.Context, _ protocol.Request) (provider.Stream, error) {
			return &blockingStream{started: started, release: release, events: &sliceStream{}}, nil
		}}
		h := newRunnerHarness(t, fake, nil)
		h.admit(t, "user", "cancel", session.DeliverySteer)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- h.runner.Drain(ctx, h.sessionID) }()
		<-started
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("Drain error = %v", err)
		}
		messages, _ := h.sessions.ListMessages(context.Background(), h.sessionID)
		if messages[len(messages)-1].Status != "interrupted" {
			t.Fatalf("assistant status = %s", messages[len(messages)-1].Status)
		}
	})

	t.Run("tools", func(t *testing.T) {
		started := make(chan struct{})
		item := &fakeTool{id: "block", execute: func(ctx context.Context) (tool.Result, error) {
			close(started)
			<-ctx.Done()
			return tool.Result{}, ctx.Err()
		}}
		fake := &fakeProvider{stream: func(_ int, _ context.Context, _ protocol.Request) (provider.Stream, error) {
			call := protocol.ToolCall{ID: "blocked", Name: "block", Input: json.RawMessage(`{}`)}
			return events(protocol.Event{Type: protocol.EventToolCallComplete, ToolCall: &call}, protocol.Event{Type: protocol.EventFinish, FinishReason: protocol.FinishToolCalls}), nil
		}}
		h := newRunnerHarness(t, fake, nil, item)
		h.admit(t, "user", "cancel tool", session.DeliverySteer)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- h.runner.Drain(ctx, h.sessionID) }()
		<-started
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("Drain error = %v", err)
		}
		status, _ := toolState(t, h, "blocked")
		if status != "interrupted" {
			t.Fatalf("tool status = %s", status)
		}
	})
}

func TestRunnerCancellationPersistsToolOutputBeforeNextProviderTurn(t *testing.T) {
	started := make(chan struct{})
	item := &fakeTool{id: "block", execute: func(ctx context.Context) (tool.Result, error) {
		close(started)
		<-ctx.Done()
		return tool.Result{}, ctx.Err()
	}}
	fake := &fakeProvider{stream: func(index int, _ context.Context, request protocol.Request) (provider.Stream, error) {
		if index == 0 {
			call := protocol.ToolCall{ID: "call_cancelled", Name: "block", Input: json.RawMessage(`{}`)}
			return events(protocol.Event{Type: protocol.EventToolCallComplete, ToolCall: &call}, protocol.Event{Type: protocol.EventFinish, FinishReason: protocol.FinishToolCalls}), nil
		}
		var callSeen, outputSeen bool
		for _, message := range request.Messages {
			for _, part := range message.Content {
				if part.Type == protocol.ContentToolCall && part.ToolCall != nil && part.ToolCall.ID == "call_cancelled" {
					callSeen = true
				}
				if part.Type == protocol.ContentToolResult && part.ToolCallID == "call_cancelled" {
					outputSeen = true
					if !strings.Contains(part.Text, "interrupted") {
						return nil, errors.New("interrupted tool output did not explain cancellation")
					}
				}
			}
		}
		if !callSeen || !outputSeen {
			return nil, errors.New("resumed provider request contains an orphaned tool call")
		}
		return events(protocol.Event{Type: protocol.EventFinish, FinishReason: protocol.FinishStop}), nil
	}}
	h := newRunnerHarness(t, fake, nil, item)
	h.admit(t, "first", "run", session.DeliverySteer)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.runner.Drain(ctx, h.sessionID) }()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Drain error = %v", err)
	}

	h.admit(t, "second", "try something else", session.DeliverySteer)
	if err := h.runner.Drain(context.Background(), h.sessionID); err != nil {
		t.Fatalf("resumed Drain error = %v", err)
	}
}

func TestRunnerCancellationDuringProviderStreamDropsUnexecutedToolCalls(t *testing.T) {
	started := make(chan struct{})
	call := protocol.ToolCall{ID: "partial_call", Name: "read", Input: json.RawMessage(`{}`)}
	fake := &fakeProvider{stream: func(index int, _ context.Context, request protocol.Request) (provider.Stream, error) {
		if index == 0 {
			return &callThenBlockingStream{call: call, started: started}, nil
		}
		for _, message := range request.Messages {
			for _, part := range message.Content {
				if part.Type == protocol.ContentToolCall && part.ToolCall != nil && part.ToolCall.ID == call.ID {
					return nil, errors.New("unexecuted tool call survived interrupted provider stream")
				}
			}
		}
		return events(protocol.Event{Type: protocol.EventFinish, FinishReason: protocol.FinishStop}), nil
	}}
	h := newRunnerHarness(t, fake, nil, &fakeTool{id: "read"})
	h.admit(t, "first", "read", session.DeliverySteer)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.runner.Drain(ctx, h.sessionID) }()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Drain error = %v", err)
	}
	h.admit(t, "second", "continue", session.DeliverySteer)
	if err := h.runner.Drain(context.Background(), h.sessionID); err != nil {
		t.Fatalf("resumed Drain error = %v", err)
	}
}

func TestRunnerPlanDeniesMutationEvenWhenToolIsRegistered(t *testing.T) {
	var executed atomic.Bool
	mutation := &fakeTool{id: "mutate", execute: func(context.Context) (tool.Result, error) {
		executed.Store(true)
		return tool.Result{Text: "mutated", ModelText: "mutated"}, nil
	}}
	fake := &fakeProvider{stream: func(index int, _ context.Context, _ protocol.Request) (provider.Stream, error) {
		if index == 0 {
			call := protocol.ToolCall{ID: "denied", Name: "mutate", Input: json.RawMessage(`{}`)}
			return events(protocol.Event{Type: protocol.EventToolCallComplete, ToolCall: &call}, protocol.Event{Type: protocol.EventFinish, FinishReason: protocol.FinishToolCalls}), nil
		}
		return events(protocol.Event{Type: protocol.EventFinish, FinishReason: protocol.FinishStop}), nil
	}}
	h := newRunnerHarness(t, fake, []Profile{Builtins()[1]}, mutation)
	h.admit(t, "user", "plan", session.DeliverySteer)
	if err := h.runner.Drain(context.Background(), h.sessionID); err != nil {
		t.Fatal(err)
	}
	if executed.Load() {
		t.Fatal("mutation tool executed")
	}
	if len(fake.Requests()[0].Tools) != 0 {
		t.Fatalf("plan request exposed mutation tool: %#v", fake.Requests()[0].Tools)
	}
	status, _ := toolState(t, h, "denied")
	if status != "failure" {
		t.Fatalf("denied tool status = %s", status)
	}
}

func TestRunnerMaxTurnsOmitsTools(t *testing.T) {
	item := &fakeTool{id: "available"}
	fake := &fakeProvider{stream: func(_ int, _ context.Context, request protocol.Request) (provider.Stream, error) {
		if len(request.Tools) != 0 {
			return nil, errors.New("tools were present on final turn")
		}
		return events(protocol.Event{Type: protocol.EventFinish, FinishReason: protocol.FinishStop}), nil
	}}
	profile := Profile{ID: "one-turn", Prompt: "finish", MaxTurns: 1}
	h := newRunnerHarness(t, fake, []Profile{profile}, item)
	h.admit(t, "user", "finish", session.DeliverySteer)
	if err := h.runner.Drain(context.Background(), h.sessionID); err != nil {
		t.Fatal(err)
	}
	if len(fake.Requests()) != 1 {
		t.Fatalf("provider turns = %d", len(fake.Requests()))
	}
}

func TestRunnerProviderErrorLeavesTerminalAssistant(t *testing.T) {
	fake := &fakeProvider{stream: func(_ int, _ context.Context, _ protocol.Request) (provider.Stream, error) {
		return events(
			protocol.Event{Type: protocol.EventTextDelta, Text: "partial"},
			protocol.Event{Type: protocol.EventProviderError, ProviderError: &protocol.ProviderError{Message: "provider failed"}},
		), nil
	}}
	h := newRunnerHarness(t, fake, nil)
	h.admit(t, "user", "error", session.DeliverySteer)
	if err := h.runner.Drain(context.Background(), h.sessionID); err == nil {
		t.Fatal("Drain succeeded")
	}
	messages, _ := h.sessions.ListMessages(context.Background(), h.sessionID)
	last := messages[len(messages)-1]
	if last.Status != "error" || last.Content != "partial" || last.Error != "provider failed" {
		t.Fatalf("assistant = %#v", last)
	}
}

func TestRunnerInvokesAutomaticCompactionWithCompleteRequestCost(t *testing.T) {
	item := &fakeTool{id: "read"}
	fake := &fakeProvider{stream: func(_ int, _ context.Context, _ protocol.Request) (provider.Stream, error) {
		return events(protocol.Event{Type: protocol.EventFinish, FinishReason: protocol.FinishStop}), nil
	}}
	h := newRunnerHarness(t, fake, nil, item)
	compactor := &fakeCompactor{}
	h.runner.config.Compactor = compactor
	h.admit(t, "user", "automatic", session.DeliverySteer)
	if err := h.runner.Drain(context.Background(), h.sessionID); err != nil {
		t.Fatal(err)
	}
	if len(compactor.requests) != 1 || compactor.requests[0].Force || compactor.requests[0].ProviderID != "fake" || compactor.requests[0].Model.ID != "model" || len(compactor.requests[0].Tools) != 1 {
		t.Fatalf("compaction requests = %#v", compactor.requests)
	}
}

func TestRunnerRetriesCanonicalOverflowExactlyOnce(t *testing.T) {
	fake := &fakeProvider{stream: func(index int, _ context.Context, _ protocol.Request) (provider.Stream, error) {
		if index == 0 {
			return events(protocol.Event{Type: protocol.EventProviderError, ProviderError: &protocol.ProviderError{Type: "invalid_request_error", Code: "context_length_exceeded", Message: "too long"}}), nil
		}
		return events(protocol.Event{Type: protocol.EventFinish, FinishReason: protocol.FinishStop}), nil
	}}
	h := newRunnerHarness(t, fake, nil)
	compactor := &fakeCompactor{compact: func(request compaction.Request) (compaction.Result, error) {
		if request.Force {
			return compaction.Result{Status: "complete", RecordID: "cmpr_retry"}, nil
		}
		return compaction.Result{Status: "skipped"}, nil
	}}
	h.runner.config.Compactor = compactor
	h.admit(t, "user", "overflow", session.DeliverySteer)
	if err := h.runner.Drain(context.Background(), h.sessionID); err != nil {
		t.Fatal(err)
	}
	if len(fake.Requests()) != 2 || len(compactor.requests) != 2 || !compactor.requests[1].Force {
		t.Fatalf("provider=%d compactor=%#v", len(fake.Requests()), compactor.requests)
	}
	events, err := h.repository.List(context.Background(), h.sessionID, -1, 100)
	if err != nil {
		t.Fatal(err)
	}
	retries := 0
	for _, item := range events {
		if item.Type == "session.compaction.retry" {
			retries++
		}
	}
	if retries != 1 {
		t.Fatalf("retry events = %d", retries)
	}
}

func TestRunnerRetriesMessageOnlyOverflowExactlyOnce(t *testing.T) {
	// Kimi/Moonshot reports a context overflow with a generic
	// "invalid_request_error" type and the reason only in the message text.
	fake := &fakeProvider{stream: func(index int, _ context.Context, _ protocol.Request) (provider.Stream, error) {
		if index == 0 {
			return nil, &provider.HTTPError{StatusCode: 400, Type: "invalid_request_error", Message: "Invalid request: Your request exceeded model token limit: 262144 (requested: 265424)"}
		}
		return events(protocol.Event{Type: protocol.EventFinish, FinishReason: protocol.FinishStop}), nil
	}}
	h := newRunnerHarness(t, fake, nil)
	compactor := &fakeCompactor{compact: func(request compaction.Request) (compaction.Result, error) {
		if request.Force {
			return compaction.Result{Status: "complete", RecordID: "cmpr_retry"}, nil
		}
		return compaction.Result{Status: "skipped"}, nil
	}}
	h.runner.config.Compactor = compactor
	h.admit(t, "user", "overflow", session.DeliverySteer)
	if err := h.runner.Drain(context.Background(), h.sessionID); err != nil {
		t.Fatal(err)
	}
	if len(fake.Requests()) != 2 || len(compactor.requests) != 2 || !compactor.requests[1].Force {
		t.Fatalf("provider=%d compactor=%#v", len(fake.Requests()), compactor.requests)
	}
	events, err := h.repository.List(context.Background(), h.sessionID, -1, 100)
	if err != nil {
		t.Fatal(err)
	}
	retries := 0
	for _, item := range events {
		if item.Type == "session.compaction.retry" {
			retries++
		}
	}
	if retries != 1 {
		t.Fatalf("retry events = %d", retries)
	}
}

func TestRunnerDoesNotRetryUnknownProviderError(t *testing.T) {
	fake := &fakeProvider{stream: func(_ int, _ context.Context, _ protocol.Request) (provider.Stream, error) {
		return events(protocol.Event{Type: protocol.EventProviderError, ProviderError: &protocol.ProviderError{Code: "mystery", Message: "unknown"}}), nil
	}}
	h := newRunnerHarness(t, fake, nil)
	compactor := &fakeCompactor{}
	h.runner.config.Compactor = compactor
	h.admit(t, "user", "unknown", session.DeliverySteer)
	if err := h.runner.Drain(context.Background(), h.sessionID); err == nil {
		t.Fatal("Drain succeeded")
	}
	if len(fake.Requests()) != 1 || len(compactor.requests) != 1 {
		t.Fatalf("provider=%d compactor=%d", len(fake.Requests()), len(compactor.requests))
	}
}

func toolState(t *testing.T, h *runnerHarness, callID string) (string, string) {
	t.Helper()
	var status, result string
	if err := h.db.SQL().QueryRow(`SELECT status,result_text FROM session_tool_call WHERE session_id=? AND id=?`, h.sessionID, callID).Scan(&status, &result); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("tool call %s was not persisted", callID)
		}
		t.Fatal(err)
	}
	return status, result
}

func containsText(messages []protocol.Message, want string) bool {
	for _, message := range messages {
		for _, part := range message.Content {
			if part.Text == want {
				return true
			}
		}
	}
	return false
}
