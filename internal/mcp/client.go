package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

type protocolClient struct {
	config       Config
	endpoint     endpoint
	protocol     string
	serverInfo   Implementation
	capabilities Capabilities
}

func initialize(ctx context.Context, config Config, transport endpoint) (*protocolClient, error) {
	params := map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo": Implementation{
			Name:    "parrot-coder",
			Version: "1",
		},
	}
	raw, err := transport.call(ctx, "initialize", params)
	if err != nil {
		return nil, fmt.Errorf("mcp: initialize %s: %w", config.Name, err)
	}
	var result struct {
		ProtocolVersion string         `json:"protocolVersion"`
		Capabilities    Capabilities   `json:"capabilities"`
		ServerInfo      Implementation `json:"serverInfo"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, errors.New("mcp: invalid initialize result")
	}
	if result.ProtocolVersion != ProtocolVersion && result.ProtocolVersion != "2025-03-26" {
		return nil, fmt.Errorf("mcp: unsupported negotiated protocol version %q", result.ProtocolVersion)
	}
	if result.ServerInfo.Name == "" || result.ServerInfo.Version == "" {
		return nil, errors.New("mcp: initialize result is missing serverInfo")
	}
	transport.setProtocolVersion(result.ProtocolVersion)
	if err := transport.notify(ctx, "notifications/initialized", map[string]any{}); err != nil {
		return nil, fmt.Errorf("mcp: send initialized notification: %w", err)
	}
	return &protocolClient{
		config:       config,
		endpoint:     transport,
		protocol:     result.ProtocolVersion,
		serverInfo:   result.ServerInfo,
		capabilities: result.Capabilities,
	}, nil
}

func (c *protocolClient) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	callCtx, cancel := context.WithTimeout(ctx, c.config.CallTimeout)
	defer cancel()
	return c.endpoint.call(callCtx, method, params)
}

func (c *protocolClient) listTools(ctx context.Context) ([]Tool, error) {
	var output []Tool
	err := c.paginate(ctx, "tools/list", func(raw json.RawMessage) (string, int, error) {
		var page struct {
			Tools      []Tool `json:"tools"`
			NextCursor string `json:"nextCursor"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return "", 0, errors.New("mcp: invalid tools/list result")
		}
		for i := range page.Tools {
			tool := &page.Tools[i]
			if tool.Name == "" || len(tool.Name) > 256 || strings.IndexByte(tool.Name, 0) >= 0 || !validJSONObject(tool.InputSchema) {
				return "", 0, errors.New("mcp: tools/list returned an invalid tool")
			}
			if len(tool.OutputSchema) != 0 && !validJSONObject(tool.OutputSchema) {
				return "", 0, errors.New("mcp: tools/list returned an invalid output schema")
			}
			tool.InputSchema = append(json.RawMessage(nil), tool.InputSchema...)
			tool.OutputSchema = append(json.RawMessage(nil), tool.OutputSchema...)
		}
		output = append(output, page.Tools...)
		return page.NextCursor, len(page.Tools), nil
	})
	return output, err
}

func (c *protocolClient) listPrompts(ctx context.Context) ([]Prompt, error) {
	var output []Prompt
	err := c.paginate(ctx, "prompts/list", func(raw json.RawMessage) (string, int, error) {
		var page struct {
			Prompts    []Prompt `json:"prompts"`
			NextCursor string   `json:"nextCursor"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return "", 0, errors.New("mcp: invalid prompts/list result")
		}
		for _, prompt := range page.Prompts {
			if prompt.Name == "" {
				return "", 0, errors.New("mcp: prompts/list returned a prompt without a name")
			}
		}
		output = append(output, page.Prompts...)
		return page.NextCursor, len(page.Prompts), nil
	})
	return output, err
}

func (c *protocolClient) listResources(ctx context.Context) ([]Resource, error) {
	var output []Resource
	err := c.paginate(ctx, "resources/list", func(raw json.RawMessage) (string, int, error) {
		var page struct {
			Resources  []Resource `json:"resources"`
			NextCursor string     `json:"nextCursor"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return "", 0, errors.New("mcp: invalid resources/list result")
		}
		for _, resource := range page.Resources {
			if resource.URI == "" || resource.Name == "" {
				return "", 0, errors.New("mcp: resources/list returned an invalid resource")
			}
		}
		output = append(output, page.Resources...)
		return page.NextCursor, len(page.Resources), nil
	})
	return output, err
}

