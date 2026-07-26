package chatview

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	v1 "github.com/amirulashraf/parrot-coder/internal/api/v1"
	"github.com/amirulashraf/parrot-coder/internal/terminal"
)

func TestShellOutputTailKeepsLastTenLines(t *testing.T) {
	var tail ShellOutputTail
	for _, delta := range []string{"one\ntw", "o\nthree\nfour\nfive\nsix", "\nseven\neight\nnine\nten\neleven\ntwelve"} {
		tail.Write(delta)
	}
	want := "three\nfour\nfive\nsix\nseven\neight\nnine\nten\neleven\ntwelve"
	if got := tail.String(); got != want {
		t.Fatalf("shell output tail = %q, want %q", got, want)
	}
}

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

func TestResultCountPresentationUpdatesCompletedLabel(t *testing.T) {
	presentations := NewPresentations(v1.ToolList{Items: []v1.Tool{{
		ID: "search", Presentation: v1.ToolPresentation{
			Label:           v1.ToolLabel{Fields: []v1.ToolLabelPart{{Names: []string{"pattern"}, Quote: true}}},
			ResultCountNoun: "match",
		},
	}}})
	tracker := StreamToolTracker{Presentation: presentations}
	pending := tracker.DescribeReport(v1.Event{Type: "session.tool.pending", Data: json.RawMessage(`{"call_id":"call","name":"search","input":{"pattern":"needle"}}`)})
	success := tracker.DescribeReport(v1.Event{Type: "session.tool.success", Data: json.RawMessage(`{"call_id":"call","result":"one\ntwo\n"}`)})
	if pending.Label != `search · "needle"` || success.Label != `search · "needle" · 2 matches` {
		t.Fatalf("result count labels: pending=%q success=%q", pending.Label, success.Label)
	}

	tracker.DescribeReport(v1.Event{Type: "session.tool.pending", Data: json.RawMessage(`{"call_id":"empty","name":"search","input":{"pattern":"absent"}}`)})
	empty := tracker.DescribeReport(v1.Event{Type: "session.tool.success", Data: json.RawMessage(`{"call_id":"empty","result":""}`)})
	if empty.Label != `search · "absent" · 0 matches` {
		t.Fatalf("empty result label = %q", empty.Label)
	}
}

