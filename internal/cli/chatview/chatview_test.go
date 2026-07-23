package chatview

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	v1 "github.com/amirulashraf/parrot-coder/internal/api/v1"
)

func TestPresentationResolvesFriendlyTaskNames(t *testing.T) {
	presentations := NewPresentations(v1.ToolList{Items: []v1.Tool{{
		ID: "control", Presentation: v1.ToolPresentation{Label: v1.ToolLabel{Fields: []v1.ToolLabelPart{{Names: []string{"task_id"}, TaskName: true}}}},
	}, {
		ID: "spawn", Presentation: v1.ToolPresentation{Label: v1.ToolLabel{Fields: []v1.ToolLabelPart{{Names: []string{"name", "agent"}}}}},
	}}})
	presentations.taskNames["task_known"] = "review-happy-otter"

	if got := presentations.Label("control", map[string]any{"task_id": "task_known"}); got != "control · review-happy-otter" {
		t.Fatalf("known task label = %q", got)
	}
	if got := presentations.Label("control", map[string]any{"task_id": "task_unknown"}); got != "control · task_unknown" {
		t.Fatalf("unknown task label = %q", got)
	}
	input := presentations.EnrichLabelInput("spawn", map[string]any{"agent": "review"}, `{"name":"review-kind-ibex"}`)
	if got := presentations.Label("spawn", input); got != "spawn · review-kind-ibex" {
		t.Fatalf("result-enriched spawn label = %q", got)
	}

	tracker := NewTaskTracker()
	tracker.Presentation = presentations
	if _, err := tracker.Apply(taskLifecycleEvent(v1.EventTaskStart, v1.TaskEvent{TaskID: "task_named", ParentTaskID: "task_main", Kind: "agent", Agent: "review", Name: "review-kind-ibex"}), false); err != nil {
		t.Fatal(err)
	}
	reports, err := tracker.Apply(taskDelta("task_named", v1.MessagePartDelta{MessageID: "m", Kind: "text", Delta: "working"}), false)
	if err != nil || len(reports) != 1 || !strings.Contains(reports[0].Line, "[review-kind-ibex]") {
		t.Fatalf("friendly task report = %#v, %v", reports, err)
	}
}

func TestTaskActivityLabelsUseTaskID(t *testing.T) {
	for _, test := range []struct {
		name  string
		input map[string]any
		want  string
	}{
		{name: "agent_send", input: map[string]any{"task_id": "task_agent", "message": "continue"}, want: "agent_send · task_agent · continue"},
		{name: "monitor", input: map[string]any{"task_id": "task_agent"}, want: "monitor · task_agent"},
		{name: "wait_task", input: map[string]any{"task_id": "task_agent"}, want: "wait_task · task_agent"},
		{name: "task_interrupt", input: map[string]any{"task_id": "task_agent"}, want: "task_interrupt · task_agent"},
		{name: "task_list_active", input: map[string]any{}, want: "task_list_active"},
		{name: "write_stdin", input: map[string]any{"task_id": "task_shell", "chars": "input"}, want: "write_stdin · task_shell · input"},
	} {
		if got := ToolActivityLabel(test.name, test.input); got != test.want {
			t.Errorf("ToolActivityLabel(%q) = %q, want %q", test.name, got, test.want)
		}
	}
}

func taskLifecycleEvent(eventType string, event v1.TaskEvent) v1.Event {
	data, _ := json.Marshal(event)
	return v1.Event{Type: eventType, TaskID: event.TaskID, Data: data}
}

func taskDelta(taskID string, delta v1.MessagePartDelta) v1.Event {
	data, _ := json.Marshal(delta)
	return v1.Event{Type: v1.EventMessagePartDelta, TaskID: taskID, Data: data}
}

