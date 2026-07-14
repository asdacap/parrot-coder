package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "github.com/amirulashraf/parrot-coder/internal/api/v1"
	customcommand "github.com/amirulashraf/parrot-coder/internal/command"
	"github.com/amirulashraf/parrot-coder/internal/terminal"
)

func TestHelp(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	code := New(BuildInfo{}).Run(context.Background(), nil, strings.NewReader(""), &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("Run() code = %d, want %d", code, exitOK)
	}
	if !strings.Contains(stdout.String(), "run        execute one prompt") {
		t.Fatalf("help output does not contain run command: %q", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestVersion(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	app := New(BuildInfo{Version: "1.2.3", Commit: "abc123", Date: "2026-07-13"})
	code := app.Run(context.Background(), []string{"version"}, strings.NewReader(""), &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("Run() code = %d, want %d", code, exitOK)
	}
	for _, value := range []string{"1.2.3", "abc123", "2026-07-13"} {
		if !strings.Contains(stdout.String(), value) {
			t.Errorf("version output %q does not contain %q", stdout.String(), value)
		}
	}
}

func TestUnknownCommand(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	code := New(BuildInfo{}).Run(context.Background(), []string{"missing"}, strings.NewReader(""), &stdout, &stderr)
	if code != exitUsage {
		t.Fatalf("Run() code = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr.String(), `unknown command "missing"`) {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestChatHelpListsCustomCommandsAndSubtaskUsesNormalPrompt(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, ".parrot", "commands", "review.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("---\ndescription: Review changes\n---\nReview"), 0o600); err != nil {
		t.Fatal(err)
	}
	commands, err := customcommand.Discover(customcommand.Options{ProjectRoot: root, CWD: root})
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	shell := chatShell{ctx: context.Background(), stdout: &stdout, stderr: &stderr, commands: commands}
	exit, code := shell.slash("/help", "")
	if exit || code != exitOK || !strings.Contains(stdout.String(), "/review\tReview changes") {
		t.Fatalf("help = %q, exit=%t, code=%d", stdout.String(), exit, code)
	}
	prompt := subtaskPrompt(customcommand.Expansion{Prompt: "Inspect this", Agent: "explore", Model: "local/model", Subtask: true})
	if !strings.Contains(prompt, "using the task tool") || !strings.Contains(prompt, `agent "explore"`) || !strings.HasSuffix(prompt, "Inspect this") {
		t.Fatalf("subtask prompt = %q", prompt)
	}
}

func TestRunTextAndJSONLThroughInProcessClient(t *testing.T) {
	provider, cleanup := configureCLIProvider(t, "hello\x1b[2J world")
	defer cleanup()
	_ = provider
	application := New(BuildInfo{Version: "test"})
	for _, test := range []struct {
		name   string
		args   []string
		stdout string
	}{
		{"text", []string{"run", "say hello"}, "hello[2J world\n"},
		{"jsonl", []string{"run", "--format", "jsonl", "say hello"}, "\"type\":\"server.connected\""},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := application.Run(context.Background(), test.args, strings.NewReader(""), &stdout, &stderr)
			if code != exitOK {
				t.Fatalf("code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
			}
			if test.name == "text" {
				if stdout.String() != test.stdout {
					t.Fatalf("stdout = %q", stdout.String())
				}
			} else {
				if !strings.Contains(stdout.String(), test.stdout) || !strings.Contains(stdout.String(), `"type":"session.status"`) {
					t.Fatalf("jsonl stdout = %q", stdout.String())
				}
				for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
					if !strings.HasPrefix(line, "{") || !strings.HasSuffix(line, "}") {
						t.Fatalf("invalid JSONL line %q", line)
					}
				}
			}
			if strings.Contains(stdout.String(), "\x1b") || strings.Contains(stderr.String(), "\x1b") {
				t.Fatalf("terminal escape leaked: stdout=%q stderr=%q", stdout.String(), stderr.String())
			}
		})
	}
}

func TestChatTranscriptAndSlashCommands(t *testing.T) {
	_, cleanup := configureCLIProvider(t, "answer")
	defer cleanup()
	var stdout, stderr bytes.Buffer
	input := strings.NewReader("/help\nhello\n/thinking\n/new\n/exit\n")
	code := New(BuildInfo{}).Run(context.Background(), []string{"chat"}, input, &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("code = %d, stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	for _, value := range []string{"$ hello", "- answer", "---", "/model", "/undo"} {
		if !strings.Contains(stdout.String(), value) {
			t.Errorf("transcript missing %q: %q", value, stdout.String())
		}
	}
	if strings.Contains(stdout.String(), "\x1b") || strings.Contains(stderr.String(), "\x1b") {
		t.Fatalf("escape leaked into transcript")
	}
}

func TestRemoveNoColorPreservesArgumentsAndRecordsPreference(t *testing.T) {
	args, disabled := removeNoColor([]string{"chat", "--no-color", "hello"})
	if !disabled || len(args) != 2 || args[0] != "chat" || args[1] != "hello" {
		t.Fatalf("removeNoColor() = %#v, %t", args, disabled)
	}
	args, disabled = removeNoColor([]string{"run", "--", "--no-color"})
	if disabled || len(args) != 3 || args[2] != "--no-color" {
		t.Fatalf("removeNoColor() after terminator = %#v, %t", args, disabled)
	}
}

func TestModelLessChatHelpThenExit(t *testing.T) {
	configureCLIConfig(t, `{}`)
	var stdout, stderr bytes.Buffer
	code := New(BuildInfo{}).Run(context.Background(), []string{"chat"}, strings.NewReader("/help\n/exit\n"), &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("code = %d, stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "/status\tshow chat state") || strings.Contains(stdout.String(), "\x1b") {
		t.Fatalf("model-less help output = %q", stdout.String())
	}
}

func TestNoModelPromptPickerPreservesDraftAndSlashPickers(t *testing.T) {
	var requestBody string
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		requestBody = string(data)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"answer\"}\n\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
	}))
	defer provider.Close()
	configuration := fmt.Sprintf(`{"providers":{"local":{"type":"compatible","protocol":"responses","base_url":%q,"api_key_env":"PARROT_CLI_TEST_KEY","allow_insecure_localhost":true,"models":{"test":{"tools":true}}}}}`, provider.URL+"/v1")
	configureCLIConfig(t, configuration)
	t.Setenv("PARROT_CLI_TEST_KEY", "secret")

	var stdout, stderr bytes.Buffer
	input := strings.NewReader("preserved draft\nlocal/test\n/agent\nplan\n/sessions\n/session\n1\n/exit\n")
	code := New(BuildInfo{}).Run(context.Background(), []string{"chat"}, input, &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("code = %d, stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	combined := stdout.String() + stderr.String()
	for _, value := range []string{"model> ", "status: model selected local/test", "agent> ", "status: agent selected plan", "session> "} {
		if !strings.Contains(combined, value) {
			t.Errorf("transcript missing %q: %q", value, combined)
		}
	}
	if !strings.Contains(requestBody, "preserved draft") {
		t.Fatalf("provider request lost draft: %s", requestBody)
	}
	if strings.Contains(stdout.String(), "\x1b") || strings.Contains(stderr.String(), "\x1b") {
		t.Fatal("plain fallback emitted ANSI")
	}
}

func TestCreateChatSessionIncludesSelectionAtomically(t *testing.T) {
	creator := &recordingSessionCreator{}
	_, err := createChatSession(context.Background(), creator, "project", "title", chatSelection{agent: "plan", provider: "local", model: "test"})
	if err != nil {
		t.Fatal(err)
	}
	want := (v1.CreateSessionRequest{ProjectID: "project", Title: "title", Agent: "plan", Model: "local/test"})
	if creator.request != want {
		t.Fatalf("request = %#v; want %#v", creator.request, want)
	}
}

type recordingSessionCreator struct{ request v1.CreateSessionRequest }

func (r *recordingSessionCreator) CreateSession(_ context.Context, request v1.CreateSessionRequest) (v1.Session, error) {
	r.request = request
	return v1.Session{ID: "session"}, nil
}

func TestChatCompletionCandidatesIncludeBuiltinsAndCustomCommands(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, ".parrot", "commands", "review.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("---\ndescription: Review changes\n---\nReview"), 0o600); err != nil {
		t.Fatal(err)
	}
	commands, err := customcommand.Discover(customcommand.Options{ProjectRoot: root, CWD: root})
	if err != nil {
		t.Fatal(err)
	}
	items := chatCompletionCandidates(commands)
	seen := make(map[string]string, len(items))
	for _, item := range items {
		seen[item.Value] = item.Description
	}
	for _, name := range []string{"/help", "/models", "/model", "/agents", "/agent", "/sessions", "/session", "/status", "/exit", "/review"} {
		if seen[name] == "" {
			t.Errorf("missing completion %s in %#v", name, items)
		}
	}
}

func TestEnhancedFinishCommitsAssistantFinalOnce(t *testing.T) {
	var output bytes.Buffer
	renderer := terminal.NewLiveRenderer(&output, terminal.RendererConfig{TTY: true, MaxRows: 6})
	if err := renderer.UpdateMessage("- ", "partial"); err != nil {
		t.Fatal(err)
	}
	before := output.Len()
	api := staticMessageClient{items: v1.MessageList{Items: []v1.Message{{ID: "answer", Role: "assistant", Content: "complete answer"}}}}
	result := finishStream(api, "session", v1.MessageList{}, "partial", false, streamOptions{format: "text", chat: true, renderer: renderer})
	if result.err != nil {
		t.Fatal(result.err)
	}
	if strings.Count(output.String(), "complete answer") != 1 {
		t.Fatalf("final assistant response was not committed once: %q", output.String())
	}
	committed := output.String()[before:]
	if !strings.HasPrefix(committed, "\x1b[?25l") || !strings.Contains(committed, "\x1b[2K") || strings.Count(committed, "- complete answer") != 1 {
		t.Fatalf("live response was not cleared before final commit: %q", committed)
	}
}

func TestEnhancedSubmissionCommitsUserMessage(t *testing.T) {
	var output bytes.Buffer
	renderer := terminal.NewLiveRenderer(&output, terminal.RendererConfig{TTY: true, MaxRows: 6})
	if err := renderer.Prompt(terminal.PromptState{Prefix: "$ ", Text: "keep this", Cursor: 9}); err != nil {
		t.Fatal(err)
	}
	before := output.Len()
	shell := &chatShell{renderer: renderer}
	if err := shell.commitUser("keep this"); err != nil {
		t.Fatal(err)
	}
	committed := output.String()[before:]
	if strings.Count(committed, "$ keep this") != 1 || !strings.Contains(committed, "───") {
		t.Fatalf("submitted user message was not committed once: %q", output.String())
	}
}

func TestEnhancedNoModelPickerCancellationRestoresDraft(t *testing.T) {
	input := bytes.NewBufferString("preserved\r\x1b\x03\x04")
	var output bytes.Buffer
	renderer := terminal.NewLiveRenderer(&output, terminal.RendererConfig{TTY: true, MaxRows: 6})
	decoder := terminal.NewKeyDecoder(input)
	shell := &chatShell{
		ctx: context.Background(), api: catalogOnlyAPI{models: v1.ModelList{Items: []v1.Model{{Provider: "local", ID: "test", Name: "Test"}}}},
		selection: chatSelection{agent: "build"}, stdout: &output, stderr: io.Discard,
		decoder: decoder, renderer: renderer, enhanced: true,
	}
	shell.editor = terminal.NewEditorDecoder(decoder, &output, terminal.WithEditorRenderer(renderer))
	if code := shell.run(""); code != exitOK {
		t.Fatalf("code = %d, output=%q", code, output.String())
	}
	if !strings.Contains(output.String(), "$ preserved") {
		t.Fatalf("draft was not restored after picker cancellation: %q", output.String())
	}
}

func TestEnhancedPermissionEscapeDeniesAndInterruptPropagates(t *testing.T) {
	for _, test := range []struct {
		name          string
		input         string
		wantInterrupt bool
		wantReplies   int
	}{
		{name: "escape denies", input: "\x1b", wantReplies: 1},
		{name: "control c interrupts", input: "\x03", wantInterrupt: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			api := &promptReplyAPI{permissions: v1.PermissionList{Items: []v1.Permission{{ID: "permission", ToolID: "shell", Reason: "test"}}}}
			decoder := terminal.NewKeyDecoder(bytes.NewBufferString(test.input))
			var output bytes.Buffer
			renderer := terminal.NewLiveRenderer(&output, terminal.RendererConfig{TTY: true})
			err := settleStreamPrompts(context.Background(), api, "session", streamOptions{stdout: &output, stderr: io.Discard, renderer: renderer, keyInput: decoder})
			if test.wantInterrupt != errors.Is(err, terminal.ErrInterrupted) {
				t.Fatalf("error = %v", err)
			}
			if len(api.permissionReplies) != test.wantReplies {
				t.Fatalf("replies = %#v", api.permissionReplies)
			}
			if test.wantReplies == 1 && api.permissionReplies[0].Decision != "deny" {
				t.Fatalf("reply = %#v", api.permissionReplies[0])
			}
		})
	}
}

func TestEnhancedBusySubmissionQueuesAndPromotionCommits(t *testing.T) {
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
		selection: chatSelection{agent: "build", provider: "local", model: "test"}, renderer: renderer, stdout: &output,
	}
	runtime := &enhancedChatRuntime{shell: shell, state: state, busy: true, knownMessages: map[string]bool{}, events: make(chan enhancedSessionEvent, 1)}
	if err := runtime.submitPrompt("next task"); err != nil {
		t.Fatal(err)
	}
	if len(api.prompts) != 1 || api.prompts[0].Delivery != "queue" || len(runtime.pending) != 1 {
		t.Fatalf("prompts=%#v pending=%#v", api.prompts, runtime.pending)
	}
	if err := runtime.render(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "next task") || !strings.Contains(output.String(), "⠋ $ ") {
		t.Fatalf("queue frame = %q", output.String())
	}
	payload, _ := json.Marshal(v1.SessionInputPromoted{InputID: "input", MessageID: "message"})
	if err := runtime.handleEvent(v1.Event{Type: v1.EventSessionInputPromoted, Data: payload}); err != nil {
		t.Fatal(err)
	}
	if len(runtime.pending) != 0 || !strings.Contains(output.String(), "$ next task") {
		t.Fatalf("promoted queue pending=%#v output=%q", runtime.pending, output.String())
	}
}

