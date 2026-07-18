package app

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
	"sync/atomic"
	"testing"
	"time"

	v1 "github.com/amirulashraf/parrot-coder/internal/api/v1"
	"github.com/amirulashraf/parrot-coder/internal/appdirs"
	"github.com/amirulashraf/parrot-coder/internal/client"
	"github.com/amirulashraf/parrot-coder/internal/config"
	"github.com/amirulashraf/parrot-coder/internal/event"
	"github.com/amirulashraf/parrot-coder/internal/session"
	"github.com/amirulashraf/parrot-coder/internal/subagent"
	"github.com/amirulashraf/parrot-coder/internal/tool"
)

type appDrainerFunc func(context.Context, string) error

func (f appDrainerFunc) Drain(ctx context.Context, sessionID string) error {
	return f(ctx, sessionID)
}

func TestStatusDrainerPublishesOnlyLifecycleCompletion(t *testing.T) {
	live := event.NewBroker()
	events, unsubscribe := live.Subscribe("session", 2)
	defer unsubscribe()
	drainer := statusDrainer{
		runner: appDrainerFunc(func(context.Context, string) error { return nil }),
		live:   live,
	}

	if err := drainer.Drain(context.Background(), "session"); err != nil {
		t.Fatal(err)
	}
	select {
	case item := <-events:
		t.Fatalf("Drain published completion event: %#v", item)
	default:
	}

	drainer.LifecycleComplete("session", nil)
	select {
	case item := <-events:
		payload, err := v1.DecodeEventData(item)
		if err != nil {
			t.Fatal(err)
		}
		if status := payload.(*v1.SessionStatus); status.Kind != "idle" {
			t.Fatalf("completion status = %#v", status)
		}
	case <-time.After(time.Second):
		t.Fatal("lifecycle completion event was not published")
	}
}

func TestStatusDrainerPublishesLifecycleError(t *testing.T) {
	live := event.NewBroker()
	events, unsubscribe := live.Subscribe("session", 1)
	defer unsubscribe()
	drainer := statusDrainer{runner: appDrainerFunc(func(context.Context, string) error { return nil }), live: live}
	drainer.LifecycleComplete("session", errors.New("failed"))

	item := <-events
	payload, err := v1.DecodeEventData(item)
	if err != nil {
		t.Fatal(err)
	}
	status := payload.(*v1.SessionStatus)
	if status.Kind != "error" || status.ErrorCode != "runner_error" {
		t.Fatalf("completion status = %#v", status)
	}
}

