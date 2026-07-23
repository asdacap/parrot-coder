package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/amirulashraf/parrot-coder/internal/subagent"
)

const (
	agentSpawnID = "agent_spawn"
	agentSendID  = "agent_send"
)

type AgentLookup func(string) (bool, error)

type AgentTool struct {
	BasePresentation
	Kind    string
	Manager *subagent.Manager
	Agents  AgentLookup
}

func NewAgentTools(manager *subagent.Manager, agents AgentLookup) []Tool {
	return []Tool{
		&AgentTool{Kind: agentSpawnID, Manager: manager, Agents: agents},
		&AgentTool{Kind: agentSendID, Manager: manager, Agents: agents},
	}
}

func (t *AgentTool) ID() string { return t.Kind }

func (t *AgentTool) Presentation() Presentation {
	presentation := Presentation{Subagent: true}
	if t.Kind == agentSpawnID {
		presentation.Label = LabelSpec{Fields: []LabelField{
			{Names: []string{"name", "agent"}},
		}}
		presentation.CompletedInput = CompletedInputSpec{
			Fields: []string{"name", "agent", "model", "prompt"}, TerminalOnly: true,
		}
		return presentation
	}
	presentation.Label = LabelSpec{Fields: []LabelField{
		{Names: []string{"session_id"}, TaskName: true}, {Names: []string{"message"}},
	}}
	return presentation
}
func (t *AgentTool) Description() string {
	switch t.Kind {
	case agentSpawnID:
		return "Start a reusable child agent in an isolated session and return its session ID immediately."
	case agentSendID:
		return "Send a message to a child agent session. Running agents are steered; idle agents start a follow-up turn."
	default:
		return "Manage a reusable child agent task."
	}
}

func (t *AgentTool) DescribeRequest(raw json.RawMessage) (string, error) {
	input, err := decodeAgentInput(raw)
	if err != nil {
		return "", err
	}
	switch t.Kind {
	case agentSpawnID:
		return fmt.Sprintf("Start %q agent", input.Agent), nil
	case agentSendID:
		return fmt.Sprintf("Send input to child session %q", input.SessionID), nil
	default:
		return "Manage child agent", nil
	}
}

func (t *AgentTool) JSONSchema() json.RawMessage {
	switch t.Kind {
	case agentSpawnID:
		return json.RawMessage(`{"type":"object","properties":{"prompt":{"type":"string"},"agent":{"type":"string"},"model":{"type":"string"},"name":{"type":"string","description":"Optional UI name. It is lowercased and sanitized to letters, digits, and hyphens; omitted or empty names are generated."}},"required":["prompt","agent"],"additionalProperties":false}`)
	case agentSendID:
		return json.RawMessage(`{"type":"object","properties":{"session_id":{"type":"string","description":"Identifier of the child agent session."},"message":{"type":"string"}},"required":["session_id","message"],"additionalProperties":false}`)
	default:
		return json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)
	}
}

type agentInput struct {
	Prompt    string `json:"prompt"`
	Agent     string `json:"agent"`
	Model     string `json:"model"`
	Name      string `json:"name"`
	SessionID string `json:"session_id"`
	Message   string `json:"message"`
}

func decodeAgentInput(raw json.RawMessage) (agentInput, error) {
	var input agentInput
	err := json.Unmarshal(raw, &input)
	return input, err
}

func (t *AgentTool) Plan(_ context.Context, raw json.RawMessage, call CallContext) (Plan, error) {
	if t.Manager == nil || call.SessionID == "" || call.Agent == "" {
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
		task, err := t.agentTask(call.SessionID, input.SessionID)
		if err != nil {
			return Plan{}, err
		}
		targetReadOnly, err := t.Agents(task.Agent)
		if err != nil {
			return Plan{}, err
		}
		if callerReadOnly && !targetReadOnly {
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
		id, err := t.Manager.Spawn(ctx, call.SessionID, call.Agent, subagent.Request{Prompt: input.Prompt, Agent: input.Agent, Model: input.Model, Name: input.Name, ToolCallID: call.ToolCallID})
		if err != nil {
			return Result{}, err
		}
		task, err := t.Manager.Status(call.SessionID, id)
		return agentResult(task), err
	case agentSendID:
		task, err := t.agentTask(call.SessionID, input.SessionID)
		if err != nil {
			return Result{}, err
		}
		if task.Status == subagent.StatusRunning || task.Status == subagent.StatusPending {
			messageID, err := t.Manager.Send(ctx, call.SessionID, task.SessionID, input.Message)
			if err != nil && !errors.Is(err, subagent.ErrNotRunning) {
				return Result{}, err
			}
			if err == nil {
				task, err = t.Manager.Status(call.SessionID, task.SessionID)
				result := agentResult(task)
				result.Metadata["message_id"] = messageID
				return result, err
			}
		}
		task, err = t.Manager.FollowUp(call.SessionID, task.SessionID, subagent.Request{Prompt: input.Message, ToolCallID: call.ToolCallID})
		return agentResult(task), err
	default:
		return Result{}, errors.New("agent: unknown operation")
	}
}

func (t *AgentTool) agentTask(callerSession, childSession string) (subagent.Task, error) {
	task, ok := t.Manager.TaskForSession(childSession)
	if !ok {
		return subagent.Task{}, subagent.ErrNotFound
	}
	return t.Manager.Status(callerSession, task.SessionID)
}

func agentResult(task subagent.Task) Result {
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

func agentMetadata(task subagent.Task) map[string]any {
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
