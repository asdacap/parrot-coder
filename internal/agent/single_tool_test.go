package agent

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amirulashraf/parrot-coder/internal/event"
	"github.com/amirulashraf/parrot-coder/internal/protocol"
	"github.com/amirulashraf/parrot-coder/internal/provider"
	"github.com/amirulashraf/parrot-coder/internal/session"
	"github.com/amirulashraf/parrot-coder/internal/store"
	"github.com/amirulashraf/parrot-coder/internal/systemcontext"
	"github.com/amirulashraf/parrot-coder/internal/tool"
)

// TestRunnerSendsSingleToolResultAutomatically verifies that when the agent
// requests exactly one tool call, the tool result is automatically sent back
// to the model on a continuation provider turn within the same Drain, without
// requiring the user to prompt again.
func TestRunnerSendsSingleToolResultAutomatically(t *testing.T) {
	tests := []struct {
		name     string
		delivery session.Delivery
		withGoal bool
		withText bool
	}{
		{"steer no goal no text", session.DeliverySteer, false, false},
		{"steer no goal with text", session.DeliverySteer, false, true},
		{"queue no goal no text", session.DeliveryQueue, false, false},
		{"steer with goal no text", session.DeliverySteer, true, false},
		{"queue with goal with text", session.DeliveryQueue, true, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			item := &fakeTool{id: "read"}
			fake := &fakeProvider{stream: func(index int, _ context.Context, request protocol.Request) (provider.Stream, error) {
				if index == 0 {
					var evs []protocol.Event
					if test.withText {
						evs = append(evs, protocol.Event{Type: protocol.EventTextDelta, Text: "let me check"})
					}
					call := protocol.ToolCall{ID: "call-1", Name: "read", Input: json.RawMessage(`{}`)}
					evs = append(evs, protocol.Event{Type: protocol.EventToolCallComplete, ToolCall: &call})
					evs = append(evs, protocol.Event{Type: protocol.EventFinish, FinishReason: protocol.FinishToolCalls})
					return events(evs...), nil
				}
				if len(request.Messages) == 0 || request.Messages[len(request.Messages)-1].Role != protocol.RoleTool {
					return nil, errors.New("continuation started before tool result was sent")
				}
				return events(protocol.Event{Type: protocol.EventTextDelta, Text: "done"}, protocol.Event{Type: protocol.EventFinish, FinishReason: protocol.FinishStop}), nil
			}}
			h := newRunnerHarness(t, fake, nil, item)
			if test.withGoal {
				if _, err := h.goals.Create(context.Background(), h.sessionID, "continue", nil); err != nil {
					t.Fatal(err)
				}
			}
			h.admit(t, "user", "run tool", test.delivery)
			if err := h.runner.drainOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			if turns := len(fake.Requests()); turns != 2 {
				t.Fatalf("provider turns = %d, want 2 (tool result not sent automatically)", turns)
			}
		})
	}
}

// TestRunnerSendsSingleToolResultWithoutDuplication checks that the tool
// result for exactly one tool call appears exactly once in the continuation
// provider request: the runner appends it as a separate message and
// repairToolHistory must not synthesize a duplicate.
func TestRunnerSendsSingleToolResultWithoutDuplication(t *testing.T) {
	item := &fakeTool{id: "read"}
	fake := &fakeProvider{stream: func(index int, _ context.Context, request protocol.Request) (provider.Stream, error) {
		if index == 0 {
			call := protocol.ToolCall{ID: "call-1", Name: "read", Input: json.RawMessage(`{}`)}
			return events(
				protocol.Event{Type: protocol.EventToolCallComplete, ToolCall: &call},
				protocol.Event{Type: protocol.EventFinish, FinishReason: protocol.FinishToolCalls},
			), nil
		}
		var results int
		for _, message := range request.Messages {
			for _, part := range message.Content {
				if part.Type == protocol.ContentToolResult && part.ToolCallID == "call-1" {
					results++
				}
			}
		}
		if results != 1 {
			return nil, errors.New("tool result appeared %d times in continuation request, want 1")
		}
		return events(protocol.Event{Type: protocol.EventFinish, FinishReason: protocol.FinishStop}), nil
	}}
	h := newRunnerHarness(t, fake, nil, item)
	h.admit(t, "user", "run tool", session.DeliverySteer)
	if err := h.runner.drainOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if turns := len(fake.Requests()); turns != 2 {
		t.Fatalf("provider turns = %d, want 2", turns)
	}
}

