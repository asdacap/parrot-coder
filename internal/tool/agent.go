package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const (
	agentSpawnID = "agent_spawn"
	agentSendID  = "agent_send"
)

type AgentLookup func(string) (bool, error)

type AgentTask struct {
	SessionID string
	Agent     string
	Name      string
	Status    string
	Turn      int
	Depth     int
	Output    string
	Error     string
}

type AgentMessage struct {
	Sender  string
	Content string
}

func (m AgentMessage) String() string {
	return fmt.Sprintf("Agent message from %s:\n\n%s", m.Sender, m.Content)
}

type ChildAgent interface {
	Status() AgentTask
	Send(context.Context, AgentMessage) (AgentTask, string, error)
}

type AgentChildren interface {
	Create(context.Context, string, string, string, string, string, string) (ChildAgent, error)
	Resolve(string, string) (ChildAgent, error)
}

type AgentTool struct {
	BasePresentation
	Kind     string
	Children AgentChildren // retained for direct construction in focused tests
	Session  AgentSession
	Agents   AgentLookup
}

func NewAgentTools(children AgentChildren, agents AgentLookup) []Tool {
	return []Tool{
		&AgentTool{Kind: agentSpawnID, Children: children, Agents: agents},
		&AgentTool{Kind: agentSendID, Children: children, Agents: agents},
	}
}

func (t *AgentTool) ID() string { return t.Kind }

func (t *AgentTool) Descriptor() Descriptor { return AgentDescriptor(t.Kind) }

func AgentDescriptor(kind string) Descriptor {
	descriptor := Descriptor{ID: kind}
	switch kind {
	case agentSpawnID:
		descriptor.Description = "Start a reusable child agent in an isolated session and return its session ID immediately. If concurrency is full, the returned agent is blocked until capacity is available; wait for it or interrupt it to cancel. The final result will automatically be sent to the caller when the child agent finishes."
		descriptor.Schema = json.RawMessage(`{"type":"object","properties":{"prompt":{"type":"string"},"agent":{"type":"string"},"model":{"type":"string","description":"Optional configured alias or canonical provider/model[/variant] selector; omitted to inherit the parent's complete selector."},"name":{"type":"string","description":"Optional friendly name for easy identification. It is lowercased and sanitized to letters, digits, and hyphens; omitted or empty names are generated."}},"required":["prompt","agent"],"additionalProperties":false}`)
		descriptor.Presentation = Presentation{Subagent: true, Label: LabelSpec{Fields: []LabelField{{Names: []string{"name", "agent"}}}}, CompletedInput: CompletedInputSpec{Fields: []string{"name", "agent", "model", "prompt"}, TerminalOnly: true}}
	case agentSendID:
		descriptor.Description = "Send a message to an accessible agent session (a direct parent or descendant) by canonical ID or friendly name. Running agents are steered; idle agents start a follow-up turn."
		descriptor.Schema = json.RawMessage(`{"type":"object","properties":{"session_id":{"type":"string","description":"Canonical session ID or friendly name of an accessible direct parent or descendant."},"message":{"type":"string"}},"required":["session_id","message"],"additionalProperties":false}`)
		descriptor.Presentation = Presentation{Subagent: true, Label: LabelSpec{Fields: []LabelField{{Names: []string{"session_id"}, TaskName: true}, {Names: []string{"message"}}}}}
	default:
		descriptor.Description = "Manage a reusable child agent task."
		descriptor.Schema = json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)
		descriptor.Presentation = Presentation{Subagent: true}
	}
	return descriptor
}

func (t *AgentTool) Presentation() Presentation { return t.Descriptor().Presentation }

func (t *AgentTool) DescribeRequest(raw json.RawMessage) (string, error) {
	input, err := decodeAgentInput(raw)
	if err != nil {
		return "", err
	}
	switch t.Kind {
	case agentSpawnID:
		return fmt.Sprintf("Start %q agent", input.Agent), nil
	case agentSendID:
		return fmt.Sprintf("Send input to agent session %q", input.SessionID), nil
	default:
		return "Manage child agent", nil
	}
}

func (t *AgentTool) JSONSchema() json.RawMessage { return t.Descriptor().Schema }

type agentInput struct {
	Prompt     string        `json:"prompt"`
	Agent      string        `json:"agent"`
	Model      string        `json:"model"`
	Name       string        `json:"name"`
	SessionID  string        `json:"session_id"`
	Message    string        `json:"message"`
	Target     ResolvedAgent `json:"-"`
	SenderName string        `json:"-"`
}

func decodeAgentInput(raw json.RawMessage) (agentInput, error) {
	var input agentInput
	err := json.Unmarshal(raw, &input)
	return input, err
}