func TestCompositionEndToEndInProcess(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Errorf("provider path = %q", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-secret" {
			t.Errorf("provider authorization = %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello from provider\"}\n\n"+
			"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
	}))
	defer provider.Close()

	root := t.TempDir()
	configHome := filepath.Join(root, "config")
	dataHome := filepath.Join(root, "data")
	stateHome := filepath.Join(root, "state")
	cacheHome := filepath.Join(root, "cache")
	if err := os.MkdirAll(filepath.Join(configHome, "parrot"), 0o700); err != nil {
		t.Fatal(err)
	}
	configuration := fmt.Sprintf(`{
  "model": "local/test-model",
  "providers": {
    "local": {
      "type": "compatible",
      "protocol": "responses",
      "base_url": %q,
      "api_key_env": "PARROT_TEST_KEY",
      "allow_insecure_localhost": true,
      "models": {"test-model": {"name": "Test Model", "tools": true}}
    }
  }
}`, provider.URL+"/v1")
	if err := os.WriteFile(filepath.Join(configHome, "parrot", "parrot.jsonc"), []byte(configuration), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PARROT_TEST_KEY", "test-secret")

	runtime, err := Open(context.Background(), Options{
		CWD:     root,
		Paths:   appdirs.Overrides{Home: root, ConfigHome: configHome, DataHome: dataHome, StateHome: stateHome, CacheHome: cacheHome},
		Version: "test", NonInteractive: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	created, err := runtime.Client.CreateSession(context.Background(), v1.CreateSessionRequest{ProjectID: runtime.Project.ID, Title: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if created.Provider != "local" || created.Model != "test-model" || created.Agent != "build" {
		t.Fatalf("created selection = %#v", created)
	}
	after := int64(^uint64(0) >> 1)
	events, err := runtime.Client.Events(context.Background(), created.ID, &after)
	if err != nil {
		t.Fatal(err)
	}
	defer events.Close()
	connected, err := events.Next()
	if err != nil || connected.Type != v1.EventServerConnected {
		t.Fatalf("connected = %#v, %v", connected, err)
	}
	if _, err := runtime.Client.Prompt(context.Background(), created.ID, v1.PromptRequest{MessageID: "msg_test", Content: "hello", Delivery: "steer"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(5 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for idle")
		default:
		}
		item, err := events.Next()
		if err != nil {
			t.Fatal(err)
		}
		if item.Type == v1.EventSessionStatus {
			payload, err := v1.DecodeEventData(item)
			if err != nil {
				t.Fatal(err)
			}
			if payload.(*v1.SessionStatus).Kind == "idle" {
				break
			}
		}
	}
	messages, err := runtime.Client.Messages(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := messages.Items[len(messages.Items)-1]; got.Role != "assistant" || got.Content != "hello from provider" || got.Status != "complete" {
		t.Fatalf("last message = %#v", got)
	}
}

func TestOpenDiscoversSkillsCommandsAndNeedsNoOptionalConfig(t *testing.T) {
	root := t.TempDir()
	configHome := filepath.Join(root, "config")
	paths := appdirs.Overrides{Home: root, ConfigHome: configHome, DataHome: filepath.Join(root, "data"), StateHome: filepath.Join(root, "state"), CacheHome: filepath.Join(root, "cache")}
	if err := os.MkdirAll(filepath.Join(root, ".parrot", "skills", "review"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".parrot", "skills", "review", "SKILL.md"), []byte("---\nname: review\ndescription: Review code\n---\nExact instructions\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".parrot", "commands"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".parrot", "commands", "check.md"), []byte("---\ndescription: Check code\n---\nCheck $ARGUMENTS"), 0o600); err != nil {
		t.Fatal(err)
	}
	runtime, err := Open(context.Background(), Options{CWD: root, Paths: paths, AllowNoModel: true, NonInteractive: true})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	if got := runtime.Skills.List(); len(got) != 1 || got[0].Name != "review" {
		t.Fatalf("skills = %#v", got)
	}
	expansion, err := runtime.Commands.Expand("check", "this")
	if err != nil || expansion.Prompt != "Check this" {
		t.Fatalf("command expansion = %#v, %v", expansion, err)
	}
}

func TestOpenModelLessCatalogsAndExplicitSessionSelection(t *testing.T) {
	root := t.TempDir()
	paths := appdirs.Overrides{
		Home: root, ConfigHome: filepath.Join(root, "config"), DataHome: filepath.Join(root, "data"),
		StateHome: filepath.Join(root, "state"), CacheHome: filepath.Join(root, "cache"),
	}
	strict, err := Open(context.Background(), Options{CWD: root, Paths: paths, NonInteractive: true})
	if strict != nil {
		_ = strict.Close()
	}
	if err == nil {
		t.Fatal("strict Open accepted a missing default model")
	}
	runtime, err := Open(context.Background(), Options{CWD: root, Paths: paths, AllowNoModel: true, NonInteractive: true})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	if runtime.Handler == nil || runtime.Client == nil || runtime.Commands == nil {
		t.Fatal("model-less app did not expose its application surfaces")
	}
	if runtime.DefaultSelection.Agent != "build" || runtime.DefaultSelection.Provider != "" || runtime.DefaultSelection.Model != "" {
		t.Fatalf("default selection = %#v", runtime.DefaultSelection)
	}
	models, err := runtime.Client.Models(context.Background())
	if err != nil || len(models.Items) == 0 {
		t.Fatalf("Models = %#v, %v", models, err)
	}
	agents, err := runtime.Client.Agents(context.Background())
	if err != nil || len(agents.Items) == 0 {
		t.Fatalf("Agents = %#v, %v", agents, err)
	}

	_, err = runtime.Client.CreateSession(context.Background(), v1.CreateSessionRequest{Title: "missing"})
	assertAppProblem(t, err, "model_required")
	listed, err := runtime.Client.Sessions(context.Background())
	if err != nil || len(listed.Items) != 0 {
		t.Fatalf("sessions after missing selection = %#v, %v", listed, err)
	}

	model := models.Items[0]
	created, err := runtime.Client.CreateSession(context.Background(), v1.CreateSessionRequest{
		Title: "selected", Agent: agents.Items[0].ID, Model: model.Provider + "/" + model.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.Agent != agents.Items[0].ID || created.Provider != model.Provider || created.Model != model.ID {
		t.Fatalf("created selection = %#v", created)
	}
	_, err = runtime.Client.CreateSession(context.Background(), v1.CreateSessionRequest{Agent: "build", Model: model.Provider + "/missing"})
	assertAppProblem(t, err, "invalid_selection")
	listed, err = runtime.Client.Sessions(context.Background())
	if err != nil || len(listed.Items) != 1 {
		t.Fatalf("sessions after invalid selection = %#v, %v", listed, err)
	}

	legacy, err := runtime.Backend.Sessions.Create(context.Background(), session.CreateParams{Title: "legacy"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = runtime.Client.Prompt(context.Background(), legacy.ID, v1.PromptRequest{MessageID: "msg_legacy", Content: "hello", Delivery: "steer"})
	assertAppProblem(t, err, "model_required")
}

func TestOpenRestoresLatestProjectModelSelection(t *testing.T) {
	root := t.TempDir()
	paths := appdirs.Overrides{
		Home: root, ConfigHome: filepath.Join(root, "config"), DataHome: filepath.Join(root, "data"),
		StateHome: filepath.Join(root, "state"), CacheHome: filepath.Join(root, "cache"),
	}
	runtime, err := Open(context.Background(), Options{CWD: root, Paths: paths, AllowNoModel: true, NonInteractive: true})
	if err != nil {
		t.Fatal(err)
	}
	models, err := runtime.Client.Models(context.Background())
	if err != nil || len(models.Items) == 0 {
		t.Fatalf("Models = %#v, %v", models, err)
	}
	model := models.Items[0]
	variant := "high"
	if _, err := runtime.Client.CreateSession(context.Background(), v1.CreateSessionRequest{
		ProjectID: runtime.Project.ID, Title: "remember", Agent: "build", Model: model.Provider + "/" + model.ID, Variant: &variant,
	}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(context.Background(), Options{CWD: root, Paths: paths, AllowNoModel: true, NonInteractive: true})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.DefaultSelection.Provider != model.Provider || reopened.DefaultSelection.Model != model.ID || reopened.DefaultSelection.Variant != variant {
		t.Fatalf("restored selection = %#v, want %s/%s with %s effort", reopened.DefaultSelection, model.Provider, model.ID, variant)
	}
}

func assertAppProblem(t *testing.T, err error, code string) {
	t.Helper()
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) || apiErr.Problem.Code != code {
		t.Fatalf("error = %T %v, want problem %q", err, err, code)
	}
}

func TestOpenStartsEnabledMCPAndDiscoversBeforeCompletion(t *testing.T) {
	var lists atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		if request.Method == "notifications/initialized" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result any
		switch request.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "app-fixture", "version": "1"}}
		case "tools/list":
			lists.Add(1)
			result = map[string]any{"tools": []any{map[string]any{"name": "echo", "description": "Echo", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"value": map[string]any{"type": "string"}}}}}}
		default:
			t.Errorf("unexpected MCP method %q", request.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
	}))
	defer server.Close()
	root := t.TempDir()
	configHome := filepath.Join(root, "config")
	configDir := filepath.Join(configHome, "parrot")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	configuration := fmt.Sprintf(`{"mcp":{"fixture":{"transport":"http","url":%q,"enabled":true,"allow_insecure_localhost":true}}}`, server.URL)
	if err := os.WriteFile(filepath.Join(configDir, "parrot.jsonc"), []byte(configuration), 0o600); err != nil {
		t.Fatal(err)
	}
	paths := appdirs.Overrides{Home: root, ConfigHome: configHome, DataHome: filepath.Join(root, "data"), StateHome: filepath.Join(root, "state"), CacheHome: filepath.Join(root, "cache")}
	runtime, err := Open(context.Background(), Options{CWD: root, Paths: paths, AllowNoModel: true, NonInteractive: true})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	if lists.Load() == 0 {
		t.Fatal("MCP tools were not discovered during Open")
	}
}

func TestOpenDoesNotStartDisabledMCPAndNamesStartupFailure(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, "failed", http.StatusInternalServerError)
	}))
	defer server.Close()
	open := func(enabled bool) error {
		root := t.TempDir()
		configHome := filepath.Join(root, "config")
		configDir := filepath.Join(configHome, "parrot")
		if err := os.MkdirAll(configDir, 0o700); err != nil {
			return err
		}
		configuration := fmt.Sprintf(`{"mcp":{"broken":{"transport":"http","url":%q,"enabled":%t,"allow_insecure_localhost":true}}}`, server.URL, enabled)
		if err := os.WriteFile(filepath.Join(configDir, "parrot.jsonc"), []byte(configuration), 0o600); err != nil {
			return err
		}
		paths := appdirs.Overrides{Home: root, ConfigHome: configHome, DataHome: filepath.Join(root, "data"), StateHome: filepath.Join(root, "state"), CacheHome: filepath.Join(root, "cache")}
		runtime, err := Open(context.Background(), Options{CWD: root, Paths: paths, AllowNoModel: true, NonInteractive: true})
		if runtime != nil {
			_ = runtime.Close()
		}
		return err
	}
	if err := open(false); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Fatalf("disabled MCP server received %d calls", calls.Load())
	}
	err := open(true)
	if err == nil || !strings.Contains(err.Error(), `MCP server "broken" startup`) {
		t.Fatalf("startup error = %v", err)
	}
}

