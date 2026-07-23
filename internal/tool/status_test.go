package tool

import (
	"context"
	"encoding/json"
	"testing"

	statusinfo "github.com/amirulashraf/parrot-coder/internal/status"
)

func TestStatusToolReturnsSelectionAndProfileStatus(t *testing.T) {
	registry, err := statusinfo.NewRegistry(statusinfo.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	item := NewStatusTool(registry)
	plan, err := item.Plan(context.Background(), json.RawMessage(`{}`), CallContext{})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Permissions) != 0 || item.SystemPromptGuidance() != "" {
		t.Fatalf("status tool unexpectedly affects permissions or prompt: %#v", plan)
	}
	result, err := item.Execute(context.Background(), plan, CallContext{
		StatusQuery:    statusinfo.Query{SessionID: "session", Agent: "plan", Provider: "openai", Model: "gpt", Variant: "high"},
		StatusProvider: statusinfo.Static{ProviderKey: "profile:plan", Text: "Plan mode is read-only."},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "Plan mode is read-only.\n\nActive profile: plan\nModel: openai/gpt\nVariant: high"
	if result.Text != want || result.ModelText != want {
		t.Fatalf("result = %#v", result)
	}
}
