package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/amirulashraf/parrot-coder/internal/monitor"
)

const monitorSchema = `{"type":"object","properties":{"task_id":{"type":"string","minLength":1,"description":"Identifier of the running shell or agent task."},"timeout_ms":{"type":"integer","minimum":0,"description":"Maximum time to monitor in milliseconds. Zero or omitted waits without a timeout."}},"required":["task_id"],"additionalProperties":false}`

const maxMonitorTimeoutMS = int64(^uint64(0)>>1) / int64(time.Millisecond)

// ProcessMonitor starts application-owned process monitors.
type ProcessMonitor interface {
	Start(monitor.Request) error
}

type MonitorTool struct {
	BasePresentation
	Service ProcessMonitor
}

type monitorInput struct {
	TaskID    string `json:"task_id"`
	TimeoutMS int64  `json:"timeout_ms"`
}

func NewMonitorTool(service ProcessMonitor) *MonitorTool { return &MonitorTool{Service: service} }

func (*MonitorTool) ID() string { return "monitor" }
func (*MonitorTool) Presentation() Presentation {
	return Presentation{
		Subagent: true,
		Label:    LabelSpec{Fields: []LabelField{{Names: []string{"task_id"}, TaskName: true}}},
	}
}

func (*MonitorTool) Description() string {
	return "Monitors a managed process in the background and steers a notification into the caller session when it exits or the monitor times out."
}

func (*MonitorTool) JSONSchema() json.RawMessage { return json.RawMessage(monitorSchema) }

func (*MonitorTool) DescribeRequest(raw json.RawMessage) (string, error) {
	var input monitorInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return "", err
	}
	return fmt.Sprintf("Monitor task %s", input.TaskID), nil
}

func (t *MonitorTool) Plan(_ context.Context, raw json.RawMessage, call CallContext) (Plan, error) {
	var input monitorInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return Plan{}, err
	}
	if call.SessionID == "" {
		return Plan{}, errors.New("monitor: caller session is required")
	}
	if input.TaskID == "" {
		return Plan{}, errors.New("monitor: task_id is required")
	}
	if input.TimeoutMS < 0 {
		return Plan{}, errors.New("monitor: timeout_ms must be nonnegative")
	}
	if input.TimeoutMS > maxMonitorTimeoutMS {
		return Plan{}, errors.New("monitor: timeout_ms is too large")
	}
	review, _ := json.Marshal(input)
	return NewPlan(t.ID(), raw, nil, review, input)
}

func (t *MonitorTool) Execute(_ context.Context, plan Plan, call CallContext) (Result, error) {

	if t.Service == nil {
		return Result{}, errors.New("monitor: service is required")
	}
	input := plan.Data.(monitorInput)
	if err := t.Service.Start(monitor.Request{SessionID: call.SessionID, CallerTask: call.TaskID, ToolCallID: call.ToolCallID, TaskID: input.TaskID, Timeout: time.Duration(input.TimeoutMS) * time.Millisecond}); err != nil {
		return Result{}, fmt.Errorf("monitor failed: %w", err)
	}
	text := fmt.Sprintf("Monitoring task %s in the background; this session will be notified when it finishes", input.TaskID)
	if input.TimeoutMS > 0 {
		text += fmt.Sprintf(" or after %d ms", input.TimeoutMS)
	}
	output := text + "."
	return Result{Text: output, ModelText: output}, nil
}