func (c *protocolClient) paginate(ctx context.Context, method string, appendPage func(json.RawMessage) (string, int, error)) error {
	cursor := ""
	seen := make(map[string]struct{})
	total := 0
	for page := 0; page < c.config.MaxListPages; page++ {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, err := c.call(ctx, method, params)
		if err != nil {
			return err
		}
		next, count, err := appendPage(raw)
		if err != nil {
			return err
		}
		total += count
		if total > c.config.MaxListItems {
			return fmt.Errorf("mcp: %s exceeded item limit", method)
		}
		if next == "" {
			return nil
		}
		if len(next) > 4096 {
			return fmt.Errorf("mcp: %s returned an oversized cursor", method)
		}
		if _, duplicate := seen[next]; duplicate {
			return fmt.Errorf("mcp: %s repeated a pagination cursor", method)
		}
		seen[next] = struct{}{}
		cursor = next
	}
	return fmt.Errorf("mcp: %s exceeded page limit", method)
}

func (c *protocolClient) callTool(ctx context.Context, name string, arguments json.RawMessage) (ToolResult, error) {
	if !validJSONObject(arguments) {
		return ToolResult{}, errors.New("mcp: tool arguments must be a JSON object")
	}
	raw, err := c.call(ctx, "tools/call", struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}{Name: name, Arguments: arguments})
	if err != nil {
		return ToolResult{}, err
	}
	var wire struct {
		Content           []json.RawMessage `json:"content"`
		StructuredContent json.RawMessage   `json:"structuredContent"`
		IsError           bool              `json:"isError"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return ToolResult{}, errors.New("mcp: invalid tools/call result")
	}
	result := ToolResult{IsError: wire.IsError}
	remaining := c.config.MaxOutputBytes
	for _, item := range wire.Content {
		var common struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(item, &common); err != nil || common.Type == "" {
			return ToolResult{}, errors.New("mcp: tools/call returned invalid content")
		}
		content := Content{Type: common.Type, Text: common.Text}
		if int64(len(item)) <= remaining {
			content.Raw = append(json.RawMessage(nil), item...)
			remaining -= int64(len(item))
		} else if common.Type == "text" && remaining > 0 {
			content.Text = truncateUTF8(common.Text, remaining)
			remaining = 0
			result.Truncated = true
		} else {
			result.Truncated = true
			continue
		}
		result.Content = append(result.Content, content)
	}
	if len(wire.StructuredContent) != 0 {
		if !json.Valid(wire.StructuredContent) {
			return ToolResult{}, errors.New("mcp: tools/call returned invalid structured content")
		}
		if int64(len(wire.StructuredContent)) <= remaining {
			result.StructuredContent = append(json.RawMessage(nil), wire.StructuredContent...)
		} else {
			result.Truncated = true
		}
	}
	if result.IsError {
		return result, &ApplicationError{Server: c.config.Name, Tool: name, Result: result}
	}
	return result, nil
}

func validJSONObject(raw json.RawMessage) bool {
	if !json.Valid(raw) {
		return false
	}
	trimmed := strings.TrimSpace(string(raw))
	return strings.HasPrefix(trimmed, "{")
}

func truncateUTF8(value string, max int64) string {
	if int64(len(value)) <= max {
		return value
	}
	cut := int(max)
	for cut > 0 && !utf8.ValidString(value[:cut]) {
		cut--
	}
	return value[:cut]
}
