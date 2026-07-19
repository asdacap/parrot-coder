package tool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

type recordingProcessMonitor struct {
	sessionID string
	taskID    string
	timeout   time.Duration
	err       error
}

func (m *recordingProcessMonitor) Start(sessionID, taskID string, timeout time.Duration) error {
	m.sessionID, m.taskID, m.timeout = sessionID, taskID, timeout
	return m.err
}

func TestMonitorToolPlansAndStartsBackgroundMonitor(t *testing.T) {
	monitor := &recordingProcessMonitor{}
	item := NewMonitorTool(monitor)
	raw := json.RawMessage(`{"task_id":"proc_test","timeout_ms":1500}`)
	plan, err := item.Plan(context.Background(), raw, CallContext{SessionID: "caller"})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Permissions) != 0 {
		t.Fatalf("permissions = %#v", plan.Permissions)
	}
	result, err := item.Execute(context.Background(), plan, CallContext{SessionID: "caller"})
	if err != nil {
		t.Fatal(err)
	}
	if monitor.sessionID != "caller" || monitor.taskID != "proc_test" || monitor.timeout != 1500*time.Millisecond {
		t.Fatalf("monitor call = %q, %q, %s", monitor.sessionID, monitor.taskID, monitor.timeout)
	}
	if !strings.Contains(result.Text, "proc_test") || !strings.Contains(result.Text, "1500 ms") {
		t.Fatalf("result = %q", result.Text)
	}
	if described, err := item.DescribeRequest(raw); err != nil || described != "Monitor task proc_test" {
		t.Fatalf("description = %q, %v", described, err)
	}
}

func TestMonitorToolRejectsInvalidRequests(t *testing.T) {
	item := NewMonitorTool(&recordingProcessMonitor{})
	tests := []struct {
		name string
		raw  string
		call CallContext
	}{
		{name: "missing caller", raw: `{"task_id":"proc_test"}`},
		{name: "missing task", raw: `{}`, call: CallContext{SessionID: "caller"}},
		{name: "negative timeout", raw: `{"task_id":"proc_test","timeout_ms":-1}`, call: CallContext{SessionID: "caller"}},
		{name: "overflowing timeout", raw: `{"task_id":"proc_test","timeout_ms":9223372036854775807}`, call: CallContext{SessionID: "caller"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := item.Plan(context.Background(), json.RawMessage(test.raw), test.call); err == nil {
				t.Fatal("Plan succeeded")
			}
		})
	}
}
