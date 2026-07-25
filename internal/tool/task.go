package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	managedtask "github.com/amirulashraf/parrot-coder/internal/task"
)

type TaskController interface {
	Interrupt(context.Context, string, string) (managedtask.Active, error)
	ListActive(string) []managedtask.Active
	WaitKind(context.Context, string, string, managedtask.Kind) (managedtask.Result, error)
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
		&WaitTool{Kind: managedtask.KindAgent, Controller: controller},
		&WaitTool{Kind: managedtask.KindShell, Controller: controller},
	}
}

func (t *TaskTool) ID() string { return t.Kind }

func (t *TaskTool) Presentation() Presentation {
	if t.Kind == "task_interrupt" {
		return Presentation{Label: LabelSpec{Fields: []LabelField{{Names: []string{"task_id"}, TaskName: true}}}}
	}
	return Presentation{}
}
func (t *TaskTool) Description() string {
	if t.Kind == "task_interrupt" {
		return "Interrupt a running shell process or child agent session by canonical ID or friendly name. Agent sessions are retained for follow-up messages."
	}
	return "List active shell processes and child agent sessions visible to the current session. Returned task_id values are process IDs for shells and child session IDs for agents."
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
		return json.RawMessage(`{"type":"object","properties":{"task_id":{"type":"string","minLength":1,"description":"Canonical process or child session ID, or friendly task name."}},"required":["task_id"],"additionalProperties":false}`)
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
	return resultFromJSON(data)
}

const (
	waitAgentSchema   = `{"type":"object","properties":{"session_id":{"type":"string","minLength":1,"description":"Child agent session ID or friendly name."},"yield_after_ms":{"type":"integer","minimum":0,"description":"Yield if the agent has not completed after this many milliseconds. Zero or omitted waits indefinitely."}},"required":["session_id"],"additionalProperties":false}`
	waitProcessSchema = `{"type":"object","properties":{"process_id":{"type":"string","minLength":1,"description":"Shell process ID or friendly name."},"yield_after_ms":{"type":"integer","minimum":0,"description":"Yield if the process has not completed after this many milliseconds. Zero or omitted waits indefinitely."}},"required":["process_id"],"additionalProperties":false}`
	maxTaskWaitMS     = int64(^uint64(0)>>1) / int64(time.Millisecond)
)

type WaitTool struct {
	BasePresentation
	Kind       managedtask.Kind
	Controller TaskController
}

type waitInput struct {
	SessionID    string `json:"session_id"`
	ProcessID    string `json:"process_id"`
	YieldAfterMS int64  `json:"yield_after_ms"`
}

func (t *WaitTool) ID() string {
	if t.Kind == managedtask.KindAgent {
		return "wait_agent"
	}
	return "wait_process"
}

func (t *WaitTool) identifier(input waitInput) string {
	if t.Kind == managedtask.KindAgent {
		return input.SessionID
	}
	return input.ProcessID
}

func (t *WaitTool) Description() string {
	if t.Kind == managedtask.KindAgent {
		return "Wait for a child agent session to complete, yielding if the requested period elapses. session_id accepts the canonical child session ID or friendly name. Waiting never stops the agent."
	}
	return "Wait for a shell process to complete, yielding if the requested period elapses. process_id accepts the canonical process ID or friendly name. Waiting never stops the process."
}

func (t *WaitTool) Presentation() Presentation {
	field := "process_id"
	if t.Kind == managedtask.KindAgent {
		field = "session_id"
	}
	return Presentation{Subagent: t.Kind == managedtask.KindAgent, Modeline: true, LiveOnly: true, Label: LabelSpec{Fields: []LabelField{{Names: []string{field}, TaskName: true}}}}
}

func (t *WaitTool) JSONSchema() json.RawMessage {
	if t.Kind == managedtask.KindAgent {
		return json.RawMessage(waitAgentSchema)
	}
	return json.RawMessage(waitProcessSchema)
}

func (t *WaitTool) DescribeRequest(raw json.RawMessage) (string, error) {
	var input waitInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return "", err
	}
	return fmt.Sprintf("Wait for %s %s", t.Kind, t.identifier(input)), nil
}

func (t *WaitTool) Plan(_ context.Context, raw json.RawMessage, call CallContext) (Plan, error) {
	if t.Controller == nil || call.SessionID == "" {
		return Plan{}, fmt.Errorf("%s: controller and caller session are required", t.ID())
	}
	if t.Kind != managedtask.KindAgent && t.Kind != managedtask.KindShell {
		return Plan{}, errors.New("wait: unsupported task kind")
	}
	var input waitInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return Plan{}, err
	}
	if t.identifier(input) == "" {
		return Plan{}, fmt.Errorf("%s: identifier is required", t.ID())
	}
	if input.YieldAfterMS < 0 {
		return Plan{}, fmt.Errorf("%s: yield_after_ms must be nonnegative", t.ID())
	}
	if input.YieldAfterMS > maxTaskWaitMS {
		return Plan{}, fmt.Errorf("%s: yield_after_ms is too large", t.ID())
	}
	return NewPlan(t.ID(), raw, nil, nil, input)
}

func (t *WaitTool) Execute(ctx context.Context, plan Plan, call CallContext) (Result, error) {
	input, ok := plan.Data.(waitInput)
	if !ok {
		return Result{}, fmt.Errorf("%s: incompatible plan", t.ID())
	}
	waitCtx := ctx
	var cancel context.CancelFunc
	if input.YieldAfterMS > 0 {
		waitCtx, cancel = context.WithTimeout(ctx, time.Duration(input.YieldAfterMS)*time.Millisecond)
		defer cancel()
	}
	started := time.Now()
	item, err := t.Controller.WaitKind(waitCtx, call.SessionID, t.identifier(input), t.Kind)
	item.ElapsedMS = time.Since(started).Milliseconds()
	if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
		item.Yielded = true
		return taskWaitResult(item), nil
	}
	if err != nil {
		return Result{}, err
	}
	return taskWaitResult(item), nil
}

func taskWaitResult(item managedtask.Result) Result {
	data, _ := json.Marshal(item)
	return resultFromJSON(data)
}

func resultFromJSON(data []byte) Result {
	var metadata map[string]any
	_ = json.Unmarshal(data, &metadata)
	text := string(data)
	return Result{Text: text, ModelText: text, Metadata: metadata}
}
