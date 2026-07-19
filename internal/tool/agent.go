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

func (t *AgentTool) Description() string {
	switch t.Kind {
	case agentSpawnID:
		return "Start a reusable child agent in an isolated session and return its task ID immediately."
	case agentSendID:
		return "Send a message to a child agent. Running agents are steered; idle agents start a follow-up turn."
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
		return fmt.Sprintf("Send input to agent task %q", input.TaskID), nil
	default:
		return "Manage child agent", nil
	}
}

func (t *AgentTool) JSONSchema() json.RawMessage {
	switch t.Kind {
	case agentSpawnID:
		return json.RawMessage(`{"type":"object","properties":{"prompt":{"type":"string"},"agent":{"type":"string"},"model":{"type":"string"}},"required":["prompt","agent"],"additionalProperties":false}`)
	case agentSendID:
		return json.RawMessage(`{"type":"object","properties":{"task_id":{"type":"string"},"message":{"type":"string"}},"required":["task_id","message"],"additionalProperties":false}`)
	default:
		return json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)
	}
}

type agentInput struct {
	Prompt  string `json:"prompt"`
	Agent   string `json:"agent"`
	Model   string `json:"model"`
	TaskID  string `json:"task_id"`
	Message string `json:"message"`
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
		if input.TaskID == "" || strings.TrimSpace(input.Message) == "" {
			return Plan{}, errors.New("agent_send: task_id and message are required")
		}
		if t.Agents == nil {
			return Plan{}, errors.New("agent_send: agent registry is required")
		}
		callerReadOnly, err := t.Agents(call.Agent)
		if err != nil {
			return Plan{}, err
		}
		task, err := t.Manager.Status(call.SessionID, input.TaskID)
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
		id, err := t.Manager.Spawn(ctx, call.SessionID, call.Agent, subagent.Request{Prompt: input.Prompt, Agent: input.Agent, Model: input.Model, ToolCallID: call.ToolCallID})
		if err != nil {
			return Result{}, err
		}
		task, err := t.Manager.Status(call.SessionID, id)
		return agentResult(task), err
	case agentSendID:
		task, err := t.Manager.Status(call.SessionID, input.TaskID)
		if err != nil {
			return Result{}, err
		}
		if task.Status == subagent.StatusRunning || task.Status == subagent.StatusPending {
			messageID, err := t.Manager.Send(ctx, call.SessionID, input.TaskID, input.Message)
			if err != nil && !errors.Is(err, subagent.ErrNotRunning) {
				return Result{}, err
			}
			if err == nil {
				task, err = t.Manager.Status(call.SessionID, input.TaskID)
				result := agentResult(task)
				result.Metadata["message_id"] = messageID
				return result, err
			}
		}
		task, err = t.Manager.FollowUp(call.SessionID, input.TaskID, subagent.Request{Prompt: input.Message, ToolCallID: call.ToolCallID})
		return agentResult(task), err
	default:
		return Result{}, errors.New("agent: unknown operation")
	}
}

func agentResult(task subagent.Task) Result {
	metadata := agentMetadata(task)
	text, _ := json.Marshal(metadata)
	return Result{Text: string(text), Metadata: metadata}
}

func agentMetadata(task subagent.Task) map[string]any {
	metadata := map[string]any{
		"task_id": task.ID,
		"kind":    "agent",
		"agent":   task.Agent,
		"status":  task.Status,
		"turn":    task.Turn,
		"depth":   task.Depth,
	}
	if task.Output != "" {
		metadata["output"] = task.Output
	}
	if task.Error != "" {
		metadata["error"] = task.Error
	}
	return metadata
}
