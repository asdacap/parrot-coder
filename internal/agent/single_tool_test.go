package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
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
// a configured number of observations, simulating an AGENTS.md file edited
// between provider turns.
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
	return systemcontext.Observation{Available: true, Text: text}, nil
}

// newRunnerHarnessWithSource builds a runner harness like newRunnerHarness but
// with a custom system context source.
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
	if err := sessions.GetSession(created.ID).SetSelection(ctx, session.Selection{Agent: profile, Model: "fake/model"}); err != nil {
		t.Fatal(err)
	}
	providers, err := NewProviderRegistry(fake)
	if err != nil {
		t.Fatal(err)
	}
	providersList := make([]tool.ToolProvider, 0, len(tools))
	for _, item := range tools {
		item := item
		providersList = append(providersList, &tool.ProviderFunc{ToolDescriptor: tool.DescriptorOf(item), CreateTool: func(tool.AgentSession) (tool.Tool, error) { return item, nil }})
	}
	toolProviders, err := tool.NewProviders(providersList...)
	if err != nil {
		t.Fatal(err)
	}
	contextRegistry, _ := systemcontext.NewRegistry(source)
	agentSessions, err := NewUserSession(ctx, sessions, contextRegistry, nil, UserSessionConfig{AgentSession: AgentSessionConfig{
		MaxConcurrentTools: 2,
		CleanupTimeout:     time.Second,
	}, MaxConcurrentChildTurns: 8, MaxConcurrentChildTurnsPerParent: 4},
		testSessionStateDirectories(t), agents, providers, toolProviders, nil, nil, nil, nil, nil, nil, nil, nil, goals, nil, nil, nil,
		nil, nil, nil, nil, nil)
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

// TestRunnerRefreshesSystemContextWhileContinuingToolResult verifies each
// provider turn gets fresh system context without interrupting tool continuation.
func TestRunnerRefreshesSystemContextWhileContinuingToolResult(t *testing.T) {
	item := &fakeTool{id: "read"}
	fake := &fakeProvider{stream: func(index int, _ context.Context, request protocol.Request) (provider.Stream, error) {
		if index == 0 {
			if !strings.Contains(request.Instructions, "baseline") || strings.Contains(request.Instructions, "changed") {
				return nil, errors.New("first request did not contain baseline system context")
			}
			call := protocol.ToolCall{ID: "call-1", Name: "read", Input: json.RawMessage(`{}`)}
			return events(
				protocol.Event{Type: protocol.EventToolCallComplete, ToolCall: &call},
				protocol.Event{Type: protocol.EventFinish, FinishReason: protocol.FinishToolCalls},
			), nil
		}
		if !strings.Contains(request.Instructions, "changed") || strings.Contains(request.Instructions, "baseline") {
			return nil, errors.New("second request did not contain changed system context")
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
	// Flip the source after the first provider turn observes it.
	source := &mutableSource{key: "agent:context", flipAt: 2}
	h := newRunnerHarnessWithSource(t, fake, nil, source, item)
	h.admit(t, "user", "run tool", session.DeliverySteer)
	if err := h.runner.drainOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if turns := len(fake.Requests()); turns != 2 {
		t.Fatalf("provider turns = %d, want 2", turns)
	}
	if observations := source.count.Load(); observations != int32(len(fake.Requests())) {
		t.Fatalf("system context observations = %d, provider turns = %d", observations, len(fake.Requests()))
	}
}
