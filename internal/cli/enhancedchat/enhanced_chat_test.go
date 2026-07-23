package enhancedchat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	v1 "github.com/amirulashraf/parrot-coder/internal/api/v1"
	"github.com/amirulashraf/parrot-coder/internal/cli/chatview"
	customcommand "github.com/amirulashraf/parrot-coder/internal/command"
	"github.com/amirulashraf/parrot-coder/internal/terminal"
)

type apiClient = API

type agentModeAPI struct {
	apiClient
	agents v1.AgentList
	modes  v1.ModeList
}

func (a *agentModeAPI) Agents(context.Context) (v1.AgentList, error) { return a.agents, nil }
func (a *agentModeAPI) Modes(context.Context) (v1.ModeList, error)   { return a.modes, nil }

func TestSubtaskPromptUsesSpawnAndMonitor(t *testing.T) {
	prompt := subtaskPrompt(customcommand.Expansion{Prompt: "Inspect this", Agent: "explorer", Model: "local/model", Subtask: true})
	want := "Delegate the following work using agent_spawn with agent \"explorer\" and model \"local/model\". agent_spawn returns a task_id. Call wait_task(task_id), then relay its output.\n\nInspect this"
	if prompt != want {
		t.Fatalf("subtask prompt = %q, want %q", prompt, want)
	}
}

func TestShellCallbacksSynchronizeExtractedState(t *testing.T) {
	firstAPI, secondAPI := &enhancedQueueAPI{}, &enhancedQueueAPI{}
	external := v1.Session{ID: "second", Agent: "plan", Provider: "remote", Model: "model"}
	var updated v1.Session
	shell := &chatShell{
		api: firstAPI, current: v1.Session{ID: "first"}, selection: chatSelection{agent: "build", model: "local/model"},
		config: &Config{
			CurrentAPI:      func() API { return secondAPI },
			ThinkingEnabled: func() bool { return true },
			Current:         func() v1.Session { return external },
			SetCurrent:      func(item v1.Session) { updated = item },
			Agent:           func() string { return external.Agent },
			ModelName:       func() string { return external.Provider + "/" + external.Model },
		},
	}

	shell.refreshState()
	if shell.api != secondAPI || shell.current != external || shell.selection.agent != "plan" || shell.selection.model != "remote/model" || !shell.options.thinking {
		t.Fatalf("refreshed shell = %#v", shell)
	}
	want := v1.Session{ID: "third", Agent: "explore", Provider: "remote", Model: "other"}
	shell.setCurrent(want)
	if shell.current != want || updated != want || shell.selection.agent != "explore" || shell.selection.model != "remote/other" {
		t.Fatalf("updated shell current=%#v selection=%#v callback=%#v", shell.current, shell.selection, updated)
	}
}

func TestEnhancedSubmissionCommitsUserMessage(t *testing.T) {
	var output bytes.Buffer
	renderer := terminal.NewLiveRenderer(&output, terminal.RendererConfig{TTY: true, Color: true, MaxRows: 6})
	if err := renderer.Prompt(terminal.PromptState{Prefix: "$ ", Text: "keep this", Cursor: 9}); err != nil {
		t.Fatal(err)
	}
	before := output.Len()
	shell := &chatShell{renderer: renderer}
	if err := shell.commitUser("keep this"); err != nil {
		t.Fatal(err)
	}
	committed := output.String()[before:]
	if strings.Count(committed, "$ keep this") != 1 {
		t.Fatalf("submitted user message was not committed once: %q", output.String())
	}
	if strings.Contains(committed, "─") || !strings.Contains(committed, "\n\x1b[38;5;230m$ keep this\x1b[0m") {
		t.Fatalf("submitted user message did not use its role colors after an empty line: %q", committed)
	}
}

func TestEnhancedBusySubmissionSteersAndPromotionCommits(t *testing.T) {
	api := &enhancedQueueAPI{}
	var output bytes.Buffer
	renderer := terminal.NewLiveRenderer(&output, terminal.RendererConfig{TTY: true, Columns: 40, MaxRows: 6})
	editor := terminal.NewEditorIO(bytes.NewBuffer(nil), nil, terminal.WithEditorPrompt("$ "))
	state, err := editor.Start("")
	if err != nil {
		t.Fatal(err)
	}
	shell := &chatShell{
		ctx: context.Background(), api: api, current: v1.Session{ID: "session", Agent: "build", Provider: "local", Model: "test"},
		selection: chatSelection{agent: "build", model: "local/test"}, renderer: renderer,
	}
	runtime := &enhancedChatRuntime{shell: shell, state: state, busy: true, knownMessages: map[string]bool{}, events: make(chan enhancedSessionEvent, 1)}
	if err := runtime.submitPrompt("next task"); err != nil {
		t.Fatal(err)
	}
	if len(api.prompts) != 1 || api.prompts[0].Delivery != "steer" || len(runtime.pending) != 1 {
		t.Fatalf("prompts=%#v pending=%#v", api.prompts, runtime.pending)
	}
	if err := runtime.render(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "$ next task  (○ pending)") || !strings.Contains(output.String(), "⠋ $ ") {
		t.Fatalf("steer frame = %q", output.String())
	}
	payload, _ := json.Marshal(v1.SessionInputPromoted{InputID: "input", MessageID: "message"})
	if err := runtime.handleEvent(v1.Event{Type: v1.EventSessionInputPromoted, Data: payload}); err != nil {
		t.Fatal(err)
	}
	if len(runtime.pending) != 0 || !strings.Contains(output.String(), "$ next task") {
		t.Fatalf("promoted steer pending=%#v output=%q", runtime.pending, output.String())
	}
}

func TestEnhancedBusySlashRunsSafeAndRejectsMutation(t *testing.T) {
	api := &enhancedQueueAPI{}
	var output bytes.Buffer
	renderer := terminal.NewLiveRenderer(&output, terminal.RendererConfig{TTY: true, Columns: 50})
	shell := &chatShell{
		ctx: context.Background(), api: api, current: v1.Session{ID: "session", Agent: "build", Provider: "local", Model: "test"},
		selection: chatSelection{agent: "build", model: "local/test"}, renderer: renderer,
	}
	shell.config = &Config{Slash: func(name, _ string) (bool, int) {
		if name == "/status" {
			_ = renderer.Commit("session: " + shell.current.ID)
		}
		return false, exitOK
	}}
	runtime := &enhancedChatRuntime{shell: shell, busy: true, knownMessages: map[string]bool{}, events: make(chan enhancedSessionEvent, 1)}
	runtime.handleBuiltin("/status", "")
	if !strings.Contains(output.String(), "session: session") || len(api.prompts) != 0 {
		t.Fatalf("safe slash output=%q prompts=%#v", output.String(), api.prompts)
	}
	runtime.handleBuiltin("/new", "")
	if shell.current.ID != "session" || !strings.Contains(output.String(), "unavailable while the agent is working") {
		t.Fatalf("busy mutation changed session: %#v output=%q", shell.current, output.String())
	}
}

func TestEnhancedProviderRetryNoticeFlushesImmediately(t *testing.T) {
	var output bytes.Buffer
	renderer := terminal.NewLiveRenderer(&output, terminal.RendererConfig{TTY: true, Columns: 50})
	shell := &chatShell{ctx: context.Background(), renderer: renderer}
	runtime := &enhancedChatRuntime{shell: shell, busy: true, knownMessages: map[string]bool{}, events: make(chan enhancedSessionEvent, 1)}
	status, _ := json.Marshal(v1.SessionStatus{Kind: "provider_retry", Message: "provider fake is overloaded; retrying in 2s (attempt 1)"})
	if err := runtime.handleEvent(v1.Event{Type: v1.EventSessionStatus, Data: status}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "provider fake is overloaded; retrying in 2s (attempt 1)") {
		t.Fatalf("retry notice was not flushed: %q", output.String())
	}
}

func TestEnhancedIdleWaitsForQueuedPromotionBeforeFinalAssistant(t *testing.T) {
	api := &enhancedQueueAPI{messages: v1.MessageList{Items: []v1.Message{
		{ID: "first", Role: "assistant", Content: "first answer", Status: "complete"},
		{ID: "second", Role: "assistant", Content: "second answer", Status: "complete"},
	}}}
	var output bytes.Buffer
	renderer := terminal.NewLiveRenderer(&output, terminal.RendererConfig{TTY: true, Columns: 50})
	shell := &chatShell{ctx: context.Background(), api: api, current: v1.Session{ID: "session"}, renderer: renderer}
	runtime := &enhancedChatRuntime{
		shell: shell, busy: true, knownMessages: map[string]bool{}, events: make(chan enhancedSessionEvent, 1),
		pending: []queuedChatInput{{inputID: "input", messageID: "queued", content: "queued question"}},
	}
	idle, _ := json.Marshal(v1.SessionStatus{Kind: "idle"})
	if err := runtime.handleEvent(v1.Event{Type: v1.EventSessionStatus, Data: idle}); err != nil {
		t.Fatal(err)
	}
	if !runtime.busy || strings.Contains(output.String(), "second answer") {
		t.Fatalf("idle settled before promotion: busy=%t output=%q", runtime.busy, output.String())
	}
	complete, _ := json.Marshal(map[string]string{"message_id": "first"})
	if err := runtime.handleEvent(v1.Event{Type: "session.assistant.complete", Data: complete}); err != nil {
		t.Fatal(err)
	}
	promoted, _ := json.Marshal(v1.SessionInputPromoted{InputID: "input", MessageID: "queued"})
	if err := runtime.handleEvent(v1.Event{Type: v1.EventSessionInputPromoted, Data: promoted}); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	first := strings.Index(text, "first answer")
	queued := strings.Index(text, "queued question")
	second := strings.Index(text, "second answer")
	if first < 0 || queued < first || second < queued || runtime.busy {
		t.Fatalf("turn order first=%d queued=%d second=%d busy=%t output=%q", first, queued, second, runtime.busy, text)
	}
}

func TestEnhancedTurnCompleteCallbackRunsOnceOnlyForSuccessfulNewTurns(t *testing.T) {
	for _, test := range []struct {
		name, eventType, status, messageError string
		wantCallback                          bool
	}{
		{name: "complete", eventType: "session.assistant.complete", status: "complete", wantCallback: true},
		{name: "error", eventType: "session.assistant.error", status: "error", messageError: "failed"},
		{name: "interrupted", eventType: "session.assistant.interrupted", status: "interrupted"},
	} {
		t.Run(test.name, func(t *testing.T) {
			api := &enhancedQueueAPI{messages: v1.MessageList{Items: []v1.Message{{ID: "assistant", Role: "assistant", Content: "finished plan", Status: test.status, Error: test.messageError}}}}
			var output bytes.Buffer
			renderer := terminal.NewLiveRenderer(&output, terminal.RendererConfig{TTY: true})
			editor := terminal.NewEditorIO(bytes.NewBuffer(nil), nil)
			calls := 0
			var completed TurnComplete
			shell := &chatShell{
				ctx: context.Background(), api: api, current: v1.Session{ID: "session", Agent: "custom"}, selection: chatSelection{agent: "custom"},
				renderer: renderer, editor: editor,
				config: &Config{OnTurnComplete: func(item TurnComplete) *TurnCompleteDialog {
					calls++
					completed = item
					if !strings.Contains(output.String(), "finished plan") {
						t.Fatal("turn completion callback ran before the assistant response was committed")
					}
					return &TurnCompleteDialog{Prompt: "continue? ", Context: []string{"turn finished"}, Handle: func(value string) (TurnCompleteResult, error) {
						if strings.TrimSpace(value) == "" {
							return TurnCompleteResult{ValidationError: "answer required"}, nil
						}
						return TurnCompleteResult{}, nil
					}}
				}},
			}
			runtime := &enhancedChatRuntime{shell: shell, busy: true, idleSeen: true, knownMessages: map[string]bool{}, events: make(chan enhancedSessionEvent, 1)}
			payload, _ := json.Marshal(map[string]string{"message_id": "assistant"})
			if err := runtime.handleEvent(v1.Event{Type: test.eventType, Data: payload}); err != nil {
				t.Fatal(err)
			}

			if !test.wantCallback {
				if calls != 0 || runtime.modal != nil {
					t.Fatalf("callback calls=%d modal=%#v", calls, runtime.modal)
				}
				return
			}
			if calls != 1 || completed.Session.ID != "session" || completed.Mode != "custom" || completed.MessageID != "assistant" || runtime.modal == nil || runtime.modal.kind != "turn_complete" {
				t.Fatalf("calls=%d completed=%#v modal=%#v", calls, completed, runtime.modal)
			}
			runtime.idleSeen = true
			if err := runtime.settleIdle(); err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatalf("callback called %d times", calls)
			}
			if err := runtime.answerModal(""); !errors.Is(err, errInvalidModalAnswer) || runtime.modal == nil {
				t.Fatalf("empty answer err=%v modal=%#v", err, runtime.modal)
			}
			if err := runtime.answerModal("done"); err != nil || runtime.modal != nil {
				t.Fatalf("valid answer err=%v modal=%#v", err, runtime.modal)
			}
		})
	}

	calls := 0
	shell := &chatShell{ctx: context.Background(), api: &enhancedQueueAPI{}, current: v1.Session{ID: "restored"}, config: &Config{OnTurnComplete: func(TurnComplete) *TurnCompleteDialog {
		calls++
		return nil
	}}}
	runtime := &enhancedChatRuntime{shell: shell, busy: true, idleSeen: true, lastCompleteID: "historical", turnCompleteID: "historical", knownMessages: map[string]bool{}}
	if err := runtime.settleIdle(); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("callback ran %d times for restored history", calls)
	}
}