func TestTaskTrackerProjectsTreeAndReportOwners(t *testing.T) {
	tracker := NewTaskTracker()
	for _, start := range []v1.TaskEvent{
		{TaskID: "task-z", ParentTaskID: "task_main", Kind: "agent", Agent: "review", Name: "review-z"},
		{TaskID: "task-child", ParentTaskID: "task-z", Kind: "agent", Agent: "explore"},
		{TaskID: "task-a", ParentTaskID: "task_main", Kind: "agent", Agent: "worker"},
	} {
		if _, err := tracker.Apply(taskLifecycleEvent(v1.EventTaskStart, start), false); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tracker.Apply(taskLifecycleEvent(v1.EventTaskIdle, v1.TaskEvent{TaskID: "task-a"}), false); err != nil {
		t.Fatal(err)
	}

	wantTasks := []TaskInfo{
		{TaskID: "task_main", Kind: "main"},
		{TaskID: "task-a", ParentTaskID: "task_main", Kind: "agent", Agent: "worker", Status: "idle"},
		{TaskID: "task-z", ParentTaskID: "task_main", Kind: "agent", Agent: "review", Name: "review-z", Status: "working"},
		{TaskID: "task-child", ParentTaskID: "task-z", Kind: "agent", Agent: "explore", Status: "working"},
	}
	if got := tracker.Tasks(); !reflect.DeepEqual(got, wantTasks) {
		t.Fatalf("Tasks() = %#v, want %#v", got, wantTasks)
	}
	// The returned slice is a snapshot, not mutable tracker state.
	tasks := tracker.Tasks()
	tasks[1].Status = "changed"
	if got := tracker.Tasks()[1].Status; got != "idle" {
		t.Fatalf("mutating Tasks result changed tracker status to %q", got)
	}

	reports, err := tracker.Apply(taskDelta("task-child", v1.MessagePartDelta{MessageID: "m", Kind: "text", Delta: "work"}), false)
	if err != nil || len(reports) != 1 {
		t.Fatalf("content reports = %#v, %v", reports, err)
	}
	if report := reports[0]; report.TaskID != "task-child" || report.ParentTaskID != "task-z" || report.MainStatus {
		t.Fatalf("content report metadata = %#v", report)
	}

	data, _ := json.Marshal(v1.TaskProgress{TaskID: "task-child", ToolCallID: "call", Agent: "explore", Status: "running"})
	reports, err = tracker.Apply(v1.Event{Type: v1.EventTaskProgress, TaskID: "task-child", Data: data}, false)
	if err != nil || len(reports) != 1 {
		t.Fatalf("progress reports = %#v, %v", reports, err)
	}
	if report := reports[0]; report.TaskID != "task-child" || report.ParentTaskID != "task-z" || !report.MainStatus {
		t.Fatalf("progress report metadata = %#v", report)
	}
}

func TestTaskTrackerTracksTreeAndReportsUnknownTasks(t *testing.T) {
	tracker := NewTaskTracker()

	// Content for a task which never started is an unknown task error, shown
	// once no matter how many events reference the unknown id.
	for i := 0; i < 2; i++ {
		reports, err := tracker.Apply(taskDelta("task-ghost", v1.MessagePartDelta{MessageID: "m", Kind: "text", Delta: "hi"}), false)
		if err != nil {
			t.Fatal(err)
		}
		want := 1
		if i == 1 {
			want = 0
		}
		if len(reports) != want {
			t.Fatalf("ghost reports[%d] = %#v", i, reports)
		}
		if want == 1 && (!reports[0].Terminal || reports[0].Line != "✗ unknown task task-ghost (message.part.delta)") {
			t.Fatalf("unknown task report = %#v", reports[0])
		}
	}

	// A start with an unknown parent registers the task and reports the
	// missing parent; the orphan still renders its own content.
	reports, err := tracker.Apply(taskLifecycleEvent(v1.EventTaskStart, v1.TaskEvent{TaskID: "task-orphan", ParentTaskID: "task-missing", Kind: "agent", Agent: "explore"}), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 1 || reports[0].Line != "✗ unknown task task-missing (parent of task-orphan)" {
		t.Fatalf("orphan parent reports = %#v", reports)
	}
	reports, err = tracker.Apply(taskDelta("task-orphan", v1.MessagePartDelta{MessageID: "m", Kind: "text", Delta: "orphan work"}), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 1 || !strings.Contains(reports[0].Line, "[explore] ○ response: orphan work") {
		t.Fatalf("orphan content reports = %#v", reports)
	}

	// Depth comes from the parent chain the tracker builds, not from the
	// events themselves.
	for _, start := range []v1.TaskEvent{
		{TaskID: "task-parent", ParentTaskID: "task_main", Kind: "agent", Agent: "build"},
		{TaskID: "task-child", ParentTaskID: "task-parent", Kind: "agent", Agent: "review"},
	} {
		if _, err := tracker.Apply(taskLifecycleEvent(v1.EventTaskStart, start), false); err != nil {
			t.Fatal(err)
		}
	}
	reports, err = tracker.Apply(taskDelta("task-child", v1.MessagePartDelta{MessageID: "m", Kind: "text", Delta: "nested"}), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 1 || reports[0].Line != "    [review] ○ response: nested" {
		t.Fatalf("nested content report = %#v", reports)
	}

	// Main task content returns nothing: the caller renders it directly.
	reports, err = tracker.Apply(taskDelta("task_main", v1.MessagePartDelta{MessageID: "m", Kind: "text", Delta: "mine"}), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 0 {
		t.Fatalf("main task reports = %#v", reports)
	}

	// Failed tasks surface a terminal error line under their own prefix.
	reports, err = tracker.Apply(taskLifecycleEvent(v1.EventTaskFinished, v1.TaskEvent{TaskID: "task-child", Kind: "agent", Status: "failed", Error: "boom"}), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 1 || !reports[0].Terminal || reports[0].Line != "    [review] ✗ failed: boom" {
		t.Fatalf("finished report = %#v", reports)
	}

	// Successful tasks surface a terminal completion line under their own prefix.
	reports, err = tracker.Apply(taskLifecycleEvent(v1.EventTaskFinished, v1.TaskEvent{TaskID: "task-child", Kind: "agent", Status: "succeeded"}), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 1 || !reports[0].Terminal || reports[0].Line != "    [review] ✓ completed" {
		t.Fatalf("finished report = %#v", reports)
	}
}