func TestDiffPresentationPreservesRawResultThroughTaskReports(t *testing.T) {
	tracker := NewTaskTracker()
	startMainTask(tracker)
	if _, err := tracker.Apply(taskLifecycleEvent(v1.EventTaskStart, v1.TaskEvent{
		TaskID: "child", SessionID: "child", ParentSessionID: "session-main", Kind: "agent", Agent: "worker",
	}), false); err != nil {
		t.Fatal(err)
	}
	pending := v1.Event{Type: "session.tool.pending", TaskID: "child", Data: json.RawMessage(`{"call_id":"call","name":"apply_patch"}`)}
	if _, err := tracker.Apply(pending, false); err != nil {
		t.Fatal(err)
	}
	diff := "--- a/file.go\n+++ b/file.go\n@@ -1 +1 @@\n-old\n+new\n"
	data, _ := json.Marshal(map[string]string{"call_id": "call", "result": diff})
	reports, err := tracker.Apply(v1.Event{Type: "session.tool.success", TaskID: "child", Data: data}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 1 || reports[0].BlockKind != ToolResultDiff || reports[0].Block != diff {
		t.Fatalf("diff task report = %#v", reports)
	}
}

func TestTaskCodeDisplayReportCarriesRenderingMetadata(t *testing.T) {
	tracker := NewTaskTracker()
	startMainTask(tracker)
	if _, err := tracker.Apply(taskLifecycleEvent(v1.EventTaskStart, v1.TaskEvent{
		TaskID: "child", SessionID: "child", ParentSessionID: "session-main", Kind: "agent", Agent: "worker",
	}), false); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(v1.CodeDisplay{ToolCallID: "call", Source: "package main\n", Path: "cmd/main.go", Language: "go", StartLine: 12})
	reports, err := tracker.Apply(v1.Event{Type: v1.EventCodeDisplay, TaskID: "child", Data: data}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 1 {
		t.Fatalf("code display reports = %#v", reports)
	}
	report := reports[0]
	if report.ID != "child:code:call" || report.TaskID != "child" || report.SessionID != "child" || report.ParentSessionID != "session-main" || report.Line != "  • [worker] ↳ Code · cmd/main.go:12" || report.Block != "package main\n" || report.BlockKind != ToolResultCode || report.BlockLanguage != "go" || !report.Terminal || !report.EmitPlain {
		t.Fatalf("code display report = %#v", report)
	}
}

func TestCodeDisplayStatus(t *testing.T) {
	for _, test := range []struct {
		name string
		item v1.CodeDisplay
		want string
	}{
		{name: "path and line", item: v1.CodeDisplay{Path: "main.go", StartLine: 7}, want: "↳ Code · main.go:7"},
		{name: "path", item: v1.CodeDisplay{Path: "main.go"}, want: "↳ Code · main.go"},
		{name: "line", item: v1.CodeDisplay{StartLine: 7}, want: "↳ Code · line 7"},
		{name: "anonymous", want: "↳ Code"},
		{name: "single-line location", item: v1.CodeDisplay{Path: "main.go\n✓ completed\t"}, want: "↳ Code · main.go ✓ completed "},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := CodeDisplayStatus(test.item); got != test.want {
				t.Fatalf("CodeDisplayStatus() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestLiveOnlyPresentationDiscardsTerminalReport(t *testing.T) {
	presentations := NewPresentations(v1.ToolList{Items: []v1.Tool{{
		ID: "wait", Presentation: v1.ToolPresentation{LiveOnly: true},
	}}})
	tracker := StreamToolTracker{Presentation: presentations}

	pending := tracker.DescribeReport(v1.Event{Type: "session.tool.pending", Data: json.RawMessage(`{"call_id":"call","name":"wait"}`)})
	success := tracker.DescribeReport(v1.Event{Type: "session.tool.success", Data: json.RawMessage(`{"call_id":"call"}`)})
	if !presentations.LiveOnly("wait") || !pending.LiveOnly || pending.Terminal || !success.LiveOnly || !success.Terminal {
		t.Fatalf("live-only reports: pending=%#v success=%#v", pending, success)
	}
}

func TestTaskLiveOnlyToolRemovesActiveRowWithoutPlainOutput(t *testing.T) {
	tracker := NewTaskTracker()
	tracker.Presentation = NewPresentations(v1.ToolList{Items: []v1.Tool{{
		ID: "wait", Presentation: v1.ToolPresentation{LiveOnly: true},
	}}})
	startMainTask(tracker)
	if _, err := tracker.Apply(taskLifecycleEvent(v1.EventTaskStart, v1.TaskEvent{
		TaskID: "child", SessionID: "child", ParentSessionID: "session-main", Kind: "agent", Agent: "worker",
	}), false); err != nil {
		t.Fatal(err)
	}

	pending, err := tracker.Apply(v1.Event{Type: "session.tool.pending", TaskID: "child", Data: json.RawMessage(`{"call_id":"call","name":"wait"}`)}, false)
	if err != nil {
		t.Fatal(err)
	}
	terminal, err := tracker.Apply(v1.Event{Type: "session.tool.failure", TaskID: "child", Data: json.RawMessage(`{"call_id":"call","error":"stopped"}`)}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].Skip || pending[0].Terminal || len(terminal) != 1 || !terminal[0].Skip || !terminal[0].Terminal || terminal[0].EmitPlain {
		t.Fatalf("task live-only reports: pending=%#v terminal=%#v", pending, terminal)
	}
}

func TestFailureErrorBlockPresentation(t *testing.T) {
	presentations := NewPresentations(v1.ToolList{Items: []v1.Tool{{
		ID: "custom_editor", Presentation: v1.ToolPresentation{Failure: ToolFailureErrorBlock},
	}}})
	tracker := StreamToolTracker{Presentation: presentations}
	tracker.DescribeReport(v1.Event{Type: "session.tool.pending", Data: json.RawMessage(`{"call_id":"call","name":"custom_editor","input":{"patch":"original request"}}`)})
	errorText := "patch planning failed with 2 errors:\n\n[1/2] first conflict\n<<<<<<< SEARCH\nfirst\n=======\n\n[2/2] second conflict\n<<<<<<< SEARCH\nsecond\n======="
	data, err := json.Marshal(map[string]any{"call_id": "call", "name": "custom_editor", "error": errorText})
	if err != nil {
		t.Fatal(err)
	}
	report := tracker.DescribeReport(v1.Event{Type: "session.tool.failure", Data: data})
	wantBlock := "✗ [1/2] first conflict\n<<<<<<< SEARCH\nfirst\n=======\n\n✗ [2/2] second conflict\n<<<<<<< SEARCH\nsecond\n======="
	if report.Line != "✗ tool: 2 errors" || report.Block != wantBlock || strings.Contains(report.Block, "original request") {
		t.Fatalf("failure report = %#v, want concise status and separately headed error sections", report)
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
	if _, err := tracker.Apply(taskLifecycleEvent(v1.EventTaskStart, v1.TaskEvent{TaskID: "task_named", SessionID: "task_named", ParentSessionID: "session-main", Kind: "agent", Agent: "review", Name: "review-kind-ibex"}), false); err != nil {
		t.Fatal(err)
	}
	reports, err := tracker.Apply(taskDelta("task_named", v1.MessagePartDelta{MessageID: "m", Kind: "text", Delta: "working"}), false)
	if err != nil || len(reports) != 1 || !strings.Contains(reports[0].Line, "○ [review:review-kind-ibex] response: working") {
		t.Fatalf("friendly task report = %#v, %v", reports, err)
	}
}

func TestEventLineRendersAgentAndSubagentEvents(t *testing.T) {
	for _, test := range []struct {
		name, icon, agent, event, want string
		indent                         int
	}{
		{name: "main agent", icon: "↻", event: "Status prompt injected", want: "↻ Status prompt injected"},
		{name: "subagent", indent: 1, icon: "↻", agent: "explorer:plan-sandbox-flow", event: "Status prompt injected", want: "  ↻ [explorer:plan-sandbox-flow] Status prompt injected"},
		{name: "nested subagent", indent: 2, icon: "○", agent: "review", event: "response: checking", want: "    ○ [review] response: checking"},
		{name: "default icon", indent: 1, agent: "agent", event: "status: idle", want: "  • [agent] status: idle"},
		{name: "multiline", indent: 1, icon: "✓", agent: "explorer", event: "first\nsecond", want: "  ✓ [explorer] first\n  [explorer] second"},
	} {
		t.Run(test.name, func(t *testing.T) {
			event := test.event
			if test.icon != "" {
				event = test.icon + " " + event
			}
			if got := EventLine(test.indent, test.agent, event); got != test.want {
				t.Errorf("EventLine() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestTaskStatusLeadsWithActivityIcon(t *testing.T) {
	for _, status := range []v1.SessionStatus{
		{Kind: "status_prompt", Message: "Status prompt injected"},
		{Kind: "max_turns_reached", Message: "Maximum turn limit reached (32); producing final response"},
	} {
		tracker := NewTaskTracker()
		_, err := tracker.Apply(taskLifecycleEvent(v1.EventTaskStart, v1.TaskEvent{
			TaskID: "task-explorer", SessionID: "task-explorer", Kind: "agent", Agent: "explorer", Name: "plan-sandbox-flow",
		}), false)
		if err != nil {
			t.Fatal(err)
		}
		data, _ := json.Marshal(status)
		reports, err := tracker.Apply(v1.Event{Type: v1.EventSessionStatus, TaskID: "task-explorer", Data: data}, false)
		want := "  ↻ [explorer:plan-sandbox-flow] " + status.Message
		if err != nil || len(reports) != 1 || reports[0].Line != want || !reports[0].Terminal || !reports[0].EmitPlain {
			t.Fatalf("status report = %#v, %v; want %q terminal plain report", reports, err, want)
		}
	}
}

func TestTaskToolOutputDeltaLeadsWithActivityIcon(t *testing.T) {
	tracker := NewTaskTracker()
	_, err := tracker.Apply(taskLifecycleEvent(v1.EventTaskStart, v1.TaskEvent{
		TaskID: "task-explorer", SessionID: "task-explorer", Kind: "agent", Agent: "explorer", Name: "ui-hierarchy",
	}), false)
	if err != nil {
		t.Fatal(err)
	}
	_, err = tracker.Apply(v1.Event{
		Type: "session.tool.running", TaskID: "task-explorer",
		Data: json.RawMessage(`{"call_id":"call","name":"exec_command","input":{"name":"project-tests","cmd":"go test ./..."}}`),
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(v1.ToolOutputDelta{ToolCallID: "call", Delta: "ok"})
	reports, err := tracker.Apply(v1.Event{Type: v1.EventToolOutputDelta, TaskID: "task-explorer", Data: data}, false)
	if err != nil || len(reports) != 1 || reports[0].Line != "  ◐ [explorer:ui-hierarchy] Running exec_command · project-tests · go test ./..." {
		t.Fatalf("tool output report = %#v, %v", reports, err)
	}
}

func TestTaskActivityLabelsUseTargetID(t *testing.T) {
	for _, test := range []struct {
		name  string
		input map[string]any
		want  string
	}{
		{name: "agent_send", input: map[string]any{"session_id": "session_agent", "message": "continue"}, want: "agent_send · session_agent · continue"},
		{name: "wait_agent", input: map[string]any{"session_id": "task_agent"}, want: "wait_agent · task_agent"},
		{name: "task_interrupt", input: map[string]any{"task_id": "task_agent"}, want: "task_interrupt · task_agent"},
		{name: "task_list_active", input: map[string]any{}, want: "task_list_active"},
		{name: "write_stdin", input: map[string]any{"task_id": "task_shell", "chars": "input"}, want: "write_stdin · task_shell · input"},
		{name: "exec_command", input: map[string]any{"name": "project-tests", "cmd": "go test ./..."}, want: "exec_command · project-tests · go test ./..."},
		{name: "exec_command", input: map[string]any{"cmd": "go test ./..."}, want: "exec_command · go test ./..."},
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
		{TaskID: "task-z", SessionID: "task-z", ParentSessionID: "session-main", Kind: "agent", Agent: "review", Name: "review-z"},
		{TaskID: "task-child", SessionID: "task-child", ParentSessionID: "task-z", Kind: "agent", Agent: "explore"},
		{TaskID: "task-a", SessionID: "task-a", ParentSessionID: "session-main", Kind: "agent", Agent: "worker"},
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
		{TaskID: "task-a", SessionID: "task-a", ParentSessionID: "session-main", Kind: "agent", Agent: "worker", Status: "idle"},
		{TaskID: "task-z", SessionID: "task-z", ParentSessionID: "session-main", Kind: "agent", Agent: "review", Name: "review-z", Status: "working"},
		{TaskID: "task-child", SessionID: "task-child", ParentSessionID: "task-z", Kind: "agent", Agent: "explore", Status: "working"},
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
	if report := reports[0]; report.TaskID != "task-child" || report.SessionID != "task-child" || report.ParentSessionID != "task-z" || report.MainStatus {
		t.Fatalf("content report metadata = %#v", report)
	}

	data, _ := json.Marshal(v1.TaskProgress{TaskID: "task-child", Agent: "explore", Status: "running"})
	reports, err = tracker.Apply(v1.Event{Type: v1.EventTaskProgress, TaskID: "task-child", Data: data}, false)
	if err != nil || len(reports) != 1 {
		t.Fatalf("progress reports = %#v, %v", reports, err)
	}
	if report := reports[0]; report.TaskID != "task-child" || report.SessionID != "task-child" || report.ParentSessionID != "task-z" || !report.MainStatus {
		t.Fatalf("progress report metadata = %#v", report)
	}
}

func TestTaskTrackerShellLifecycleUsesStableLiveReport(t *testing.T) {
	tests := []struct {
		name, shellName, wantStart string
		finish                     v1.TaskEvent
		wantLine                   string
		wantStyle                  terminal.TextStyle
	}{
		{name: "success", shellName: "tests", wantStart: "  ⠋ [shell:tests] running", finish: v1.TaskEvent{TaskID: "proc", Status: "succeeded"}, wantLine: "  ✓ [shell:tests] completed", wantStyle: terminal.TextStyleMuted},
		{name: "failure", shellName: "tests", wantStart: "  ⠋ [shell:tests] running", finish: v1.TaskEvent{TaskID: "proc", Status: "failed", Error: "exit status 2\nmore"}, wantLine: "  ✗ [shell:tests] failed: exit status 2 more", wantStyle: terminal.TextStyleDefault},
		{name: "unnamed", wantStart: "  ⠋ [shell] running", finish: v1.TaskEvent{TaskID: "proc", Status: "succeeded"}, wantLine: "  ✓ [shell] completed", wantStyle: terminal.TextStyleMuted},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tracker := NewTaskTracker()
			startMainTask(tracker)
			reports, err := tracker.Apply(taskLifecycleEvent(v1.EventTaskStart, v1.TaskEvent{TaskID: "proc", SessionID: "session-main", Kind: "shell", Name: test.shellName}), false)
			if err != nil || len(reports) != 1 {
				t.Fatalf("start reports = %#v, %v", reports, err)
			}
			start := reports[0]
			if start.ID != "proc:lifecycle" || start.Line != test.wantStart || start.Terminal || start.EmitPlain || start.Style != terminal.TextStyleMuted {
				t.Fatalf("start report = %#v", start)
			}

			reports, err = tracker.Apply(taskLifecycleEvent(v1.EventTaskFinished, test.finish), false)
			if err != nil || len(reports) != 1 {
				t.Fatalf("finish reports = %#v, %v", reports, err)
			}
			finish := reports[0]
			if finish.ID != start.ID || finish.Line != test.wantLine || !finish.Terminal || !finish.EmitPlain || finish.Style != test.wantStyle {
				t.Fatalf("finish report = %#v", finish)
			}
			duplicate, err := tracker.Apply(taskLifecycleEvent(v1.EventTaskFinished, test.finish), false)
			if err != nil || len(duplicate) != 0 {
				t.Fatalf("duplicate finish reports = %#v, %v", duplicate, err)
			}
		})
	}
}

func TestTaskTrackerShellDoesNotOwnAgentSessionAncestry(t *testing.T) {
	tracker := NewTaskTracker()
	startMainTask(tracker)
	for _, event := range []v1.TaskEvent{
		{TaskID: "agent", SessionID: "agent-session", ParentSessionID: "session-main", Kind: "agent", Agent: "worker"},
		{TaskID: "proc-a", SessionID: "agent-session", Kind: "shell", Name: "one"},
		{TaskID: "proc-b", SessionID: "agent-session", Kind: "shell", Name: "two"},
		{TaskID: "child", SessionID: "child-session", ParentSessionID: "agent-session", Kind: "agent", Agent: "explore"},
	} {
		if _, err := tracker.Apply(taskLifecycleEvent(v1.EventTaskStart, event), false); err != nil {
			t.Fatal(err)
		}
	}

	reports, err := tracker.Apply(taskDelta("child", v1.MessagePartDelta{MessageID: "m", Kind: "text", Delta: "work"}), false)
	if err != nil || len(reports) != 1 {
		t.Fatalf("child reports = %#v, %v", reports, err)
	}
	if got := reports[0].Line; got != "    ○ [explore] response: work" {
		t.Fatalf("child line = %q", got)
	}
}

func TestTaskTrackerProgressFollowsTaskGenerations(t *testing.T) {
	tracker := NewTaskTracker()
	startMainTask(tracker)
	apply := func(event v1.Event) []TaskReport {
		reports, err := tracker.Apply(event, false)
		if err != nil {
			t.Fatal(err)
		}
		return reports
	}
	progress := func(status string, tokens int) v1.Event {
		data, _ := json.Marshal(v1.TaskProgress{TaskID: "task", Agent: "explore", Status: status, Usage: v1.Usage{TotalTokens: tokens}})
		return v1.Event{Type: v1.EventTaskProgress, TaskID: "task", Data: data}
	}

	apply(taskLifecycleEvent(v1.EventTaskStart, v1.TaskEvent{TaskID: "task", SessionID: "task", ParentSessionID: "session-main", Kind: "agent", Agent: "explore"}))
	reports := apply(progress("running", 10))
	if len(reports) != 1 || reports[0].ID != "task:task:task" || reports[0].Terminal {
		t.Fatalf("running generation = %#v", reports)
	}

	// task.finished records lifecycle state but leaves the generation open for
	// the final progress event, which carries the authoritative counters.
	reports = apply(taskLifecycleEvent(v1.EventTaskFinished, v1.TaskEvent{TaskID: "task", Status: "succeeded"}))
	if len(reports) != 1 || reports[0].Terminal || !strings.Contains(reports[0].Line, "10 tokens") {
		t.Fatalf("finished before final progress = %#v", reports)
	}
	if reports := apply(progress("running", 15)); len(reports) != 0 {
		t.Fatalf("late running progress before final = %#v", reports)
	}
	reports = apply(progress("succeeded", 20))
	if len(reports) != 1 || !reports[0].Terminal || !reports[0].EmitPlain || !strings.Contains(reports[0].Line, "20 tokens") {
		t.Fatalf("final progress = %#v", reports)
	}
	if reports := apply(progress("running", 30)); len(reports) != 0 {
		t.Fatalf("late running progress = %#v", reports)
	}

	apply(taskLifecycleEvent(v1.EventTaskWorking, v1.TaskEvent{TaskID: "task"}))
	reports = apply(progress("running", 40))
	if len(reports) != 1 || reports[0].Terminal || !strings.Contains(reports[0].Line, "40 tokens") {
		t.Fatalf("follow-up running generation = %#v", reports)
	}
	reports = apply(progress("succeeded", 50))
	if len(reports) != 1 || !reports[0].Terminal || !strings.Contains(reports[0].Line, "50 tokens") {
		t.Fatalf("follow-up terminal generation = %#v", reports)
	}
	if reports := apply(progress("running", 60)); len(reports) != 0 {
		t.Fatalf("late follow-up progress = %#v", reports)
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
		data, _ := json.Marshal(v1.TaskProgress{TaskID: taskID, Agent: "explore", Status: status, Usage: v1.Usage{TotalTokens: tokens}, ToolUses: tools})
		return v1.Event{Type: v1.EventTaskProgress, TaskID: taskID, Data: data}
	}

	apply(taskLifecycleEvent(v1.EventTaskStart, v1.TaskEvent{TaskID: "parent", SessionID: "parent", ParentSessionID: "session-main", Kind: "agent", Agent: "explore", Name: "parent"}))
	apply(progress("parent", "running", 100, 2))
	apply(taskLifecycleEvent(v1.EventTaskStart, v1.TaskEvent{TaskID: "child", SessionID: "child", ParentSessionID: "parent", Kind: "agent", Agent: "explore", Name: "child"}))
	apply(taskLifecycleEvent(v1.EventTaskStart, v1.TaskEvent{TaskID: "grandchild", SessionID: "grandchild", ParentSessionID: "child", Kind: "agent", Agent: "explore", Name: "grandchild"}))

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
	reports, err := tracker.Apply(taskLifecycleEvent(v1.EventTaskStart, v1.TaskEvent{TaskID: "task-orphan", SessionID: "task-orphan", ParentSessionID: "session-missing", Kind: "agent", Agent: "explore"}), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 1 || reports[0].Line != "✗ unknown task session-missing (parent session of task-orphan)" {
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
		{TaskID: "task-parent", SessionID: "task-parent", ParentSessionID: "session-main", Kind: "agent", Agent: "build"},
		{TaskID: "task-child", SessionID: "task-child", ParentSessionID: "task-parent", Kind: "agent", Agent: "review"},
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