func TestEnhancedTurnCompleteChoicesSelectOrRequestFeedback(t *testing.T) {
	for _, test := range []struct {
		name       string
		selected   int
		wantAnswer string
		wantInput  bool
	}{
		{name: "approve", wantAnswer: "yes"},
		{name: "stop", selected: 1, wantAnswer: "no"},
		{name: "feedback", selected: 2, wantInput: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			state, err := terminal.NewEditorIO(bytes.NewBuffer(nil), nil).Start("draft")
			if err != nil {
				t.Fatal(err)
			}
			var answer string
			dialog := &TurnCompleteDialog{
				Prompt: "Plan complete: ",
				Choices: []terminal.Candidate{
					{Value: "yes"},
					{Value: "no"},
					{Value: "feedback"},
				},
				CustomChoice: "feedback", CustomPrompt: "plan feedback: ",
				Handle: func(value string) (TurnCompleteResult, error) {
					answer = value
					return TurnCompleteResult{}, nil
				},
			}
			runtime := &enhancedChatRuntime{
				shell: &chatShell{},
				modal: &enhancedModal{kind: "turn_complete", state: state, prompt: dialog.Prompt, choices: append([]terminal.Candidate(nil), dialog.Choices...), selected: test.selected, turnComplete: dialog},
			}

			handled, err := runtime.handleTurnCompleteModalKey(terminal.Key{Kind: terminal.KeyEnter})
			if err != nil || !handled {
				t.Fatalf("selection handled=%t err=%v", handled, err)
			}
			if test.wantInput {
				if runtime.modal == nil || !runtime.modal.customInput || len(runtime.modal.choices) != 0 || runtime.modal.prompt != "plan feedback: " || state.Value() != "" || answer != "" {
					t.Fatalf("feedback modal=%#v value=%q answer=%q", runtime.modal, state.Value(), answer)
				}
				if err := runtime.answerModal("revise error handling"); err != nil {
					t.Fatal(err)
				}
				if answer != "revise error handling" || runtime.modal != nil {
					t.Fatalf("feedback answer=%q modal=%#v", answer, runtime.modal)
				}
				return
			}
			if answer != test.wantAnswer || runtime.modal != nil {
				t.Fatalf("answer=%q modal=%#v", answer, runtime.modal)
			}
		})
	}
}

func TestEnhancedThinkingActivityShowsRunningTokenUsage(t *testing.T) {
	runtime := &enhancedChatRuntime{knownMessages: map[string]bool{}}
	runtime.startAssistantActivity("assistant")
	runtime.startReasoningActivity("assistant", "", "Checking the implementation", true)

	usage, _ := json.Marshal(v1.SessionStatus{
		MessageID: "assistant",
		Kind:      "usage",
		Usage:     &v1.Usage{OutputTokens: 200, TotalTokens: 456, ReasoningTokens: 123},
	})
	if err := runtime.handleEvent(v1.Event{Type: v1.EventSessionStatus, Data: usage}); err != nil {
		t.Fatal(err)
	}
	if got := formatReasoningActivity(runtime.activity[0], runtime.activity[0].started, 100); !strings.Contains(got, "⠋ Thinking: Checking the implementation · 123 tokens · 0.0s") {
		t.Fatalf("activity after usage = %q", got)
	}
	if runtime.contextTokens != 456 {
		t.Fatalf("context tokens = %d, want 456", runtime.contextTokens)
	}

	runtime.completeAssistantActivity("assistant", "success")
	if !runtime.activity[0].terminal || !runtime.activity[0].reasoning || runtime.activity[0].tokens != 123 || !runtime.activity[0].hasUsage {
		t.Fatalf("completed activity lost usage: %#v", runtime.activity[0])
	}
}

func TestEnhancedUntitledThinkingMovesToModeline(t *testing.T) {
	runtime := &enhancedChatRuntime{knownMessages: map[string]bool{}}
	runtime.startAssistantActivity("assistant")
	started := runtime.activity[0].started

	if rows := runtime.activityRows(started, 100); len(rows) != 0 {
		t.Fatalf("untitled thinking remained in activity rows: %#v", rows)
	}
	if got := runtime.modelineThinking(started); got != "⠋ Thinking… · 0.0s" {
		t.Fatalf("modeline thinking = %q", got)
	}

	runtime.startReasoningActivity("assistant", "", "Inspecting the implementation", true)
	if got := runtime.modelineThinking(started); got != "" {
		t.Fatalf("titled thought remained in modeline: %q", got)
	}
	rows := runtime.activityRows(started, 100)
	if len(rows) != 1 || !strings.Contains(rows[0], "Thought: Inspecting the implementation") {
		t.Fatalf("titled thought was not kept in activity rows: %#v", rows)
	}
}

func TestEnhancedReasoningUsageDoesNotFallBackToOutputTokens(t *testing.T) {
	runtime := &enhancedChatRuntime{knownMessages: map[string]bool{}}
	runtime.startAssistantActivity("assistant")
	runtime.updateAssistantUsage("assistant", &v1.Usage{OutputTokens: 30})
	runtime.startReasoningActivity("assistant", "", "Checking", true)
	if runtime.activity[0].hasUsage {
		t.Fatalf("reasoning activity used output tokens: %#v", runtime.activity[0])
	}

	runtime.updateAssistantUsage("assistant", &v1.Usage{OutputTokens: 30, ReasoningTokens: 12})
	runtime.updateAssistantUsage("assistant", &v1.Usage{OutputTokens: 40})
	if runtime.activity[0].tokens != 12 || !runtime.activity[0].hasUsage {
		t.Fatalf("reasoning usage was not retained: %#v", runtime.activity[0])
	}
}

// The modeline reports what the whole task tree spent. Every session reports
// usage the same way — the main session under no task id, a subagent under its
// own — and a usage event covers one turn, so the totals accumulate. A
// subagent's task.progress repeats what it has spent and must not be counted
// again on top of its usage events.
func TestEnhancedModelineUsageCoversMainTaskAndSubagentsOnce(t *testing.T) {
	runtime := &enhancedChatRuntime{shell: &chatShell{}, knownMessages: map[string]bool{}, completedToolIDs: map[string]bool{"call-agent": true}}
	usageEvent := func(taskID string, usage v1.Usage) v1.Event {
		data, _ := json.Marshal(v1.SessionStatus{MessageID: "assistant", Kind: "usage", Usage: &usage})
		return v1.Event{Type: v1.EventSessionStatus, TaskID: taskID, Data: data}
	}
	for range 2 {
		if err := runtime.handleEvent(usageEvent("", v1.Usage{InputTokens: 1200, OutputTokens: 300, CachedInputTokens: 800, TotalTokens: 1500, InputCost: 0.25, OutputCost: 0.5})); err != nil {
			t.Fatal(err)
		}
	}
	if want := (chatview.TaskUsage{InputTokens: 2400, OutputTokens: 600, CachedTokens: 1600, Cost: 1.5}); runtime.mainTaskUsage != want {
		t.Fatalf("session usage = %#v, want %#v", runtime.mainTaskUsage, want)
	}

	if err := runtime.handleEvent(taskStart("task-1", "task_main", "explore")); err != nil {
		t.Fatal(err)
	}
	if err := runtime.handleEvent(usageEvent("task-1", v1.Usage{InputTokens: 100, OutputTokens: 50, CachedInputTokens: 25, TotalTokens: 150, InputCost: 0.125})); err != nil {
		t.Fatal(err)
	}
	want := chatview.TaskUsage{InputTokens: 2500, OutputTokens: 650, CachedTokens: 1625, Cost: 1.625}
	if runtime.mainTaskUsage != want {
		t.Fatalf("usage with subagent = %#v, want %#v", runtime.mainTaskUsage, want)
	}
	if got := formatTaskTokenUsage(runtime.mainTaskUsage); got != "+2.5ki +650o (+65.00% cache)" {
		t.Fatalf("modeline token usage = %q", got)
	}

	progress, _ := json.Marshal(v1.TaskProgress{TaskID: "task-1", ToolCallID: "call-agent", Agent: "explore", Status: "running",
		Usage: v1.Usage{InputTokens: 100, OutputTokens: 50, CachedInputTokens: 25, TotalTokens: 150}, ToolUses: 3})
	if err := runtime.handleEvent(taskContent("task-1", v1.EventTaskProgress, progress)); err != nil {
		t.Fatal(err)
	}
	if runtime.mainTaskUsage != want {
		t.Fatalf("task progress counted usage twice: %#v", runtime.mainTaskUsage)
	}
	if line := formatActivity(runtime.activity[0], runtime.activity[0].started); !strings.Contains(line, "+100i +50o (+25.00% cache) · 3 tools") {
		t.Fatalf("progress line = %q", line)
	}
}

// taskStart builds the flat task.start event introducing one task into the
// UI's task tree. Every other event for the task carries only its task_id.
func taskStart(taskID, parentTaskID, agent string, name ...string) v1.Event {
	event := v1.TaskEvent{TaskID: taskID, ParentTaskID: parentTaskID, Kind: "agent", Agent: agent}
	if len(name) > 0 {
		event.Name = name[0]
	}
	data, _ := json.Marshal(event)
	return v1.Event{Type: v1.EventTaskStart, TaskID: taskID, Data: data}
}

func taskContent(taskID string, eventType string, data json.RawMessage) v1.Event {
	return v1.Event{Type: eventType, TaskID: taskID, Data: data}
}

