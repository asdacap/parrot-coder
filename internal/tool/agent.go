package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/amirulashraf/parrot-coder/internal/subagent"
)

const (
	agentSpawnID     = "agent_spawn"
	agentSendID      = "agent_send"
	agentWaitID      = "agent_wait"
	agentInterruptID = "agent_interrupt"
	agentListID      = "agent_list"
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
		&AgentTool{Kind: agentWaitID, Manager: manager, Agents: agents},
		&AgentTool{Kind: agentInterruptID, Manager: manager, Agents: agents},
		&AgentTool{Kind: agentListID, Manager: manager, Agents: agents},
	}
}

func (t *AgentTool) ID() string { return t.Kind }

func (t *AgentTool) Description() string {
	switch t.Kind {
	case agentSpawnID:
		return "Start a reusable child agent in an isolated session and return its agent ID immediately."
	case agentSendID:
		return "Send a message to a child agent. Running agents are steered; idle agents start a follow-up turn."
	case agentWaitID:
		return "Wait until one of the selected child agents changes, or until the timeout expires."
	case agentInterruptID:
		return "Interrupt a child agent's active turn while retaining the agent for follow-up messages."
	default:
		return "List reusable child agents visible to the current session."
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
		return fmt.Sprintf("Send input to agent %q", input.AgentID), nil
	case agentWaitID:
		return fmt.Sprintf("Wait for %d agent(s)", len(input.IDs)), nil
	case agentInterruptID:
		return fmt.Sprintf("Interrupt agent %q", input.AgentID), nil
	default:
		return "List child agents", nil
	}
}

func (t *AgentTool) JSONSchema() json.RawMessage {
	switch t.Kind {
	case agentSpawnID:
		return json.RawMessage(`{"type":"object","properties":{"prompt":{"type":"string"},"agent":{"type":"string"},"model":{"type":"string"}},"required":["prompt","agent"],"additionalProperties":false}`)
	case agentSendID:
		return json.RawMessage(`{"type":"object","properties":{"agent_id":{"type":"string"},"message":{"type":"string"}},"required":["agent_id","message"],"additionalProperties":false}`)
	case agentWaitID:
		return json.RawMessage(`{"type":"object","properties":{"ids":{"type":"array","items":{"type":"string"},"minItems":1,"maxItems":64},"timeout_ms":{"type":"integer","minimum":1,"maximum":300000}},"required":["ids"],"additionalProperties":false}`)
	case agentInterruptID:
		return json.RawMessage(`{"type":"object","properties":{"agent_id":{"type":"string"}},"required":["agent_id"],"additionalProperties":false}`)
	default:
		return json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)
	}
}

type agentInput struct {
	Prompt    string   `json:"prompt"`
	Agent     string   `json:"agent"`
	Model     string   `json:"model"`
	AgentID   string   `json:"agent_id"`
	Message   string   `json:"message"`
	IDs       []string `json:"ids"`
	TimeoutMS int      `json:"timeout_ms"`
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
		if input.AgentID == "" || strings.TrimSpace(input.Message) == "" {
			return Plan{}, errors.New("agent_send: agent_id and message are required")
		}
		if t.Agents == nil {
			return Plan{}, errors.New("agent_send: agent registry is required")
		}
		callerReadOnly, err := t.Agents(call.Agent)
		if err != nil {
			return Plan{}, err
		}
		task, err := t.Manager.Status(call.SessionID, input.AgentID)
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
	case agentWaitID:
		if len(input.IDs) == 0 || len(input.IDs) > 64 {
			return Plan{}, errors.New("agent_wait: between 1 and 64 ids are required")
		}
		if input.TimeoutMS < 0 || input.TimeoutMS > 300000 {
			return Plan{}, errors.New("agent_wait: timeout_ms must not exceed 300000")
		}
	case agentInterruptID:
		if input.AgentID == "" {
			return Plan{}, errors.New("agent_interrupt: agent_id is required")
		}
	case agentListID:
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
		task, err := t.Manager.Status(call.SessionID, input.AgentID)
		if err != nil {
			return Result{}, err
		}
		if task.Status == subagent.StatusRunning || task.Status == subagent.StatusPending {
			messageID, err := t.Manager.Send(ctx, call.SessionID, input.AgentID, input.Message)
			if err != nil && !errors.Is(err, subagent.ErrNotRunning) {
				return Result{}, err
			}
			if err == nil {
				task, err = t.Manager.Status(call.SessionID, input.AgentID)
				result := agentResult(task)
				result.Metadata["message_id"] = messageID
				return result, err
			}
		}
		task, err = t.Manager.FollowUp(call.SessionID, input.AgentID, subagent.Request{Prompt: input.Message, ToolCallID: call.ToolCallID})
		return agentResult(task), err
	case agentWaitID:
		timeout := 30 * time.Second
		if input.TimeoutMS > 0 {
			timeout = time.Duration(input.TimeoutMS) * time.Millisecond
		}
		waitCtx, cancel := context.WithTimeout(ctx, timeout)
		tasks, err := t.Manager.Wait(waitCtx, call.SessionID, input.IDs)
		cancel()
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			tasks = make([]subagent.Task, 0, len(input.IDs))
			for _, id := range input.IDs {
				task, statusErr := t.Manager.Status(call.SessionID, id)
				if statusErr != nil {
					return Result{}, statusErr
				}
				tasks = append(tasks, task)
			}
			return agentsResult(tasks, true)
		}
		if err != nil {
			return Result{}, err
		}
		return agentsResult(tasks, false)
	case agentInterruptID:
		task, err := t.Manager.Interrupt(ctx, call.SessionID, input.AgentID)
		return agentResult(task), err
	default:
		return agentsResult(t.Manager.List(call.SessionID), false)
	}
}

func agentResult(task subagent.Task) Result {
	metadata := agentMetadata(task)
	text, _ := json.Marshal(metadata)
	return Result{Text: string(text), Metadata: metadata}
}

func agentsResult(tasks []subagent.Task, timedOut bool) (Result, error) {
	items := make([]map[string]any, len(tasks))
	for i, task := range tasks {
		items[i] = agentMetadata(task)
	}
	metadata := map[string]any{"agents": items, "timed_out": timedOut}
	text, _ := json.Marshal(metadata)
	return Result{Text: string(text), Metadata: metadata}, nil
}

func agentMetadata(task subagent.Task) map[string]any {
	metadata := map[string]any{
		"agent_id": task.ID,
		"agent":    task.Agent,
		"status":   task.Status,
		"turn":     task.Turn,
		"depth":    task.Depth,
	}
	if task.Output != "" {
		metadata["output"] = task.Output
	}
	if task.Error != "" {
		metadata["error"] = task.Error
	}
	return metadata
}