func (t *AgentTool) Plan(_ context.Context, raw json.RawMessage, call CallContext) (Plan, error) {
	if t.Session == nil && t.Children == nil || call.SessionID == "" || call.Agent == "" {
		return Plan{}, errors.New("agent: manager, session, and caller agent are required")
	}
	input, err := decodeAgentInput(raw)
	if err != nil {
		return Plan{}, err
	}
	switch t.Kind {
	case agentSpawnID:
		if strings.TrimSpace(input.Prompt) == "" || input.Agent == "" || t.Agents == nil {
			return Plan{}, errors.New("agent_spawn: prompt, target agent, and agent registry are required")
		}
		callerReadOnly, err := t.Agents(call.Agent)
		if err != nil {
			return Plan{}, err
		}
		targetReadOnly, err := t.Agents(input.Agent)
		if err != nil {
			return Plan{}, err
		}
		if callerReadOnly && !targetReadOnly {
			return Plan{}, fmt.Errorf("agent_spawn: read-only agent %q cannot delegate to writable agent %q", call.Agent, input.Agent)
		}
	case agentSendID:
		if input.SessionID == "" || strings.TrimSpace(input.Message) == "" {
			return Plan{}, errors.New("agent_send: session_id and message are required")
		}
		if t.Agents == nil {
			return Plan{}, errors.New("agent_send: agent registry is required")
		}
		callerReadOnly, err := t.Agents(call.Agent)
		if err != nil {
			return Plan{}, err
		}
		target, err := t.resolve(call.SessionID, input.SessionID)
		if err != nil {
			return Plan{}, err
		}
		task := target.Agent.Status()
		input.SessionID = task.SessionID
		input.Target = target
		input.SenderName = call.SessionID
		if t.Session != nil {
			if name := t.Session.SessionName(); name != "" {
				input.SenderName = name
			}
		}
		targetReadOnly, err := t.Agents(task.Agent)
		if err != nil {
			return Plan{}, err
		}
		if callerReadOnly && !targetReadOnly && target.Relationship != AgentRelationshipParent {
			return Plan{}, fmt.Errorf("agent_send: read-only agent %q cannot message writable agent %q", call.Agent, task.Agent)
		}
	default:
		return Plan{}, errors.New("agent: unknown operation")
	}
	return NewPlan(t.ID(), raw, nil, nil, input)
}

func (t *AgentTool) Execute(ctx context.Context, plan Plan, call CallContext) (Result, error) {

	input, ok := plan.Data.(agentInput)
	if !ok {
		return Result{}, errors.New("agent: incompatible plan")
	}
	switch t.Kind {
	case agentSpawnID:
		var child ChildAgent
		var err error
		if t.Session != nil {
			child, err = t.Session.CreateAgent(ctx, call.Agent, input.Prompt, input.Agent, input.Model, input.Name)
		} else {
			child, err = t.Children.Create(ctx, call.SessionID, call.Agent, input.Prompt, input.Agent, input.Model, input.Name)
		}
		if err != nil {
			return Result{}, err
		}
		task := child.Status()
		return agentResult(task), nil
	case agentSendID:
		if input.Target.Agent == nil {
			return Result{}, errors.New("agent_send: planned target is required")
		}
		task, messageID, err := input.Target.Agent.Send(ctx, AgentMessage{Sender: input.SenderName, Content: input.Message})
		if err != nil {
			return Result{}, err
		}
		result := agentResult(task)
		if messageID != "" {
			result.Metadata["message_id"] = messageID
		}
		return result, nil
	default:
		return Result{}, errors.New("agent: unknown operation")
	}
}

func (t *AgentTool) resolve(parentSession, identifier string) (ResolvedAgent, error) {
	if t.Session != nil {
		return t.Session.ResolveAgent(identifier)
	}
	child, err := t.Children.Resolve(parentSession, identifier)
	return ResolvedAgent{Agent: child, Relationship: AgentRelationshipDescendant}, err
}

func agentResult(task AgentTask) Result {
	metadata := agentMetadata(task)
	text, _ := json.Marshal(metadata)
	// The subagent transcript is bounded before encoding: truncating the encoded
	// document would hand the model invalid JSON.
	bounded := agentMetadata(task)
	for _, field := range []string{"output", "error"} {
		if value, ok := bounded[field].(string); ok {
			bounded[field] = modelText(value)
		}
	}
	encoded, _ := json.Marshal(bounded)
	return Result{Text: string(text), ModelText: string(encoded), Metadata: metadata}
}

func agentMetadata(task AgentTask) map[string]any {
	metadata := map[string]any{
		"session_id": task.SessionID,
		"kind":       "agent",
		"agent":      task.Agent,
		"name":       task.Name,
		"status":     task.Status,
		"turn":       task.Turn,
		"depth":      task.Depth,
	}
	if task.Output != "" {
		metadata["output"] = task.Output
	}
	if task.Error != "" {
		metadata["error"] = task.Error
	}
	return metadata
}