func TestEnhancedChildAgentProgressUpdatesToolActivity(t *testing.T) {
	runtime := &enhancedChatRuntime{shell: &chatShell{}, knownMessages: map[string]bool{}, completedToolIDs: map[string]bool{"call-agent": true}}
	if err := runtime.handleEvent(taskStart("task-1", "task_main", "explore")); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(v1.TaskProgress{TaskID: "task-1", ToolCallID: "call-agent", Agent: "explore", Status: "running", Usage: v1.Usage{TotalTokens: 35}, ToolUses: 3})
	if err := runtime.handleEvent(taskContent("task-1", v1.EventTaskProgress, data)); err != nil {
		t.Fatal(err)
	}
	if len(runtime.activity) != 1 {
		t.Fatalf("activity = %#v", runtime.activity)
	}
	line := formatActivity(runtime.activity[0], runtime.activity[0].started)
	if !strings.Contains(line, "35 tokens · 3 tools") {
		t.Fatalf("line = %q", line)
	}

	data, _ = json.Marshal(v1.TaskProgress{TaskID: "task-1", ToolCallID: "call-agent", Agent: "explore", Status: "succeeded", Usage: v1.Usage{TotalTokens: 35}, ToolUses: 3})
	if err := runtime.handleEvent(taskContent("task-1", v1.EventTaskProgress, data)); err != nil {
		t.Fatal(err)
	}
	if len(runtime.activity) != 0 {
		t.Fatalf("completed task remains live: %#v", runtime.activity)
	}

	data, _ = json.Marshal(v1.TaskProgress{TaskID: "task-1", ToolCallID: "call-agent", Agent: "explore", Status: "running"})
	if err := runtime.handleEvent(taskContent("task-1", v1.EventTaskProgress, data)); err != nil {
		t.Fatal(err)
	}
	if len(runtime.activity) != 0 {
		t.Fatalf("late task progress recreated activity: %#v", runtime.activity)
	}

	data, _ = json.Marshal(v1.TaskProgress{TaskID: "task-1", ToolCallID: "call-agent-2", Agent: "explore", Status: "running"})
	if err := runtime.handleEvent(taskContent("task-1", v1.EventTaskProgress, data)); err != nil {
		t.Fatal(err)
	}
	if len(runtime.activity) != 1 {
		t.Fatalf("follow-up task progress = %#v", runtime.activity)
	}
}

func TestEnhancedTaskEventUsesTreeDepthAndAgentPrefix(t *testing.T) {
	runtime := &enhancedChatRuntime{shell: &chatShell{}, knownMessages: map[string]bool{}}
	runtime.activity = append(runtime.activity,
		enhancedActivityItem{id: "call-read", label: "read · file.go", toolName: "read", status: "running", started: time.Now()},
		enhancedActivityItem{id: "call-agent", label: "agent · review", toolName: "monitor", status: "running", started: time.Now()},
	)
	// A grandchild renders two levels deep because the UI walks the parent
	// chain it tracks, not because an event envelope carries a depth.
	if err := runtime.handleEvent(taskStart("task-parent", "task_main", "build")); err != nil {
		t.Fatal(err)
	}
	if err := runtime.handleEvent(taskStart("task-review", "task-parent", "review", "ui-hierarchy")); err != nil {
		t.Fatal(err)
	}
	delta, _ := json.Marshal(v1.MessagePartDelta{MessageID: "child-message", Kind: "text", Delta: "checking"})
	if err := runtime.handleEvent(taskContent("task-review", v1.EventMessagePartDelta, delta)); err != nil {
		t.Fatal(err)
	}
	if len(runtime.activity) != 3 {
		t.Fatalf("activity = %#v", runtime.activity)
	}
	var subagent, agent string
	for _, item := range runtime.activity {
		formatted := formatActivity(item, item.started)
		if item.taskID == "task-review" {
			subagent = formatted
		}
		if item.id == "call-agent" {
			agent = formatted
		}
	}
	if subagent != "    ○ [review:ui-hierarchy] response: checking" {
		t.Fatalf("subagent activity = %q", subagent)
	}
	if !strings.Contains(agent, "Working: agent · review") {
		t.Fatalf("agent activity = %q", agent)
	}
}

func TestEnhancedMonitorLabelUsesFriendlyTaskName(t *testing.T) {
	presentation := chatview.NewPresentations(v1.ToolList{Items: []v1.Tool{{
		ID: "monitor",
		Presentation: v1.ToolPresentation{Label: v1.ToolLabel{Fields: []v1.ToolLabelPart{{
			Names: []string{"task_id"}, TaskName: true,
		}}}},
	}}})
	runtime := &enhancedChatRuntime{
		shell:         &chatShell{config: &Config{Presentation: func() chatview.Presentations { return presentation }}},
		knownMessages: map[string]bool{},
	}
	started, _ := json.Marshal(v1.TaskEvent{
		TaskID: "task-review", ParentTaskID: "task_main", Kind: "agent", Agent: "review", Name: "review-kind-ibex",
	})
	if err := runtime.handleEvent(taskContent("task-review", v1.EventTaskStart, started)); err != nil {
		t.Fatal(err)
	}
	pending := json.RawMessage(`{"call_id":"call-monitor","name":"monitor","input":{"task_id":"task-review"}}`)
	runtime.handleToolActivity(v1.Event{Type: "session.tool.pending", Data: pending})

	if len(runtime.activity) != 1 || runtime.activity[0].label != "monitor · review-kind-ibex" {
		t.Fatalf("monitor activity = %#v", runtime.activity)
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
		{name: "write_stdin", input: map[string]any{"task_id": "task_shell", "chars": "input"}, want: "write_stdin · task_shell · <redacted: 5 chars>"},
	} {
		if got := toolActivityLabel(test.name, test.input); got != test.want {
			t.Errorf("toolActivityLabel(%q) = %q, want %q", test.name, got, test.want)
		}
	}
}

func TestEnhancedSubagentCompletionRemovesAllRowsAndIgnoresLateEvents(t *testing.T) {
	runtime := &enhancedChatRuntime{shell: &chatShell{options: codingFlags{thinking: true}}, knownMessages: map[string]bool{}}
	if err := runtime.handleEvent(taskStart("task-review", "task_main", "review")); err != nil {
		t.Fatal(err)
	}
	for _, delta := range []v1.MessagePartDelta{
		{MessageID: "child-message", Kind: "reasoning", Delta: "checking"},
		{MessageID: "child-message", Kind: "text", Delta: "result"},
	} {
		data, _ := json.Marshal(delta)
		if err := runtime.handleEvent(taskContent("task-review", v1.EventMessagePartDelta, data)); err != nil {
			t.Fatal(err)
		}
	}
	if len(runtime.activity) != 2 {
		t.Fatalf("live subagent activity = %#v", runtime.activity)
	}

	data, _ := json.Marshal(map[string]string{"message_id": "child-message"})
	if err := runtime.handleEvent(taskContent("task-review", "session.assistant.complete", data)); err != nil {
		t.Fatal(err)
	}
	if len(runtime.activity) != 0 {
		t.Fatalf("completed subagent activity remains live: %#v", runtime.activity)
	}

	data, _ = json.Marshal(v1.MessagePartDelta{MessageID: "child-message", Kind: "text", Delta: "late"})
	if err := runtime.handleEvent(taskContent("task-review", v1.EventMessagePartDelta, data)); err != nil {
		t.Fatal(err)
	}
	if len(runtime.activity) != 0 {
		t.Fatalf("late subagent event recreated activity: %#v", runtime.activity)
	}
}

func TestEnhancedChildAgentProgressFormatsLargeTokenCount(t *testing.T) {
	item := enhancedActivityItem{label: "agent · explore", status: "success", started: time.Unix(100, 0), ended: time.Unix(101, 0), tokens: 300100, hasUsage: true, toolUses: 44}
	if got := formatActivity(item, item.ended); !strings.Contains(got, "300.1k tokens · 44 tools") {
		t.Fatalf("line = %q", got)
	}
}

func TestEnhancedReasoningSummaryReplacesRawReasoningActivity(t *testing.T) {
	runtime := &enhancedChatRuntime{shell: &chatShell{}, knownMessages: map[string]bool{}}
	runtime.startAssistantActivity("assistant")

	for _, delta := range []v1.MessagePartDelta{
		{MessageID: "assistant", Kind: "reasoning", Delta: "private reasoning"},
		{MessageID: "assistant", Kind: "reasoning_summary", Delta: "Inspecting the implementation"},
		{MessageID: "assistant", Kind: "reasoning", Delta: " ignored"},
		{MessageID: "assistant", Kind: "reasoning_summary", Delta: " and tests"},
	} {
		data, _ := json.Marshal(delta)
		if err := runtime.handleEvent(v1.Event{Type: v1.EventMessagePartDelta, Data: data}); err != nil {
			t.Fatal(err)
		}
	}

	if got := runtime.activity[0].label; got != "Inspecting the implementation and tests" {
		t.Fatalf("activity label = %q", got)
	}
}

func TestEnhancedReasoningSummaryPartsShowAsSeparateActivityRows(t *testing.T) {
	runtime := &enhancedChatRuntime{shell: &chatShell{}, knownMessages: map[string]bool{}}
	runtime.startAssistantActivity("assistant")

	for _, delta := range []v1.MessagePartDelta{
		{MessageID: "assistant", PartID: "reasoning:0", Kind: "reasoning_summary", Delta: "**Inspecting"},
		{MessageID: "assistant", PartID: "reasoning:0", Kind: "reasoning_summary", Delta: " the implementation**"},
		{MessageID: "assistant", PartID: "reasoning:1", Kind: "reasoning_summary", Delta: "**Running tests**"},
	} {
		data, _ := json.Marshal(delta)
		if err := runtime.handleEvent(v1.Event{Type: v1.EventMessagePartDelta, Data: data}); err != nil {
			t.Fatal(err)
		}
	}

	if len(runtime.activity) != 2 {
		t.Fatalf("activity = %#v", runtime.activity)
	}
	rows := runtime.activityRows(runtime.activity[0].started, 100)
	if len(rows) != 2 || !strings.Contains(rows[0], "Thought: Inspecting the implementation") || !strings.Contains(rows[1], "Thought: Running tests") {
		t.Fatalf("activity rows = %#v", rows)
	}
	if runtime.activity[0].status != "thinking" || runtime.activity[0].terminal || runtime.activity[1].status != "thinking" || runtime.activity[1].terminal {
		t.Fatalf("summary activity states = %#v", runtime.activity)
	}

	runtime.updateAssistantUsage("assistant", &v1.Usage{OutputTokens: 42})
	if !runtime.activity[1].hasUsage || runtime.activity[1].tokens != 42 || runtime.activity[0].hasUsage {
		t.Fatalf("newest activity did not exclusively receive usage: %#v", runtime.activity)
	}
	runtime.completeAssistantActivity("assistant", "success")
	for _, item := range runtime.activity {
		if !item.terminal || item.status != "success" {
			t.Fatalf("activity did not complete: %#v", runtime.activity)
		}
	}
}

