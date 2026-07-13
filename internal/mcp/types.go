package mcp

import (
	"encoding/json"
	"fmt"
	"time"
)

const ProtocolVersion = "2025-06-18"

type State string

const (
	StateStopped  State = "stopped"
	StateStarting State = "starting"
	StateRunning  State = "running"
	StateStopping State = "stopping"
	StateFailed   State = "failed"
)

type Status struct {
	Name            string    `json:"name"`
	Transport       Transport `json:"transport"`
	State           State     `json:"state"`
	Healthy         bool      `json:"healthy"`
	ProtocolVersion string    `json:"protocol_version,omitempty"`
	ServerName      string    `json:"server_name,omitempty"`
	ServerVersion   string    `json:"server_version,omitempty"`
	PID             int       `json:"pid,omitempty"`
	StartedAt       time.Time `json:"started_at,omitempty"`
	LastError       string    `json:"last_error,omitempty"`
}

type Implementation struct {
	Name    string `json:"name"`
	Title   string `json:"title,omitempty"`
	Version string `json:"version"`
}

type Capabilities struct {
	Tools     json.RawMessage `json:"tools,omitempty"`
	Prompts   json.RawMessage `json:"prompts,omitempty"`
	Resources json.RawMessage `json:"resources,omitempty"`
	Logging   json.RawMessage `json:"logging,omitempty"`
}

type Tool struct {
	Name         string          `json:"name"`
	Title        string          `json:"title,omitempty"`
	Description  string          `json:"description,omitempty"`
	InputSchema  json.RawMessage `json:"inputSchema"`
	OutputSchema json.RawMessage `json:"outputSchema,omitempty"`
}

type ToolDefinition struct {
	Name         string          `json:"name"`
	Description  string          `json:"description,omitempty"`
	InputSchema  json.RawMessage `json:"inputSchema"`
	OutputSchema json.RawMessage `json:"outputSchema,omitempty"`
	Server       string          `json:"server"`
	Tool         string          `json:"tool"`
}

type Prompt struct {
	Name        string          `json:"name"`
	Title       string          `json:"title,omitempty"`
	Description string          `json:"description,omitempty"`
	Arguments   json.RawMessage `json:"arguments,omitempty"`
}

type Resource struct {
	URI         string          `json:"uri"`
	Name        string          `json:"name"`
	Title       string          `json:"title,omitempty"`
	Description string          `json:"description,omitempty"`
	MIMEType    string          `json:"mimeType,omitempty"`
	Size        *int64          `json:"size,omitempty"`
	Annotations json.RawMessage `json:"annotations,omitempty"`
}

// Content retains the complete MCP content object in Raw while exposing the
// common text fields directly. Raw is bounded by Config.MaxOutputBytes.
type Content struct {
	Type string          `json:"type"`
	Text string          `json:"text,omitempty"`
	Raw  json.RawMessage `json:"raw,omitempty"`
}

type ToolResult struct {
	Content           []Content       `json:"content,omitempty"`
	StructuredContent json.RawMessage `json:"structuredContent,omitempty"`
	IsError           bool            `json:"isError,omitempty"`
	Truncated         bool            `json:"truncated,omitempty"`
}

type Notification struct {
	Server string          `json:"server"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

type RPCError struct {
	Code    int64           `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("mcp: JSON-RPC error %d: %s", e.Code, e.Message)
}

type ApplicationError struct {
	Server string
	Tool   string
	Result ToolResult
}

func (e *ApplicationError) Error() string {
	for _, content := range e.Result.Content {
		if content.Type == "text" && content.Text != "" {
			return fmt.Sprintf("mcp: tool %s/%s failed: %s", e.Server, e.Tool, content.Text)
		}
	}
	return fmt.Sprintf("mcp: tool %s/%s reported an application error", e.Server, e.Tool)
}
