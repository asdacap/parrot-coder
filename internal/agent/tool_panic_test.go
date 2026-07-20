package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/amirulashraf/parrot-coder/internal/protocol"
	"github.com/amirulashraf/parrot-coder/internal/tool"
)

// panicTool panics from either Plan or Execute to model a tool that hits an
// unexpected defect at runtime (for example a bad apply_patch edge case).
type panicTool struct {
	tool.BasePresentation
	tool.WritableTool
	inExecute bool
}

func (*panicTool) ID() string                                      { return "boom" }
func (*panicTool) Description() string                             { return "panics for testing" }
func (*panicTool) DescribeRequest(json.RawMessage) (string, error) { return "Panic", nil }
func (*panicTool) JSONSchema() json.RawMessage                     { return json.RawMessage(`{"type":"object"}`) }

func (p *panicTool) Plan(context.Context, json.RawMessage, tool.CallContext) (tool.Plan, error) {
	if !p.inExecute {
		panic("plan exploded")
	}
	return tool.NewPlan(p.ID(), json.RawMessage(`{}`), nil, nil, struct{}{})
}

func (p *panicTool) Execute(context.Context, tool.Plan, tool.CallContext) (tool.Result, error) {
	panic("execute exploded")
}

func TestExecuteToolCallRecoversPanic(t *testing.T) {
	for _, tc := range []struct {
		name      string
		inExecute bool
	}{
		{"panic in plan", false},
		{"panic in execute", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			registry := tool.NewRegistry()
			if err := registry.Register(&panicTool{inExecute: tc.inExecute}); err != nil {
				t.Fatal(err)
			}
			executor := tool.Executor{Snapshot: registry.Materialize()}
			call := completedCall{call: protocol.ToolCall{ID: "c1", Name: "boom", Input: json.RawMessage(`{}`)}}

			var loggedStack []byte
			var loggedRecovered any
			onPanic := func(recovered any, stack []byte) {
				loggedRecovered = recovered
				loggedStack = stack
			}

			// The recovery must turn the panic into an error instead of
			// letting it unwind and terminate the test process.
			result, err := executeToolCall(context.Background(), executor, call, tool.CallContext{}, onPanic)
			if err == nil {
				t.Fatal("expected an error from a panicking tool, got nil")
			}
			if !strings.Contains(err.Error(), "panicked") {
				t.Fatalf("error should identify the panic, got: %v", err)
			}
			if result.Text != "" {
				t.Fatalf("expected empty result on panic, got %q", result.Text)
			}
			if loggedRecovered == nil {
				t.Fatal("panic logger was not invoked")
			}
			if !strings.Contains(string(loggedStack), "runtime/debug.Stack") {
				t.Fatalf("logger should receive the panic stack, got: %s", loggedStack)
			}
		})
	}
}