// mutableSource is a system context source whose observed value changes after
// a configured number of observations, simulating an AGENTS.md file that was
// edited mid-session. It lets a test reproduce the effect of ReconcileContext
// appending a system message between a tool result and the next provider turn.
type mutableSource struct {
	key    string
	count  atomic.Int32
	flipAt int32
}

func (m *mutableSource) Key() string { return m.key }
func (m *mutableSource) Observe(context.Context) (systemcontext.Observation, error) {
	n := m.count.Add(1)
	text := "baseline"
	if n >= m.flipAt {
		text = "changed"
	}
	raw, _ := json.Marshal(text)
	return systemcontext.Observation{Available: true, Value: raw, Baseline: text, Update: "context changed to " + text}, nil
}

// newRunnerHarnessWithSource builds a runner harness like newRunnerHarness but
// with a custom system context source, so a test can drive ReconcileContext.
func newRunnerHarnessWithSource(t *testing.T, fake *fakeProvider, profiles []Profile, source systemcontext.Source, tools ...tool.Tool) *runnerHarness {
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
	providersList := make([]tool.ToolProvider, 0, len(tools))
	for _, item := range tools {
		item := item
		providersList = append(providersList, &tool.ProviderFunc{ToolDescriptor: tool.DescriptorOf(item), CreateTool: func(tool.SessionState) (tool.Tool, error) { return item, nil }})
	}
	toolProviders, err := tool.NewProviders(providersList...)
	if err != nil {
		t.Fatal(err)
	}
	contextRegistry, _ := systemcontext.NewRegistry(source)
	agentSessions, err := NewUserSession(ctx, UserSessionConfig{AgentSession: AgentSessionConfig{
		Sessions:           sessions,
		Contexts:           systemcontext.Manager{Registry: contextRegistry, Store: sessions},
		StateDirectories:   testSessionStateDirectories(t),
		Agents:             agents,
		Providers:          providers,
		ToolProviders:      toolProviders,
		Goals:              goals,
		MaxConcurrentTools: 2,
		CleanupTimeout:     time.Second,
	}, MaxConcurrentChildTurns: 8, MaxConcurrentChildTurnsPerParent: 4})
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := agentSessions.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	runner := runtime.(*agentSession)
	sessionDB, err := db.Session(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	return &runnerHarness{db: sessionDB, sessions: sessions, goals: goals, repository: repository, sessionID: created.ID, runner: runner}
}

// TestRunnerSendsSingleToolResultAfterContextReconcile reproduces the bug
// where a ReconcileContext system message appended after a single tool result
// makes the last history role "system", failing the ready check and stopping
// the drain before the tool result is sent to the model.
func TestRunnerSendsSingleToolResultAfterContextReconcile(t *testing.T) {
	item := &fakeTool{id: "read"}
	fake := &fakeProvider{stream: func(index int, _ context.Context, request protocol.Request) (provider.Stream, error) {
		if index == 0 {
			call := protocol.ToolCall{ID: "call-1", Name: "read", Input: json.RawMessage(`{}`)}
			return events(
				protocol.Event{Type: protocol.EventToolCallComplete, ToolCall: &call},
				protocol.Event{Type: protocol.EventFinish, FinishReason: protocol.FinishToolCalls},
			), nil
		}
		var sawToolResult bool
		for _, message := range request.Messages {
			for _, part := range message.Content {
				if part.Type == protocol.ContentToolResult && part.ToolCallID == "call-1" {
					sawToolResult = true
				}
			}
		}
		if !sawToolResult {
			return nil, errors.New("continuation request did not include the tool result")
		}
		return events(protocol.Event{Type: protocol.EventFinish, FinishReason: protocol.FinishStop}), nil
	}}
	// flip the context source after the first observation (initialize is the
	// first; reconcile on the tool-result turn is the second, which appends a
	// system message after the tool result).
	source := &mutableSource{key: "agent:context", flipAt: 2}
	h := newRunnerHarnessWithSource(t, fake, nil, source, item)
	h.admit(t, "user", "run tool", session.DeliverySteer)
	if err := h.runner.drainOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if turns := len(fake.Requests()); turns != 2 {
		t.Fatalf("provider turns = %d, want 2 (tool result not sent after context reconcile)", turns)
	}
}