func TestConfigExecutableErrorNamesEntry(t *testing.T) {
	_, _, err := buildLSPConfigs(map[string]config.LSP{"go": {Command: "gopls"}}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), `LSP server "go" command must be an absolute executable path`) {
		t.Fatalf("error = %v", err)
	}
}

func TestProjectConfigCannotIntroduceExternalCapabilities(t *testing.T) {
	projectFile := filepath.Join(t.TempDir(), ".parrot", config.FileName)
	for _, field := range []string{
		"providers.local.base_url",
		"providers.local.api_key_env",
		"providers.local.header_timeout_ms",
		"mcp.server.command",
		"lsp.go.command",
		"formatters.gofmt.command",
		"web_fetch.allow_private",
	} {
		loaded := config.Result{
			Sources:    []config.Source{{Path: projectFile, Kind: config.SourceProject}},
			Provenance: map[string]string{field: projectFile},
		}
		if err := validateConfigTrust(loaded); err == nil || !strings.Contains(err.Error(), "requires global configuration") {
			t.Fatalf("field %q error = %v", field, err)
		}
	}
	loaded := config.Result{
		Sources: []config.Source{{Path: projectFile, Kind: config.SourceProject}},
		Provenance: map[string]string{
			"model":                               projectFile,
			"providers.local.models.code.context": projectFile,
		},
	}
	if err := validateConfigTrust(loaded); err != nil {
		t.Fatalf("safe project overrides rejected: %v", err)
	}
}

