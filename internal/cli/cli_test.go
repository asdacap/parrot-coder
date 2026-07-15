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
	"regexp"
	"strings"
	"testing"
	"time"

	v1 "github.com/amirulashraf/parrot-coder/internal/api/v1"
	application "github.com/amirulashraf/parrot-coder/internal/app"
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

func TestRunResultAlwaysExplainsControlledExit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		args   []string
		code   int
		reason string
	}{
		{name: "help", args: []string{"help"}, code: exitOK, reason: "help_displayed"},
		{name: "version", args: []string{"version"}, code: exitOK, reason: "version_displayed"},
		{name: "unknown", args: []string{"missing"}, code: exitUsage, reason: "unknown_command"},
		{name: "missing prompt", args: []string{"run"}, code: exitUsage, reason: "prompt_required"},
		{name: "invalid flag", args: []string{"run", "--missing"}, code: exitUsage, reason: "invalid_arguments"},
		{name: "invalid models arguments", args: []string{"models", "extra"}, code: exitUsage, reason: "invalid_models_arguments"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			result := New(BuildInfo{}).RunResult(context.Background(), test.args, strings.NewReader(""), &stdout, &stderr)
			if result.Code != test.code || result.Reason != test.reason {
				t.Fatalf("RunResult() = %#v, want code=%d reason=%q", result, test.code, test.reason)
			}
		})
	}
}

func TestRunResultClassifiesAppOpenFailureWithoutLoggingErrorText(t *testing.T) {
	const privateText = "database is locked secret-path"
	applicationCLI := New(BuildInfo{})
	applicationCLI.open = func(context.Context, application.Options) (*application.App, error) {
		return nil, errors.New(privateText)
	}
	var stdout, stderr bytes.Buffer
	result := applicationCLI.RunResult(context.Background(), []string{"run", "hello"}, strings.NewReader(""), &stdout, &stderr)
	if result.Code != exitError || result.Reason != "app_open_database_busy" || result.ErrorType != "*errors.errorString" {
		t.Fatalf("RunResult() = %#v", result)
	}
	if strings.Contains(result.Reason, privateText) || strings.Contains(result.ErrorType, privateText) {
		t.Fatalf("structured result leaked private error text: %#v", result)
	}
}

