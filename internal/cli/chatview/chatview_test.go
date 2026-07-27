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
	pending := tracker.DescribeReport(v1.Event{Type: v1.EventSessionToolPending, Data: json.RawMessage(`{"call_id":"call","tool_name":"spawn","input":{"name":"review-task","agent":"review","model":"","prompt":"first line\nsecond line"}}`)})
	if !pending.Hidden || pending.Terminal {
		t.Fatalf("pending report = %#v", pending)
	}
	success := tracker.DescribeReport(v1.Event{Type: v1.EventSessionToolSuccess, Data: json.RawMessage(`{"call_id":"call","tool_name":"spawn"}`)})
	if success.Hidden || !success.Terminal || success.Block != want {
		t.Fatalf("success report = %#v", success)
	}
}

func TestAnswerPresentationUpdatesCompletedLabelInQuestionOrder(t *testing.T) {
	presentations := NewPresentations(v1.ToolList{Items: []v1.Tool{{
		ID: "question", Presentation: v1.ToolPresentation{
			Label:          v1.ToolLabel{Fields: []v1.ToolLabelPart{{Names: []string{"questions"}, Array: true, Item: []string{"prompt"}, Overflow: true}}},
			CompletedLabel: toolCompletedLabelAnswers,
		},
	}}})
	tracker := StreamToolTracker{Presentation: presentations}
	pending := tracker.DescribeReport(v1.Event{Type: v1.EventSessionToolPending, Data: json.RawMessage(`{"call_id":"call","tool_name":"question","input":{"questions":[{"id":"commit","prompt":"Commit changed usage text?","options":[{"id":"yes","label":"Yes"}]},{"id":"note","prompt":"Note?","custom":true}]}}`)})
	success := tracker.DescribeReport(v1.Event{Type: v1.EventSessionToolSuccess, Data: json.RawMessage(`{"call_id":"call","result":"{\"answers\":[{\"question_id\":\"note\",\"custom\":\"Ship it\"},{\"question_id\":\"commit\",\"option_ids\":[\"yes\"]}]}"}`)})
	if pending.Label != "question · Commit changed usage text? · +1 more" || success.Label != "question · Commit changed usage text? · +1 more · Yes; Ship it" {
		t.Fatalf("answer labels: pending=%q success=%q", pending.Label, success.Label)
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
	pending := tracker.DescribeReport(v1.Event{Type: v1.EventSessionToolPending, Data: json.RawMessage(`{"call_id":"call","tool_name":"search","input":{"pattern":"needle"}}`)})
	success := tracker.DescribeReport(v1.Event{Type: v1.EventSessionToolSuccess, Data: json.RawMessage(`{"call_id":"call","result":"one\ntwo\n"}`)})
	if pending.Label != `search · "needle"` || success.Label != `search · "needle" · 2 matches` {
		t.Fatalf("result count labels: pending=%q success=%q", pending.Label, success.Label)
	}

	tracker.DescribeReport(v1.Event{Type: v1.EventSessionToolPending, Data: json.RawMessage(`{"call_id":"empty","tool_name":"search","input":{"pattern":"absent"}}`)})
	empty := tracker.DescribeReport(v1.Event{Type: v1.EventSessionToolSuccess, Data: json.RawMessage(`{"call_id":"empty","result":""}`)})
	if empty.Label != `search · "absent" · 0 matches` {
		t.Fatalf("empty result label = %q", empty.Label)
	}
}

func TestDiffPresentationPreservesRawResultThroughRuntimeActivityReports(t *testing.T) {
	tracker := NewRuntimeActivityTracker("session-main")
	startRootSession(tracker)
	if _, err := tracker.Apply(agentSessionEvent(v1.EventAgentSessionStart, v1.AgentSessionEvent{
		SessionID: "child", ParentSessionID: "session-main", Agent: "worker",
	}), false); err != nil {
		t.Fatal(err)
	}
	pending := v1.Event{Type: v1.EventSessionToolPending, SessionID: "child", Data: json.RawMessage(`{"call_id":"call","tool_name":"apply_patch"}`)}
	if _, err := tracker.Apply(pending, false); err != nil {
		t.Fatal(err)
	}
	diff := "--- a/file.go\n+++ b/file.go\n@@ -1 +1 @@\n-old\n+new\n"
	data, _ := json.Marshal(map[string]string{"call_id": "call", "result": diff})
	reports, err := tracker.Apply(v1.Event{Type: v1.EventSessionToolSuccess, SessionID: "child", Data: data}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 1 || reports[0].BlockKind != ToolResultDiff || reports[0].Block != diff {
		t.Fatalf("diff runtime activity report = %#v", reports)
	}
}

func TestRuntimeActivityCodeDisplayReportCarriesRenderingMetadata(t *testing.T) {
	tracker := NewRuntimeActivityTracker("session-main")
	startRootSession(tracker)
	if _, err := tracker.Apply(agentSessionEvent(v1.EventAgentSessionStart, v1.AgentSessionEvent{
		SessionID: "child", ParentSessionID: "session-main", Agent: "worker",
	}), false); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(v1.CodeDisplay{ToolCallID: "call", Source: "package main\n", Path: "cmd/main.go", Language: "go", StartLine: 12})
	reports, err := tracker.Apply(v1.Event{Type: v1.EventCodeDisplay, SessionID: "child", Data: data}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 1 {
		t.Fatalf("code display reports = %#v", reports)
	}
	report := reports[0]
	if report.ID != "child\x00:code:call" || report.SessionID != "child" || report.ProcessID != "" || report.ParentSessionID != "session-main" || report.Line != "  • [worker] ↳ Code · cmd/main.go:12" || report.Block != "package main\n" || report.BlockKind != ToolResultCode || report.BlockLanguage != "go" || !report.Terminal || !report.EmitPlain {
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

	pending := tracker.DescribeReport(v1.Event{Type: v1.EventSessionToolPending, Data: json.RawMessage(`{"call_id":"call","tool_name":"wait"}`)})
	success := tracker.DescribeReport(v1.Event{Type: v1.EventSessionToolSuccess, Data: json.RawMessage(`{"call_id":"call"}`)})
	if !presentations.LiveOnly("wait") || !pending.LiveOnly || pending.Terminal || !success.LiveOnly || !success.Terminal {
		t.Fatalf("live-only reports: pending=%#v success=%#v", pending, success)
	}
}

func TestRuntimeActivityLiveOnlyToolRemovesActiveRowWithoutPlainOutput(t *testing.T) {
	tracker := NewRuntimeActivityTracker("session-main")
	tracker.Presentation = NewPresentations(v1.ToolList{Items: []v1.Tool{{
		ID: "wait", Presentation: v1.ToolPresentation{LiveOnly: true},
	}}})
	startRootSession(tracker)
	if _, err := tracker.Apply(agentSessionEvent(v1.EventAgentSessionStart, v1.AgentSessionEvent{
		SessionID: "child", ParentSessionID: "session-main", Agent: "worker",
	}), false); err != nil {
		t.Fatal(err)
	}

	pending, err := tracker.Apply(v1.Event{Type: v1.EventSessionToolPending, SessionID: "child", Data: json.RawMessage(`{"call_id":"call","tool_name":"wait"}`)}, false)
	if err != nil {
		t.Fatal(err)
	}
	terminal, err := tracker.Apply(v1.Event{Type: v1.EventSessionToolFailure, SessionID: "child", Data: json.RawMessage(`{"call_id":"call","error":"stopped"}`)}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].Skip || pending[0].Terminal || len(terminal) != 1 || !terminal[0].Skip || !terminal[0].Terminal || terminal[0].EmitPlain {
		t.Fatalf("runtime activity live-only reports: pending=%#v terminal=%#v", pending, terminal)
	}
}

func TestFailureErrorBlockPresentation(t *testing.T) {
	presentations := NewPresentations(v1.ToolList{Items: []v1.Tool{{
		ID: "custom_editor", Presentation: v1.ToolPresentation{Failure: ToolFailureErrorBlock},
	}}})
	tracker := StreamToolTracker{Presentation: presentations}
	tracker.DescribeReport(v1.Event{Type: v1.EventSessionToolPending, Data: json.RawMessage(`{"call_id":"call","tool_name":"custom_editor","input":{"patch":"original request"}}`)})
	errorText := "patch planning failed with 2 errors:\n\n[1/2] first conflict\n<<<<<<< SEARCH\nfirst\n=======\n\n[2/2] second conflict\n<<<<<<< SEARCH\nsecond\n======="
	data, err := json.Marshal(map[string]any{"call_id": "call", "tool_name": "custom_editor", "error": errorText})
	if err != nil {
		t.Fatal(err)
	}
	report := tracker.DescribeReport(v1.Event{Type: v1.EventSessionToolFailure, Data: data})
	wantBlock := "✗ [1/2] first conflict\n<<<<<<< SEARCH\nfirst\n=======\n\n✗ [2/2] second conflict\n<<<<<<< SEARCH\nsecond\n======="
	if report.Line != "✗ tool: 2 errors" || report.Block != wantBlock || strings.Contains(report.Block, "original request") {
		t.Fatalf("failure report = %#v, want concise status and separately headed error sections", report)
	}
}

func TestPresentationResolvesFriendlyActivityNames(t *testing.T) {
	presentations := NewPresentations(v1.ToolList{Items: []v1.Tool{{
		ID: "control", Presentation: v1.ToolPresentation{Label: v1.ToolLabel{Fields: []v1.ToolLabelPart{{Names: []string{"session_id"}, TaskName: true}}}},
	}, {
		ID: "spawn", Presentation: v1.ToolPresentation{Label: v1.ToolLabel{Fields: []v1.ToolLabelPart{{Names: []string{"name", "agent"}}}}},
	}}})
	presentations.activityNames["session-known"] = "review-happy-otter"

	if got := presentations.Label("control", map[string]any{"session_id": "session-known"}); got != "control · review-happy-otter" {
		t.Fatalf("known session label = %q", got)
	}
	if got := presentations.Label("control", map[string]any{"session_id": "session-unknown"}); got != "control · session-unknown" {
		t.Fatalf("unknown session label = %q", got)
	}
	input := presentations.EnrichLabelInput("spawn", map[string]any{"agent": "review"}, `{"name":"review-kind-ibex"}`)
	if got := presentations.Label("spawn", input); got != "spawn · review-kind-ibex" {
		t.Fatalf("result-enriched spawn label = %q", got)
	}

	tracker := NewRuntimeActivityTracker("session-main")
	tracker.Presentation = presentations
	startRootSession(tracker)
	if _, err := tracker.Apply(agentSessionEvent(v1.EventAgentSessionStart, v1.AgentSessionEvent{SessionID: "task_named", ParentSessionID: "session-main", Agent: "review", Name: "review-kind-ibex"}), false); err != nil {
		t.Fatal(err)
	}
	reports, err := tracker.Apply(sessionDelta("task_named", v1.MessagePartDelta{MessageID: "m", Kind: "text", Delta: "working"}), false)
	if err != nil || len(reports) != 1 || !strings.Contains(reports[0].Line, "○ [review:review-kind-ibex] response: working") {
		t.Fatalf("friendly runtime activity report = %#v, %v", reports, err)
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

func TestRuntimeActivityStatusLeadsWithActivityIcon(t *testing.T) {
	for _, status := range []v1.SessionStatus{
		{Kind: "status_prompt", Message: "Status prompt injected"},
		{Kind: "max_turns_reached", Message: "Maximum turn limit reached (32); producing final response"},
	} {
		tracker := NewRuntimeActivityTracker("session-main")
		_, err := tracker.Apply(agentSessionEvent(v1.EventAgentSessionStart, v1.AgentSessionEvent{
			SessionID: "task-explorer", Agent: "explorer", Name: "plan-sandbox-flow",
		}), false)
		if err != nil {
			t.Fatal(err)
		}
		data, _ := json.Marshal(status)
		reports, err := tracker.Apply(v1.Event{Type: v1.EventSessionStatus, SessionID: "task-explorer", Data: data}, false)
		want := "  ↻ [explorer:plan-sandbox-flow] " + status.Message
		if err != nil || len(reports) != 1 || reports[0].Line != want || !reports[0].Terminal || !reports[0].EmitPlain {
			t.Fatalf("status report = %#v, %v; want %q terminal plain report", reports, err, want)
		}
	}
}

func TestRuntimeActivityToolOutputDeltaLeadsWithActivityIcon(t *testing.T) {
	tracker := NewRuntimeActivityTracker("session-main")
	_, err := tracker.Apply(agentSessionEvent(v1.EventAgentSessionStart, v1.AgentSessionEvent{
		SessionID: "task-explorer", Agent: "explorer", Name: "ui-hierarchy",
	}), false)
	if err != nil {
		t.Fatal(err)
	}
	_, err = tracker.Apply(v1.Event{
		Type: v1.EventSessionToolRunning, SessionID: "task-explorer",
		Data: json.RawMessage(`{"call_id":"call","tool_name":"exec_command","input":{"name":"project-tests","cmd":"go test ./..."}}`),
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(v1.ToolOutputDelta{ToolCallID: "call", Delta: "ok"})
	reports, err := tracker.Apply(v1.Event{Type: v1.EventToolOutputDelta, SessionID: "task-explorer", Data: data}, false)
	if err != nil || len(reports) != 1 || reports[0].Line != "  ◐ [explorer:ui-hierarchy] Running exec_command · project-tests · go test ./..." {
		t.Fatalf("tool output report = %#v, %v", reports, err)
	}
}

func TestRuntimeActivityLabelsUseTargetID(t *testing.T) {
	for _, test := range []struct {
		name  string
		input map[string]any
		want  string
	}{
		{name: "agent_send", input: map[string]any{"session_id": "session_agent", "message": "continue"}, want: "agent_send · session_agent · continue"},
		{name: "wait_agent", input: map[string]any{"session_id": "session-agent"}, want: "wait_agent · session-agent"},
		{name: "task_interrupt", input: map[string]any{"session_id": "session-agent"}, want: "task_interrupt · session-agent"},
		{name: "task_list_active", input: map[string]any{}, want: "task_list_active"},
		{name: "write_stdin", input: map[string]any{"process_id": "process-shell", "chars": "input"}, want: "write_stdin · process-shell · input"},
		{name: "exec_command", input: map[string]any{"name": "project-tests", "cmd": "go test ./..."}, want: "exec_command · project-tests · go test ./..."},
		{name: "exec_command", input: map[string]any{"cmd": "go test ./..."}, want: "exec_command · go test ./..."},
	} {
		if got := ToolActivityLabel(test.name, test.input); got != test.want {
			t.Errorf("ToolActivityLabel(%q) = %q, want %q", test.name, got, test.want)
		}
	}
}

func userSessionEvent(eventType string, event v1.UserSessionEvent) v1.Event {
	data, _ := json.Marshal(event)
	return v1.Event{Type: eventType, SessionID: event.SessionID, Data: data}
}

func agentSessionEvent(eventType string, event v1.AgentSessionEvent) v1.Event {
	data, _ := json.Marshal(event)
	return v1.Event{Type: eventType, SessionID: event.SessionID, Data: data}
}

func processEvent(eventType string, event v1.ProcessEvent) v1.Event {
	data, _ := json.Marshal(event)
	return v1.Event{Type: eventType, SessionID: event.SessionID, Data: data}
}

func startRootSession(tracker *RuntimeActivityTracker) {
	_, _ = tracker.Apply(userSessionEvent(v1.EventUserSessionStart, v1.UserSessionEvent{SessionID: "session-main"}), false)
}

func sessionDelta(sessionID string, delta v1.MessagePartDelta) v1.Event {
	data, _ := json.Marshal(delta)
	return v1.Event{Type: v1.EventMessagePartDelta, SessionID: sessionID, Data: data}
}

func TestRuntimeActivityTrackerCompactionLifecycle(t *testing.T) {
	for _, test := range []struct {
		name, sessionID, attemptID, status, eventError, wantStart, wantFinish string
		wantStyle                                                             terminal.TextStyle
	}{
		{
			name: "root completed", sessionID: "session-main", attemptID: "attempt-root", status: "completed",
			wantStart: "⠋ Compaction", wantFinish: "✓ Compaction: complete", wantStyle: terminal.TextStyleMuted,
		},
		{
			name: "descendant failed", sessionID: "task-child", attemptID: "attempt-failed", status: "failed", eventError: "provider failed\nretry exhausted",
			wantStart: "  ⠋ [worker:background] Compaction", wantFinish: "  ✗ [worker:background] Compaction: failed · provider failed retry exhausted", wantStyle: terminal.TextStyleDefault,
		},
		{
			name: "descendant interrupted", sessionID: "task-child", attemptID: "attempt-interrupted", status: "interrupted", eventError: "runtime stopped",
			wantStart: "  ⠋ [worker:background] Compaction", wantFinish: "  ■ [worker:background] Compaction: interrupted · runtime stopped", wantStyle: terminal.TextStyleDefault,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			tracker := NewRuntimeActivityTracker("session-main")
			startRootSession(tracker)
			if _, err := tracker.Apply(agentSessionEvent(v1.EventAgentSessionStart, v1.AgentSessionEvent{
				SessionID: "task-child", ParentSessionID: "session-main", Agent: "worker", Name: "background",
			}), false); err != nil {
				t.Fatal(err)
			}
			event := func(eventType, status, eventError string) v1.Event {
				data, _ := json.Marshal(v1.CompactionEvent{AttemptID: test.attemptID, Status: status, Error: eventError})
				return v1.Event{Type: eventType, SessionID: test.sessionID, Data: data}
			}

			started, err := tracker.Apply(event(v1.EventSessionCompactionStarted, "started", ""), false)
			if err != nil || len(started) != 1 {
				t.Fatalf("started reports = %#v, %v", started, err)
			}
			wantID := test.sessionID + "\x00:compaction:" + test.attemptID
			if report := started[0]; report.ID != wantID || report.Line != test.wantStart || report.SessionID != test.sessionID || report.Terminal || report.EmitPlain || !report.MainStatus || report.Style != terminal.TextStyleMuted {
				t.Fatalf("started report = %#v", report)
			}

			finished, err := tracker.Apply(event(v1.EventSessionCompactionFinished, test.status, test.eventError), false)
			if err != nil || len(finished) != 1 {
				t.Fatalf("finished reports = %#v, %v", finished, err)
			}
			if report := finished[0]; report.ID != wantID || report.ID != started[0].ID || report.Line != test.wantFinish || report.SessionID != test.sessionID || !report.Terminal || !report.EmitPlain || !report.MainStatus || report.Style != test.wantStyle {
				t.Fatalf("finished report = %#v", report)
			}
		})
	}

	tracker := NewRuntimeActivityTracker("session-main")
	startRootSession(tracker)
	for _, test := range []struct {
		name  string
		event v1.Event
		want  string
	}{
		{name: "missing attempt id", event: v1.Event{Type: v1.EventSessionCompactionStarted, SessionID: "session-main", Data: json.RawMessage(`{"status":"started"}`)}},
		{name: "unknown origin", event: v1.Event{Type: v1.EventSessionCompactionFinished, SessionID: "unknown", Data: json.RawMessage(`{"attempt_id":"attempt-unknown","status":"failed"}`)}, want: "✗ unknown event origin unknown (session.compaction.finished)"},
	} {
		t.Run(test.name, func(t *testing.T) {
			reports, err := tracker.Apply(test.event, false)
			wantCount := 0
			if test.want != "" {
				wantCount = 1
			}
			if err != nil || len(reports) != wantCount {
				t.Fatalf("reports = %#v, %v", reports, err)
			}
			if test.want != "" && (!reports[0].Terminal || reports[0].Line != test.want) {
				t.Fatalf("report = %#v", reports[0])
			}
		})
	}
	if reports, err := tracker.Apply(v1.Event{Type: v1.EventSessionCompactionStarted, SessionID: "session-main", Data: json.RawMessage(`{"attempt_id":`)}, false); err == nil || reports != nil {
		t.Fatalf("malformed event reports = %#v, %v", reports, err)
	}
}

func TestRuntimeActivityTrackerProjectsTreeAndReportOwners(t *testing.T) {
	tracker := NewRuntimeActivityTracker("session-main")
	startRootSession(tracker)
	for _, start := range []v1.AgentSessionEvent{
		{SessionID: "task-z", ParentSessionID: "session-main", Agent: "review", Name: "review-z"},
		{SessionID: "task-child", ParentSessionID: "task-z", Agent: "explore"},
		{SessionID: "task-a", ParentSessionID: "session-main", Agent: "worker"},
	} {
		if _, err := tracker.Apply(agentSessionEvent(v1.EventAgentSessionStart, start), false); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tracker.Apply(agentSessionEvent(v1.EventAgentSessionIdle, v1.AgentSessionEvent{SessionID: "task-a"}), false); err != nil {
		t.Fatal(err)
	}

	wantActivities := []RuntimeActivityInfo{
		{SessionID: "session-main", Kind: "main", Status: "working"},
		{SessionID: "task-a", ParentSessionID: "session-main", Kind: "agent", Agent: "worker", Status: "idle"},
		{SessionID: "task-z", ParentSessionID: "session-main", Kind: "agent", Agent: "review", Name: "review-z", Status: "working"},
		{SessionID: "task-child", ParentSessionID: "task-z", Kind: "agent", Agent: "explore", Status: "working"},
	}
	if got := tracker.Activities(); !reflect.DeepEqual(got, wantActivities) {
		t.Fatalf("Activities() = %#v, want %#v", got, wantActivities)
	}
	// The returned slice is a snapshot, not mutable tracker state.
	activities := tracker.Activities()
	activities[1].Status = "changed"
	if got := tracker.Activities()[1].Status; got != "idle" {
		t.Fatalf("mutating Activities result changed tracker status to %q", got)
	}

	reports, err := tracker.Apply(sessionDelta("task-child", v1.MessagePartDelta{MessageID: "m", Kind: "text", Delta: "work"}), false)
	if err != nil || len(reports) != 1 {
		t.Fatalf("content reports = %#v, %v", reports, err)
	}
	if report := reports[0]; report.SessionID != "task-child" || report.ProcessID != "" || report.ParentSessionID != "task-z" || report.MainStatus {
		t.Fatalf("content report metadata = %#v", report)
	}

	data, _ := json.Marshal(v1.AgentSessionProgress{SessionID: "task-child", Agent: "explore", Status: "running"})
	reports, err = tracker.Apply(v1.Event{Type: v1.EventAgentSessionProgress, SessionID: "task-child", Data: data}, false)
	if err != nil || len(reports) != 1 {
		t.Fatalf("progress reports = %#v, %v", reports, err)
	}
	if report := reports[0]; report.SessionID != "task-child" || report.ProcessID != "" || report.ParentSessionID != "task-z" || !report.MainStatus {
		t.Fatalf("progress report metadata = %#v", report)
	}
}

func TestRuntimeActivityTrackerFoldsNestedModelineToolsIntoOwnerStatus(t *testing.T) {
	for _, test := range []struct {
		name, tool         string
		modeline, liveOnly bool
	}{
		{name: "wait agent", tool: "wait_agent", modeline: true, liveOnly: true},
		{name: "wait process", tool: "wait_process", modeline: true, liveOnly: true},
		{name: "modeline terminal result", tool: "background-work", modeline: true},
		{name: "ordinary tool", tool: "ordinary", liveOnly: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			tracker := NewRuntimeActivityTracker("session-main")
			tracker.Presentation = NewPresentations(v1.ToolList{Items: []v1.Tool{{
				ID: test.tool, Presentation: v1.ToolPresentation{Modeline: test.modeline, LiveOnly: test.liveOnly, Result: ToolResultText},
			}}})
			startRootSession(tracker)
			if _, err := tracker.Apply(agentSessionEvent(v1.EventAgentSessionStart, v1.AgentSessionEvent{
				SessionID: "child", ParentSessionID: "session-main", Agent: "worker",
			}), false); err != nil {
				t.Fatal(err)
			}
			progress, _ := json.Marshal(v1.AgentSessionProgress{SessionID: "child", Agent: "worker", Status: "running", Usage: v1.Usage{TotalTokens: 10}})
			if reports, err := tracker.Apply(v1.Event{Type: v1.EventAgentSessionProgress, SessionID: "child", Data: progress}, false); err != nil || len(reports) != 1 {
				t.Fatalf("progress reports = %#v, %v", reports, err)
			}

			var report RuntimeActivityReport
			for _, eventType := range []string{v1.EventSessionToolPending, v1.EventSessionToolRunning} {
				reports, err := tracker.Apply(v1.Event{Type: eventType, SessionID: "child", Data: json.RawMessage(`{"call_id":"call","tool_name":"` + test.tool + `"}`)}, false)
				if err != nil || len(reports) != 1 {
					t.Fatalf("%s reports = %#v, %v", eventType, reports, err)
				}
				report = reports[0]
				if test.modeline {
					if report.ID != "child\x00:status" || !report.MainStatus || report.Terminal || strings.Contains(report.ID, ":tool:") || !strings.Contains(report.Line, "] Working: "+test.tool) || strings.Contains(report.Line, "agent: worker") {
						t.Fatalf("folded %s report = %#v", eventType, report)
					}
				} else if !strings.Contains(report.ID, ":tool:") || report.MainStatus {
					t.Fatalf("ordinary %s report = %#v", eventType, report)
				}
			}

			if test.modeline && test.liveOnly {
				if reports, err := tracker.Apply(agentSessionEvent(v1.EventAgentSessionWorking, v1.AgentSessionEvent{SessionID: "child"}), false); err != nil || len(reports) != 0 {
					t.Fatalf("follow-up working reports = %#v, %v", reports, err)
				}
				if reports, err := tracker.Apply(v1.Event{Type: v1.EventAgentSessionProgress, SessionID: "child", Data: progress}, false); err != nil || len(reports) != 1 || strings.Contains(reports[0].Line, "Working:") {
					t.Fatalf("follow-up progress reports = %#v, %v", reports, err)
				}
				reports, err := tracker.Apply(v1.Event{Type: v1.EventSessionToolSuccess, SessionID: "child", Data: json.RawMessage(`{"call_id":"call"}`)}, false)
				if err != nil || len(reports) != 1 || strings.Contains(reports[0].Line, "Working:") || reports[0].Terminal || reports[0].EmitPlain {
					t.Fatalf("delayed terminal reports = %#v, %v", reports, err)
				}
				return
			}

			reports, err := tracker.Apply(v1.Event{Type: v1.EventSessionToolSuccess, SessionID: "child", Data: json.RawMessage(`{"call_id":"call","result":"finished output"}`)}, false)
			if err != nil {
				t.Fatal(err)
			}
			if test.modeline && !test.liveOnly {
				if len(reports) != 2 || reports[0].ID != "child\x00:status" || strings.Contains(reports[0].Line, "Working:") || !strings.Contains(reports[0].Line, "agent: worker") || reports[0].Terminal || reports[0].EmitPlain || !strings.Contains(reports[1].ID, ":tool:") || !reports[1].Terminal || !reports[1].EmitPlain || !strings.Contains(reports[1].Block, "finished output") {
					t.Fatalf("modeline terminal reports = %#v", reports)
				}
				return
			}
			if len(reports) != 1 {
				t.Fatalf("success reports = %#v", reports)
			}
			report = reports[0]
			if test.modeline {
				if report.ID != "child\x00:status" || strings.Contains(report.Line, "Working:") || !strings.Contains(report.Line, "agent: worker") || report.Terminal || report.EmitPlain {
					t.Fatalf("cleared modeline report = %#v", report)
				}
			} else if !report.Skip || !report.Terminal || report.EmitPlain {
				t.Fatalf("settled live-only report = %#v", report)
			}
		})
	}
}

func TestRuntimeActivityTrackerAttachesPendingUsageToNestedSessions(t *testing.T) {
	tracker := NewRuntimeActivityTracker("session-main")
	startRootSession(tracker)

	tracker.AddUsage("child", "", v1.Usage{InputTokens: 10, OutputTokens: 2, CachedInputTokens: 4, InputCost: 0.125})
	tracker.AddUsage("child", "", v1.Usage{InputTokens: 5, OutputTokens: 1, CachedInputTokens: 1, InputCost: 0.125})
	tracker.AddUsage("grandchild", "", v1.Usage{InputTokens: 20, OutputTokens: 3, CachedInputTokens: 10, OutputCost: 0.25})
	if got := tracker.CumulativeUsage("session-main", ""); got != (RuntimeActivityUsage{}) {
		t.Fatalf("unregistered usage = %#v", got)
	}

	childStart := agentSessionEvent(v1.EventAgentSessionStart, v1.AgentSessionEvent{SessionID: "child", ParentSessionID: "session-main", Agent: "explore"})
	for _, event := range []v1.Event{
		childStart,
		childStart,
		agentSessionEvent(v1.EventAgentSessionStart, v1.AgentSessionEvent{SessionID: "grandchild", ParentSessionID: "child", Agent: "review"}),
	} {
		if _, err := tracker.Apply(event, false); err != nil {
			t.Fatal(err)
		}
	}

	want := RuntimeActivityUsage{InputTokens: 35, OutputTokens: 6, CachedTokens: 15, Cost: 0.5}
	if got := tracker.CumulativeUsage("session-main", ""); got != want {
		t.Fatalf("nested cumulative usage = %#v, want %#v", got, want)
	}
}

func TestRuntimeActivityTrackerCumulativeUsageHandlesCyclicAncestry(t *testing.T) {
	tracker := NewRuntimeActivityTracker("session-main")
	for _, start := range []v1.AgentSessionEvent{
		{SessionID: "session-a", ParentSessionID: "session-b"},
		{SessionID: "session-b", ParentSessionID: "session-a"},
	} {
		if _, err := tracker.Apply(agentSessionEvent(v1.EventAgentSessionStart, start), false); err != nil {
			t.Fatal(err)
		}
	}

	tracker.AddUsage("session-a", "", v1.Usage{InputTokens: 10, OutputTokens: 2, InputCost: 0.125})
	tracker.AddUsage("session-b", "", v1.Usage{InputTokens: 20, OutputTokens: 3, OutputCost: 0.25})

	want := RuntimeActivityUsage{InputTokens: 30, OutputTokens: 5, Cost: 0.375}
	if got := tracker.CumulativeUsage("session-a", ""); got != want {
		t.Fatalf("CumulativeUsage() = %#v, want %#v", got, want)
	}
}

func TestRuntimeActivityTrackerStatusHandlesCyclicAncestry(t *testing.T) {
	tracker := NewRuntimeActivityTracker("session-main")
	for _, start := range []v1.AgentSessionEvent{
		{SessionID: "session-a", ParentSessionID: "session-b"},
		{SessionID: "session-b", ParentSessionID: "session-a"},
	} {
		if _, err := tracker.Apply(agentSessionEvent(v1.EventAgentSessionStart, start), false); err != nil {
			t.Fatal(err)
		}
	}

	for _, sessionID := range []string{"session-a", "session-b"} {
		if _, err := tracker.Apply(agentSessionEvent(v1.EventAgentSessionIdle, v1.AgentSessionEvent{SessionID: sessionID}), false); err != nil {
			t.Fatal(err)
		}
	}
	if tracker.activityActive(tracker.known("session-a", "")) {
		t.Fatal("idle cyclic session ancestry reported as active")
	}
}

func TestRuntimeActivityTrackerShellLifecycleUsesStableLiveReport(t *testing.T) {
	tests := []struct {
		name, shellName, wantStart string
		finish                     v1.ProcessEvent
		wantLine                   string
		wantStyle                  terminal.TextStyle
	}{
		{name: "success", shellName: "tests", wantStart: "  ⠋ [shell:tests] running", finish: v1.ProcessEvent{SessionID: "session-main", ProcessID: "proc", Status: "succeeded"}, wantLine: "  ✓ [shell:tests] completed", wantStyle: terminal.TextStyleMuted},
		{name: "failure", shellName: "tests", wantStart: "  ⠋ [shell:tests] running", finish: v1.ProcessEvent{SessionID: "session-main", ProcessID: "proc", Status: "failed", Error: "exit status 2\nmore"}, wantLine: "  ✗ [shell:tests] failed: exit status 2 more", wantStyle: terminal.TextStyleDefault},
		{name: "unnamed", wantStart: "  ⠋ [shell] running", finish: v1.ProcessEvent{SessionID: "session-main", ProcessID: "proc", Status: "succeeded"}, wantLine: "  ✓ [shell] completed", wantStyle: terminal.TextStyleMuted},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tracker := NewRuntimeActivityTracker("session-main")
			startRootSession(tracker)
			reports, err := tracker.Apply(processEvent(v1.EventProcessStart, v1.ProcessEvent{SessionID: "session-main", ProcessID: "proc", Name: test.shellName}), false)
			if err != nil || len(reports) != 1 {
				t.Fatalf("start reports = %#v, %v", reports, err)
			}
			start := reports[0]
			if start.ID != "session-main\x00proc:lifecycle" || start.SessionID != "session-main" || start.ProcessID != "proc" || start.Line != test.wantStart || start.Terminal || start.EmitPlain || start.Style != terminal.TextStyleMuted {
				t.Fatalf("start report = %#v", start)
			}

			reports, err = tracker.Apply(processEvent(v1.EventProcessFinished, test.finish), false)
			if err != nil || len(reports) != 1 {
				t.Fatalf("finish reports = %#v, %v", reports, err)
			}
			finish := reports[0]
			if finish.ID != start.ID || finish.Line != test.wantLine || !finish.Terminal || !finish.EmitPlain || finish.Style != test.wantStyle {
				t.Fatalf("finish report = %#v", finish)
			}
			duplicate, err := tracker.Apply(processEvent(v1.EventProcessFinished, test.finish), false)
			if err != nil || len(duplicate) != 0 {
				t.Fatalf("duplicate finish reports = %#v, %v", duplicate, err)
			}
		})
	}
}

func TestRuntimeActivityTrackerShellDoesNotOwnAgentSessionAncestry(t *testing.T) {
	tracker := NewRuntimeActivityTracker("session-main")
	startRootSession(tracker)
	for _, event := range []v1.Event{
		agentSessionEvent(v1.EventAgentSessionStart, v1.AgentSessionEvent{SessionID: "agent-session", ParentSessionID: "session-main", Agent: "worker"}),
		processEvent(v1.EventProcessStart, v1.ProcessEvent{SessionID: "agent-session", ProcessID: "proc-a", Name: "one"}),
		processEvent(v1.EventProcessStart, v1.ProcessEvent{SessionID: "agent-session", ProcessID: "proc-b", Name: "two"}),
		agentSessionEvent(v1.EventAgentSessionStart, v1.AgentSessionEvent{SessionID: "child-session", ParentSessionID: "agent-session", Agent: "explore"}),
	} {
		if _, err := tracker.Apply(event, false); err != nil {
			t.Fatal(err)
		}
	}

	reports, err := tracker.Apply(sessionDelta("child-session", v1.MessagePartDelta{MessageID: "m", Kind: "text", Delta: "work"}), false)
	if err != nil || len(reports) != 1 {
		t.Fatalf("child reports = %#v, %v", reports, err)
	}
	if got := reports[0].Line; got != "    ○ [explore] response: work" {
		t.Fatalf("child line = %q", got)
	}
}

func TestRuntimeActivityTrackerProgressFollowsLifecycleGenerations(t *testing.T) {
	tracker := NewRuntimeActivityTracker("session-main")
	startRootSession(tracker)
	apply := func(event v1.Event) []RuntimeActivityReport {
		reports, err := tracker.Apply(event, false)
		if err != nil {
			t.Fatal(err)
		}
		return reports
	}
	progress := func(status string, tokens int) v1.Event {
		data, _ := json.Marshal(v1.AgentSessionProgress{SessionID: "task", Agent: "explore", Status: status, Usage: v1.Usage{TotalTokens: tokens}})
		return v1.Event{Type: v1.EventAgentSessionProgress, SessionID: "task", Data: data}
	}

	apply(agentSessionEvent(v1.EventAgentSessionStart, v1.AgentSessionEvent{SessionID: "task", ParentSessionID: "session-main", Agent: "explore"}))
	reports := apply(progress("running", 10))
	if len(reports) != 1 || reports[0].ID != "task\x00:status" || reports[0].Terminal {
		t.Fatalf("running generation = %#v", reports)
	}

	// agent_session.finished records lifecycle state but leaves the generation
	// open for the final progress event, which carries authoritative counters.
	reports = apply(agentSessionEvent(v1.EventAgentSessionFinished, v1.AgentSessionEvent{SessionID: "task", Status: "succeeded"}))
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

	apply(agentSessionEvent(v1.EventAgentSessionWorking, v1.AgentSessionEvent{SessionID: "task"}))
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

func TestRuntimeActivityTrackerDefersProgressUntilDescendantsFinish(t *testing.T) {
	tracker := NewRuntimeActivityTracker("session-main")
	startRootSession(tracker)
	apply := func(event v1.Event) []RuntimeActivityReport {
		reports, err := tracker.Apply(event, false)
		if err != nil {
			t.Fatal(err)
		}
		return reports
	}
	progress := func(sessionID, status string, tokens, tools int) v1.Event {
		data, _ := json.Marshal(v1.AgentSessionProgress{SessionID: sessionID, Agent: "explore", Status: status, Usage: v1.Usage{TotalTokens: tokens}, ToolUses: tools})
		return v1.Event{Type: v1.EventAgentSessionProgress, SessionID: sessionID, Data: data}
	}

	apply(agentSessionEvent(v1.EventAgentSessionStart, v1.AgentSessionEvent{SessionID: "parent", ParentSessionID: "session-main", Agent: "explore", Name: "parent"}))
	apply(progress("parent", "running", 100, 2))
	apply(agentSessionEvent(v1.EventAgentSessionStart, v1.AgentSessionEvent{SessionID: "child", ParentSessionID: "parent", Agent: "explore", Name: "child"}))
	apply(agentSessionEvent(v1.EventAgentSessionStart, v1.AgentSessionEvent{SessionID: "grandchild", ParentSessionID: "child", Agent: "explore", Name: "grandchild"}))

	reports := apply(progress("parent", "succeeded", 125, 3))
	if len(reports) != 1 || reports[0].Terminal || !strings.Contains(reports[0].Line, "⠋ [explore:parent] agent: explore · 125 tokens · 3 tools · 1 active activity") {
		t.Fatalf("parent terminal progress with descendants = %#v", reports)
	}
	apply(agentSessionEvent(v1.EventAgentSessionFinished, v1.AgentSessionEvent{SessionID: "child", Status: "succeeded"}))
	reports = apply(agentSessionEvent(v1.EventAgentSessionFinished, v1.AgentSessionEvent{SessionID: "grandchild", Status: "succeeded"}))
	var parent *RuntimeActivityReport
	for i := range reports {
		if reports[i].SessionID == "parent" {
			parent = &reports[i]
		}
	}
	if parent == nil || !parent.Terminal || !parent.EmitPlain || !strings.Contains(parent.Line, "✓ [explore:parent] agent: explore · 125 tokens · 3 tools") {
		t.Fatalf("settled parent progress = %#v", reports)
	}
	if reports := apply(agentSessionEvent(v1.EventAgentSessionIdle, v1.AgentSessionEvent{SessionID: "grandchild"})); len(reports) != 0 {
		t.Fatalf("duplicate settled progress = %#v", reports)
	}
}

func TestRuntimeActivityTrackerIgnoresUnownedSessionEventsBeforeChildStart(t *testing.T) {
	tracker := NewRuntimeActivityTracker("session-main")
	startRootSession(tracker)

	for _, test := range []struct {
		name      string
		eventType string
		data      json.RawMessage
	}{
		{name: "input admitted", eventType: v1.EventSessionInputAdmitted, data: json.RawMessage(`{"input_id":"input","message_id":"message","content":"inspect","delivery":"steer"}`)},
		{name: "input promoted", eventType: v1.EventSessionInputPromoted, data: json.RawMessage(`{"input_id":"input","message_id":"message"}`)},
		{name: "message appended", eventType: v1.EventSessionMessageAppended, data: json.RawMessage(`{}`)},
		{name: "status prompt appended", eventType: v1.EventSessionStatusPromptAppended, data: json.RawMessage(`{}`)},
		{name: "selection changed", eventType: v1.EventSessionSelectionChanged, data: json.RawMessage(`{}`)},
	} {
		t.Run(test.name, func(t *testing.T) {
			reports, err := tracker.Apply(v1.Event{Type: test.eventType, SessionID: "child", Data: test.data}, false)
			if err != nil || len(reports) != 0 {
				t.Fatalf("reports = %#v, err = %v", reports, err)
			}
		})
	}

	reports, err := tracker.Apply(sessionDelta("child", v1.MessagePartDelta{MessageID: "message", Kind: "text", Delta: "lost"}), false)
	if err != nil || len(reports) != 1 || reports[0].Line != "✗ unknown event origin child (message.part.delta)" {
		t.Fatalf("unknown activity reports = %#v, err = %v", reports, err)
	}

	if _, err := tracker.Apply(agentSessionEvent(v1.EventAgentSessionStart, v1.AgentSessionEvent{SessionID: "child", ParentSessionID: "session-main", Agent: "explore"}), false); err != nil {
		t.Fatal(err)
	}
	reports, err = tracker.Apply(sessionDelta("child", v1.MessagePartDelta{MessageID: "message", Kind: "text", Delta: "working"}), false)
	if err != nil || len(reports) != 1 || reports[0].Line != "  ○ [explore] response: working" {
		t.Fatalf("registered child reports = %#v, err = %v", reports, err)
	}
}

func TestRuntimeActivityTrackerTracksTreeAndReportsUnknownOrigins(t *testing.T) {
	tracker := NewRuntimeActivityTracker("session-main")
	startRootSession(tracker)

	// Content for a session which never started reports an unknown origin once,
	// no matter how many events reference it.
	for i := 0; i < 2; i++ {
		reports, err := tracker.Apply(sessionDelta("task-ghost", v1.MessagePartDelta{MessageID: "m", Kind: "text", Delta: "hi"}), false)
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
		if want == 1 && (!reports[0].Terminal || reports[0].Line != "✗ unknown event origin task-ghost (message.part.delta)") {
			t.Fatalf("unknown origin report = %#v", reports[0])
		}
	}

	// A start with an unknown parent registers the session and reports the
	// missing parent; the orphan still renders its own content.
	reports, err := tracker.Apply(agentSessionEvent(v1.EventAgentSessionStart, v1.AgentSessionEvent{SessionID: "task-orphan", ParentSessionID: "session-missing", Agent: "explore"}), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 1 || reports[0].Line != "✗ unknown event origin session-missing (parent session of task-orphan)" {
		t.Fatalf("orphan parent reports = %#v", reports)
	}
	reports, err = tracker.Apply(sessionDelta("task-orphan", v1.MessagePartDelta{MessageID: "m", Kind: "text", Delta: "orphan work"}), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 1 || !strings.Contains(reports[0].Line, "○ [explore] response: orphan work") {
		t.Fatalf("orphan content reports = %#v", reports)
	}

	// Depth comes from the parent chain the tracker builds, not from the
	// events themselves.
	for _, start := range []v1.AgentSessionEvent{
		{SessionID: "task-parent", ParentSessionID: "session-main", Agent: "build"},
		{SessionID: "task-child", ParentSessionID: "task-parent", Agent: "review"},
	} {
		if _, err := tracker.Apply(agentSessionEvent(v1.EventAgentSessionStart, start), false); err != nil {
			t.Fatal(err)
		}
	}
	reports, err = tracker.Apply(sessionDelta("task-child", v1.MessagePartDelta{MessageID: "m", Kind: "text", Delta: "nested"}), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 1 || reports[0].Line != "    ○ [review] response: nested" {
		t.Fatalf("nested content report = %#v", reports)
	}

	// Main session content returns nothing: the caller renders it directly.
	reports, err = tracker.Apply(sessionDelta("session-main", v1.MessagePartDelta{MessageID: "m", Kind: "text", Delta: "mine"}), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 0 {
		t.Fatalf("root session reports = %#v", reports)
	}

	// Failed sessions surface a terminal error line under their own prefix.
	reports, err = tracker.Apply(agentSessionEvent(v1.EventAgentSessionFinished, v1.AgentSessionEvent{SessionID: "task-child", Status: "failed", Error: "boom"}), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 1 || !reports[0].Terminal || reports[0].Line != "    ✗ [review] failed: boom" {
		t.Fatalf("finished report = %#v", reports)
	}

	// Successful sessions surface a terminal completion line under their own prefix.
	reports, err = tracker.Apply(agentSessionEvent(v1.EventAgentSessionFinished, v1.AgentSessionEvent{SessionID: "task-child", Status: "succeeded"}), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 1 || !reports[0].Terminal || reports[0].Line != "    ✓ [review] completed" {
		t.Fatalf("finished report = %#v", reports)
	}
}
