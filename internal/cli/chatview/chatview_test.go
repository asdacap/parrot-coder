package chatview

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	v1 "github.com/amirulashraf/parrot-coder/internal/api/v1"
)

func TestCompletedInputPresentation(t *testing.T) {
	presentations := NewPresentations(v1.ToolList{Items: []v1.Tool{{
		ID: "spawn", Presentation: v1.ToolPresentation{CompletedInput: v1.ToolCompletedInput{
			Fields: []string{"name", "agent", "model", "prompt"}, TerminalOnly: true,
		}},
	}}})

	if !presentations.TerminalOnly("spawn") {
		t.Fatal("spawn presentation should suppress transient rows")
	}
	input := map[string]any{"prompt": "first line\nsecond line", "agent": "review", "model": "", "name": "review-task"}
	want := "name: review-task\nagent: review\nprompt: |-\n  first line\n  second line"
	if got := presentations.CompletedInputBlock("spawn", input); got != want {
		t.Fatalf("completed input block = %q, want %q", got, want)
	}

	tracker := StreamToolTracker{Presentation: presentations}
	pending := tracker.DescribeReport(v1.Event{Type: "session.tool.pending", Data: json.RawMessage(`{"call_id":"call","name":"spawn","input":{"name":"review-task","agent":"review","model":"","prompt":"first line\nsecond line"}}`)})
	if !pending.Hidden || pending.Terminal {
		t.Fatalf("pending report = %#v", pending)
	}
	success := tracker.DescribeReport(v1.Event{Type: "session.tool.success", Data: json.RawMessage(`{"call_id":"call","name":"spawn"}`)})
	if success.Hidden || !success.Terminal || success.Block != want {
		t.Fatalf("success report = %#v", success)
	}
}

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
	startMainTask(tracker)
	if _, err := tracker.Apply(taskLifecycleEvent(v1.EventTaskStart, v1.TaskEvent{TaskID: "task_named", SessionID: "session-named", ParentSessionID: "session-main", Kind: "agent", Agent: "review", Name: "review-kind-ibex"}), false); err != nil {
		t.Fatal(err)
	}
	reports, err := tracker.Apply(taskDelta("task_named", v1.MessagePartDelta{MessageID: "m", Kind: "text", Delta: "working"}), false)
	if err != nil || len(reports) != 1 || !strings.Contains(reports[0].Line, "○ [review:review-kind-ibex] response: working") {
		t.Fatalf("friendly task report = %#v, %v", reports, err)
	}
}