func TestEncodeOutputRecordsWriteFailureReason(t *testing.T) {
	state := &exitState{}
	ctx := context.WithValue(context.Background(), exitStateKey{}, state)
	var stderr bytes.Buffer
	code := encodeOutput(ctx, failingWriter{}, &stderr, map[string]string{"ok": "yes"})
	if code != exitError || state.reason != "output_write_failed" || state.errorType != "*errors.errorString" {
		t.Fatalf("encodeOutput() code=%d state=%#v", code, state)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

func TestEnhancedRenderFailureIsPrintedAndClassified(t *testing.T) {
	state := &exitState{}
	ctx := context.WithValue(context.Background(), exitStateKey{}, state)
	var stderr bytes.Buffer
	shell := &chatShell{
		ctx:      ctx,
		stderr:   &stderr,
		editor:   terminal.NewEditorIO(strings.NewReader(""), io.Discard, terminal.WithEditorRenderer(nil)),
		renderer: terminal.NewLiveRenderer(failingWriter{}, terminal.RendererConfig{TTY: true}),
	}
	code := shell.runEnhanced("")
	if code != exitError || state.reason != "enhanced_render_failed" || state.errorType != "*errors.errorString" {
		t.Fatalf("runEnhanced() code=%d state=%#v", code, state)
	}
	if !strings.Contains(stderr.String(), "enhanced chat render failed") || !strings.Contains(stderr.String(), "write failed") {
		t.Fatalf("stderr did not explain enhanced render failure: %q", stderr.String())
	}
}

func TestEnhancedRendererCleanupIsSkippedOnError(t *testing.T) {
	for _, code := range []int{exitError, exitUsage} {
		t.Run(fmt.Sprintf("error_%d", code), func(t *testing.T) {
			var output bytes.Buffer
			renderer := terminal.NewLiveRenderer(&output, terminal.RendererConfig{TTY: true})
			if err := renderer.Update([]string{"diagnostic context"}); err != nil {
				t.Fatal(err)
			}
			output.Reset()

			if code == exitError {
				shell := &chatShell{ctx: context.Background(), stderr: io.Discard, renderer: renderer}
				code = shell.enhancedRenderError(errors.New("render failure"))
			}
			cleanupEnhancedRenderer(renderer, code)
			if output.Len() != 0 {
				t.Fatalf("error cleanup wrote %q", output.String())
			}
			if err := renderer.Update([]string{"renderer remains open"}); err != nil {
				t.Fatalf("renderer was closed after an error: %v", err)
			}
		})
	}

	for _, code := range []int{exitOK, exitInterrupt} {
		t.Run(fmt.Sprintf("exit_%d", code), func(t *testing.T) {
			var output bytes.Buffer
			renderer := terminal.NewLiveRenderer(&output, terminal.RendererConfig{TTY: true})
			if err := renderer.Update([]string{"temporary frame"}); err != nil {
				t.Fatal(err)
			}
			output.Reset()

			cleanupEnhancedRenderer(renderer, code)
			if output.Len() == 0 {
				t.Fatal("ordinary cleanup did not clear the live frame")
			}
			if err := renderer.Update([]string{"unexpected"}); err == nil {
				t.Fatal("renderer remained open after ordinary cleanup")
			}
		})
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
	for _, value := range []string{"$ hello", "- answer", "───", "/model", "/undo"} {
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
	for _, value := range []string{"model> ", "✓ Model selected: local/test", "mode> ", "✓ Mode selected: plan", "session> "} {
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

func TestDefaultChatSelectionPreservesRestoredEffort(t *testing.T) {
	defaults := v1.SessionSelection{Agent: "build", Provider: "chatgpt", Model: "gpt", Variant: "high"}
	if got := defaultChatSelection(defaults, ""); got.variant != "high" {
		t.Fatalf("restored selection = %#v, want high effort", got)
	}
	if got := defaultChatSelection(defaults, "low"); got.variant != "low" {
		t.Fatalf("overridden selection = %#v, want low effort", got)
	}
}

func TestEffortSlashCommandUpdatesActiveSession(t *testing.T) {
	api := &effortSwitchAPI{models: v1.ModelList{Items: []v1.Model{{
		Provider: "chatgpt", ID: "gpt", Variants: map[string]v1.ModelVariant{
			"low": {ReasoningEffort: "low"}, "high": {ReasoningEffort: "high"},
		},
	}}}}
	var stdout, stderr bytes.Buffer
	shell := chatShell{
		ctx: context.Background(), api: api, current: v1.Session{ID: "session", Provider: "chatgpt", Model: "gpt"},
		selection: chatSelection{provider: "chatgpt", model: "gpt"}, stdout: &stdout, stderr: &stderr,
	}
	exit, code := shell.slash("/effort", "high")
	if exit || code != exitOK {
		t.Fatalf("slash exit=%t code=%d", exit, code)
	}
	if shell.selection.variant != "high" || shell.current.Variant != "high" {
		t.Fatalf("selection = %#v, session variant = %q", shell.selection, shell.current.Variant)
	}
	if len(api.updates) != 1 || api.updates[0].Variant == nil || *api.updates[0].Variant != "high" {
		t.Fatalf("updates = %#v", api.updates)
	}
	if !strings.Contains(stderr.String(), "✓ Model effort selected: high") {
		t.Fatalf("stderr = %q", stderr.String())
	}

	shell.slash("/effort", "missing")
	if !strings.Contains(stderr.String(), `unknown effort "missing"`) || len(api.updates) != 1 {
		t.Fatalf("invalid effort: stderr=%q updates=%#v", stderr.String(), api.updates)
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
	for _, name := range []string{"/help", "/version", "/run", "/chat", "/models", "/model", "/modes", "/mode", "/agents", "/agent", "/sessions", "/session", "/auth", "/serve", "/status", "/exit", "/review"} {
		if seen[name] == "" {
			t.Errorf("missing completion %s in %#v", name, items)
		}
	}
	for _, item := range builtinChatCommands {
		if !isBuiltinSlash(item.Value) {
			t.Errorf("advertised command %s is not dispatched as a builtin", item.Value)
		}
	}
}

func TestSlashVersionAndAgentModeActionsMatchCLIConcepts(t *testing.T) {
	api := &agentModeAPI{
		agents: v1.AgentList{Items: []v1.Agent{{ID: "explore", MaxTurns: 12}}},
		modes:  v1.ModeList{Items: []v1.Mode{{ID: "build", MaxTurns: 64}}},
	}
	var stdout bytes.Buffer
	shell := &chatShell{ctx: context.Background(), api: api, stdout: &stdout, stderr: io.Discard, build: BuildInfo{Version: "1.2.3", Commit: "abc", Date: "today"}}
	shell.slash("/version", "")
	shell.slash("/agents", "")
	shell.slash("/modes", "")
	output := stdout.String()
	for _, want := range []string{"parrot 1.2.3", "commit: abc", "explore", "build"} {
		if !strings.Contains(output, want) {
			t.Fatalf("slash output missing %q: %q", want, output)
		}
	}
}

type agentModeAPI struct {
	apiClient
	agents v1.AgentList
	modes  v1.ModeList
}

func (a *agentModeAPI) Agents(context.Context) (v1.AgentList, error) { return a.agents, nil }
func (a *agentModeAPI) Modes(context.Context) (v1.ModeList, error)   { return a.modes, nil }

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
	plain := regexp.MustCompile("\\x1b(?:\\[\\?25[lh]|\\[2K|\\[[0-9]+[AB])").ReplaceAllString(committed, "")
	if !strings.Contains(plain, "───\n$ keep this") {
		t.Fatalf("thin rule is not immediately before user message: %q", plain)
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
			api := &promptReplyAPI{permissions: v1.PermissionList{Items: []v1.Permission{{
				ID: "permission", ToolID: "shell", Reason: "test", Description: "Run shell command:\nprintf ok",
				CanonicalInput: json.RawMessage(`{"command":"CANONICAL_SECRET"}`), Review: json.RawMessage(`{"secret":"REVIEW_SECRET"}`),
			}}}}
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
			if got := output.String(); strings.Contains(got, "CANONICAL_SECRET") || strings.Contains(got, "REVIEW_SECRET") || strings.Contains(got, "review:") {
				t.Fatalf("authorization JSON leaked into picker output: %q", got)
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
	if got := formatReasoningActivity(runtime.activity[0], runtime.activity[0].started, 100); !strings.Contains(got, "⠋ Thought: Checking the implementation · 123 tokens · 0.0s") {
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

func TestEnhancedTaskProgressUpdatesToolActivity(t *testing.T) {
	runtime := &enhancedChatRuntime{knownMessages: map[string]bool{}}
	runtime.upsertActivity("call-task", "task · explore", "running", false, false, false)
	data, _ := json.Marshal(v1.TaskProgress{TaskID: "task-1", ToolCallID: "call-task", Agent: "explore", Status: "running", Usage: v1.Usage{TotalTokens: 35}, ToolUses: 3})
	if err := runtime.handleEvent(v1.Event{Type: v1.EventTaskProgress, Data: data}); err != nil {
		t.Fatal(err)
	}
	if len(runtime.activity) != 1 {
		t.Fatalf("activity = %#v", runtime.activity)
	}
	line := formatActivity(runtime.activity[0], runtime.activity[0].started)
	if !strings.Contains(line, "35 tokens · 3 tools") {
		t.Fatalf("line = %q", line)
	}
}

func TestFormatTokenCountByHundreds(t *testing.T) {
	tests := []struct {
		tokens int
		want   string
	}{
		{tokens: 999, want: "999"},
		{tokens: 1000, want: "1.0k"},
		{tokens: 300100, want: "300.1k"},
		{tokens: 377845, want: "377.8k"},
		{tokens: 690776, want: "690.8k"},
	}
	for _, test := range tests {
		if got := formatTokenCount(test.tokens); got != test.want {
			t.Errorf("formatTokenCount(%d) = %q, want %q", test.tokens, got, test.want)
		}
	}
}

func TestEnhancedTaskProgressFormatsLargeTokenCount(t *testing.T) {
	item := enhancedActivityItem{label: "task · explore", status: "success", started: time.Unix(100, 0), ended: time.Unix(101, 0), tokens: 300100, hasUsage: true, toolUses: 44}
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

	want := "Investigating summary stripping issue Analyzing reasoning summary handling flaws"
	if got := runtime.activity[0].label; got != want {
		t.Fatalf("activity label = %q, want %q", got, want)
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

	if len(runtime.activity) != 2 || runtime.activity[0].label != "First item" || runtime.activity[1].label != "Second item" {
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
	if got := output.String(); !strings.Contains(got, "✓ Final first item") {
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
	shell := &chatShell{ctx: context.Background(), api: api, current: v1.Session{ID: "session"}, renderer: renderer, stdout: &output}
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
			shell := &chatShell{ctx: context.Background(), api: api, current: v1.Session{ID: "session"}, renderer: renderer, stdout: &output}
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
			flushed := strings.Contains(output.String(), "✓ Checking · 12 tokens")
			if flushed != test.wantFlushed {
				t.Fatalf("flushed activity = %t, want %t; output=%q", flushed, test.wantFlushed, output.String())
			}
		})
	}
}

func TestEnhancedReasoningSummaryIsPlainSingleLineAndRetainedBeforeAnswer(t *testing.T) {
	api := &enhancedQueueAPI{messages: v1.MessageList{Items: []v1.Message{{ID: "assistant", Role: "assistant", Content: "final answer", Status: "complete"}}}}
	var output bytes.Buffer
	renderer := terminal.NewLiveRenderer(&output, terminal.RendererConfig{TTY: true, Columns: 100})
	shell := &chatShell{ctx: context.Background(), api: api, current: v1.Session{ID: "session"}, renderer: renderer, stdout: &output}
	runtime := &enhancedChatRuntime{shell: shell, knownMessages: map[string]bool{}}
	runtime.startAssistantActivity("assistant")
	runtime.startReasoningActivity("assistant", "", "- **Verifying\ncomplete suite**", true)
	runtime.updateAssistantUsage("assistant", &v1.Usage{OutputTokens: 30, ReasoningTokens: 12})
	runtime.completeAssistantActivity("assistant", "success")

	if err := runtime.commitCompletedAssistants("assistant"); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	summary := strings.Index(got, "✓ Verifying complete suite · 12 tokens")
	answer := strings.Index(got, "final answer")
	if summary < 0 || answer < summary || strings.Contains(got, "**") {
		t.Fatalf("output = %q", got)
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

func TestEnhancedRawReasoningIsNeverRetained(t *testing.T) {
	for _, content := range []string{"", "final answer"} {
		t.Run(fmt.Sprintf("content=%q", content), func(t *testing.T) {
			api := &enhancedQueueAPI{messages: v1.MessageList{Items: []v1.Message{{ID: "assistant", Role: "assistant", Content: content, Status: "complete"}}}}
			var output bytes.Buffer
			renderer := terminal.NewLiveRenderer(&output, terminal.RendererConfig{TTY: true, Columns: 100})
			shell := &chatShell{ctx: context.Background(), api: api, current: v1.Session{ID: "session"}, renderer: renderer, stdout: &output}
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

func TestStreamToolStatusUsesAccessibleIcons(t *testing.T) {
	tests := map[string]string{
		"pending":     "○ Queued tool",
		"running":     "◌ Working: tool",
		"success":     "✓ tool",
		"failure":     "✗ tool",
		"interrupted": "■ Interrupted: tool",
		"custom":      "Status: tool custom",
	}
	for status, want := range tests {
		if got := streamToolStatus(status, ""); got != want {
			t.Errorf("streamToolStatus(%q) = %q, want %q", status, got, want)
		}
	}
	if got := streamToolStatus("failure", "permission denied"); got != "✗ tool: permission denied" {
		t.Errorf("failure detail = %q", got)
	}
}

func TestToolActivityStyleMutesReadOnlyRetrievalTools(t *testing.T) {
	tests := []struct {
		name string
		want terminal.TextStyle
	}{
		{name: "read", want: terminal.TextStyleMuted},
		{name: "grep", want: terminal.TextStyleMuted},
		{name: "glob", want: terminal.TextStyleMuted},
		{name: "web_fetch", want: terminal.TextStyleMuted},
		{name: "shell", want: terminal.TextStyleDefault},
		{name: "todowrite", want: terminal.TextStyleDefault},
	}
	for _, test := range tests {
		if got := toolActivityStyle(test.name); got != test.want {
			t.Errorf("toolActivityStyle(%q) = %v, want %v", test.name, got, test.want)
		}
	}
}

func TestStreamToolTrackerCommitsEditAndFailureBlocks(t *testing.T) {
	var output bytes.Buffer
	options := streamOptions{stderr: &output}
	var tracker streamToolTracker

	pendingEdit := json.RawMessage(`{"call_id":"edit_call","name":"edit","input":{"path":"file.go"}}`)
	if err := writeStreamToolEvent(options, &tracker, v1.Event{Type: "session.tool.pending", Data: pendingEdit}); err != nil {
		t.Fatal(err)
	}
	diff := strings.Join([]string{"--- a/file.go", "+++ b/file.go", "@@ -1 +1 @@", "-old", "+new"}, "\n")
	editSuccess, _ := json.Marshal(map[string]string{"call_id": "edit_call", "result": diff})
	if err := writeStreamToolEvent(options, &tracker, v1.Event{Type: "session.tool.success", Data: editSuccess}); err != nil {
		t.Fatal(err)
	}

	pendingShell := json.RawMessage(`{"call_id":"shell_call","name":"shell","input":{"command":"exit 1","limit":9007199254740993}}`)
	if err := writeStreamToolEvent(options, &tracker, v1.Event{Type: "session.tool.pending", Data: pendingShell}); err != nil {
		t.Fatal(err)
	}
	failure := json.RawMessage(`{"call_id":"shell_call","error":"exit status 1"}`)
	if err := writeStreamToolEvent(options, &tracker, v1.Event{Type: "session.tool.failure", Data: failure}); err != nil {
		t.Fatal(err)
	}

	got := output.String()
	for _, want := range []string{
		"✓ tool\n--- a/file.go\n+++ b/file.go\n@@ -1 +1 @@\n-old\n+new",
		"✗ tool: exit status 1\nrequest:\n  command: exit 1\n  limit: 9007199254740993",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("plain tool output = %q, want %q", got, want)
		}
	}
	if len(tracker.calls) != 0 {
		t.Fatalf("terminal tool calls remained tracked: %#v", tracker.calls)
	}
}

func TestStreamToolTrackerTruncatesBlocks(t *testing.T) {
	var tracker streamToolTracker
	tracker.describe(v1.Event{Type: "session.tool.pending", Data: json.RawMessage(`{"call_id":"edit_call","name":"edit","input":{"path":"file.go"}}`)})
	lines := make([]string, 12)
	for i := range lines {
		lines[i] = fmt.Sprintf("line-%02d", i+1)
	}
	success, _ := json.Marshal(map[string]string{"call_id": "edit_call", "result": strings.Join(lines, "\r\n") + "\r\n"})
	_, block, terminalEvent := tracker.describe(v1.Event{Type: "session.tool.success", Data: success})
	if !terminalEvent || !strings.Contains(block, "line-10\n… 2 more lines") {
		t.Fatalf("truncated block = %q, terminal = %t", block, terminalEvent)
	}
	if strings.Contains(block, "line-11") || strings.Contains(block, "\r") {
		t.Fatalf("truncated block retained omitted or CRLF content: %q", block)
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
		models:    []v1.Model{{Provider: "local", ID: "test", ContextWindow: 128000}},
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

func TestToolActivityLabelDescribesInputs(t *testing.T) {
	tests := []struct {
		name  string
		input map[string]any
		want  string
	}{
		{name: "read", input: map[string]any{"path": "README.md"}, want: "read · README.md"},
		{name: "glob", input: map[string]any{"pattern": "**/*.go"}, want: `glob · "**/*.go"`},
		{name: "grep", input: map[string]any{"pattern": "handleToolActivity", "path": "internal/cli"}, want: `grep · "handleToolActivity" · internal/cli`},
		{name: "grep", input: map[string]any{"pattern": "TODO"}, want: `grep · "TODO" · .`},
		{name: "skill", input: map[string]any{"name": "review"}, want: "skill · review"},
		{name: "web_fetch", input: map[string]any{"url": "https://example.com/docs"}, want: "web_fetch · https://example.com/docs"},
		{name: "todowrite", input: map[string]any{"todos": []any{map[string]any{}, map[string]any{}}}, want: "TODO · 2 items"},
		{name: "todo_write", input: map[string]any{"todos": []any{}}, want: "TODO · 0 items"},
		{name: "todowrite", input: map[string]any{"todos": []any{map[string]any{}}}, want: "TODO · 1 item"},
		{name: "custom", input: map[string]any{"token": "hidden", "path": "src/main.go"}, want: "custom · path=src/main.go"},
	}
	for _, test := range tests {
		if got := toolActivityLabel(test.name, test.input); got != test.want {
			t.Errorf("toolActivityLabel(%q) = %q, want %q", test.name, got, test.want)
		}
	}
}

func TestToolActivityLabelSanitizesAndTruncatesDetails(t *testing.T) {
	input := map[string]any{"path": "src\n\x1b[31m" + strings.Repeat("x", 100)}
	got := toolActivityLabel("read", input)
	if strings.ContainsAny(got, "\n\x1b") || !strings.HasSuffix(got, "...") {
		t.Fatalf("unsafe or unbounded activity label: %q", got)
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

func TestPermissionContextUsesOnlySingleLineToolDescription(t *testing.T) {
	lines := permissionContextLines(v1.Permission{
		ToolID:         "shell",
		Reason:         "default policy",
		Description:    "Run shell command:\r\nprintf 'a  b'\nWorking directory: /tmp",
		CanonicalInput: json.RawMessage(`{"shell":"bash","command":"do not render this canonical JSON","env":{"SECRET":"hidden"}}`),
		Review:         json.RawMessage(`{"review_secret":"do not render this review JSON","diff":"--- a/file.go\n+++ b/file.go\n-old\n+new\n"}`),
		Resources:      []v1.PermissionResource{{Kind: "process", Operation: "execute", Identifier: "/bin/bash"}},
	})
	want := []string{"Run shell command: printf 'a  b' Working directory: /tmp"}
	if got := strings.Join(lines, "\n"); got != strings.Join(want, "\n") {
		t.Fatalf("permission context:\n%s\nwant:\n%s", got, strings.Join(want, "\n"))
	}
	if got := strings.Join(lines, "\n"); strings.Contains(got, "permission:") || strings.Contains(got, "reason:") || strings.Contains(got, "resource:") || strings.Contains(got, "canonical JSON") || strings.Contains(got, "SECRET") || strings.Contains(got, "review JSON") || strings.Contains(got, "review:") {
		t.Fatalf("authorization JSON leaked into permission context: %s", got)
	}
}

func TestPermissionContextOmitsEmptyDescription(t *testing.T) {
	if got := permissionContextLines(v1.Permission{ToolID: "shell", Reason: "default policy", Resources: []v1.PermissionResource{{Kind: "process", Operation: "execute", Identifier: "/bin/bash"}}}); len(got) != 0 {
		t.Fatalf("permission context = %#v, want none", got)
	}
}

func TestPermissionContextIgnoresMalformedReview(t *testing.T) {
	got := strings.Join(permissionContextLines(v1.Permission{
		ToolID:      "edit",
		Reason:      "default policy",
		Description: "Edit workspace file \"main.go\"",
		Review:      json.RawMessage(`REVIEW_SECRET: [unterminated`),
	}), "\n")
	if strings.Contains(got, "REVIEW_SECRET") || strings.Contains(got, "review:") {
		t.Fatalf("malformed review leaked into permission context: %s", got)
	}
}

func TestEnhancedPermissionModalSelectionStopsSpinnerAndRepliesScope(t *testing.T) {
	api := &enhancedQueueAPI{permissions: v1.PermissionList{Items: []v1.Permission{{
		ID: "permission", ToolID: "shell", Reason: "default policy",
		Description:    "Run shell command:\nrm -rf build",
		CanonicalInput: json.RawMessage(`{"shell":"bash","command":"rm -rf build"}`),
		Review:         json.RawMessage(`{"review_secret":"not for the dialog"}`),
		Resources:      []v1.PermissionResource{{Kind: "process", Operation: "execute", Identifier: "/bin/bash"}},
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
	if !strings.Contains(frame, "Run shell command: rm -rf build") ||
		!strings.Contains(frame, "permission decision:") || !strings.Contains(frame, "allow all for workspace") ||
		!strings.Contains(frame, "enable yolo") {
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

func TestPermissionReplyEnableYolo(t *testing.T) {
	reply := permissionReplyFromAnswer("enable yolo")
	if reply.Decision != "allow" || reply.Scope != "yolo" {
		t.Fatalf("reply = %#v", reply)
	}
}

func TestSettlePromptsStopsReplyingAfterEnableYolo(t *testing.T) {
	api := &promptReplyAPI{permissions: v1.PermissionList{Items: []v1.Permission{
		{ID: "first", ToolID: "shell", Reason: "test"},
		{ID: "second", ToolID: "edit", Reason: "test"},
	}}}
	if err := settlePrompts(context.Background(), api, "session", strings.NewReader("enable yolo\n"), io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(api.permissionReplies) != 1 {
		t.Fatalf("replies = %#v, want only the YOLO reply", api.permissionReplies)
	}
	if reply := api.permissionReplies[0]; reply.Decision != "allow" || reply.Scope != "yolo" {
		t.Fatalf("reply = %#v", reply)
	}
}

func TestSettlePromptsDoesNotRenderAuthorizationJSON(t *testing.T) {
	api := &promptReplyAPI{permissions: v1.PermissionList{Items: []v1.Permission{{
		ID: "permission", ToolID: "shell", Reason: "test", Description: "Run shell command:\nprintf ok",
		CanonicalInput: json.RawMessage(`{"command":"CANONICAL_SECRET"}`), Review: json.RawMessage(`{"secret":"REVIEW_SECRET"}`),
	}}}}
	var output bytes.Buffer
	if err := settlePrompts(context.Background(), api, "session", strings.NewReader("no\n"), &output); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	if !strings.Contains(got, "Run shell command: printf ok") {
		t.Fatalf("tool description missing from permission prompt: %q", got)
	}
	if strings.Contains(got, "CANONICAL_SECRET") || strings.Contains(got, "REVIEW_SECRET") || strings.Contains(got, "review:") {
		t.Fatalf("authorization JSON leaked into line prompt: %q", got)
	}
}

func TestSettleStreamPromptsStopsReplyingAfterEnableYolo(t *testing.T) {
	api := &promptReplyAPI{permissions: v1.PermissionList{Items: []v1.Permission{
		{ID: "first", ToolID: "shell", Reason: "test"},
		{ID: "second", ToolID: "edit", Reason: "test"},
	}}}
	// Move from the initially selected "yes" choice to "enable yolo".
	decoder := terminal.NewKeyDecoder(strings.NewReader("\t\t\t\t\t\r"))
	var output bytes.Buffer
	renderer := terminal.NewLiveRenderer(&output, terminal.RendererConfig{TTY: true})
	err := settleStreamPrompts(context.Background(), api, "session", streamOptions{
		stdout: &output, stderr: io.Discard, renderer: renderer, keyInput: decoder,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(api.permissionReplies) != 1 {
		t.Fatalf("replies = %#v, want only the YOLO reply", api.permissionReplies)
	}
	if reply := api.permissionReplies[0]; reply.Decision != "allow" || reply.Scope != "yolo" {
		t.Fatalf("reply = %#v", reply)
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

type effortSwitchAPI struct {
	apiClient
	models  v1.ModelList
	updates []v1.UpdateSessionSelectionRequest
}

func (a *effortSwitchAPI) Models(context.Context) (v1.ModelList, error) { return a.models, nil }

func (a *effortSwitchAPI) UpdateSessionSelection(_ context.Context, _ string, request v1.UpdateSessionSelectionRequest) (v1.SessionSelection, error) {
	a.updates = append(a.updates, request)
	return v1.SessionSelection{Variant: *request.Variant}, nil
}

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

func TestFormatSubscriptionUsageShowsRemainingAndReset(t *testing.T) {
	now := time.Date(2030, time.January, 1, 12, 0, 0, 0, time.UTC)
	usage := v1.SubscriptionUsage{
		PlanType:        "plus",
		PrimaryWindow:   &v1.UsageWindow{UsedPercent: 27.5, RemainingPercent: 72.5, ResetAt: now.Add(2*time.Hour + 15*time.Minute)},
		SecondaryWindow: &v1.UsageWindow{UsedPercent: 4, RemainingPercent: 96, ResetAt: now.Add(48 * time.Hour)},
	}
	output := formatSubscriptionUsage(usage, now)
	for _, want := range []string{"ChatGPT subscription (plus)", "primary: 72.5% remaining", "in 2h 15m", "secondary: 96.0% remaining", "in 2d 0h"} {
		if !strings.Contains(output, want) {
			t.Fatalf("output %q does not contain %q", output, want)
		}
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
	result := make(chan ExitResult, 1)
	go func() {
		var stdout, stderr bytes.Buffer
		result <- New(BuildInfo{}).RunResult(ctx, []string{"run", "wait"}, strings.NewReader(""), &stdout, &stderr)
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("provider request did not start")
	}
	cancel()
	select {
	case got := <-result:
		if got.Code != exitInterrupt || got.Reason != "turn_interrupted" || got.ErrorType != "context_canceled" {
			t.Fatalf("result = %#v", got)
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
