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
	processID int32
	timeout   time.Duration
	err       error
}

func (m *recordingProcessMonitor) Start(sessionID string, processID int32, timeout time.Duration) error {
	m.sessionID, m.processID, m.timeout = sessionID, processID, timeout
	return m.err
}

func TestMonitorToolPlansAndStartsBackgroundMonitor(t *testing.T) {
	monitor := &recordingProcessMonitor{}
	item := NewMonitorTool(monitor)
	raw := json.RawMessage(`{"session_id":42,"timeout_ms":1500}`)
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
	if monitor.sessionID != "caller" || monitor.processID != 42 || monitor.timeout != 1500*time.Millisecond {
		t.Fatalf("monitor call = %q, %d, %s", monitor.sessionID, monitor.processID, monitor.timeout)
	}
	if !strings.Contains(result.Text, "42") || !strings.Contains(result.Text, "1500 ms") {
		t.Fatalf("result = %q", result.Text)
	}
	if described, err := item.DescribeRequest(raw); err != nil || described != "Monitor process 42" {
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
		{name: "missing caller", raw: `{"session_id":1}`},
		{name: "invalid process", raw: `{"session_id":0}`, call: CallContext{SessionID: "caller"}},
		{name: "negative timeout", raw: `{"session_id":1,"timeout_ms":-1}`, call: CallContext{SessionID: "caller"}},
		{name: "overflowing timeout", raw: `{"session_id":1,"timeout_ms":9223372036854775807}`, call: CallContext{SessionID: "caller"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := item.Plan(context.Background(), json.RawMessage(test.raw), test.call); err == nil {
				t.Fatal("Plan succeeded")
			}
		})
	}
}