func TestTaskToolUsesIsolatedChildSessionAndReturnsOutput(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		switch {
		case bytes.Contains(body, []byte("function_call_output")):
			_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"parent received child output\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
		case bytes.Contains(body, []byte("child prompt")):
			_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"child output\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
		default:
			arguments := `{"prompt":"child prompt","agent":"explore"}`
			fmt.Fprintf(w, "data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"item_task\",\"type\":\"function_call\",\"call_id\":\"call_task\",\"name\":\"task\",\"arguments\":%q}}\n\n", arguments)
			_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
		}
	}))
	defer provider.Close()
	root := t.TempDir()
	configHome := filepath.Join(root, "config")
	configDir := filepath.Join(configHome, "parrot")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	configuration := fmt.Sprintf(`{"model":"local/model","providers":{"local":{"type":"compatible","protocol":"responses","base_url":%q,"api_key_env":"PARROT_TASK_KEY","allow_insecure_localhost":true,"models":{"model":{"tools":true}}}}}`, provider.URL+"/v1")
	if err := os.WriteFile(filepath.Join(configDir, "parrot.jsonc"), []byte(configuration), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PARROT_TASK_KEY", "secret")
	paths := appdirs.Overrides{Home: root, ConfigHome: configHome, DataHome: filepath.Join(root, "data"), StateHome: filepath.Join(root, "state"), CacheHome: filepath.Join(root, "cache")}
	runtime, err := Open(context.Background(), Options{CWD: root, Paths: paths, NonInteractive: true})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	parent, err := runtime.Client.CreateSession(context.Background(), v1.CreateSessionRequest{ProjectID: runtime.Project.ID, Title: "parent"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Client.Prompt(context.Background(), parent.ID, v1.PromptRequest{MessageID: "msg_parent", Content: "delegate", Delivery: "steer"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		messages, err := runtime.Client.Messages(context.Background(), parent.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, message := range messages.Items {
			if message.Role == "assistant" && message.Content == "parent received child output" {
				sessions, err := runtime.Client.Sessions(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				if len(sessions.Items) != 2 {
					t.Fatalf("sessions = %#v", sessions.Items)
				}
				for _, item := range sessions.Items {
					if item.ID != parent.ID && (!strings.HasPrefix(item.Title, "Subtask ") || item.ProjectID != parent.ProjectID || item.Agent != "explore") {
						t.Fatalf("child session = %#v", item)
					}
				}
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	messages, _ := runtime.Client.Messages(context.Background(), parent.ID)
	sessions, _ := runtime.Client.Sessions(context.Background())
	t.Fatalf("task tool did not return child output; messages=%#v sessions=%#v", messages.Items, sessions.Items)
}

func TestReportSubagentEventConvertsUsageAndToolCalls(t *testing.T) {
	var progress []subagent.Progress
	report := func(item subagent.Progress) { progress = append(progress, item) }
	usage, _ := json.Marshal(v1.SessionStatus{Kind: "usage", Usage: &v1.Usage{InputTokens: 10, OutputTokens: 2, TotalTokens: 12, ReasoningTokens: 1, CachedInputTokens: 3}})
	reportSubagentEvent(report, v1.Event{Type: v1.EventSessionStatus, Data: usage})
	toolCall, _ := json.Marshal(v1.SessionStatus{Kind: "tool_call_complete"})
	reportSubagentEvent(report, v1.Event{Type: v1.EventSessionStatus, Data: toolCall})
	if len(progress) != 2 || progress[0].Usage.TotalTokens != 12 || progress[0].Usage.CachedInputTokens != 3 || progress[1].ToolUses != 1 {
		t.Fatalf("progress = %#v", progress)
	}
}

func TestPublishSubagentEventPreservesNestedTaskAndDepth(t *testing.T) {
	live := event.NewBroker()
	parentEvents, unsubscribe := live.Subscribe("parent", 4)
	defer unsubscribe()
	executor := &appSubagentExecutor{live: live}
	execution := subagent.Execution{TaskID: "outer-task", ParentSession: "parent", Request: subagent.Request{Agent: "explore"}}

	delta, _ := json.Marshal(v1.MessagePartDelta{MessageID: "child-message", Kind: "text", Delta: "working"})
	executor.publishSubagentEvent(execution, v1.Event{Type: v1.EventMessagePartDelta, SessionID: "child", Data: delta})
	direct := decodeSubagentEvent(t, <-parentEvents)
	if direct.TaskID != "outer-task" || direct.TaskName != "explore" || direct.Depth != 1 || direct.Event.SessionID != "child" {
		t.Fatalf("direct projection = %#v", direct)
	}

	innerData, _ := json.Marshal(v1.SubagentEvent{TaskID: "inner-task", TaskName: "review", Depth: 1, Event: v1.Event{Type: v1.EventMessagePartDelta, SessionID: "grandchild", Data: delta}})
	executor.publishSubagentEvent(execution, v1.Event{Type: v1.EventSubagent, SessionID: "child", Data: innerData})
	nested := decodeSubagentEvent(t, <-parentEvents)
	if nested.TaskID != "inner-task" || nested.TaskName != "review" || nested.Depth != 2 || nested.Event.SessionID != "grandchild" {
		t.Fatalf("nested projection = %#v", nested)
	}

	progressData, _ := json.Marshal(v1.TaskProgress{TaskID: "inner-task", Agent: "review", Status: "running"})
	executor.publishSubagentEvent(execution, v1.Event{Type: v1.EventTaskProgress, SessionID: "child", Data: progressData})
	progress := decodeSubagentEvent(t, <-parentEvents)
	if progress.TaskID != "inner-task" || progress.TaskName != "review" || progress.Depth != 2 {
		t.Fatalf("nested progress projection = %#v", progress)
	}
}

func TestForwardSubagentEventsRelaysLiveEventsAndProgress(t *testing.T) {
	live := event.NewBroker()
	parentEvents, unsubscribeParent := live.Subscribe("parent", 2)
	defer unsubscribeParent()
	var progress []subagent.Progress
	executor := &appSubagentExecutor{live: live}
	stop := executor.forwardEvents("child", subagent.Execution{
		TaskID: "task-explore", ParentSession: "parent", Request: subagent.Request{Agent: "explore"},
		ReportProgress: func(item subagent.Progress) { progress = append(progress, item) },
	})

	usage, _ := json.Marshal(v1.SessionStatus{MessageID: "child-message", Kind: "usage", Usage: &v1.Usage{TotalTokens: 42}})
	live.PublishEvent(v1.Event{Type: v1.EventSessionStatus, SessionID: "child", Data: usage})
	select {
	case item := <-parentEvents:
		projected := decodeSubagentEvent(t, item)
		if projected.Depth != 1 || projected.TaskName != "explore" || projected.Event.Type != v1.EventSessionStatus {
			t.Fatalf("projection = %#v", projected)
		}
	case <-time.After(time.Second):
		t.Fatal("live child event was not relayed")
	}
	stop()
	if len(progress) != 1 || progress[0].Usage.TotalTokens != 42 {
		t.Fatalf("progress = %#v", progress)
	}
}

func decodeSubagentEvent(t *testing.T, item v1.Event) *v1.SubagentEvent {
	t.Helper()
	if item.Type != v1.EventSubagent {
		t.Fatalf("event type = %q, want %q", item.Type, v1.EventSubagent)
	}
	payload, err := v1.DecodeEventData(item)
	if err != nil {
		t.Fatal(err)
	}
	return payload.(*v1.SubagentEvent)
}

func TestMaintainCleansOnlyManagedOutputs(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	outputDir := filepath.Join(root, "outputs")
	outputs, err := tool.NewOutputStore(tool.OutputConfig{Directory: outputDir, PreviewBytes: 16, PreviewLines: 4, PerOutput: 1024, Total: 4096, Retention: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	old := now.Add(-48 * time.Hour)
	files := map[string]time.Time{
		"0123456789abcdef0123456789abcdef": old,
		".parrot-output-stale":             old,
		".parrot-output-fresh":             now,
		"unmanaged":                        old,
	}
	for name, modified := range files {
		path := filepath.Join(outputDir, name)
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, modified, modified); err != nil {
			t.Fatal(err)
		}
	}

	application := &App{outputs: outputs}
	report, err := application.Maintain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if report.OutputsRemoved != 2 {
		t.Fatalf("report = %#v", report)
	}
	for _, name := range []string{".parrot-output-fresh", "unmanaged"} {
		if _, err := os.Stat(filepath.Join(outputDir, name)); err != nil {
			t.Fatalf("preserved file %s: %v", name, err)
		}
	}
	second, err := application.Maintain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if second != (MaintenanceReport{}) {
		t.Fatalf("second maintenance was not a no-op: %#v", second)
	}
}