func TestEnhancedReasoningSummaryStripsAdjacentBoldDelimiters(t *testing.T) {
	runtime := &enhancedChatRuntime{shell: &chatShell{}, knownMessages: map[string]bool{}}
	runtime.startAssistantActivity("assistant")

	for _, delta := range []string{
		"**Investigating summary stripping issue**",
		"**Analyzing reasoning summary handling flaws**",
	} {
		data, _ := json.Marshal(v1.MessagePartDelta{MessageID: "assistant", Kind: "reasoning_summary", Delta: delta})
		if err := runtime.handleEvent(v1.Event{Type: v1.EventMessagePartDelta, Data: data}); err != nil {
			t.Fatal(err)
		}
	}

	want := "**Investigating summary stripping issue**\n\n**Analyzing reasoning summary handling flaws**"
	if got := runtime.activity[0].label; got != want {
		t.Fatalf("activity label = %q, want %q", got, want)
	}

	for input, want := range map[string]string{
		"****":                              "****",
		"`**one****two**`":                  "`**one****two**`",
		"````go\n```\n**one****two**\n````": "````go\n```\n**one****two**\n````",
	} {
		if got := cleanReasoningActivityLabel(input); got != want {
			t.Errorf("cleanReasoningActivityLabel(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestEnhancedReasoningSummaryPartsUpdateIndependently(t *testing.T) {
	runtime := &enhancedChatRuntime{shell: &chatShell{}, knownMessages: map[string]bool{}}
	runtime.startAssistantActivity("assistant")

	for _, delta := range []v1.MessagePartDelta{
		{MessageID: "assistant", Kind: "reasoning", Delta: "private reasoning"},
		{MessageID: "assistant", PartID: "reasoning:0", Kind: "reasoning_summary", Delta: "**First"},
		{MessageID: "assistant", PartID: "reasoning:1", Kind: "reasoning_summary", Delta: "**Second item**"},
		{MessageID: "assistant", PartID: "reasoning:0", Kind: "reasoning_summary", Delta: " item**"},
	} {
		data, _ := json.Marshal(delta)
		if err := runtime.handleEvent(v1.Event{Type: v1.EventMessagePartDelta, Data: data}); err != nil {
			t.Fatal(err)
		}
	}

	if len(runtime.activity) != 2 || runtime.activity[0].label != "**First item**" || runtime.activity[1].label != "**Second item**" {
		t.Fatalf("activity = %#v", runtime.activity)
	}
	if runtime.activity[0].messageID != "assistant" || runtime.activity[1].messageID != "assistant" {
		t.Fatalf("summary rows lost assistant identity: %#v", runtime.activity)
	}
	rows := runtime.activityRows(time.Now(), 100)
	active := 0
	for _, row := range rows {
		if strings.Contains(row, "Thought:") {
			active++
		}
	}
	if active != 2 || runtime.activity[0].status != "thinking" || runtime.activity[1].status != "thinking" {
		t.Fatalf("interleaved summary parts have %d active thoughts: rows=%#v activity=%#v", active, rows, runtime.activity)
	}
}

func TestEnhancedReasoningSummaryDoneCommitsOnlyFinalizedPart(t *testing.T) {
	var output bytes.Buffer
	renderer := terminal.NewLiveRenderer(&output, terminal.RendererConfig{TTY: true, Columns: 100})
	runtime := &enhancedChatRuntime{
		shell: &chatShell{renderer: renderer}, knownMessages: map[string]bool{},
	}

	for _, delta := range []v1.MessagePartDelta{
		{MessageID: "assistant", PartID: "reasoning:0", Kind: "reasoning_summary", Delta: "First item"},
		{MessageID: "assistant", PartID: "reasoning:1", Kind: "reasoning_summary", Delta: "Second item"},
		{MessageID: "assistant", PartID: "reasoning:0", Kind: "reasoning_summary", Delta: "Final first item", Done: true},
	} {
		data, _ := json.Marshal(delta)
		if err := runtime.handleEvent(v1.Event{Type: v1.EventMessagePartDelta, Data: data}); err != nil {
			t.Fatal(err)
		}
	}

	if len(runtime.activity) != 1 || runtime.activity[0].label != "Second item" || runtime.activity[0].status != "thinking" {
		t.Fatalf("live activity = %#v", runtime.activity)
	}
	if got := output.String(); !strings.Contains(got, "· Final first item") {
		t.Fatalf("finalized summary was not committed: %q", got)
	}
}

func TestEnhancedLateAssistantStartDoesNotRecreateFinalizedSummary(t *testing.T) {
	var output bytes.Buffer
	renderer := terminal.NewLiveRenderer(&output, terminal.RendererConfig{TTY: true, Columns: 100})
	runtime := &enhancedChatRuntime{
		shell: &chatShell{renderer: renderer}, knownMessages: map[string]bool{},
	}

	for _, delta := range []v1.MessagePartDelta{
		{MessageID: "assistant", PartID: "reasoning:0", Kind: "reasoning_summary", Delta: "Finished item"},
		{MessageID: "assistant", PartID: "reasoning:0", Kind: "reasoning_summary", Done: true},
	} {
		data, _ := json.Marshal(delta)
		if err := runtime.handleEvent(v1.Event{Type: v1.EventMessagePartDelta, Data: data}); err != nil {
			t.Fatal(err)
		}
	}
	started, _ := json.Marshal(map[string]string{"message_id": "assistant"})
	if err := runtime.handleEvent(v1.Event{Type: "session.assistant.started", Data: started}); err != nil {
		t.Fatal(err)
	}
	if len(runtime.activity) != 0 {
		t.Fatalf("late assistant start recreated activity: %#v", runtime.activity)
	}
}

func TestEnhancedLateAssistantStartKeepsReasoningSummaryRows(t *testing.T) {
	runtime := &enhancedChatRuntime{shell: &chatShell{}, knownMessages: map[string]bool{}}
	delta, _ := json.Marshal(v1.MessagePartDelta{
		MessageID: "assistant", PartID: "reasoning:0", Kind: "reasoning_summary", Delta: "Checking tests",
	})
	if err := runtime.handleEvent(v1.Event{Type: v1.EventMessagePartDelta, Data: delta}); err != nil {
		t.Fatal(err)
	}
	started, _ := json.Marshal(map[string]string{"message_id": "assistant"})
	if err := runtime.handleEvent(v1.Event{Type: "session.assistant.started", Data: started}); err != nil {
		t.Fatal(err)
	}
	if len(runtime.activity) != 1 || runtime.activity[0].label != "Checking tests" || !runtime.activity[0].reasoning {
		t.Fatalf("late assistant start replaced summary activity: %#v", runtime.activity)
	}
}

func TestEnhancedReasoningSummaryPartsFlushInProviderOrder(t *testing.T) {
	api := &enhancedQueueAPI{messages: v1.MessageList{Items: []v1.Message{{ID: "assistant", Role: "assistant", Status: "complete"}}}}
	var output bytes.Buffer
	renderer := terminal.NewLiveRenderer(&output, terminal.RendererConfig{TTY: true, Columns: 80})
	shell := &chatShell{ctx: context.Background(), api: api, current: v1.Session{ID: "session"}, renderer: renderer}
	runtime := &enhancedChatRuntime{shell: shell, knownMessages: map[string]bool{}}

	runtime.startReasoningActivity("assistant", "reasoning:0", "First item", true)
	runtime.startReasoningActivity("assistant", "reasoning:1", "Second item", true)
	runtime.completeAssistantActivity("assistant", "success")
	if err := runtime.commitCompletedAssistants("assistant"); err != nil {
		t.Fatal(err)
	}

	first := strings.Index(output.String(), "First item")
	second := strings.Index(output.String(), "Second item")
	if first < 0 || second < first {
		t.Fatalf("summary rows flushed out of order: %q", output.String())
	}
	if len(runtime.activity) != 0 {
		t.Fatalf("completed activity remains live: %#v", runtime.activity)
	}
}

func TestEnhancedCompletedAssistantActivityIsRemovedOrFlushed(t *testing.T) {
	for _, test := range []struct {
		name        string
		content     string
		wantFlushed bool
	}{
		{name: "message retains summary", content: "answer", wantFlushed: true},
		{name: "no message flushes activity", wantFlushed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			api := &enhancedQueueAPI{messages: v1.MessageList{Items: []v1.Message{{ID: "assistant", Role: "assistant", Content: test.content, Status: "complete"}}}}
			var output bytes.Buffer
			renderer := terminal.NewLiveRenderer(&output, terminal.RendererConfig{TTY: true, Columns: 80})
			shell := &chatShell{ctx: context.Background(), api: api, current: v1.Session{ID: "session"}, renderer: renderer}
			runtime := &enhancedChatRuntime{shell: shell, knownMessages: map[string]bool{}}
			runtime.startAssistantActivity("assistant")
			runtime.startReasoningActivity("assistant", "", "Checking", true)
			runtime.updateAssistantUsage("assistant", &v1.Usage{OutputTokens: 30, ReasoningTokens: 12})
			runtime.completeAssistantActivity("assistant", "success")

			if err := runtime.commitCompletedAssistants("assistant"); err != nil {
				t.Fatal(err)
			}
			if len(runtime.activity) != 0 {
				t.Fatalf("completed activity remains live: %#v", runtime.activity)
			}
			flushed := strings.Contains(output.String(), "· Checking")
			if flushed != test.wantFlushed {
				t.Fatalf("flushed activity = %t, want %t; output=%q", flushed, test.wantFlushed, output.String())
			}
		})
	}
}

func TestEnhancedReasoningSummaryRendersMarkdownAndIsRetainedBeforeAnswer(t *testing.T) {
	api := &enhancedQueueAPI{messages: v1.MessageList{Items: []v1.Message{{ID: "assistant", Role: "assistant", Content: "final answer", Status: "complete"}}}}
	var output bytes.Buffer
	renderer := terminal.NewLiveRenderer(&output, terminal.RendererConfig{TTY: true, Columns: 100})
	shell := &chatShell{ctx: context.Background(), api: api, current: v1.Session{ID: "session"}, renderer: renderer}
	runtime := &enhancedChatRuntime{shell: shell, knownMessages: map[string]bool{}}
	runtime.startAssistantActivity("assistant")
	runtime.startReasoningActivity("assistant", "", "# Verifying\n\n- **complete suite**", true)
	runtime.updateAssistantUsage("assistant", &v1.Usage{OutputTokens: 30, ReasoningTokens: 12})
	runtime.completeAssistantActivity("assistant", "success")

	if err := runtime.commitCompletedAssistants("assistant"); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	summary := strings.Index(got, "· Verifying")
	list := strings.Index(got, "• complete suite")
	answer := strings.Index(got, "final answer")
	if summary < 0 || list < summary || answer < list || strings.Contains(got, "# Verifying") || strings.Contains(got, "**") {
		t.Fatalf("output = %q", got)
	}
}

func TestEnhancedEmptyResponseShowsPlaceholderUnlessToolTurn(t *testing.T) {
	for _, test := range []struct {
		name      string
		toolInput bool
		wantShown bool
	}{
		{name: "empty response shows placeholder", wantShown: true},
		{name: "tool turn hides placeholder", toolInput: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			api := &enhancedQueueAPI{messages: v1.MessageList{Items: []v1.Message{{ID: "assistant", Role: "assistant", Status: "complete"}}}}
			var output bytes.Buffer
			renderer := terminal.NewLiveRenderer(&output, terminal.RendererConfig{TTY: true, Columns: 80})
			shell := &chatShell{ctx: context.Background(), api: api, current: v1.Session{ID: "session"}, renderer: renderer}
			runtime := &enhancedChatRuntime{shell: shell, knownMessages: map[string]bool{}, streamMessageID: "assistant", toolInput: test.toolInput}
			if err := runtime.commitCompletedAssistants("assistant"); err != nil {
				t.Fatal(err)
			}
			contains := strings.Contains(output.String(), chatview.AgentEmptyResponseText)
			if contains != test.wantShown {
				t.Fatalf("placeholder shown=%t, want %t; output=%q", contains, test.wantShown, output.String())
			}
		})
	}
}

func TestEnhancedReasoningSummariesFlushBeforeFirstAnswerRow(t *testing.T) {
	var output bytes.Buffer
	renderer := terminal.NewLiveRenderer(&output, terminal.RendererConfig{TTY: true, Columns: 20, MaxRows: 6})
	state, err := terminal.NewEditorIO(bytes.NewBuffer(nil), nil).Start("")
	if err != nil {
		t.Fatal(err)
	}
	runtime := &enhancedChatRuntime{
		shell:         &chatShell{renderer: renderer},
		state:         state,
		knownMessages: map[string]bool{},
	}

	for _, delta := range []v1.MessagePartDelta{
		{MessageID: "assistant", PartID: "reasoning:0", Kind: "reasoning_summary", Delta: "Inspecting code"},
		{MessageID: "assistant", PartID: "reasoning:1", Kind: "reasoning_summary", Delta: "Running tests"},
	} {
		data, _ := json.Marshal(delta)
		if err := runtime.handleEvent(v1.Event{Type: v1.EventMessagePartDelta, Data: data}); err != nil {
			t.Fatal(err)
		}
	}
	if output.Len() != 0 {
		t.Fatalf("reasoning was committed before the answer boundary: %q", output.String())
	}

	answer, _ := json.Marshal(v1.MessagePartDelta{
		MessageID: "assistant", Kind: "text", Delta: "A sufficiently long answer to promote a wrapped row.",
	})
	if err := runtime.handleEvent(v1.Event{Type: v1.EventMessagePartDelta, Data: answer}); err != nil {
		t.Fatal(err)
	}
	if len(runtime.activity) != 0 {
		t.Fatalf("reasoning remained in the live region at the answer boundary: %#v", runtime.activity)
	}
	if err := runtime.render(); err != nil {
		t.Fatal(err)
	}

	got := output.String()
	first := strings.Index(got, "Inspecting code")
	second := strings.Index(got, "Running tests")
	answerRow := strings.Index(got, "A sufficiently")
	if first < 0 || second < first || answerRow < second {
		t.Fatalf("flush order first=%d second=%d answer=%d; output=%q", first, second, answerRow, got)
	}
}

func TestEnhancedRawReasoningIsDroppedAtAnswerBoundary(t *testing.T) {
	var output bytes.Buffer
	runtime := &enhancedChatRuntime{
		shell:         &chatShell{renderer: terminal.NewLiveRenderer(&output, terminal.RendererConfig{})},
		knownMessages: map[string]bool{},
	}
	runtime.startReasoningActivity("assistant", "", "private chain of thought", false)

	answer, _ := json.Marshal(v1.MessagePartDelta{MessageID: "assistant", Kind: "text", Delta: "answer"})
	if err := runtime.handleEvent(v1.Event{Type: v1.EventMessagePartDelta, Data: answer}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "private chain of thought") || len(runtime.activity) != 0 {
		t.Fatalf("raw reasoning crossed the answer boundary: output=%q activity=%#v", output.String(), runtime.activity)
	}
}

func TestEnhancedLateReasoningSummaryDoesNotCrossAnswerBoundary(t *testing.T) {
	runtime := &enhancedChatRuntime{shell: &chatShell{}, knownMessages: map[string]bool{}}
	answer, _ := json.Marshal(v1.MessagePartDelta{MessageID: "assistant", Kind: "text", Delta: "answer started"})
	if err := runtime.handleEvent(v1.Event{Type: v1.EventMessagePartDelta, Data: answer}); err != nil {
		t.Fatal(err)
	}
	late, _ := json.Marshal(v1.MessagePartDelta{MessageID: "assistant", Kind: "reasoning_summary", Delta: "too late"})
	if err := runtime.handleEvent(v1.Event{Type: v1.EventMessagePartDelta, Data: late}); err != nil {
		t.Fatal(err)
	}
	if len(runtime.activity) != 0 || runtime.reasoningText.Len() != 0 {
		t.Fatalf("late reasoning crossed the answer boundary: activity=%#v reasoning=%q", runtime.activity, runtime.reasoningText.String())
	}
}

func TestEnhancedRawReasoningIsNeverRetained(t *testing.T) {
	for _, content := range []string{"", "final answer"} {
		t.Run(fmt.Sprintf("content=%q", content), func(t *testing.T) {
			api := &enhancedQueueAPI{messages: v1.MessageList{Items: []v1.Message{{ID: "assistant", Role: "assistant", Content: content, Status: "complete"}}}}
			var output bytes.Buffer
			renderer := terminal.NewLiveRenderer(&output, terminal.RendererConfig{TTY: true, Columns: 100})
			shell := &chatShell{ctx: context.Background(), api: api, current: v1.Session{ID: "session"}, renderer: renderer}
			runtime := &enhancedChatRuntime{shell: shell, knownMessages: map[string]bool{}}
			runtime.startAssistantActivity("assistant")
			runtime.startReasoningActivity("assistant", "", "private chain of thought", false)
			runtime.completeAssistantActivity("assistant", "success")

			if err := runtime.commitCompletedAssistants("assistant"); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(output.String(), "private chain of thought") || len(runtime.activity) != 0 {
				t.Fatalf("raw reasoning was retained: output=%q activity=%#v", output.String(), runtime.activity)
			}
		})
	}
}

func TestEnhancedKeyPumpDoesNotConsumePastUnacknowledgedKey(t *testing.T) {
	decoder := terminal.NewKeyDecoder(bytes.NewBufferString("\rZ"))
	pump := startEnhancedKeyPump(context.Background(), decoder)
	result := <-pump.events
	if result.err != nil || result.key.Kind != terminal.KeyEnter {
		t.Fatalf("first key = %#v, %v", result.key, result.err)
	}
	pump.stop()
	pump = startEnhancedKeyPump(context.Background(), decoder)
	result = <-pump.events
	if result.err != nil || result.key.Kind != terminal.KeyRune || result.key.Rune != 'Z' {
		t.Fatalf("handoff key = %#v, %v", result.key, result.err)
	}
	close(result.ack)
	pump.stop()
}

func TestEnhancedCycleModeAppliesNextAgentAndUpdatesLabels(t *testing.T) {
	api := &modeSwitchAPI{agents: v1.AgentList{Items: []v1.Agent{{ID: "build"}, {ID: "plan"}, {ID: "explore"}}}}
	shell := &chatShell{
		ctx: context.Background(), api: api,
		current:   v1.Session{ID: "session", Agent: "build", Provider: "local", Model: "test"},
		selection: chatSelection{agent: "build", model: "local/test"},
	}
	shell.config = &Config{
		ModelineModelLabel: func(int) string { return "local/test (1.2k/128k)" },
		NextAgent:          func(string) (string, error) { return "plan", nil },
		ApplyAgent: func(agent string, _ bool) error {
			_, err := api.UpdateSessionSelection(context.Background(), shell.current.ID, v1.UpdateSessionSelectionRequest{Agent: agent})
			shell.current.Agent = agent
			return err
		},
	}
	runtime := &enhancedChatRuntime{shell: shell}
	if got := runtime.inputModeLabel(); got != "mode: build" {
		t.Fatalf("inputModeLabel() = %q", got)
	}
	if got := shell.selection.modelLabel(); got != "local/test" {
		t.Fatalf("modelLabel() = %q", got)
	}
	if got := shell.modelineModelLabel(1200); got != "local/test (1.2k/128k)" {
		t.Fatalf("modelineModelLabel() = %q", got)
	}
	if err := runtime.cycleMode(); err != nil {
		t.Fatal(err)
	}
	if shell.selection.agent != "plan" || shell.current.Agent != "plan" || runtime.status != "mode: plan" {
		t.Fatalf("mode not updated: selection=%#v current=%#v status=%q", shell.selection, shell.current, runtime.status)
	}
	if len(api.updates) != 1 || api.updates[0].Agent != "plan" {
		t.Fatalf("updates = %#v", api.updates)
	}
	if got := (chatSelection{}).modelLabel(); got != "no model" {
		t.Fatalf("empty modelLabel() = %q", got)
	}
}

func TestExecutionHaltKeys(t *testing.T) {
	for _, kind := range []terminal.KeyKind{terminal.KeyEscape, terminal.KeyInterrupt} {
		if !isExecutionHaltKey(terminal.Key{Kind: kind}) {
			t.Fatalf("key %v did not halt execution", kind)
		}
	}
	if isExecutionHaltKey(terminal.Key{Kind: terminal.KeyTab}) {
		t.Fatal("Tab halted execution")
	}
}

func TestEnhancedErrorStopsSpinnerAndLateDeltaIsIgnored(t *testing.T) {
	api := &enhancedQueueAPI{}
	state, err := terminal.NewEditorIO(bytes.NewBuffer(nil), nil).Start("")
	if err != nil {
		t.Fatal(err)
	}
	runtime := &enhancedChatRuntime{
		shell: &chatShell{ctx: context.Background(), api: api, current: v1.Session{ID: "session"}},
		state: state, busy: true, knownMessages: map[string]bool{"finished": true}, events: make(chan enhancedSessionEvent, 1),
	}
	status, _ := json.Marshal(v1.SessionStatus{Kind: "error"})
	if err := runtime.handleEvent(v1.Event{Type: v1.EventSessionStatus, Data: status}); err != nil {
		t.Fatal(err)
	}
	if runtime.busy {
		t.Fatal("runner error left enhanced chat busy")
	}
	delta, _ := json.Marshal(v1.MessagePartDelta{MessageID: "finished", Kind: "text", Delta: "stale"})
	if err := runtime.handleEvent(v1.Event{Type: v1.EventMessagePartDelta, Data: delta}); err != nil {
		t.Fatal(err)
	}
	if runtime.streamed.Len() != 0 {
		t.Fatalf("late finalized delta was rendered: %q", runtime.streamed.String())
	}
}

func TestEnhancedNextAssistantDeltaSettlesPriorRendererStream(t *testing.T) {
	var output bytes.Buffer
	state, err := terminal.NewEditorIO(bytes.NewBuffer(nil), nil).Start("")
	if err != nil {
		t.Fatal(err)
	}
	api := &enhancedQueueAPI{messages: v1.MessageList{Items: []v1.Message{
		{ID: "first", Role: "assistant", Content: "first answer", Status: "complete"},
		{ID: "second", Role: "assistant", Status: "active"},
	}}}
	runtime := &enhancedChatRuntime{
		shell: &chatShell{
			ctx:      context.Background(),
			api:      api,
			current:  v1.Session{ID: "session"},
			renderer: terminal.NewLiveRenderer(&output, terminal.RendererConfig{TTY: true, Columns: 80}),
		},
		state:         state,
		busy:          true,
		knownMessages: map[string]bool{},
	}

	first, _ := json.Marshal(v1.MessagePartDelta{MessageID: "first", Kind: "text", Delta: "first"})
	if err := runtime.handleEvent(v1.Event{Type: v1.EventMessagePartDelta, Data: first}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.render(); err != nil {
		t.Fatal(err)
	}

	// Disposable events use a separate queue from durable lifecycle events. A
	// delta for the next tool-turn assistant can therefore arrive before the
	// prior assistant-complete event, while the renderer still owns its stream.
	second, _ := json.Marshal(v1.MessagePartDelta{MessageID: "second", Kind: "text", Delta: "second"})
	if err := runtime.handleEvent(v1.Event{Type: v1.EventMessagePartDelta, Data: second}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.render(); err != nil {
		t.Fatalf("next assistant raced the prior renderer stream: %v", err)
	}
	if !runtime.knownMessages["first"] || runtime.streamMessageID != "second" || runtime.streamed.String() != "second" {
		t.Fatalf("stream handoff = known %#v, id %q, text %q", runtime.knownMessages, runtime.streamMessageID, runtime.streamed.String())
	}
}

func TestEnhancedConflictingAssistantDeltaDoesNotCloseActiveStream(t *testing.T) {
	state, err := terminal.NewEditorIO(bytes.NewBuffer(nil), nil).Start("")
	if err != nil {
		t.Fatal(err)
	}
	api := &enhancedQueueAPI{messages: v1.MessageList{Items: []v1.Message{
		{ID: "current", Role: "assistant", Status: "active"},
	}}}
	runtime := &enhancedChatRuntime{
		shell: &chatShell{
			ctx:      context.Background(),
			api:      api,
			current:  v1.Session{ID: "session"},
			renderer: terminal.NewLiveRenderer(io.Discard, terminal.RendererConfig{}),
		},
		state:           state,
		busy:            true,
		knownMessages:   map[string]bool{},
		streamMessageID: "current",
	}
	runtime.streamed.WriteString("current answer")

	// Disposable provider deltas and durable assistant lifecycle events use
	// separate queues. A stale delta can therefore arrive after another
	// assistant has taken ownership of the renderer stream.
	stale, _ := json.Marshal(v1.MessagePartDelta{MessageID: "stale", Kind: "text", Delta: "stale answer"})
	if err := runtime.handleEvent(v1.Event{Type: v1.EventMessagePartDelta, Data: stale}); err != nil {
		t.Fatalf("stale assistant delta returned an error: %v", err)
	}
	if runtime.streamMessageID != "current" || runtime.streamed.String() != "current answer" {
		t.Fatalf("stale delta replaced active stream: id=%q text=%q", runtime.streamMessageID, runtime.streamed.String())
	}
}

func TestEnhancedUnsynchronizedAssistantIgnoresSuffixAndCommitsDurableMessage(t *testing.T) {
	api := &enhancedQueueAPI{messages: v1.MessageList{Items: []v1.Message{{
		ID: "assistant", Role: "assistant", Content: "complete durable answer", Status: "complete",
	}}}}
	var output bytes.Buffer
	runtime := &enhancedChatRuntime{
		shell: &chatShell{
			ctx: context.Background(), api: api, current: v1.Session{ID: "session"},
			renderer: terminal.NewLiveRenderer(&output, terminal.RendererConfig{TTY: true}),
		},
		knownMessages:    map[string]bool{},
		unsyncedMessages: map[string]bool{"assistant": true},
	}
	delta, _ := json.Marshal(v1.MessagePartDelta{MessageID: "assistant", Kind: "text", Delta: " answer"})
	if err := runtime.handleEvent(v1.Event{Type: v1.EventMessagePartDelta, Data: delta}); err != nil {
		t.Fatal(err)
	}
	if runtime.streamMessageID != "" || runtime.streamed.Len() != 0 {
		t.Fatalf("unsynchronized suffix was accepted: id=%q text=%q", runtime.streamMessageID, runtime.streamed.String())
	}
	if err := runtime.commitCompletedAssistants("assistant"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "complete durable answer") || runtime.unsyncedMessages["assistant"] {
		t.Fatalf("durable message was not committed: output=%q unsynced=%#v", output.String(), runtime.unsyncedMessages)
	}
}

func TestEnhancedSnapshotRaceMarksNewAssistantUnsynchronized(t *testing.T) {
	before := v1.MessageList{Items: []v1.Message{{ID: "user", Role: "user", Status: "complete"}}}
	after := v1.MessageList{Items: []v1.Message{
		{ID: "user", Role: "user", Status: "complete"},
		{ID: "assistant", Role: "assistant", Status: "complete"},
	}}
	snapshotMessages := make(map[string]bool, len(before.Items))
	for _, item := range before.Items {
		snapshotMessages[item.ID] = true
	}
	runtime := &enhancedChatRuntime{unsyncedMessages: map[string]bool{}}
	runtime.markNewAssistantsUnsynced(after, snapshotMessages)
	if !runtime.unsyncedMessages["assistant"] || runtime.unsyncedMessages["user"] {
		t.Fatalf("snapshot race classification = %#v", runtime.unsyncedMessages)
	}
}

func TestEnhancedDivergentDurableMessageClosesDisplayedStream(t *testing.T) {
	api := &enhancedQueueAPI{messages: v1.MessageList{Items: []v1.Message{{
		ID: "assistant", Role: "assistant", Content: "authoritative answer", Status: "complete",
	}}}}
	var output bytes.Buffer
	renderer := terminal.NewLiveRenderer(&output, terminal.RendererConfig{TTY: true, Columns: 20})
	if err := renderer.Frame(terminal.LiveFrame{Stream: &terminal.StreamMessage{ID: "assistant", Prefix: "● ", Text: "displayed suffix"}}); err != nil {
		t.Fatal(err)
	}
	runtime := &enhancedChatRuntime{
		shell:         &chatShell{ctx: context.Background(), api: api, current: v1.Session{ID: "session"}, renderer: renderer},
		knownMessages: map[string]bool{}, streamMessageID: "assistant",
	}
	runtime.streamed.WriteString("displayed suffix")
	if err := runtime.commitCompletedAssistants("assistant"); err != nil {
		t.Fatalf("divergent durable message terminated enhanced chat: %v", err)
	}
	if runtime.streamMessageID != "" || !runtime.knownMessages["assistant"] || strings.Contains(output.String(), "authoritative answer") {
		t.Fatalf("stream was not recovered: id=%q known=%#v output=%q", runtime.streamMessageID, runtime.knownMessages, output.String())
	}
}

func TestEnhancedCompletedToolKeepsNameAndWaitsForAssistantBoundary(t *testing.T) {
	var output bytes.Buffer
	runtime := &enhancedChatRuntime{
		shell:           &chatShell{renderer: terminal.NewLiveRenderer(&output, terminal.RendererConfig{})},
		streamMessageID: "assistant",
	}
	pending, _ := json.Marshal(map[string]any{"ID": "call_opaque", "Name": "read", "Input": map[string]any{"path": "internal/cli/enhanced_chat.go"}})
	runtime.handleToolActivity(v1.Event{Type: "session.tool.pending", Data: pending})
	running, _ := json.Marshal(map[string]string{"call_id": "call_opaque", "status": "running"})
	runtime.handleToolActivity(v1.Event{Type: "session.tool.running", Data: running})
	success, _ := json.Marshal(map[string]string{"call_id": "call_opaque", "status": "success"})
	runtime.handleToolActivity(v1.Event{Type: "session.tool.success", Data: success})

	if output.Len() != 0 {
		t.Fatalf("completed tool split the active assistant message: %q", output.String())
	}
	if len(runtime.completedTools) != 1 || runtime.completedTools[0].label != "read · internal/cli/enhanced_chat.go" {
		t.Fatalf("queued tools = %#v", runtime.completedTools)
	}
	runtime.streamMessageID = ""
	if err := runtime.flushCompletedTools(); err != nil {
		t.Fatal(err)
	}
	if got := output.String(); !strings.Contains(got, "✓ read · internal/cli/enhanced_chat.go ·") || strings.Contains(got, "call_opaque") {
		t.Fatalf("committed tool activity = %q", got)
	}
}

func TestShellOutputTailStreamsAndCommitsLastThreeLines(t *testing.T) {
	var output bytes.Buffer
	runtime := &enhancedChatRuntime{shell: &chatShell{renderer: terminal.NewLiveRenderer(&output, terminal.RendererConfig{Columns: 80})}}
	pending := json.RawMessage(`{"call_id":"shell_call","name":"shell","input":{"command":"run"}}`)
	runtime.handleToolActivity(v1.Event{Type: "session.tool.pending", Data: pending})
	runtime.handleToolActivity(v1.Event{Type: "session.tool.running", Data: json.RawMessage(`{"call_id":"shell_call"}`)})

	for _, delta := range []string{"one\ntw", "o\nthree\nfour", "\nfive"} {
		data, _ := json.Marshal(v1.ToolOutputDelta{ToolCallID: "shell_call", Delta: delta})
		if err := runtime.handleEvent(v1.Event{Type: v1.EventToolOutputDelta, Data: data}); err != nil {
			t.Fatal(err)
		}
	}
	rows := runtime.activityRows(time.Now(), 80)
	if len(rows) != 1 || !strings.Contains(rows[0], "three\nfour\nfive") || strings.Contains(rows[0], "one") || strings.Contains(rows[0], "two") {
		t.Fatalf("live rows = %#v", rows)
	}

	runtime.handleToolActivity(v1.Event{Type: "session.tool.success", Data: json.RawMessage(`{"call_id":"shell_call","tool_name":"shell","result":"one\ntwo\nthree\nfour\nfive"}`)})
	got := output.String()
	if !strings.Contains(got, "three\nfour\nfive") || strings.Contains(got, "one\ntwo") {
		t.Fatalf("committed shell output = %q", got)
	}
}

func TestEnhancedFailedToolCommitsInputAsIndentedYAML(t *testing.T) {
	var output bytes.Buffer
	runtime := &enhancedChatRuntime{
		shell: &chatShell{renderer: terminal.NewLiveRenderer(&output, terminal.RendererConfig{Columns: 80})},
	}
	pending, _ := json.Marshal(map[string]any{
		"call_id": "shell_call",
		"name":    "shell",
		"input": map[string]any{
			"shell":   "bash",
			"command": "exit 1",
			"options": map[string]any{"cwd": "/tmp", "env": []string{"CI=1", "COLOR=0"}},
		},
	})
	runtime.handleToolActivity(v1.Event{Type: "session.tool.pending", Data: pending})
	failure, _ := json.Marshal(map[string]string{"call_id": "shell_call", "tool_name": "shell", "error": "exit status 1"})
	runtime.handleToolActivity(v1.Event{Type: "session.tool.failure", Data: failure})

	got := output.String()
	want := "request:\n  command: exit 1\n  options:\n    cwd: /tmp\n    env:\n      - CI=1\n      - COLOR=0\n  shell: bash"
	if !strings.Contains(got, "✗ shell · exit 1 · exit status 1") || !strings.Contains(got, want) {
		t.Fatalf("failed tool block = %q, want YAML request containing %q", got, want)
	}
	if strings.Contains(got, `"command":`) || strings.Contains(got, `{"`) {
		t.Fatalf("failed tool request was displayed as JSON instead of YAML: %q", got)
	}
}

func TestEnhancedFailedToolTruncatesRequestAfterTenLines(t *testing.T) {
	var output bytes.Buffer
	runtime := &enhancedChatRuntime{
		shell: &chatShell{renderer: terminal.NewLiveRenderer(&output, terminal.RendererConfig{Columns: 80})},
	}
	arguments := make([]string, 12)
	for i := range arguments {
		arguments[i] = fmt.Sprintf("argument-%02d", i+1)
	}
	pending, _ := json.Marshal(map[string]any{
		"call_id": "shell_call",
		"name":    "shell",
		"input":   map[string]any{"arguments": arguments, "command": "exit 1"},
	})
	runtime.handleToolActivity(v1.Event{Type: "session.tool.pending", Data: pending})
	failure, _ := json.Marshal(map[string]string{"call_id": "shell_call", "tool_name": "shell", "error": "exit status 1"})
	runtime.handleToolActivity(v1.Event{Type: "session.tool.failure", Data: failure})

	got := output.String()
	if !strings.Contains(got, "    - argument-08\n… 5 more lines") {
		t.Fatalf("failed tool block did not contain the truncated request and remaining line count: %q", got)
	}
	if strings.Contains(got, "argument-09") || strings.Contains(got, "argument-12") {
		t.Fatalf("failed tool block included lines after the preview: %q", got)
	}
}

func TestEnhancedFailedToolRetainsMoreThanTwelvePendingRequests(t *testing.T) {
	var output bytes.Buffer
	runtime := &enhancedChatRuntime{
		shell:           &chatShell{renderer: terminal.NewLiveRenderer(&output, terminal.RendererConfig{Columns: 80})},
		streamMessageID: "assistant",
	}
	for i := 1; i <= 13; i++ {
		pending, _ := json.Marshal(map[string]any{
			"call_id": fmt.Sprintf("call-%02d", i), "name": "shell", "input": map[string]any{"command": fmt.Sprintf("command-%02d", i)},
		})
		runtime.handleToolActivity(v1.Event{Type: "session.tool.pending", Data: pending})
	}
	for i := 1; i <= 13; i++ {
		failure, _ := json.Marshal(map[string]string{"call_id": fmt.Sprintf("call-%02d", i), "error": "failed"})
		runtime.handleToolActivity(v1.Event{Type: "session.tool.failure", Data: failure})
	}
	runtime.streamMessageID = ""
	if err := runtime.flushCompletedTools(); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	for _, want := range []string{"request:\n  command: command-01", "request:\n  command: command-13"} {
		if !strings.Contains(got, want) {
			t.Fatalf("pending request was evicted; output = %q, want %q", got, want)
		}
	}
}

func TestEnhancedTodoWriteCommitsAccessibleOrderedChecklist(t *testing.T) {
	var output bytes.Buffer
	runtime := &enhancedChatRuntime{
		shell:           &chatShell{renderer: terminal.NewLiveRenderer(&output, terminal.RendererConfig{Columns: 100})},
		streamMessageID: "assistant",
	}
	pending, _ := json.Marshal(map[string]any{
		"call_id": "todo_call", "name": "todowrite",
		"input": map[string]any{"todos": []any{map[string]any{"content": "old input", "status": "pending", "priority": "low"}}},
	})
	runtime.handleToolActivity(v1.Event{Type: "session.tool.pending", Data: pending})
	result, _ := json.Marshal([]map[string]any{
		{"id": "todo_1", "content": "Plan work", "status": "pending", "priority": "high", "position": 0},
		{"id": "todo_2", "content": "Implement UI", "status": "in_progress", "priority": "medium", "position": 1},
		{"id": "todo_3", "content": "Run tests", "status": "completed", "priority": "low", "position": 2},
		{"id": "todo_4", "content": "Discard old approach", "status": "cancelled", "priority": "low", "position": 3},
	})
	success, _ := json.Marshal(map[string]string{"call_id": "todo_call", "status": "success", "result": string(result)})
	runtime.handleToolActivity(v1.Event{Type: "session.tool.success", Data: success})

	if output.Len() != 0 {
		t.Fatalf("todowrite split the active assistant message: %q", output.String())
	}
	runtime.streamMessageID = ""
	if err := runtime.flushCompletedTools(); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	for _, want := range []string{
		"✓ TODO · 4 items ·",
		"○ high · Plan work",
		"◐ medium · Implement UI",
		"✓ low · Run tests",
		"■ low · Discard old approach",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("todowrite block = %q, want %q", got, want)
		}
	}
	if strings.Contains(got, "todo_1") || strings.Contains(got, "old input") {
		t.Fatalf("todowrite block exposed IDs or ignored authoritative result: %q", got)
	}
	if strings.Contains(got, "todowrite") {
		t.Fatalf("todo block exposed the internal tool name: %q", got)
	}
}

func TestEnhancedPermissionModalPreservesDraft(t *testing.T) {
	api := &enhancedQueueAPI{permissions: v1.PermissionList{Items: []v1.Permission{{ID: "permission", ToolID: "shell"}}}}
	editor := terminal.NewEditorIO(bytes.NewBuffer(nil), nil)
	state, err := editor.Start("keep draft")
	if err != nil {
		t.Fatal(err)
	}
	runtime := &enhancedChatRuntime{
		shell: &chatShell{
			ctx: context.Background(), api: api, current: v1.Session{ID: "session"}, editor: editor,
			renderer: terminal.NewLiveRenderer(io.Discard, terminal.RendererConfig{}),
		},
		state: state, busy: true, knownMessages: map[string]bool{}, events: make(chan enhancedSessionEvent, 1),
	}
	runtime.detectModal()
	if runtime.modal == nil || state.Value() != "keep draft" {
		t.Fatalf("modal=%#v draft=%q", runtime.modal, state.Value())
	}
	if err := runtime.answerModal("once"); err != nil {
		t.Fatal(err)
	}
	if runtime.modal != nil || state.Value() != "keep draft" || len(api.permissionReplies) != 1 || api.permissionReplies[0].Decision != "allow" {
		t.Fatalf("modal=%#v draft=%q replies=%#v", runtime.modal, state.Value(), api.permissionReplies)
	}
}

func TestEnhancedPermissionModalSelectionStopsSpinnerAndReplies(t *testing.T) {
	api := &enhancedQueueAPI{permissions: v1.PermissionList{Items: []v1.Permission{{
		ID: "permission", ToolID: "shell",
		Description:    "Run shell command:\nrm -rf build",
		CanonicalInput: json.RawMessage(`{"shell":"bash","command":"rm -rf build"}`),
		Review:         json.RawMessage(`{"review_secret":"not for the dialog"}`),
	}}}}
	editor := terminal.NewEditorIO(bytes.NewBuffer(nil), nil)
	state, err := editor.Start("draft")
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	// Permission details belong beside the selector and must not be clipped by
	// the deliberately tiny upper live-arena budget.
	renderer := terminal.NewLiveRenderer(&output, terminal.RendererConfig{TTY: true, Columns: 80, MaxRows: 1})
	runtime := &enhancedChatRuntime{
		shell: &chatShell{
			ctx: context.Background(), api: api, current: v1.Session{ID: "session"}, editor: editor,
			renderer: renderer,
		},
		state: state, busy: true, knownMessages: map[string]bool{}, events: make(chan enhancedSessionEvent, 1),
	}
	runtime.detectModal()
	if runtime.modal == nil {
		t.Fatal("permission modal was not shown")
	}
	if output.Len() != 0 {
		t.Fatalf("permission context was committed to scrollback: %q", output.String())
	}
	if err := runtime.render(); err != nil {
		t.Fatal(err)
	}
	frame := output.String()
	if strings.Contains(frame, "⠋ permission decision:") || strings.Contains(frame, "⠋ $") {
		t.Fatalf("spinner rendered during permission modal: %q", frame)
	}
	if !strings.Contains(frame, "Run shell command: rm -rf build") ||
		!strings.Contains(frame, "permission decision:") || !strings.Contains(frame, "yes") ||
		!strings.Contains(frame, "no") || !strings.Contains(frame, "reject with reason") {
		t.Fatalf("permission choices missing from frame: %q", frame)
	}
	if contextIndex, selectorIndex := strings.Index(frame, "Run shell command: rm -rf build"), strings.Index(frame, "permission decision:"); contextIndex < 0 || selectorIndex <= contextIndex {
		t.Fatalf("permission context was not rendered before its selector: %q", frame)
	}
	if strings.Contains(frame, "permission: shell") || strings.Contains(frame, "reason: default policy") || strings.Contains(frame, "request:") || strings.Contains(frame, "resource: process execute /bin/bash") {
		t.Fatalf("permission metadata rendered in frame: %q", frame)
	}
	if strings.Contains(frame, "review_secret") || strings.Contains(frame, "not for the dialog") || strings.Contains(frame, "review:") {
		t.Fatalf("permission review JSON leaked into frame: %q", frame)
	}

	// Move from the initially selected "yes" to "no".
	if done, err := runtime.handlePermissionModalKey(terminal.Key{Kind: terminal.KeyDown}); err != nil || done {
		t.Fatalf("down = done %t err %v", done, err)
	}
	done, err := runtime.handlePermissionModalKey(terminal.Key{Kind: terminal.KeyEnter})
	if err != nil || !done {
		t.Fatalf("enter = done %t err %v", done, err)
	}
	if len(api.permissionReplies) != 1 || api.permissionReplies[0].Decision != "deny" {
		t.Fatalf("reply = %#v", api.permissionReplies)
	}
	if state.Value() != "draft" {
		t.Fatalf("draft changed to %q", state.Value())
	}
}

func TestEnhancedPermissionAnswerAllows(t *testing.T) {
	api := &enhancedQueueAPI{}
	state, err := terminal.NewEditorIO(bytes.NewBuffer(nil), nil).Start("")
	if err != nil {
		t.Fatal(err)
	}
	runtime := &enhancedChatRuntime{
		shell: &chatShell{ctx: context.Background(), api: api, current: v1.Session{ID: "session"}},
		state: state, modal: &enhancedModal{kind: "permission", permission: &v1.Permission{ID: "permission"}},
	}
	if err := runtime.answerModal("yes"); err != nil {
		t.Fatal(err)
	}
	if len(api.permissionReplies) != 1 || api.permissionReplies[0].Decision != "allow" {
		t.Fatalf("reply = %#v", api.permissionReplies)
	}
}

func TestEnhancedQuestionOptionEnterSubmitsSelection(t *testing.T) {
	editor := terminal.NewEditorIO(bytes.NewBuffer(nil), nil)
	state, err := editor.Start("")
	if err != nil {
		t.Fatal(err)
	}
	request := &v1.QuestionRequest{ID: "request", Questions: []v1.Question{{
		ID: "question", Prompt: "Choose", Options: []v1.Option{
			{ID: "one", Label: "First"}, {ID: "two", Label: "Second", Description: "preferred"},
		},
	}}}
	api := &promptReplyAPI{}
	runtime := &enhancedChatRuntime{
		shell: &chatShell{ctx: context.Background(), api: api, current: v1.Session{ID: "session"}},
		modal: &enhancedModal{kind: "question", state: state, question: request},
	}
	runtime.updateQuestionPrompt()
	runtime.showQuestionContext(request.Questions[0])
	if len(runtime.modal.context) != 1 || len(runtime.modal.choices) != 2 {
		t.Fatalf("context=%#v choices=%#v", runtime.modal.context, runtime.modal.choices)
	}
	if runtime.modal.context[0] != "question: Choose" || runtime.modal.prompt != "answer [one/two]: " {
		t.Fatalf("context=%q prompt=%q", runtime.modal.context[0], runtime.modal.prompt)
	}
	if strings.Contains(runtime.modal.prompt, request.Questions[0].Prompt) {
		t.Fatalf("question duplicated in answer prompt %q", runtime.modal.prompt)
	}
	if runtime.modal.choices[1].Description != "Second - preferred" {
		t.Fatalf("choice description = %q", runtime.modal.choices[1].Description)
	}
	if handled, err := runtime.handleQuestionModalKey(terminal.Key{Kind: terminal.KeyDown}); !handled || err != nil {
		t.Fatalf("Down handled=%t err=%v", handled, err)
	}
	if handled, err := runtime.handleQuestionModalKey(terminal.Key{Kind: terminal.KeyEnter}); !handled || err != nil {
		t.Fatalf("Enter handled=%t err=%v", handled, err)
	}
	if runtime.modal != nil {
		t.Fatalf("question modal remained open after selection: %#v", runtime.modal)
	}
	if len(api.questionReplies) != 1 || len(api.questionReplies[0].Answers) != 1 {
		t.Fatalf("question replies = %#v", api.questionReplies)
	}
	answer := api.questionReplies[0].Answers[0]
	if answer.QuestionID != "question" || len(answer.OptionIDs) != 1 || answer.OptionIDs[0] != "two" || answer.Custom != "" {
		t.Fatalf("answer = %#v", answer)
	}
}

func TestEnhancedQuestionCustomAnswerHasSeparateInputRow(t *testing.T) {
	editor := terminal.NewEditorIO(bytes.NewBuffer(nil), nil)
	state, err := editor.Start("")
	if err != nil {
		t.Fatal(err)
	}
	request := &v1.QuestionRequest{ID: "request", Questions: []v1.Question{{
		ID: "question", Prompt: "Choose", Custom: true,
		Options: []v1.Option{{ID: "one", Label: "First"}, {ID: "two", Label: "Second"}},
	}}}
	api := &promptReplyAPI{}
	runtime := &enhancedChatRuntime{
		shell: &chatShell{ctx: context.Background(), api: api, current: v1.Session{ID: "session"}},
		modal: &enhancedModal{kind: "question", state: state, question: request},
	}
	runtime.updateQuestionPrompt()
	if len(runtime.modal.choices) != 3 {
		t.Fatalf("choices = %#v", runtime.modal.choices)
	}
	custom := runtime.modal.choices[2]
	if custom.Value != "Custom input" || custom.Description != "Type another answer" {
		t.Fatalf("custom choice = %#v", custom)
	}
	if handled, err := runtime.handleQuestionModalKey(terminal.Key{Kind: terminal.KeyRune, Rune: 'x'}); !handled || err != nil || state.Value() != "" {
		t.Fatalf("option menu accepted text: handled=%t err=%v value=%q", handled, err, state.Value())
	}
	for range 2 {
		if handled, err := runtime.handleQuestionModalKey(terminal.Key{Kind: terminal.KeyDown}); !handled || err != nil {
			t.Fatalf("Down handled=%t err=%v", handled, err)
		}
	}
	if handled, err := runtime.handleQuestionModalKey(terminal.Key{Kind: terminal.KeyEnter}); !handled || err != nil {
		t.Fatalf("Enter handled=%t err=%v", handled, err)
	}
	if runtime.modal == nil || len(runtime.modal.choices) != 0 || runtime.modal.prompt != "custom answer: " || state.Value() != "" {
		t.Fatalf("custom input modal = %#v, value = %q", runtime.modal, state.Value())
	}
	// Even text equal to an option ID remains a custom answer after explicitly
	// choosing the custom-input row.
	if err := runtime.answerModal("one"); err != nil {
		t.Fatal(err)
	}
	if len(api.questionReplies) != 1 || len(api.questionReplies[0].Answers) != 1 || api.questionReplies[0].Answers[0].Custom != "one" || len(api.questionReplies[0].Answers[0].OptionIDs) != 0 {
		t.Fatalf("question replies = %#v", api.questionReplies)
	}
}

func TestEnhancedMultipleQuestionEnterSubmitsStagedSelections(t *testing.T) {
	editor := terminal.NewEditorIO(bytes.NewBuffer(nil), nil)
	state, err := editor.Start("")
	if err != nil {
		t.Fatal(err)
	}
	request := &v1.QuestionRequest{ID: "request", Questions: []v1.Question{{
		ID: "question", Prompt: "Choose", Multiple: true,
		Options: []v1.Option{{ID: "one", Label: "First"}, {ID: "two", Label: "Second"}, {ID: "three", Label: "Third"}},
	}}}
	api := &promptReplyAPI{}
	runtime := &enhancedChatRuntime{
		shell: &chatShell{ctx: context.Background(), api: api, current: v1.Session{ID: "session"}},
		modal: &enhancedModal{kind: "question", state: state, question: request},
	}
	runtime.updateQuestionPrompt()
	if handled, err := runtime.handleQuestionModalKey(terminal.Key{Kind: terminal.KeyRune, Rune: ' '}); !handled || err != nil {
		t.Fatalf("Space handled=%t err=%v", handled, err)
	}
	if !strings.HasPrefix(runtime.modal.choices[0].Description, "selected · ") {
		t.Fatalf("staged choice description = %q", runtime.modal.choices[0].Description)
	}
	for range 2 {
		if handled, err := runtime.handleQuestionModalKey(terminal.Key{Kind: terminal.KeyDown}); !handled || err != nil {
			t.Fatalf("Down handled=%t err=%v", handled, err)
		}
	}
	if handled, err := runtime.handleQuestionModalKey(terminal.Key{Kind: terminal.KeyEnter}); !handled || err != nil {
		t.Fatalf("Enter handled=%t err=%v", handled, err)
	}
	if runtime.modal != nil || len(api.questionReplies) != 1 || len(api.questionReplies[0].Answers) != 1 {
		t.Fatalf("modal=%#v replies=%#v", runtime.modal, api.questionReplies)
	}
	answer := api.questionReplies[0].Answers[0]
	if len(answer.OptionIDs) != 2 || answer.OptionIDs[0] != "one" || answer.OptionIDs[1] != "three" {
		t.Fatalf("answer = %#v", answer)
	}
}

type enhancedQueueAPI struct {
	apiClient
	prompts           []v1.PromptRequest
	messages          v1.MessageList
	permissions       v1.PermissionList
	permissionReplies []v1.PermissionReply
}

func (a *enhancedQueueAPI) Prompt(_ context.Context, _ string, request v1.PromptRequest) (v1.PromptAccepted, error) {
	a.prompts = append(a.prompts, request)
	return v1.PromptAccepted{InputID: "input", MessageID: "message", Delivery: request.Delivery, Status: "pending", Created: true}, nil
}

func (a *enhancedQueueAPI) Messages(context.Context, string) (v1.MessageList, error) {
	return a.messages, nil
}

func (a *enhancedQueueAPI) Permissions(context.Context, string) (v1.PermissionList, error) {
	return a.permissions, nil
}

func (a *enhancedQueueAPI) Questions(context.Context, string) (v1.QuestionList, error) {
	return v1.QuestionList{}, nil
}

func (a *enhancedQueueAPI) ReplyPermission(_ context.Context, _, _ string, reply v1.PermissionReply) error {
	a.permissionReplies = append(a.permissionReplies, reply)
	return nil
}

type promptReplyAPI struct {
	apiClient
	permissions       v1.PermissionList
	permissionReplies []v1.PermissionReply
	questionReplies   []v1.QuestionReply
}

func (a *promptReplyAPI) Permissions(context.Context, string) (v1.PermissionList, error) {
	return a.permissions, nil
}

func (a *promptReplyAPI) Questions(context.Context, string) (v1.QuestionList, error) {
	return v1.QuestionList{}, nil
}

func (a *promptReplyAPI) ReplyPermission(_ context.Context, _, _ string, reply v1.PermissionReply) error {
	a.permissionReplies = append(a.permissionReplies, reply)
	return nil
}

func (a *promptReplyAPI) ReplyQuestion(_ context.Context, _, _ string, reply v1.QuestionReply) error {
	a.questionReplies = append(a.questionReplies, reply)
	return nil
}

type catalogOnlyAPI struct {
	apiClient
	models v1.ModelList
}

func (a catalogOnlyAPI) Models(context.Context) (v1.ModelList, error) { return a.models, nil }

type modeSwitchAPI struct {
	apiClient
	agents  v1.AgentList
	updates []v1.UpdateSessionSelectionRequest
}

func (a *modeSwitchAPI) Agents(context.Context) (v1.AgentList, error) { return a.agents, nil }

func (a *modeSwitchAPI) UpdateSessionSelection(_ context.Context, _ string, request v1.UpdateSessionSelectionRequest) (v1.SessionSelection, error) {
	a.updates = append(a.updates, request)
	return v1.SessionSelection{Agent: request.Agent}, nil
}

type staticMessageClient struct{ items v1.MessageList }

func (s staticMessageClient) Messages(context.Context, string) (v1.MessageList, error) {
	return s.items, nil
}

func TestEnhancedEditCommitsStatusAndDiffAsBlock(t *testing.T) {
	var output bytes.Buffer
	runtime := &enhancedChatRuntime{shell: &chatShell{renderer: terminal.NewLiveRenderer(&output, terminal.RendererConfig{Columns: 80})}}
	if err := runtime.shell.renderer.Commit("✓ Done: read · README.md"); err != nil {
		t.Fatal(err)
	}
	pending, _ := json.Marshal(map[string]any{"call_id": "edit_call", "name": "edit", "input": map[string]any{"path": "file.go"}})
	runtime.handleToolActivity(v1.Event{Type: "session.tool.pending", Data: pending})
	diff := "--- a/file.go\n+++ b/file.go\n@@ -1,1 +1,1 @@\n-before\n+after\n"
	success, _ := json.Marshal(map[string]string{"call_id": "edit_call", "tool_name": "edit", "status": "success", "result": diff})
	runtime.handleToolActivity(v1.Event{Type: "session.tool.success", Data: success})
	got := output.String()
	if !strings.Contains(got, "README.md\n\n✓ edit · file.go ·") {
		t.Fatalf("edit block was not separated from compact output: %q", got)
	}
	if !strings.Contains(got, "\n--- a/file.go\n+++ b/file.go\n") || !strings.Contains(got, "\n-before\n+after\n") {
		t.Fatalf("edit block omitted its before/after diff: %q", got)
	}
}

func TestEnhancedEditTruncatesDiffAfterTenLines(t *testing.T) {
	var output bytes.Buffer
	runtime := &enhancedChatRuntime{shell: &chatShell{renderer: terminal.NewLiveRenderer(&output, terminal.RendererConfig{Columns: 80})}}
	pending, _ := json.Marshal(map[string]any{"call_id": "edit_call", "name": "edit", "input": map[string]any{"path": "file.go"}})
	runtime.handleToolActivity(v1.Event{Type: "session.tool.pending", Data: pending})
	diffLines := []string{
		"--- a/file.go",
		"+++ b/file.go",
		"@@ -1,6 +1,6 @@",
		"-old 1",
		"+new 1",
		"-old 2",
		"+new 2",
		"-old 3",
		"+new 3",
		"-old 4",
		"+new 4",
		" context",
	}
	success, _ := json.Marshal(map[string]string{
		"call_id": "edit_call", "tool_name": "edit", "status": "success", "result": strings.Join(diffLines, "\n") + "\n",
	})
	runtime.handleToolActivity(v1.Event{Type: "session.tool.success", Data: success})

	got := output.String()
	if !strings.Contains(got, strings.Join(diffLines[:10], "\n")+"\n… 2 more lines") {
		t.Fatalf("edit block did not contain the truncated diff and remaining line count: %q", got)
	}
	if strings.Contains(got, diffLines[10]) || strings.Contains(got, diffLines[11]) {
		t.Fatalf("edit block included lines after the preview: %q", got)
	}
}

func TestSingleLineReasoningSummaryRemovesMarkdownWithoutCorruptingText(t *testing.T) {
	tests := map[string]string{
		"**one** **two**":        "one two",
		"- **one** two":          "one two",
		"*one* and **two**":      "one and two",
		"# heading\n- next item": "heading next item",
		"check `file_name.go`":   "check file_name.go",
	}
	for input, want := range tests {
		if got := singleLineReasoningSummary(input); got != want {
			t.Errorf("singleLineReasoningSummary(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestActivityStatusUsesAccessibleIcons(t *testing.T) {
	tests := []struct {
		status string
		want   string
	}{
		{status: "thinking", want: "⠹ Thought: task · 1.2s"},
		{status: "pending", want: "○ Queued tool: task · 1.2s"},
		{status: "running", want: "⠹ Working: task · 1.2s"},
		{status: "success", want: "✓ task · 1.2s"},
		{status: "failure", want: "✗ task · 1.2s"},
		{status: "interrupted", want: "■ Interrupted: task · 1.2s"},
		{status: "unknown", want: "Status: task · 1.2s"},
	}
	started := time.Unix(100, 0)
	for _, test := range tests {
		item := enhancedActivityItem{label: "task", status: test.status, started: started}
		if got := formatActivity(item, started.Add(1200*time.Millisecond)); got != test.want {
			t.Errorf("formatActivity(%q) = %q, want %q", test.status, got, test.want)
		}
	}
}

func TestTodoWriteBlockFallsBackToInputAndHandlesEmptyList(t *testing.T) {
	input := map[string]any{"todos": []any{
		map[string]any{"content": "Safe\ncontent", "status": "pending", "priority": "high"},
	}}
	block, count, ok := formatTodoWriteBlock("", input)
	if !ok || count != 1 || !strings.Contains(block, "○ high · Safe content") || strings.Contains(block, "\ncontent") {
		t.Fatalf("fallback block = %q, ok = %v", block, ok)
	}
	empty, count, ok := formatTodoWriteBlock("[]", input)
	if !ok || count != 0 || empty != "  No todos" {
		t.Fatalf("empty block = %q, ok = %v", empty, ok)
	}
	if _, _, ok := formatTodoWriteBlock("bad", input); ok {
		t.Fatal("malformed authoritative result fell back to input")
	}
	if _, _, ok := formatTodoWriteBlock("", map[string]any{"todos": "bad"}); ok {
		t.Fatal("malformed input produced a todo block")
	}
	invalid := `[{"content":"bad","status":"done","priority":"urgent"}]`
	if _, _, ok := formatTodoWriteBlock(invalid, nil); ok {
		t.Fatal("invalid todo enums produced a todo block")
	}
}

func TestWritePermissionModalUsesSpecializedChoicesAndReplies(t *testing.T) {
	api := &enhancedQueueAPI{}
	state, err := terminal.NewEditorIO(bytes.NewBuffer(nil), nil).Start("")
	if err != nil {
		t.Fatal(err)
	}
	permissionItem := &v1.Permission{ID: "permission", ToolID: "request_write_permission"}
	runtime := &enhancedChatRuntime{
		shell: &chatShell{ctx: context.Background(), api: api, current: v1.Session{ID: "session"}},
		state: state, modal: &enhancedModal{kind: "permission", state: state, permission: permissionItem, choices: permissionChoicesFor(*permissionItem)},
	}
	if got := runtime.modal.choices; len(got) != 3 || got[0].Value != "grant" || got[1].Value != "reject" || got[2].Value != "reject with reason" {
		t.Fatalf("choices = %#v", got)
	}
	runtime.modal.selected = 2
	done, err := runtime.handlePermissionModalKey(terminal.Key{Kind: terminal.KeyEnter})
	if err != nil || !done || !runtime.modal.customInput || runtime.modal.prompt != "rejection reason: " {
		t.Fatalf("reason selection = done %t, custom %t, prompt %q, err %v", done, runtime.modal.customInput, runtime.modal.prompt, err)
	}
	if err := runtime.answerModal("use the project cache instead"); err != nil {
		t.Fatal(err)
	}
	if len(api.permissionReplies) != 1 || api.permissionReplies[0].Decision != "deny" || api.permissionReplies[0].Reason != "use the project cache instead" {
		t.Fatalf("reason reply = %#v", api.permissionReplies)
	}

	runtime.modal = &enhancedModal{kind: "permission", state: state, permission: permissionItem}
	if err := runtime.answerModal("grant"); err != nil {
		t.Fatal(err)
	}
	if len(api.permissionReplies) != 2 || api.permissionReplies[1].Decision != "allow" {
		t.Fatalf("grant reply = %#v", api.permissionReplies)
	}
}
