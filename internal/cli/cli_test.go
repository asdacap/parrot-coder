package cli

import (
	"bytes"
	"context"
	"encoding/json"
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
	var api apiClient
	var current v1.Session
	var stdout, stderr bytes.Buffer
	exit, code := chatSlash(context.Background(), &api, &current, "/help", &stdout, &stderr, &codingFlags{}, commands)
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
	for _, value := range []string{"you> ", "assistant> answer", "/model", "/undo"} {
		if !strings.Contains(stdout.String(), value) {
			t.Errorf("transcript missing %q: %q", value, stdout.String())
		}
	}
	if strings.Contains(stdout.String(), "\x1b") || strings.Contains(stderr.String(), "\x1b") {
		t.Fatalf("escape leaked into transcript")
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
	configuration := fmt.Sprintf(`{"model":"local/test","providers":{"local":{"type":"compatible","protocol":"responses","base_url":%q,"api_key_env":"PARROT_CLI_TEST_KEY","allow_insecure_localhost":true,"models":{"test":{"tools":true}}}}}`, providerURL+"/v1")
	if err := os.WriteFile(filepath.Join(configHome, "parrot", "parrot.jsonc"), []byte(configuration), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "cache"))
	t.Setenv("PARROT_CLI_TEST_KEY", "secret")
}