func TestTaskPrefixIncludesAgentNameAndLeadsWithActivityIcon(t *testing.T) {
	for _, test := range []struct {
		name       string
		depth      int
		agent      string
		session    string
		activity   string
		wantPrefix string
		want       string
	}{
		{name: "named tool", depth: 1, agent: "explorer", session: "ui-hierarchy", activity: "✓ read · internal/app/app.go", wantPrefix: "  [explorer:ui-hierarchy] ", want: "  ✓ [explorer:ui-hierarchy] read · internal/app/app.go"},
		{name: "streaming tool", depth: 1, agent: "explorer", session: "ui-hierarchy", activity: "  ◐ Running exec_command", wantPrefix: "  [explorer:ui-hierarchy] ", want: "  ◐ [explorer:ui-hierarchy] Running exec_command"},
		{name: "agent only", depth: 2, agent: "review", activity: "○ response: checking", wantPrefix: "    [review] ", want: "    ○ [review] response: checking"},
		{name: "name only", depth: 1, session: "ui-hierarchy", activity: "⠋ Thought: inspecting", wantPrefix: "  [ui-hierarchy] ", want: "  ⠋ [ui-hierarchy] Thought: inspecting"},
		{name: "generic fallback", depth: 0, activity: "status: idle", wantPrefix: "  [agent] ", want: "  [agent] status: idle"},
	} {
		t.Run(test.name, func(t *testing.T) {
			prefix := taskPrefix(test.depth, test.agent, test.session)
			if prefix != test.wantPrefix {
				t.Errorf("taskPrefix() = %q, want %q", prefix, test.wantPrefix)
			}
			if got := prefixTaskActivity(prefix, test.activity); got != test.want {
				t.Errorf("prefixTaskActivity() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestTaskToolOutputDeltaLeadsWithActivityIcon(t *testing.T) {
	tracker := NewTaskTracker()
	_, err := tracker.Apply(taskLifecycleEvent(v1.EventTaskStart, v1.TaskEvent{
		TaskID: "task-explorer", SessionID: "session-explorer", Kind: "agent", Agent: "explorer", Name: "ui-hierarchy",
	}), false)
	if err != nil {
		t.Fatal(err)
	}
	_, err = tracker.Apply(v1.Event{
		Type: "session.tool.running", TaskID: "task-explorer",
		Data: json.RawMessage(`{"call_id":"call","name":"exec_command","input":{"cmd":"go test ./..."}}`),
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(v1.ToolOutputDelta{ToolCallID: "call", Delta: "ok"})
	reports, err := tracker.Apply(v1.Event{Type: v1.EventToolOutputDelta, TaskID: "task-explorer", Data: data}, false)
	if err != nil || len(reports) != 1 || reports[0].Line != "  ◐ [explorer:ui-hierarchy] Running exec_command · go test ./..." {
		t.Fatalf("tool output report = %#v, %v", reports, err)
	}
}

func TestTaskTrackerRendersMonitorLifecycleForMainAndChildTasks(t *testing.T) {
	tracker := NewTaskTracker()
	startMainTask(tracker)
	if _, err := tracker.Apply(taskLifecycleEvent(v1.EventTaskStart, v1.TaskEvent{
		TaskID: "task-child", SessionID: "session-child", ParentSessionID: "session-main", Kind: "agent", Agent: "review",
	}), false); err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name     string
		taskID   string
		prefix   string
		wantLine string
	}{
		{name: "main", taskID: "", prefix: "task_main:", wantLine: "⠋ Monitoring task proc_main · timeout 1.5s"},
		{name: "child", taskID: "task-child", prefix: "task-child:", wantLine: "  ⠋ [review] Monitoring task proc_main · timeout 1.5s"},
	} {
		t.Run(test.name, func(t *testing.T) {
			started, _ := json.Marshal(v1.MonitorEvent{ToolCallID: "call-shared", TaskID: "proc_main", TimeoutMS: 1500})
			reports, err := tracker.Apply(v1.Event{Type: v1.EventMonitorStarted, TaskID: test.taskID, Data: started}, false)
			if err != nil || len(reports) != 1 {
				t.Fatalf("started reports = %#v, %v", reports, err)
			}
			if report := reports[0]; report.ID != test.prefix+"monitor:call-shared" || report.Line != test.wantLine || report.Terminal || report.EmitPlain {
				t.Fatalf("started report = %#v", report)
			}
			if reports[0].ID == test.prefix+"tool:call-shared" {
				t.Fatalf("monitor ID collided with tool ID: %q", reports[0].ID)
			}

			finished, _ := json.Marshal(v1.MonitorEvent{ToolCallID: "call-shared", TaskID: "proc_main", TimeoutMS: 1500, Status: "failed", Error: "wait failed"})
			reports, err = tracker.Apply(v1.Event{Type: v1.EventMonitorFinished, TaskID: test.taskID, Data: finished}, false)
			if err != nil || len(reports) != 1 {
				t.Fatalf("finished reports = %#v, %v", reports, err)
			}
			if report := reports[0]; report.ID != test.prefix+"monitor:call-shared" || !report.Terminal || !report.EmitPlain || !strings.Contains(report.Line, "✗") || !strings.Contains(report.Line, "wait failed") {
				t.Fatalf("finished report = %#v", report)
			}
		})
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
	streamSessionID := event.ParentSessionID
	if streamSessionID == "" {
		streamSessionID = event.SessionID
	}
	return v1.Event{Type: eventType, SessionID: streamSessionID, TaskID: event.TaskID, Data: data}
}

func startMainTask(tracker *TaskTracker) {
	_, _ = tracker.Apply(taskLifecycleEvent(v1.EventTaskStart, v1.TaskEvent{TaskID: "task_main", SessionID: "session-main", Kind: "main"}), false)
}

func taskDelta(taskID string, delta v1.MessagePartDelta) v1.Event {
	data, _ := json.Marshal(delta)
	return v1.Event{Type: v1.EventMessagePartDelta, TaskID: taskID, Data: data}
}

func TestTaskTrackerProjectsTreeAndReportOwners(t *testing.T) {
	tracker := NewTaskTracker()
	startMainTask(tracker)
	for _, start := range []v1.TaskEvent{
		{TaskID: "task-z", SessionID: "session-z", ParentSessionID: "session-main", Kind: "agent", Agent: "review", Name: "review-z"},
		{TaskID: "task-child", SessionID: "session-child", ParentSessionID: "session-z", Kind: "agent", Agent: "explore"},
		{TaskID: "task-a", SessionID: "session-a", ParentSessionID: "session-main", Kind: "agent", Agent: "worker"},
	} {
		if _, err := tracker.Apply(taskLifecycleEvent(v1.EventTaskStart, start), false); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tracker.Apply(taskLifecycleEvent(v1.EventTaskIdle, v1.TaskEvent{TaskID: "task-a"}), false); err != nil {
		t.Fatal(err)
	}

	wantTasks := []TaskInfo{
		{TaskID: "task_main", SessionID: "session-main", Kind: "main", Status: "working"},
		{TaskID: "task-a", SessionID: "session-a", ParentSessionID: "session-main", Kind: "agent", Agent: "worker", Status: "idle"},
		{TaskID: "task-z", SessionID: "session-z", ParentSessionID: "session-main", Kind: "agent", Agent: "review", Name: "review-z", Status: "working"},
		{TaskID: "task-child", SessionID: "session-child", ParentSessionID: "session-z", Kind: "agent", Agent: "explore", Status: "working"},
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
	if report := reports[0]; report.TaskID != "task-child" || report.SessionID != "session-child" || report.ParentSessionID != "session-z" || report.MainStatus {
		t.Fatalf("content report metadata = %#v", report)
	}

	data, _ := json.Marshal(v1.TaskProgress{TaskID: "task-child", ToolCallID: "call", Agent: "explore", Status: "running"})
	reports, err = tracker.Apply(v1.Event{Type: v1.EventTaskProgress, TaskID: "task-child", Data: data}, false)
	if err != nil || len(reports) != 1 {
		t.Fatalf("progress reports = %#v, %v", reports, err)
	}
	if report := reports[0]; report.TaskID != "task-child" || report.SessionID != "session-child" || report.ParentSessionID != "session-z" || !report.MainStatus {
		t.Fatalf("progress report metadata = %#v", report)
	}
}

func TestTaskTrackerDefersProgressUntilDescendantsFinish(t *testing.T) {
	tracker := NewTaskTracker()
	startMainTask(tracker)
	apply := func(event v1.Event) []TaskReport {
		reports, err := tracker.Apply(event, false)
		if err != nil {
			t.Fatal(err)
		}
		return reports
	}
	progress := func(taskID, status string, tokens, tools int) v1.Event {
		data, _ := json.Marshal(v1.TaskProgress{TaskID: taskID, ToolCallID: "call-" + taskID, Agent: "explore", Status: status, Usage: v1.Usage{TotalTokens: tokens}, ToolUses: tools})
		return v1.Event{Type: v1.EventTaskProgress, TaskID: taskID, Data: data}
	}

	apply(taskLifecycleEvent(v1.EventTaskStart, v1.TaskEvent{TaskID: "parent", SessionID: "session-parent", ParentSessionID: "session-main", Kind: "agent", Agent: "explore", Name: "parent"}))
	apply(progress("parent", "running", 100, 2))
	apply(taskLifecycleEvent(v1.EventTaskStart, v1.TaskEvent{TaskID: "child", SessionID: "session-child", ParentSessionID: "session-parent", Kind: "agent", Agent: "explore", Name: "child"}))
	apply(taskLifecycleEvent(v1.EventTaskStart, v1.TaskEvent{TaskID: "grandchild", SessionID: "session-grandchild", ParentSessionID: "session-child", Kind: "agent", Agent: "explore", Name: "grandchild"}))

	reports := apply(progress("parent", "succeeded", 125, 3))
	if len(reports) != 1 || reports[0].Terminal || !strings.Contains(reports[0].Line, "⠋ [explore:parent] agent: explore · 125 tokens · 3 tools · 1 active task") {
		t.Fatalf("parent terminal progress with descendants = %#v", reports)
	}
	apply(taskLifecycleEvent(v1.EventTaskFinished, v1.TaskEvent{TaskID: "child", Status: "succeeded"}))
	reports = apply(taskLifecycleEvent(v1.EventTaskFinished, v1.TaskEvent{TaskID: "grandchild", Status: "succeeded"}))
	var parent *TaskReport
	for i := range reports {
		if reports[i].TaskID == "parent" {
			parent = &reports[i]
		}
	}
	if parent == nil || !parent.Terminal || !parent.EmitPlain || !strings.Contains(parent.Line, "✓ [explore:parent] agent: explore · 125 tokens · 3 tools") {
		t.Fatalf("settled parent progress = %#v", reports)
	}
	if reports := apply(taskLifecycleEvent(v1.EventTaskIdle, v1.TaskEvent{TaskID: "grandchild"})); len(reports) != 0 {
		t.Fatalf("duplicate settled progress = %#v", reports)
	}
}

func TestTaskTrackerTracksTreeAndReportsUnknownTasks(t *testing.T) {
	tracker := NewTaskTracker()
	startMainTask(tracker)

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
	reports, err := tracker.Apply(taskLifecycleEvent(v1.EventTaskStart, v1.TaskEvent{TaskID: "task-orphan", SessionID: "session-orphan", ParentSessionID: "session-missing", Kind: "agent", Agent: "explore"}), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 1 || reports[0].Line != "✗ unknown task session-missing (parent session of session-orphan)" {
		t.Fatalf("orphan parent reports = %#v", reports)
	}
	reports, err = tracker.Apply(taskDelta("task-orphan", v1.MessagePartDelta{MessageID: "m", Kind: "text", Delta: "orphan work"}), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 1 || !strings.Contains(reports[0].Line, "○ [explore] response: orphan work") {
		t.Fatalf("orphan content reports = %#v", reports)
	}

	// Depth comes from the parent chain the tracker builds, not from the
	// events themselves.
	for _, start := range []v1.TaskEvent{
		{TaskID: "task-parent", SessionID: "session-parent", ParentSessionID: "session-main", Kind: "agent", Agent: "build"},
		{TaskID: "task-child", SessionID: "session-child", ParentSessionID: "session-parent", Kind: "agent", Agent: "review"},
	} {
		if _, err := tracker.Apply(taskLifecycleEvent(v1.EventTaskStart, start), false); err != nil {
			t.Fatal(err)
		}
	}
	reports, err = tracker.Apply(taskDelta("task-child", v1.MessagePartDelta{MessageID: "m", Kind: "text", Delta: "nested"}), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 1 || reports[0].Line != "    ○ [review] response: nested" {
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
	if len(reports) != 1 || !reports[0].Terminal || reports[0].Line != "    ✗ [review] failed: boom" {
		t.Fatalf("finished report = %#v", reports)
	}

	// Successful tasks surface a terminal completion line under their own prefix.
	reports, err = tracker.Apply(taskLifecycleEvent(v1.EventTaskFinished, v1.TaskEvent{TaskID: "task-child", Kind: "agent", Status: "succeeded"}), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 1 || !reports[0].Terminal || reports[0].Line != "    ✓ [review] completed" {
		t.Fatalf("finished report = %#v", reports)
	}
}
