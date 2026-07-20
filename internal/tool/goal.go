package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/amirulashraf/parrot-coder/internal/session"
)

type GetGoalTool struct{ Service *session.GoalService }

func NewGetGoalTool(service *session.GoalService) *GetGoalTool { return &GetGoalTool{Service: service} }
func (*GetGoalTool) ID() string                                { return "get_goal" }
func (*GetGoalTool) Description() string {
	return "Get the current goal for this session, including status, budget, usage, and remaining tokens."
}
func (*GetGoalTool) DescribeRequest(json.RawMessage) (string, error) {
	return "Get the current goal", nil
}
func (*GetGoalTool) JSONSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","additionalProperties":false}`)
}
func (t *GetGoalTool) Plan(_ context.Context, raw json.RawMessage, _ CallContext) (Plan, error) {
	return NewPlan(t.ID(), raw, nil, nil, nil)
}
func (t *GetGoalTool) Execute(ctx context.Context, _ Plan, call CallContext) (Result, error) {
	if t.Service == nil || call.SessionID == "" {
		return Result{}, errors.New("get_goal: service and session are required")
	}
	goal, err := t.Service.Get(ctx, call.SessionID)
	if errors.Is(err, session.ErrGoalNotFound) {
		return Result{Text: "no goal set", ModelText: "no goal set"}, nil
	}
	if err != nil {
		return Result{}, err
	}
	return goalResult(goal), nil
}

type CreateGoalTool struct{ Service *session.GoalService }

type createGoalInput struct {
	Objective   string `json:"objective"`
	TokenBudget *int64 `json:"token_budget,omitempty"`
}

func NewCreateGoalTool(service *session.GoalService) *CreateGoalTool {
	return &CreateGoalTool{Service: service}
}
func (*CreateGoalTool) ID() string { return "create_goal" }
func (*CreateGoalTool) Description() string {
	return "Create a goal only when explicitly requested by the user or system. Fails while an unfinished goal exists; omit token_budget unless explicitly requested."
}
func (*CreateGoalTool) DescribeRequest(raw json.RawMessage) (string, error) {
	var input createGoalInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return "", err
	}
	return fmt.Sprintf("Create goal: %s", input.Objective), nil
}
func (*CreateGoalTool) JSONSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"objective":{"type":"string","minLength":1,"description":"Concrete objective explicitly requested by the user or system."},"token_budget":{"type":"integer","minimum":1,"description":"Positive token budget. Omit unless explicitly requested."}},"required":["objective"],"additionalProperties":false}`)
}
func (t *CreateGoalTool) Plan(_ context.Context, raw json.RawMessage, _ CallContext) (Plan, error) {
	var input createGoalInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return Plan{}, err
	}
	return NewPlan(t.ID(), raw, nil, nil, input)
}
func (t *CreateGoalTool) Execute(ctx context.Context, plan Plan, call CallContext) (Result, error) {
	if t.Service == nil || call.SessionID == "" {
		return Result{}, errors.New("create_goal: service and session are required")
	}
	input := plan.Data.(createGoalInput)
	goal, err := t.Service.Create(ctx, call.SessionID, input.Objective, input.TokenBudget)
	if err != nil {
		return Result{}, err
	}
	return goalResult(goal), nil
}

type UpdateGoalTool struct{ Service *session.GoalService }

type updateGoalInput struct {
	Status session.GoalStatus `json:"status"`
}

func NewUpdateGoalTool(service *session.GoalService) *UpdateGoalTool {
	return &UpdateGoalTool{Service: service}
}
func (*UpdateGoalTool) ID() string { return "update_goal" }
func (*UpdateGoalTool) Description() string {
	return "Mark the current goal complete only when achieved, or blocked only after the same blocking condition recurs for at least three consecutive goal turns. Pause, resume, and limit statuses are user/system controlled."
}
func (*UpdateGoalTool) DescribeRequest(raw json.RawMessage) (string, error) {
	var input updateGoalInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return "", err
	}
	return "Set goal status to " + string(input.Status), nil
}
func (*UpdateGoalTool) JSONSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"status":{"type":"string","enum":["complete","blocked"],"description":"Use complete only when all required work is achieved; blocked only after the same blocker recurs for three consecutive goal turns."}},"required":["status"],"additionalProperties":false}`)
}
func (t *UpdateGoalTool) Plan(_ context.Context, raw json.RawMessage, _ CallContext) (Plan, error) {
	var input updateGoalInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return Plan{}, err
	}
	return NewPlan(t.ID(), raw, nil, nil, input)
}
func (t *UpdateGoalTool) Execute(ctx context.Context, plan Plan, call CallContext) (Result, error) {
	if t.Service == nil || call.SessionID == "" {
		return Result{}, errors.New("update_goal: service and session are required")
	}
	goal, err := t.Service.UpdateAgentStatus(ctx, call.SessionID, plan.Data.(updateGoalInput).Status)
	if err != nil {
		return Result{}, err
	}
	return goalResult(goal), nil
}

func goalResult(goal session.Goal) Result {
	data, _ := json.Marshal(struct {
		session.Goal
		RemainingTokens *int64 `json:"remaining_tokens,omitempty"`
	}{Goal: goal, RemainingTokens: goal.RemainingTokens()})
	text := string(data)
	return Result{Text: text, ModelText: text, Metadata: map[string]any{"goal_id": goal.ID, "status": string(goal.Status)}}
}
