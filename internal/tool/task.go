package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	managedtask "github.com/amirulashraf/parrot-coder/internal/task"
)

type TaskController interface {
	Interrupt(context.Context, string, string) (managedtask.Active, error)
	ListActive(string) []managedtask.Active
}

type TaskTool struct {
	BasePresentation
	Kind       string
	Controller TaskController
}

func NewTaskTools(controller TaskController) []Tool {
	return []Tool{
		&TaskTool{Kind: "task_interrupt", Controller: controller},
		&TaskTool{Kind: "task_list_active", Controller: controller},
	}
}

func (t *TaskTool) ID() string { return t.Kind }

func (t *TaskTool) Presentation() Presentation {
	if t.Kind == "task_interrupt" {
		return Presentation{Label: LabelSpec{Fields: []LabelField{{Names: []string{"task_id"}}}}}
	}
	return Presentation{}
}
func (t *TaskTool) Description() string {
	if t.Kind == "task_interrupt" {
		return "Interrupt a running shell or agent task. Agent tasks are retained for follow-up messages."
	}
	return "List active shell and agent tasks visible to the current session."
}

func (t *TaskTool) DescribeRequest(raw json.RawMessage) (string, error) {
	if t.Kind == "task_list_active" {
		return "List active tasks", nil
	}
	var input taskInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return "", err
	}
	return fmt.Sprintf("Interrupt task %s", input.TaskID), nil
}

func (t *TaskTool) JSONSchema() json.RawMessage {
	if t.Kind == "task_interrupt" {
		return json.RawMessage(`{"type":"object","properties":{"task_id":{"type":"string","minLength":1}},"required":["task_id"],"additionalProperties":false}`)
	}
	return json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)
}

type taskInput struct {
	TaskID string `json:"task_id"`
}

func (t *TaskTool) Plan(_ context.Context, raw json.RawMessage, call CallContext) (Plan, error) {
	if t.Controller == nil || call.SessionID == "" {
		return Plan{}, errors.New("task: controller and caller session are required")
	}
	var input taskInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return Plan{}, err
	}
	if t.Kind == "task_interrupt" && input.TaskID == "" {
		return Plan{}, errors.New("task_interrupt: task_id is required")
	}
	if t.Kind != "task_interrupt" && t.Kind != "task_list_active" {
		return Plan{}, errors.New("task: unknown operation")
	}
	return NewPlan(t.ID(), raw, nil, nil, input)
}

func (t *TaskTool) Execute(ctx context.Context, plan Plan, call CallContext) (Result, error) {

	input, ok := plan.Data.(taskInput)
	if !ok {
		return Result{}, errors.New("task: incompatible plan")
	}
	if t.Kind == "task_interrupt" {
		item, err := t.Controller.Interrupt(ctx, call.SessionID, input.TaskID)
		if err != nil {
			return Result{}, err
		}
		return taskResult(item), nil
	}
	items := t.Controller.ListActive(call.SessionID)
	if items == nil {
		items = []managedtask.Active{}
	}
	metadata := map[string]any{"tasks": items}
	text, _ := json.Marshal(metadata)
	output := string(text)
	return Result{Text: output, ModelText: output, Metadata: metadata}, nil
}

func taskResult(item managedtask.Active) Result {
	data, _ := json.Marshal(item)
	var metadata map[string]any
	_ = json.Unmarshal(data, &metadata)
	text := string(data)
	return Result{Text: text, ModelText: text, Metadata: metadata}
}
