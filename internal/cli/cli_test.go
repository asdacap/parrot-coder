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
	application "github.com/amirulashraf/parrot-coder/internal/app"
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

func TestRunTextAndJSONLThroughInProcessClient(t *testing.T) {
	provider, cleanup := configureCLIProvider(t, "hello\x1b[2J world")
	defer cleanup()
	_ = provider
	agentsPath := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "parrot", "AGENTS.md")
	if err := os.WriteFile(agentsPath, []byte("Use project conventions."), 0o600); err != nil {
		t.Fatal(err)
	}
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
			if want := "✓ Loaded AGENTS.md from " + agentsPath; !strings.Contains(stderr.String(), want) {
				t.Fatalf("startup output %q does not contain %q", stderr.String(), want)
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
	for _, value := range []string{"$ hello", "● answer", "───", "/model"} {
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

func TestChatDoesNotReportAgentsFilesBeforeSessionInitialization(t *testing.T) {
	configHome := configureCLIConfig(t, `{}`)
	path := filepath.Join(configHome, "parrot", "AGENTS.md")
	if err := os.WriteFile(path, []byte("Use project conventions."), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := New(BuildInfo{}).Run(context.Background(), []string{"chat"}, strings.NewReader("/exit\n"), &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if strings.Contains(stderr.String(), path) || strings.Contains(stderr.String(), "No AGENTS.md files loaded") {
		t.Fatalf("startup output reported AGENTS.md before session initialization: %q", stderr.String())
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

func configureCLIConfig(t *testing.T, configuration string) string {
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
	return configHome
}
