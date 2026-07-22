package tool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSetConfigTool_Plan_RejectsMissingKey(t *testing.T) {
	tool := NewSetConfigTool("/tmp")
	_, err := tool.Plan(context.Background(), json.RawMessage(`{"key":"","value":"x"}`), CallContext{})
	if err == nil {
		t.Fatal("expected error for empty key")
	}
}

func TestSetConfigTool_Plan_RejectsMissingValueForSet(t *testing.T) {
	tool := NewSetConfigTool("/tmp")
	_, err := tool.Plan(context.Background(), json.RawMessage(`{"key":"model","value":"","operation":"set"}`), CallContext{})
	if err == nil {
		t.Fatal("expected error for empty value with set operation")
	}
}

func TestSetConfigTool_Plan_RejectsMissingValueForAppend(t *testing.T) {
	tool := NewSetConfigTool("/tmp")
	_, err := tool.Plan(context.Background(), json.RawMessage(`{"key":"model","value":"","operation":"append"}`), CallContext{})
	if err == nil {
		t.Fatal("expected error for empty value with append operation")
	}
}

func TestSetConfigTool_Plan_RejectsMissingValueForRemove(t *testing.T) {
	tool := NewSetConfigTool("/tmp")
	_, err := tool.Plan(context.Background(), json.RawMessage(`{"key":"model","value":"","operation":"remove"}`), CallContext{})
	if err == nil {
		t.Fatal("expected error for empty value with remove operation")
	}
}

