package httpapi

import (
	"context"
	"testing"

	v1 "github.com/amirulashraf/parrot-coder/internal/api/v1"
	"github.com/amirulashraf/parrot-coder/internal/cli/chatview"
	managedtask "github.com/amirulashraf/parrot-coder/internal/task"
	"github.com/amirulashraf/parrot-coder/internal/tool"
)

// The declarative label engine replaced a switch over tool IDs. This asserts it
// reproduces that switch exactly for every tool which declares a label, so the
// abstraction is provably behaviour-preserving rather than merely plausible.
func TestDeclaredPresentationReproducesLegacyLabels(t *testing.T) {
	registry := tool.NewRegistry()
	for _, item := range []tool.Tool{
		tool.NewReadTool(tool.ReadConfig{}),
		tool.NewGlobTool(tool.GlobConfig{}),
		tool.NewRgTool(tool.RgConfig{}),
		tool.NewApplyPatchTool(nil),
		tool.NewExecCommandTool(nil),
		tool.NewWriteStdinTool(nil),
		tool.NewTodoWriteTool(nil),
		tool.NewQuestionTool(nil),
		tool.NewSkillTool(nil),
		tool.NewWebFetchTool(nil),
	} {
		if err := registry.Register(item); err != nil {
			t.Fatalf("register %s: %v", item.ID(), err)
		}
	}
	backend := &DomainBackend{Tools: registry.Materialize()}
	list, err := backend.ListTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	presentations := chatview.NewPresentations(list)
	for _, item := range list.Items {
		if item.ID == "question" && item.Presentation.CompletedLabel != "answers" {
			t.Fatalf("question completed label presentation = %#v", item.Presentation)
		}
	}

	input := map[string]any{
		"path": "src/main.go", "file": "src/main.go", "pattern": "needle", "id": "out_1",
		"patchText": "", "command": "go build ./...", "cmd": "go test ./...",
		"process_id": "proc_1", "chars": "hello", "name": "deploy", "url": "https://example.com",
		"todos":     []any{map[string]any{"content": "a"}, map[string]any{"content": "b"}},
		"questions": []any{map[string]any{"header": "Pick"}, map[string]any{"header": "Other"}},
	}
	for _, item := range list.Items {
		t.Run(item.ID, func(t *testing.T) {
			redacted := presentations.Redact(item.ID, input)
			want := chatview.ToolActivityLabel(item.ID, chatview.RedactToolInputForDisplay(item.ID, input))
			if got := presentations.Label(item.ID, redacted); got != want {
				t.Fatalf("label = %q, want %q", got, want)
			}
		})
	}
}

func TestSuccessIconPresentationSurvivesToolAPIProjection(t *testing.T) {
	presentation := toolPresentationDTO(tool.AgentDescriptor("agent_spawn").Presentation)
	declared := chatview.NewPresentations(v1.ToolList{Items: []v1.Tool{{ID: "agent_spawn", Presentation: presentation}}})
	if presentation.SuccessIcon != "♟" || declared.SuccessIcon("agent_spawn") != "♟" {
		t.Fatalf("agent_spawn presentation = %#v", presentation)
	}
}

func TestModelinePresentationSurvivesToolAPIProjection(t *testing.T) {
	registry := tool.NewRegistry()
	if err := registry.Register(&tool.WaitTool{Kind: managedtask.KindAgent}); err != nil {
		t.Fatal(err)
	}
	list, err := (&DomainBackend{Tools: registry.Materialize()}).ListTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 || !list.Items[0].Presentation.Modeline || !list.Items[0].Presentation.LiveOnly || !chatview.NewPresentations(list).Modeline("wait_agent") || !chatview.NewPresentations(list).LiveOnly("wait_agent") {
		t.Fatalf("wait_agent presentation = %#v", list.Items)
	}
}

// What a tool declares must match what the retired ID switch did, and the
// switch must survive as the fallback for a server which declares nothing.
// Together these mean neither a current nor an older server changes behaviour.
func TestDeclaredRenderingMatchesLegacyAndFallsBack(t *testing.T) {
	registry := tool.NewRegistry()
	for _, item := range []tool.Tool{
		tool.NewTodoWriteTool(nil),
		tool.NewExecCommandTool(nil), tool.NewWriteStdinTool(nil), tool.NewReadTool(tool.ReadConfig{}),
	} {
		if err := registry.Register(item); err != nil {
			t.Fatal(err)
		}
	}
	list, err := (&DomainBackend{Tools: registry.Materialize()}).ListTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	declared := chatview.NewPresentations(list)
	var empty chatview.Presentations

	for _, test := range []struct{ id, result, output string }{
		{id: "todowrite", result: "todos"},
		{id: "exec_command", output: "tail"},
		{id: "write_stdin", output: "none"},
		{id: "read"},
	} {
		t.Run(test.id, func(t *testing.T) {
			if got := declared.Result(test.id); got != test.result {
				t.Errorf("declared result = %q, want %q", got, test.result)
			}
			if got := declared.Output(test.id); got != test.output {
				t.Errorf("declared output = %q, want %q", got, test.output)
			}
			// An undeclared tool must render identically via the fallback.
			if got := empty.Result(test.id); got != test.result {
				t.Errorf("fallback result = %q, want %q", got, test.result)
			}
			if got := empty.Output(test.id); got != test.output {
				t.Errorf("fallback output = %q, want %q", got, test.output)
			}
		})
	}
}
