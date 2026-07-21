package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/amirulashraf/parrot-coder/internal/config"
	"github.com/amirulashraf/parrot-coder/internal/permission"
)

// SetConfigTool modifies a single key in the global parrot.yaml configuration
// file. Every invocation triggers a user permission dialog so the user is
// always aware of configuration changes.
type SetConfigTool struct {
	BasePresentation
	ConfigDir string
}

type setConfigInput struct {
	Key       string `json:"key"`
	Value     string `json:"value"`
	Operation string `json:"operation"`
}

func NewSetConfigTool(configDir string) *SetConfigTool {
	return &SetConfigTool{ConfigDir: configDir}
}

func (*SetConfigTool) ID() string { return "set_config" }

// PermissionChoices omits every scoped allow because modifying persistent
// configuration should never be auto-approved for a session or workspace.
func (*SetConfigTool) PermissionChoices() []permission.Choice {
	return []permission.Choice{
		{Value: "set", Decision: "allow", Label: "set", Description: "Allow this config change once"},
		{Value: "reject", Decision: "deny", Label: "reject", Description: "Reject this request"},
		{Value: "reject with reason", Decision: "deny", Label: "reject with reason", Description: "Reject and provide feedback to the agent", RequiresReason: true},
	}
}

func (*SetConfigTool) Description() string {
	return "Set, delete, append to, or remove from a configuration value in the global parrot.yaml file. Operates on one key at a time. Always triggers a permission dialog."
}

func (*SetConfigTool) JSONSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"key":{"type":"string","description":"Dotted config path (e.g. model, providers.openai, web_fetch.allow_private, formatters.gofmt.extensions)"},"value":{"type":"string","description":"YAML value for the operation. For 'set': the new value (scalar, inline mapping, or inline sequence). For 'append': element to add. For 'remove': element to remove. Omit for 'delete'."},"operation":{"type":"string","enum":["set","delete","append","remove"],"description":"Operation to perform: set (replace value), delete (remove key), append (add to array), remove (remove from array)"}},"required":["key","operation"],"additionalProperties":false}`)
}

func (t *SetConfigTool) DescribeRequest(raw json.RawMessage) (string, error) {
	var input setConfigInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return "", err
	}
	if input.Operation == "" {
		input.Operation = "set"
	}
	switch input.Operation {
	case "set":
		return fmt.Sprintf("Set config key %q to %q in global parrot.yaml", input.Key, input.Value), nil
	case "delete":
		return fmt.Sprintf("Delete config key %q from global parrot.yaml", input.Key), nil
	case "append":
		return fmt.Sprintf("Append %q to array at %q in global parrot.yaml", input.Value, input.Key), nil
	case "remove":
		return fmt.Sprintf("Remove %q from array at %q in global parrot.yaml", input.Value, input.Key), nil
	default:
		return "", fmt.Errorf("set_config: unsupported operation %q", input.Operation)
	}
}

func (t *SetConfigTool) Plan(_ context.Context, raw json.RawMessage, _ CallContext) (Plan, error) {
	if t.ConfigDir == "" {
		return Plan{}, errors.New("set_config: config directory is not available")
	}
	var input setConfigInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return Plan{}, err
	}
	if input.Key == "" {
		return Plan{}, errors.New("set_config: key is required")
	}
	if input.Operation == "" {
		input.Operation = "set"
	}

	// Validate operation and value combination.
	switch input.Operation {
	case "set", "append", "remove":
		if input.Value == "" {
			return Plan{}, fmt.Errorf("set_config: value is required for %q operation", input.Operation)
		}
	case "delete":
		// value is not needed
	default:
		return Plan{}, fmt.Errorf("set_config: unsupported operation %q", input.Operation)
	}

	configPath := filepath.Join(t.ConfigDir, config.FileName)
	review, _ := json.Marshal(map[string]string{"key": input.Key, "value": input.Value, "operation": input.Operation, "path": configPath})
	request, err := permission.NewRequest(t.ID(), raw, []permission.Resource{
		{Kind: "configuration", Identifier: input.Key, Operation: input.Operation, Attributes: map[string]string{"path": configPath}},
	}, review)
	if err != nil {
		return Plan{}, err
	}
	return NewPlan(t.ID(), raw, []permission.Request{request}, review, input)
}

func (t *SetConfigTool) Execute(_ context.Context, plan Plan, call CallContext) (Result, error) {
	if call.SecurityProfile != nil && call.SecurityProfile.IsReadOnly() {
		return Result{}, errors.New("set_config is not permitted by the current security profile")
	}
	input, ok := plan.Data.(setConfigInput)
	if !ok {
		return Result{}, errors.New("set_config: incompatible plan")
	}

	configPath := filepath.Join(t.ConfigDir, config.FileName)

	// Create a backup before modifying.
	if err := config.BackupConfig(configPath); err != nil {
		return Result{}, fmt.Errorf("set_config: backup config: %w", err)
	}

	op := config.ConfigOp(input.Operation)
	if err := config.UpdateConfigField(configPath, input.Key, input.Value, op); err != nil {
		return Result{}, fmt.Errorf("set_config: %w", err)
	}

	text := fmt.Sprintf("Applied %q operation on config key %q", input.Operation, input.Key)
	if input.Value != "" {
		text = fmt.Sprintf("%s with value %q", text, input.Value)
	}
	return Result{Text: text, ModelText: text}, nil
}