func TestSetConfigTool_Plan_AcceptsDeleteWithoutValue(t *testing.T) {
	tool := NewSetConfigTool("/tmp")
	plan, err := tool.Plan(context.Background(), json.RawMessage(`{"key":"model","operation":"delete"}`), CallContext{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(plan.Permissions) == 0 {
		t.Fatal("expected permission request")
	}
}

func TestSetConfigTool_Plan_AlwaysRequestsPermission(t *testing.T) {
	tool := NewSetConfigTool("/tmp")
	plan, err := tool.Plan(context.Background(), json.RawMessage(`{"key":"model","value":"openai/gpt-4","operation":"set"}`), CallContext{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(plan.Permissions) == 0 {
		t.Fatal("set_config must always request permission")
	}
}

func TestSetConfigTool_Plan_DefaultsToSetOperation(t *testing.T) {
	tool := NewSetConfigTool("/tmp")
	plan, err := tool.Plan(context.Background(), json.RawMessage(`{"key":"model","value":"openai/gpt-4"}`), CallContext{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(plan.Permissions) == 0 {
		t.Fatal("expected permission request")
	}
}

func TestSetConfigTool_Plan_RejectsInvalidOperation(t *testing.T) {
	tool := NewSetConfigTool("/tmp")
	_, err := tool.Plan(context.Background(), json.RawMessage(`{"key":"model","value":"x","operation":"invalid"}`), CallContext{})
	if err == nil {
		t.Fatal("expected error for invalid operation")
	}
}

func TestSetConfigTool_Plan_RejectsEmptyConfigDir(t *testing.T) {
	tool := NewSetConfigTool("")
	_, err := tool.Plan(context.Background(), json.RawMessage(`{"key":"model","value":"openai/gpt-4"}`), CallContext{})
	if err == nil {
		t.Fatal("expected error for empty config directory")
	}
}

func TestSetConfigTool_PermissionChoices_OfferAllowAndDeny(t *testing.T) {
	choices := NewSetConfigTool("/tmp").PermissionChoices()
	var allow, deny int
	for _, c := range choices {
		switch c.Decision {
		case "allow":
			allow++
		case "deny":
			deny++
		default:
			t.Fatalf("permission choice %q has decision %q", c.Label, c.Decision)
		}
	}
	if allow != 1 || deny != 2 {
		t.Fatalf("choices = %#v", choices)
	}
}

func TestSetConfigTool_Execute_RejectsReadOnlyProfile(t *testing.T) {
	tool := NewSetConfigTool("/tmp")
	input, _ := json.Marshal(map[string]string{"key": "model", "value": "openai/gpt-4", "operation": "set"})
	plan, err := tool.Plan(context.Background(), input, CallContext{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = tool.Execute(context.Background(), plan, CallContext{
		SecurityProfile: readOnlyProfile{},
	})
	if err == nil {
		t.Fatal("expected error for read-only profile")
	}
}

func TestSetConfigTool_DescribeRequest_Set(t *testing.T) {
	tool := NewSetConfigTool("/tmp")
	desc, err := tool.DescribeRequest(json.RawMessage(`{"key":"model","value":"openai/gpt-4","operation":"set"}`))
	if err != nil {
		t.Fatal(err)
	}
	if desc == "" {
		t.Fatal("expected non-empty description")
	}
}

func TestSetConfigTool_DescribeRequest_Delete(t *testing.T) {
	tool := NewSetConfigTool("/tmp")
	desc, err := tool.DescribeRequest(json.RawMessage(`{"key":"providers.openai","operation":"delete"}`))
	if err != nil {
		t.Fatal(err)
	}
	if desc == "" {
		t.Fatal("expected non-empty description")
	}
}

func TestSetConfigTool_DescribeRequest_Append(t *testing.T) {
	tool := NewSetConfigTool("/tmp")
	desc, err := tool.DescribeRequest(json.RawMessage(`{"key":"formatters.gofmt.extensions","value":"py","operation":"append"}`))
	if err != nil {
		t.Fatal(err)
	}
	if desc == "" {
		t.Fatal("expected non-empty description")
	}
}

func TestSetConfigTool_DescribeRequest_Remove(t *testing.T) {
	tool := NewSetConfigTool("/tmp")
	desc, err := tool.DescribeRequest(json.RawMessage(`{"key":"formatters.gofmt.extensions","value":"go","operation":"remove"}`))
	if err != nil {
		t.Fatal(err)
	}
	if desc == "" {
		t.Fatal("expected non-empty description")
	}
}

func TestSetConfigTool_Integration_ExecuteSet(t *testing.T) {
	configDir := t.TempDir()
	configPath := filepath.Join(configDir, "parrot.yaml")
	if err := os.WriteFile(configPath, []byte("model: openai/gpt-4\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	tool := NewSetConfigTool(configDir)
	input, _ := json.Marshal(map[string]string{"key": "model", "value": "anthropic/claude-3", "operation": "set"})
	plan, err := tool.Plan(context.Background(), input, CallContext{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = tool.Execute(context.Background(), plan, CallContext{})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	data, _ := os.ReadFile(configPath)
	if !strings.Contains(string(data), "claude-3") {
		t.Fatalf("expected claude-3 in content, got: %q", string(data))
	}

	// Verify backup file exists.
	if _, err := os.Stat(configPath + ".bak"); err != nil {
		t.Fatal("backup file was not created")
	}
}

func TestSetConfigTool_Integration_ExecuteSetProvider(t *testing.T) {
	configDir := t.TempDir()
	configPath := filepath.Join(configDir, "parrot.yaml")
	if err := os.WriteFile(configPath, []byte("model: openai/gpt-4\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	tool := NewSetConfigTool(configDir)
	input, _ := json.Marshal(map[string]string{"key": "providers.openai", "value": "{base_url: https://api.openai.com/v1, api_key_env: OPENAI_KEY}", "operation": "set"})
	plan, err := tool.Plan(context.Background(), input, CallContext{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = tool.Execute(context.Background(), plan, CallContext{})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	data, _ := os.ReadFile(configPath)
	content := string(data)
	if !strings.Contains(content, "providers") || !strings.Contains(content, "api.openai.com") {
		t.Fatalf("expected provider config, got: %q", content)
	}
}

func TestSetConfigTool_Integration_ExecuteDelete(t *testing.T) {
	configDir := t.TempDir()
	configPath := filepath.Join(configDir, "parrot.yaml")
	if err := os.WriteFile(configPath, []byte("model: openai/gpt-4\nother: value\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	tool := NewSetConfigTool(configDir)
	input, _ := json.Marshal(map[string]string{"key": "model", "operation": "delete"})
	plan, err := tool.Plan(context.Background(), input, CallContext{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = tool.Execute(context.Background(), plan, CallContext{})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	data, _ := os.ReadFile(configPath)
	if strings.Contains(string(data), "model:") {
		t.Fatalf("model key should have been deleted, got: %q", string(data))
	}
}

func TestSetConfigTool_Integration_ExecuteAppend(t *testing.T) {
	configDir := t.TempDir()
	configPath := filepath.Join(configDir, "parrot.yaml")
	if err := os.WriteFile(configPath, []byte("formatters:\n  gofmt:\n    extensions: [go]\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	tool := NewSetConfigTool(configDir)
	input, _ := json.Marshal(map[string]string{"key": "formatters.gofmt.extensions", "value": "py", "operation": "append"})
	plan, err := tool.Plan(context.Background(), input, CallContext{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = tool.Execute(context.Background(), plan, CallContext{})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	data, _ := os.ReadFile(configPath)
	if !strings.Contains(string(data), "py") {
		t.Fatalf("expected py in array, got: %q", string(data))
	}
}

func TestSetConfigTool_Integration_ExecuteRemove(t *testing.T) {
	configDir := t.TempDir()
	configPath := filepath.Join(configDir, "parrot.yaml")
	if err := os.WriteFile(configPath, []byte("formatters:\n  gofmt:\n    extensions: [go, py]\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	tool := NewSetConfigTool(configDir)
	input, _ := json.Marshal(map[string]string{"key": "formatters.gofmt.extensions", "value": "py", "operation": "remove"})
	plan, err := tool.Plan(context.Background(), input, CallContext{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = tool.Execute(context.Background(), plan, CallContext{})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	data, _ := os.ReadFile(configPath)
	if strings.Contains(string(data), "py") {
		t.Fatalf("py should have been removed from array, got: %q", string(data))
	}
	// Verify the backup was created.
	if _, err := os.Stat(configPath + ".bak"); err != nil {
		t.Fatal("backup file was not created")
	}
}

// readOnlyProfile implements SecurityProfile for testing.
type readOnlyProfile struct{}

func (readOnlyProfile) IsReadOnly() bool { return true }
func (readOnlyProfile) AllowReadPaths() []string  { return nil }
func (readOnlyProfile) AllowWritePaths() []string { return nil }
func (readOnlyProfile) DenyWritePaths() []string  { return nil }
