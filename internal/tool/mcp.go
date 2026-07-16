package tool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/amirulashraf/parrot-coder/internal/mcp"
	"github.com/amirulashraf/parrot-coder/internal/permission"
	"strings"
)

type MCPCaller interface {
	CallTool(context.Context, string, json.RawMessage) (mcp.ToolResult, error)
}

type MCPTool struct {
	definition mcp.ToolDefinition
	caller     MCPCaller
	schema     json.RawMessage
}

func NewMCPTool(caller MCPCaller, definition mcp.ToolDefinition) (*MCPTool, error) {
	if caller == nil || definition.Name == "" || definition.Server == "" || definition.Tool == "" {
		return nil, errors.New("mcp tool: caller and complete definition are required")
	}
	var object map[string]any
	if err := json.Unmarshal(definition.InputSchema, &object); err != nil || object["type"] != "object" {
		return nil, fmt.Errorf("mcp tool %s: input schema must be an object schema", definition.Name)
	}
	if _, ok := object["additionalProperties"]; !ok {
		object["additionalProperties"] = false
	}
	schema, err := json.Marshal(object)
	if err != nil {
		return nil, err
	}
	definition.InputSchema = append(json.RawMessage(nil), schema...)
	return &MCPTool{definition: definition, caller: caller, schema: schema}, nil
}
func (t *MCPTool) ID() string { return t.definition.Name }
func (t *MCPTool) Description() string {
	return fmt.Sprintf("MCP tool %s/%s. %s", t.definition.Server, t.definition.Tool, t.definition.Description)
}
func (t *MCPTool) DescribeRequest(json.RawMessage) (string, error) {
	return fmt.Sprintf("Call MCP tool %s/%s", t.definition.Server, t.definition.Tool), nil
}
func (t *MCPTool) JSONSchema() json.RawMessage { return append(json.RawMessage(nil), t.schema...) }
func (t *MCPTool) Plan(_ context.Context, raw json.RawMessage, _ CallContext) (Plan, error) {
	canonical, err := permission.CanonicalJSON(raw)
	if err != nil {
		return Plan{}, err
	}
	digest := sha256.Sum256(canonical)
	hash := hex.EncodeToString(digest[:])
	review, _ := json.Marshal(map[string]any{"server": t.definition.Server, "tool": t.definition.Tool, "arguments_sha256": hash})
	request, err := permission.NewRequest(t.ID(), raw, []permission.Resource{{Kind: "mcp", Identifier: t.definition.Server + "/" + t.definition.Tool, Operation: "call", Attributes: map[string]string{"arguments_sha256": hash}}}, review)
	if err != nil {
		return Plan{}, err
	}
	return NewPlan(t.ID(), raw, []permission.Request{request}, review, mcpPlan{arguments: canonical, hash: hash})
}
func (t *MCPTool) Execute(ctx context.Context, plan Plan, _ CallContext) (Result, error) {
	planned, ok := plan.Data.(mcpPlan)
	if !ok {
		return Result{}, errors.New("mcp tool: incompatible plan")
	}
	digest := sha256.Sum256(planned.arguments)
	if hex.EncodeToString(digest[:]) != planned.hash {
		return Result{}, errors.New("mcp tool: arguments changed after planning")
	}
	result, err := t.caller.CallTool(ctx, t.definition.Name, planned.arguments)
	if err != nil {
		return Result{}, err
	}
	var text strings.Builder
	types := make([]string, 0, len(result.Content))
	for _, content := range result.Content {
		types = append(types, content.Type)
		if content.Type == "text" && content.Text != "" {
			if text.Len() != 0 {
				text.WriteByte('\n')
			}
			text.WriteString(content.Text)
		}
	}
	metadata := map[string]any{"server": t.definition.Server, "tool": t.definition.Tool, "content_types": types, "truncated": result.Truncated}
	if len(result.StructuredContent) != 0 {
		metadata["structured_content"] = json.RawMessage(append([]byte(nil), result.StructuredContent...))
	}
	return Result{Text: text.String(), Metadata: metadata}, nil
}

type mcpPlan struct {
	arguments json.RawMessage
	hash      string
}
