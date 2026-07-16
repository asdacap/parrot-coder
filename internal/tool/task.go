package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/amirulashraf/parrot-coder/internal/subagent"
	"strings"
	"time"
)

type AgentLookup func(string) (bool, error)

type TaskTool struct {
	Kind       string
	Manager    *subagent.Manager
	Agents     AgentLookup
	CancelWait time.Duration
}

func NewTaskTools(manager *subagent.Manager, agents AgentLookup) []Tool {
	return []Tool{
		&TaskTool{Kind: "task", Manager: manager, Agents: agents, CancelWait: 5 * time.Second},
		&TaskTool{Kind: "task_status", Manager: manager, Agents: agents},
		&TaskTool{Kind: "task_cancel", Manager: manager, Agents: agents, CancelWait: 5 * time.Second},
	}
}

func (t *TaskTool) ID() string { return t.Kind }
func (t *TaskTool) Description() string {
	switch t.Kind {
	case "task":
		return "Launch a child agent in an isolated session and wait for its final output."
	case "task_status":
		return "Read the status of a child task owned by this session."
	default:
		return "Cancel a child task owned by this session."
	}
}
func (t *TaskTool) DescribeRequest(raw json.RawMessage) (string, error) {
	var input taskInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return "", err
	}
	switch t.Kind {
	case "task":
		return fmt.Sprintf("Launch %q subagent", input.Agent), nil
	case "task_status":
		return fmt.Sprintf("Read status of task %q", input.TaskID), nil
	default:
		return fmt.Sprintf("Cancel task %q", input.TaskID), nil
	}
}
func (t *TaskTool) JSONSchema() json.RawMessage {
	if t.Kind == "task" {
		return json.RawMessage(`{"type":"object","properties":{"prompt":{"type":"string"},"agent":{"type":"string"},"model":{"type":"string"}},"required":["prompt","agent"],"additionalProperties":false}`)
	}
	return json.RawMessage(`{"type":"object","properties":{"task_id":{"type":"string"}},"required":["task_id"],"additionalProperties":false}`)
}

type taskInput struct {
	Prompt string `json:"prompt"`
	Agent  string `json:"agent"`
	Model  string `json:"model"`
	TaskID string `json:"task_id"`
}

func (t *TaskTool) Plan(_ context.Context, raw json.RawMessage, call CallContext) (Plan, error) {
	if t.Manager == nil || call.SessionID == "" || call.Agent == "" {
		return Plan{}, errors.New("task: manager, session, and caller agent are required")
	}
	var input taskInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return Plan{}, err
	}
	if t.Kind == "task" {
		if strings.TrimSpace(input.Prompt) == "" || input.Agent == "" || t.Agents == nil {
			return Plan{}, errors.New("task: prompt, target agent, and agent registry are required")
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
			return Plan{}, fmt.Errorf("task: read-only agent %q cannot delegate to writable agent %q", call.Agent, input.Agent)
		}
	} else if input.TaskID == "" {
		return Plan{}, errors.New("task: task_id is required")
	}
	return NewPlan(t.ID(), raw, nil, nil, input)
}
func (t *TaskTool) Execute(ctx context.Context, plan Plan, call CallContext) (Result, error) {
	input, ok := plan.Data.(taskInput)
	if !ok {
		return Result{}, errors.New("task: incompatible plan")
	}
	if t.Kind == "task_status" {
		task, err := t.Manager.Status(call.SessionID, input.TaskID)
		return taskResult(task), err
	}
	if t.Kind == "task_cancel" {
		if err := t.Manager.Cancel(ctx, call.SessionID, input.TaskID); err != nil {
			return Result{}, err
		}
		task, err := t.Manager.Status(call.SessionID, input.TaskID)
		return taskResult(task), err
	}
	id, err := t.Manager.Launch(call.SessionID, []string{call.Agent}, subagent.Request{Prompt: input.Prompt, Agent: input.Agent, Model: input.Model, ToolCallID: call.ToolCallID})
	if err != nil {
		return Result{}, err
	}
	task, err := t.Manager.Await(ctx, call.SessionID, id)
	if ctx.Err() != nil {
		cancelCtx, cancel := context.WithTimeout(context.Background(), t.CancelWait)
		_ = t.Manager.Cancel(cancelCtx, call.SessionID, id)
		cancel()
		return Result{}, ctx.Err()
	}
	if err != nil {
		return Result{}, err
	}
	return taskResult(task), nil
}
func taskResult(task subagent.Task) Result {
	metadata := map[string]any{"task_id": task.ID, "status": task.Status, "agent": task.Agent, "model": task.Model, "depth": task.Depth, "truncated": task.Truncated, "usage": task.Usage, "tool_uses": task.ToolUses}
	if task.Error != "" {
		metadata["error"] = task.Error
	}
	return Result{Text: task.Output, Metadata: metadata}
}