func TestEnhancedBusySlashRunsSafeAndRejectsMutation(t *testing.T) {
	api := &enhancedQueueAPI{}
	var output bytes.Buffer
	renderer := terminal.NewLiveRenderer(&output, terminal.RendererConfig{TTY: true, Columns: 50})
	shell := &chatShell{
		ctx: context.Background(), api: api, current: v1.Session{ID: "session", Agent: "build", Provider: "local", Model: "test"},
		selection: chatSelection{agent: "build", provider: "local", model: "test"}, renderer: renderer, stdout: &output,
		projectRoot: "/project",
	}
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

func TestEnhancedIdleWaitsForQueuedPromotionBeforeFinalAssistant(t *testing.T) {
	api := &enhancedQueueAPI{messages: v1.MessageList{Items: []v1.Message{
		{ID: "first", Role: "assistant", Content: "first answer", Status: "complete"},
		{ID: "second", Role: "assistant", Content: "second answer", Status: "complete"},
	}}}
	var output bytes.Buffer
	renderer := terminal.NewLiveRenderer(&output, terminal.RendererConfig{TTY: true, Columns: 50})
	shell := &chatShell{ctx: context.Background(), api: api, current: v1.Session{ID: "session"}, renderer: renderer, stdout: &output}
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
		selection: chatSelection{agent: "build", provider: "local", model: "test"},
	}
	runtime := &enhancedChatRuntime{shell: shell}
	if got := runtime.inputModeLabel(); got != "mode: build" {
		t.Fatalf("inputModeLabel() = %q", got)
	}
	if got := shell.selection.modelLabel(); got != "local/test" {
		t.Fatalf("modelLabel() = %q", got)
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

func TestEnhancedCompletedToolKeepsNameAndWaitsForAssistantBoundary(t *testing.T) {
	var output bytes.Buffer
	runtime := &enhancedChatRuntime{
		shell:           &chatShell{renderer: terminal.NewLiveRenderer(&output, terminal.RendererConfig{})},
		streamMessageID: "assistant",
	}
	pending, _ := json.Marshal(map[string]any{"ID": "call_opaque", "Name": "read", "Input": map[string]any{}})
	runtime.handleToolActivity(v1.Event{Type: "session.tool.pending", Data: pending})
	running, _ := json.Marshal(map[string]string{"call_id": "call_opaque", "status": "running"})
	runtime.handleToolActivity(v1.Event{Type: "session.tool.running", Data: running})
	success, _ := json.Marshal(map[string]string{"call_id": "call_opaque", "status": "success"})
	runtime.handleToolActivity(v1.Event{Type: "session.tool.success", Data: success})

	if output.Len() != 0 {
		t.Fatalf("completed tool split the active assistant message: %q", output.String())
	}
	if len(runtime.completedTools) != 1 || runtime.completedTools[0].label != "read" {
		t.Fatalf("queued tools = %#v", runtime.completedTools)
	}
	runtime.streamMessageID = ""
	if err := runtime.flushCompletedTools(); err != nil {
		t.Fatal(err)
	}
	if got := output.String(); !strings.Contains(got, "Done: read ·") || strings.Contains(got, "call_opaque") {
		t.Fatalf("committed tool activity = %q", got)
	}
}

func TestEnhancedPermissionModalPreservesDraft(t *testing.T) {
	api := &enhancedQueueAPI{permissions: v1.PermissionList{Items: []v1.Permission{{ID: "permission", ToolID: "shell", Reason: "test"}}}}
	editor := terminal.NewEditorIO(bytes.NewBuffer(nil), nil)
	state, err := editor.Start("keep draft")
	if err != nil {
		t.Fatal(err)
	}
	runtime := &enhancedChatRuntime{
		shell: &chatShell{
			ctx: context.Background(), api: api, current: v1.Session{ID: "session"}, editor: editor, stdout: io.Discard,
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

func TestEnhancedPermissionModalSelectionStopsSpinnerAndRepliesScope(t *testing.T) {
	api := &enhancedQueueAPI{permissions: v1.PermissionList{Items: []v1.Permission{{
		ID: "permission", ToolID: "shell", Reason: "default policy",
		Resources: []v1.PermissionResource{{Kind: "process", Operation: "execute", Identifier: "/bin/bash"}},
	}}}}
	editor := terminal.NewEditorIO(bytes.NewBuffer(nil), nil)
	state, err := editor.Start("draft")
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	renderer := terminal.NewLiveRenderer(&output, terminal.RendererConfig{TTY: true, Columns: 80, MaxRows: 12})
	runtime := &enhancedChatRuntime{
		shell: &chatShell{
			ctx: context.Background(), api: api, current: v1.Session{ID: "session"}, editor: editor, stdout: &output,
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
	if !strings.Contains(frame, "permission: shell") || !strings.Contains(frame, "reason: default policy") ||
		!strings.Contains(frame, "resource: process execute /bin/bash") ||
		!strings.Contains(frame, "permission decision:") || !strings.Contains(frame, "allow all for workspace") {
		t.Fatalf("permission choices missing from frame: %q", frame)
	}

	for i := 0; i < 3; i++ {
		done, err := runtime.handlePermissionModalKey(terminal.Key{Kind: terminal.KeyDown})
		if err != nil || done {
			t.Fatalf("down = done %t err %v", done, err)
		}
	}
	done, err := runtime.handlePermissionModalKey(terminal.Key{Kind: terminal.KeyEnter})
	if err != nil || !done {
		t.Fatalf("enter = done %t err %v", done, err)
	}
	if len(api.permissionReplies) != 1 || api.permissionReplies[0].Decision != "allow" || api.permissionReplies[0].Scope != "workspace" {
		t.Fatalf("reply = %#v", api.permissionReplies)
	}
	if state.Value() != "draft" {
		t.Fatalf("draft changed to %q", state.Value())
	}
}

func TestEnhancedPermissionAnswerAllowsProcessScope(t *testing.T) {
	api := &enhancedQueueAPI{}
	state, err := terminal.NewEditorIO(bytes.NewBuffer(nil), nil).Start("")
	if err != nil {
		t.Fatal(err)
	}
	runtime := &enhancedChatRuntime{
		shell: &chatShell{ctx: context.Background(), api: api, current: v1.Session{ID: "session"}},
		state: state, modal: &enhancedModal{kind: "permission", permission: &v1.Permission{ID: "permission"}},
	}
	if err := runtime.answerModal("allow all for process"); err != nil {
		t.Fatal(err)
	}
	if len(api.permissionReplies) != 1 || api.permissionReplies[0].Decision != "allow" || api.permissionReplies[0].Scope != "process" {
		t.Fatalf("reply = %#v", api.permissionReplies)
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

func TestProviderFailureReturnsToChatPrompt(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "provider unavailable", http.StatusServiceUnavailable)
	}))
	defer provider.Close()
	configureCLIEnvironment(t, provider.URL)
	var stdout, stderr bytes.Buffer
	code := New(BuildInfo{}).Run(context.Background(), []string{"chat"}, strings.NewReader("hello\n/help\n/exit\n"), &stdout, &stderr)
	if code != exitOK || !strings.Contains(stdout.String(), "/status") {
		t.Fatalf("chat did not recover: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestServeRejectsNonLoopback(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := New(BuildInfo{}).Run(context.Background(), []string{"serve", "--host", "0.0.0.0"}, strings.NewReader(""), &stdout, &stderr)
	if code != exitUsage || !strings.Contains(stderr.String(), "non-loopback") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestAuthStoreOutputDoesNotContainSecret(t *testing.T) {
	_, cleanup := configureCLIProvider(t, "unused")
	defer cleanup()
	secret := "super-secret-api-key"
	application := New(BuildInfo{})
	var loginOut, loginErr bytes.Buffer
	code := application.Run(context.Background(), []string{"auth", "login", "local", "--api-key-stdin"}, strings.NewReader(secret), &loginOut, &loginErr)
	if code != exitOK {
		t.Fatalf("login code=%d stdout=%q stderr=%q", code, loginOut.String(), loginErr.String())
	}
	var listOut, listErr bytes.Buffer
	code = application.Run(context.Background(), []string{"auth", "list"}, strings.NewReader(""), &listOut, &listErr)
	if code != exitOK {
		t.Fatalf("list code=%d stdout=%q stderr=%q", code, listOut.String(), listErr.String())
	}
	combined := loginOut.String() + loginErr.String() + listOut.String() + listErr.String()
	if strings.Contains(combined, secret) || !strings.Contains(listOut.String(), "local\tapi_key") {
		t.Fatalf("unsafe or incomplete auth output %q", combined)
	}
}

func TestModelsDoesNotRequireDefaultModel(t *testing.T) {
	root := t.TempDir()
	for name, path := range map[string]string{
		"HOME": root, "XDG_CONFIG_HOME": filepath.Join(root, "config"),
		"XDG_DATA_HOME": filepath.Join(root, "data"), "XDG_STATE_HOME": filepath.Join(root, "state"),
		"XDG_CACHE_HOME": filepath.Join(root, "cache"),
	} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
		t.Setenv(name, path)
	}
	var stdout, stderr bytes.Buffer
	code := New(BuildInfo{}).Run(context.Background(), []string{"models"}, strings.NewReader(""), &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "chatgpt/") {
		t.Fatalf("models output = %q", stdout.String())
	}
}

func TestRunCancellationInterruptsActiveSession(t *testing.T) {
	started := make(chan struct{})
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	}))
	defer provider.Close()
	configureCLIEnvironment(t, provider.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan int, 1)
	go func() {
		var stdout, stderr bytes.Buffer
		result <- New(BuildInfo{}).Run(ctx, []string{"run", "wait"}, strings.NewReader(""), &stdout, &stderr)
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("provider request did not start")
	}
	cancel()
	select {
	case code := <-result:
		if code != exitInterrupt {
			t.Fatalf("code = %d, want %d", code, exitInterrupt)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not stop after cancellation")
	}
}

func configureCLIProvider(t *testing.T, text string) (*httptest.Server, func()) {
	t.Helper()
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		encoded, _ := json.Marshal(text)
		fmt.Fprintf(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":%s}\n\n", encoded)
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
	}))
	configureCLIEnvironment(t, provider.URL)
	return provider, provider.Close
}

func configureCLIEnvironment(t *testing.T, providerURL string) {
	t.Helper()
	configuration := fmt.Sprintf(`{"model":"local/test","providers":{"local":{"type":"compatible","protocol":"responses","base_url":%q,"api_key_env":"PARROT_CLI_TEST_KEY","allow_insecure_localhost":true,"models":{"test":{"tools":true}}}}}`, providerURL+"/v1")
	configureCLIConfig(t, configuration)
	t.Setenv("PARROT_CLI_TEST_KEY", "secret")
}

func configureCLIConfig(t *testing.T, configuration string) {
	t.Helper()
	root := t.TempDir()
	configHome := filepath.Join(root, "config")
	for _, item := range []string{configHome, filepath.Join(root, "data"), filepath.Join(root, "state"), filepath.Join(root, "cache")} {
		if err := os.MkdirAll(item, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(configHome, "parrot"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configHome, "parrot", "parrot.jsonc"), []byte(configuration), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "cache"))
}
