package tool

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/amirulashraf/parrot-coder/internal/subagent"
	"strings"
	"time"
)

const reviewAgentID = "review"

// ReviewTool starts the built-in, read-only review worker. It is deliberately
// separate from the reusable child-agent controls so a parent model can request
// a review without selecting or knowing the implementation profile used by the
// child session.
type ReviewTool struct {
	Manager    *subagent.Manager
	Agents     AgentLookup
	CancelWait time.Duration
}

func NewReviewTool(manager *subagent.Manager, agents AgentLookup) Tool {
	return &ReviewTool{Manager: manager, Agents: agents, CancelWait: 5 * time.Second}
}

func (t *ReviewTool) ID() string { return "review" }
func (t *ReviewTool) Description() string {
	return "Launch the built-in read-only review subagent and wait for its actionable findings. Use after implementing changes or when explicitly asked to review code."
}
func (t *ReviewTool) DescribeRequest(raw json.RawMessage) (string, error) {
	var input reviewInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return "", err
	}
	return "Launch read-only review subagent", nil
}
func (t *ReviewTool) JSONSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"prompt":{"type":"string","description":"The exact change or review target, including any additional review instructions."},"model":{"type":"string","description":"Optional model override for the reviewer."}},"required":["prompt"],"additionalProperties":false}`)
}

type reviewInput struct {
	Prompt string `json:"prompt"`
	Model  string `json:"model"`
}

func (t *ReviewTool) Plan(_ context.Context, raw json.RawMessage, call CallContext) (Plan, error) {
	if t.Manager == nil || t.Agents == nil || call.SessionID == "" || call.Agent == "" {
		return Plan{}, errors.New("review: manager, agent registry, session, and caller agent are required")
	}
	var input reviewInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return Plan{}, err
	}
	if strings.TrimSpace(input.Prompt) == "" {
		return Plan{}, errors.New("review: prompt is required")
	}
	if _, err := t.Agents(call.Agent); err != nil {
		return Plan{}, err
	}
	reviewerReadOnly, err := t.Agents(reviewAgentID)
	if err != nil {
		return Plan{}, err
	}
	if !reviewerReadOnly {
		return Plan{}, errors.New("review: built-in reviewer must be read-only")
	}
	return NewPlan(t.ID(), raw, nil, nil, input)
}

func (t *ReviewTool) Execute(ctx context.Context, plan Plan, call CallContext) (Result, error) {
	input, ok := plan.Data.(reviewInput)
	if !ok {
		return Result{}, errors.New("review: incompatible plan")
	}
	id, err := t.Manager.Spawn(ctx, call.SessionID, call.Agent, subagent.Request{
		Prompt: input.Prompt, Agent: reviewAgentID, Model: input.Model, ToolCallID: call.ToolCallID,
	})
	if err != nil {
		return Result{}, err
	}
	task, err := t.Manager.Await(ctx, call.SessionID, id)
	if ctx.Err() != nil {
		cancelWait := t.CancelWait
		if cancelWait <= 0 {
			cancelWait = 5 * time.Second
		}
		cancelCtx, cancel := context.WithTimeout(context.Background(), cancelWait)
		cancelErr := t.Manager.Cancel(cancelCtx, call.SessionID, id)
		cancel()
		if cancelErr == nil {
			_ = t.Manager.Forget(call.SessionID, id)
		}
		return Result{}, ctx.Err()
	}
	if err != nil {
		_ = t.Manager.Forget(call.SessionID, id)
		return Result{}, err
	}
	result := reviewResult(task)
	if err := t.Manager.Forget(call.SessionID, id); err != nil {
		return Result{}, err
	}
	return result, nil
}

func reviewResult(task subagent.Task) Result {
	metadata := map[string]any{"task_id": task.ID, "status": task.Status, "agent": task.Agent, "model": task.Model, "depth": task.Depth, "truncated": task.Truncated, "usage": task.Usage, "tool_uses": task.ToolUses}
	if task.Error != "" {
		metadata["error"] = task.Error
	}
	return Result{Text: task.Output, ModelText: modelText(task.Output), Metadata: metadata}
}
