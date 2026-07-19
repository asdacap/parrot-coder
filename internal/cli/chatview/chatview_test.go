package chatview

import "testing"

func TestTaskActivityLabelsUseTaskID(t *testing.T) {
	for _, test := range []struct {
		name  string
		input map[string]any
		want  string
	}{
		{name: "agent_send", input: map[string]any{"task_id": "task_agent", "message": "continue"}, want: "agent_send · task_agent · continue"},
		{name: "monitor", input: map[string]any{"task_id": "task_agent"}, want: "monitor · task_agent"},
		{name: "task_interrupt", input: map[string]any{"task_id": "task_agent"}, want: "task_interrupt · task_agent"},
		{name: "task_list_active", input: map[string]any{}, want: "task_list_active"},
		{name: "write_stdin", input: map[string]any{"task_id": "task_shell", "chars": "input"}, want: "write_stdin · task_shell · input"},
	} {
		if got := ToolActivityLabel(test.name, test.input); got != test.want {
			t.Errorf("ToolActivityLabel(%q) = %q, want %q", test.name, got, test.want)
		}
	}
}
