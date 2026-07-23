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
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amirulashraf/parrot-coder/internal/agent"
	v1 "github.com/amirulashraf/parrot-coder/internal/api/v1"
	"github.com/amirulashraf/parrot-coder/internal/appdirs"
	"github.com/amirulashraf/parrot-coder/internal/auth"
	"github.com/amirulashraf/parrot-coder/internal/client"
	"github.com/amirulashraf/parrot-coder/internal/config"
	"github.com/amirulashraf/parrot-coder/internal/event"
	"github.com/amirulashraf/parrot-coder/internal/mode"
	"github.com/amirulashraf/parrot-coder/internal/session"
	"github.com/amirulashraf/parrot-coder/internal/subagent"
	managedtask "github.com/amirulashraf/parrot-coder/internal/task"
	"github.com/amirulashraf/parrot-coder/internal/tool"
)

type appDrainerFunc func(context.Context, string) error

type waitingManagedTask struct{ snapshot managedtask.Snapshot }

func (t waitingManagedTask) Snapshot() managedtask.Snapshot { return t.snapshot }
func (waitingManagedTask) Wait(ctx context.Context) (managedtask.Completion, error) {
	<-ctx.Done()
	return managedtask.Completion{}, ctx.Err()
}
func (t waitingManagedTask) Interrupt(context.Context) (managedtask.Snapshot, error) {
	return t.snapshot, nil
}

func TestManagedTaskControllerPreservesTaskStateWhenWaitIsCanceled(t *testing.T) {
	tasks := managedtask.NewManager()
	snapshot := managedtask.Snapshot{ID: "task_agent", SessionID: "session", Kind: managedtask.KindAgent, Status: "running"}
	if err := tasks.Register(waitingManagedTask{snapshot: snapshot}, func(caller string) bool { return caller == "session" }); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := (&managedTaskController{tasks: tasks}).Wait(ctx, "session", snapshot.ID)
	if !errors.Is(err, context.Canceled) || result.ID != snapshot.ID || result.Kind != snapshot.Kind || result.Status != snapshot.Status {
		t.Fatalf("Wait() = %#v, %v", result, err)
	}
}

func TestAgentRecursionLimitPolicy(t *testing.T) {
	modes, err := mode.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	agents, err := agent.NewRegistry(agent.Subagents()...)
	if err != nil {
		t.Fatal(err)
	}
	resolver := combinedProfileResolver{modes: modes, agents: agents}
	for _, test := range []struct {
		id       string
		identity string
		limit    int
	}{
		{id: mode.PlanID, identity: mode.PlanID, limit: 1},
		{id: mode.QueryID, identity: mode.QueryID, limit: 1},
		{id: mode.BuildID, identity: mode.BuildID, limit: 3},
		{id: agent.ExploreID, identity: agent.ExplorerID, limit: 3},
		{id: agent.ExplorerID, identity: agent.ExplorerID, limit: 3},
		{id: agent.ReviewID, identity: agent.ReviewID, limit: 3},
		{id: agent.WorkerID, identity: agent.WorkerID, limit: 3},
	} {
		profile, resolveErr := resolver.GetProfile(test.id)
		if resolveErr != nil {
			t.Fatal(resolveErr)
		}
		if profile.ID != test.identity || profile.RecursionLimit != test.limit {
			t.Fatalf("policy for %q = identity %q, limit %d", test.id, profile.ID, profile.RecursionLimit)
		}
	}
}

func (f appDrainerFunc) Drain(ctx context.Context, sessionID string) error {
	return f(ctx, sessionID)
}

func TestStatusDrainerPublishesOnlyLifecycleCompletion(t *testing.T) {
	live := event.NewBroker(nil, nil)
	events, unsubscribe := live.Subscribe("session", 2)
	defer unsubscribe()
	drainer := statusReporter{
		live: live,
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
	live := event.NewBroker(nil, nil)
	events, unsubscribe := live.Subscribe("session", 1)
	defer unsubscribe()
	drainer := statusReporter{live: live}
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
	requests := make(chan []byte, 1)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_, _ = io.WriteString(w, `{"data":[{"id":"test"}]}`)
			return
		}
		if r.URL.Path != "/v1/responses" {
			t.Errorf("provider path = %q", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-secret" {
			t.Errorf("provider authorization = %q", r.Header.Get("Authorization"))
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		} else {
			requests <- body
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
	configuration := fmt.Sprintf(`prompt: configured base prompt
model: local/test-model
providers:
  local:
    type: compatible
    protocol: responses
    base_url: %q
    api_key_env: PARROT_TEST_KEY
    allow_insecure_localhost: true
    models:
      test-model:
        name: Test Model
        tools: true
`, provider.URL+"/v1")
	if err := os.WriteFile(filepath.Join(configHome, "parrot", "parrot.yaml"), []byte(configuration), 0o600); err != nil {
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
	body := <-requests
	if !bytes.Contains(body, []byte("configured base prompt")) || bytes.Contains(body, []byte("You are Parrot Coder, a local coding agent.")) {
		t.Fatalf("provider request did not use the configured base prompt: %s", body)
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
	qualifiedModel := model.Provider + "/" + model.ID
	for _, agentID := range []string{agent.ExploreID, agent.ExplorerID} {
		created, createErr := runtime.Client.CreateSession(context.Background(), v1.CreateSessionRequest{
			Title: "selected", Agent: agentID, Model: qualifiedModel,
		})
		if createErr != nil {
			t.Fatal(createErr)
		}
		if created.Agent != agentID || created.Provider != model.Provider || created.Model != model.ID {
			t.Fatalf("%s selection = %#v", agentID, created)
		}
		selected, updateErr := runtime.Client.UpdateSessionSelection(context.Background(), created.ID, v1.UpdateSessionSelectionRequest{Model: qualifiedModel})
		if updateErr != nil || selected.Agent != agentID {
			t.Fatalf("update %s selection = %#v, %v", agentID, selected, updateErr)
		}
	}
	_, err = runtime.Client.CreateSession(context.Background(), v1.CreateSessionRequest{Agent: "build", Model: model.Provider + "/missing"})
	assertAppProblem(t, err, "invalid_selection")
	listed, err = runtime.Client.Sessions(context.Background())
	if err != nil || len(listed.Items) != 2 {
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

func TestOpenVariantPrecedenceAndValidation(t *testing.T) {
	providerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_, _ = io.WriteString(w, `{"data":[{"id":"reasoning"},{"id":"plain"}]}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer providerServer.Close()
	root := t.TempDir()
	paths := appdirs.Overrides{
		Home: root, ConfigHome: filepath.Join(root, "config"), DataHome: filepath.Join(root, "data"),
		StateHome: filepath.Join(root, "state"), CacheHome: filepath.Join(root, "cache"),
	}
	configPath := filepath.Join(root, "config", "parrot", config.FileName)
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	providerConfig := fmt.Sprintf(`providers:
  local:
    type: compatible
    protocol: responses
    base_url: %q
    api_key_env: PARROT_VARIANT_TEST_KEY
    allow_insecure_localhost: true
    models:
      reasoning:
        context: 1000
        variants:
          low:
            reasoning_effort: low
          medium:
            reasoning_effort: medium
          high:
            reasoning_effort: high
      plain:
        context: 1000
`, providerServer.URL+"/v1")
	if err := os.WriteFile(configPath, []byte(providerConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PARROT_VARIANT_TEST_KEY", "test")
	runtime, err := Open(context.Background(), Options{CWD: root, Paths: paths, AllowNoModel: true, NonInteractive: true})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := runtime.Client.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var selected, variantless v1.Model
	for _, item := range catalog.Items {
		if len(item.Variants) >= 3 && selected.ID == "" {
			selected = item
		}
		if len(item.Variants) == 0 && variantless.ID == "" {
			variantless = item
		}
	}
	if selected.ID == "" || variantless.ID == "" {
		t.Fatalf("catalog lacks variant fixtures: %#v", catalog.Items)
	}
	model := selected.Provider + "/" + selected.ID
	historyVariant, configVariant, cliVariant := selected.Variants[0].Name, selected.Variants[1].Name, selected.Variants[2].Name
	if _, err := runtime.Client.CreateSession(context.Background(), v1.CreateSessionRequest{
		ProjectID: runtime.Project.ID, Title: "history", Agent: "build", Model: model, Variant: &historyVariant,
	}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	if err := config.UpdateDefaultSelection(configPath, model, configVariant); err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name    string
		options Options
		want    string
	}{
		{name: "config supersedes history", options: Options{}, want: configVariant},
		{name: "cli supersedes config", options: Options{Variant: cliVariant}, want: cliVariant},
	} {
		t.Run(test.name, func(t *testing.T) {
			options := test.options
			options.CWD, options.Paths, options.NonInteractive = root, paths, true
			opened, openErr := Open(context.Background(), options)
			if openErr != nil {
				t.Fatal(openErr)
			}
			defer opened.Close()
			if opened.DefaultSelection.Provider+"/"+opened.DefaultSelection.Model != model || opened.DefaultSelection.Variant != test.want {
				t.Fatalf("selection = %#v, want %s variant %q", opened.DefaultSelection, model, test.want)
			}
		})
	}

	for _, test := range []struct {
		name    string
		model   string
		variant string
		wantErr string
	}{
		{name: "unknown", model: model, variant: "bogus", wantErr: `variant "bogus" is not available`},
		{name: "incompatible", model: variantless.Provider + "/" + variantless.ID, variant: historyVariant, wantErr: `variant "` + historyVariant + `" is not available`},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, openErr := Open(context.Background(), Options{CWD: root, Paths: paths, Model: test.model, Variant: test.variant, NonInteractive: true})
			if openErr == nil || !strings.Contains(openErr.Error(), test.wantErr) {
				t.Fatalf("Open error = %v, want %q", openErr, test.wantErr)
			}
		})
	}

	modelLessRoot := t.TempDir()
	modelLessPaths := appdirs.Overrides{Home: modelLessRoot, ConfigHome: filepath.Join(modelLessRoot, "config"), DataHome: filepath.Join(modelLessRoot, "data"), StateHome: filepath.Join(modelLessRoot, "state"), CacheHome: filepath.Join(modelLessRoot, "cache")}
	_, err = Open(context.Background(), Options{CWD: modelLessRoot, Paths: modelLessPaths, Variant: "high", AllowNoModel: true, NonInteractive: true})
	if err == nil || !strings.Contains(err.Error(), `variant "high" requires a model`) {
		t.Fatalf("model-less variant error = %v", err)
	}
}

func TestOpenRestoresVariantWhenModelMatches(t *testing.T) {
	root := t.TempDir()
	paths := appdirs.Overrides{
		Home: root, ConfigHome: filepath.Join(root, "config"), DataHome: filepath.Join(root, "data"),
		StateHome: filepath.Join(root, "state"), CacheHome: filepath.Join(root, "cache"),
	}
	// First, open without a model to create a session with a variant.
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
		ProjectID: runtime.Project.ID, Title: "variant", Agent: "build", Model: model.Provider + "/" + model.ID, Variant: &variant,
	}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen with the model explicitly configured; variant should still be restored.
	modelArg := model.Provider + "/" + model.ID
	reopened, err := Open(context.Background(), Options{CWD: root, Paths: paths, Model: modelArg, NonInteractive: true})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.DefaultSelection.Variant != variant {
		t.Fatalf("restored variant = %q, want %q", reopened.DefaultSelection.Variant, variant)
	}
	if reopened.DefaultSelection.Provider != model.Provider || reopened.DefaultSelection.Model != model.ID {
		t.Fatalf("restored model = %s/%s, want %s/%s", reopened.DefaultSelection.Provider, reopened.DefaultSelection.Model, model.Provider, model.ID)
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
	configuration := fmt.Sprintf(`mcp:
  fixture:
    transport: http
    url: %q
    enabled: true
    allow_insecure_localhost: true
`, server.URL)
	if err := os.WriteFile(filepath.Join(configDir, "parrot.yaml"), []byte(configuration), 0o600); err != nil {
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
		configuration := fmt.Sprintf(`mcp:
  broken:
    transport: http
    url: %q
    enabled: %t
    allow_insecure_localhost: true
`, server.URL, enabled)
		if err := os.WriteFile(filepath.Join(configDir, "parrot.yaml"), []byte(configuration), 0o600); err != nil {
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

func TestProjectConfigCannotIntroduceExternalCapabilities(t *testing.T) {
	projectFile := filepath.Join(t.TempDir(), ".parrot", config.FileName)
	for _, field := range []string{
		"providers.local.base_url",
		"providers.local.api_key_env",
		"providers.local.header_timeout_ms",
		"mcp.server.command",
		"web_fetch.allow_private",
		"sandbox_rules",
		"sandbox_rules.0.path",
		"sandbox_rules.0.rule",
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
			"prompt":                              projectFile,
			"model":                               projectFile,
			"providers.local.models.code.context": projectFile,
		},
	}
	if err := validateConfigTrust(loaded); err != nil {
		t.Fatalf("safe project overrides rejected: %v", err)
	}
}

func TestAgentToolsUseIsolatedChildSessionAndReturnOutput(t *testing.T) {
	var parentContinuations atomic.Int32
	agentIDPattern := regexp.MustCompile(`task_[0-9a-f]+`)
	releaseChild := make(chan struct{})
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		switch {
		case bytes.Contains(body, []byte("Task monitor notification")) && bytes.Contains(body, []byte("child output")):
			_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"parent received child output\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
		case bytes.Contains(body, []byte("function_call_output")) && parentContinuations.Add(1) == 1:
			agentID := string(agentIDPattern.Find(body))
			if agentID == "" {
				t.Errorf("spawn output omitted task ID: %s", body)
				return
			}
			arguments := fmt.Sprintf(`{"task_id":%q}`, agentID)
			fmt.Fprintf(w, "data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"item_monitor\",\"type\":\"function_call\",\"call_id\":\"call_monitor\",\"name\":\"monitor\",\"arguments\":%q}}\n\n", arguments)
			_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
			close(releaseChild)
		case bytes.Contains(body, []byte("function_call_output")):
			_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
		case bytes.Contains(body, []byte("child prompt")):
			<-releaseChild
			_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"child output\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
		default:
			arguments := `{"prompt":"child prompt","agent":"explorer"}`
			fmt.Fprintf(w, "data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"item_spawn\",\"type\":\"function_call\",\"call_id\":\"call_spawn\",\"name\":\"agent_spawn\",\"arguments\":%q}}\n\n", arguments)
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
	configuration := fmt.Sprintf(`model: local/model
providers:
  local:
    type: compatible
    protocol: responses
    base_url: %q
    api_key_env: PARROT_TASK_KEY
    allow_insecure_localhost: true
    models:
      model:
        tools: true
`, provider.URL+"/v1")
	if err := os.WriteFile(filepath.Join(configDir, "parrot.yaml"), []byte(configuration), 0o600); err != nil {
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
					if item.ID != parent.ID && (!strings.HasPrefix(item.Title, "Subtask ") || item.ProjectID != parent.ProjectID || item.Agent != "explorer") {
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
	t.Fatalf("agent tools did not return child output; messages=%#v sessions=%#v", messages.Items, sessions.Items)
}

func TestMigrateLegacyCredentials(t *testing.T) {
	for _, testCase := range []struct {
		name           string
		legacyContent  string
		newContent     string
		wantNewContent string
		wantLegacyGone bool
	}{
		{
			name:           "legacy file is moved to config",
			legacyContent:  `{"version":1,"credentials":{"local":{"version":1,"type":"api_key","api_key":{"key":"secret"}}}}`,
			wantNewContent: `{"version":1,"credentials":{"local":{"version":1,"type":"api_key","api_key":{"key":"secret"}}}}`,
			wantLegacyGone: true,
		},
		{
			name:           "legacy file is moved when new does not exist",
			legacyContent:  `{"version":1,"credentials":{"chatgpt":{"version":1,"type":"oauth","oauth":{"access_token":"a","refresh_token":"r","expires_at":"2030-01-01T00:00:00Z"}}}}`,
			wantNewContent: `{"version":1,"credentials":{"chatgpt":{"version":1,"type":"oauth","oauth":{"access_token":"a","refresh_token":"r","expires_at":"2030-01-01T00:00:00Z"}}}}`,
			wantLegacyGone: true,
		},
		{
			name:           "new file wins over legacy file",
			legacyContent:  `{"version":1,"credentials":{"local":{"version":1,"type":"api_key","api_key":{"key":"old"}}}}`,
			newContent:     `{"version":1,"credentials":{"local":{"version":1,"type":"api_key","api_key":{"key":"new"}}}}`,
			wantNewContent: `{"version":1,"credentials":{"local":{"version":1,"type":"api_key","api_key":{"key":"new"}}}}`,
			wantLegacyGone: false,
		},
		{
			name:           "no migration when neither file exists",
			wantNewContent: "",
			wantLegacyGone: true,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			root := t.TempDir()
			paths := appdirs.Paths{
				Config: filepath.Join(root, "config", "parrot"),
				Data:   filepath.Join(root, "data", "parrot"),
			}
			legacyPath := filepath.Join(paths.Data, CredentialFile)
			newPath := CredentialFilePath(paths)

			if testCase.legacyContent != "" {
				if err := os.MkdirAll(paths.Data, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(legacyPath, []byte(testCase.legacyContent), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if testCase.newContent != "" {
				if err := os.MkdirAll(paths.Config, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(newPath, []byte(testCase.newContent), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			if err := MigrateLegacyCredentials(paths); err != nil {
				t.Fatalf("MigrateLegacyCredentials() = %v", err)
			}

			if testCase.wantNewContent != "" {
				got, err := os.ReadFile(newPath)
				if err != nil {
					t.Fatalf("read new credential file: %v", err)
				}
				if string(got) != testCase.wantNewContent {
					t.Fatalf("new credential file = %q, want %q", got, testCase.wantNewContent)
				}
			} else {
				if _, err := os.Stat(newPath); !os.IsNotExist(err) {
					t.Fatalf("new credential file should not exist, got err = %v", err)
				}
			}

			_, legacyErr := os.Stat(legacyPath)
			legacyExists := !os.IsNotExist(legacyErr)
			if legacyExists == testCase.wantLegacyGone {
				t.Fatalf("legacy file exists = %t, want %t", legacyExists, !testCase.wantLegacyGone)
			}
		})
	}
}

func TestCredentialFilePathUsesConfigDir(t *testing.T) {
	root := t.TempDir()
	paths := appdirs.Paths{
		Config: filepath.Join(root, "config", "parrot"),
		Data:   filepath.Join(root, "data", "parrot"),
	}
	got := CredentialFilePath(paths)
	want := filepath.Join(paths.Config, CredentialFile)
	if got != want {
		t.Fatalf("CredentialFilePath() = %q, want %q", got, want)
	}
}

func TestMigrateLegacyCredentialsConcurrent(t *testing.T) {
	root := t.TempDir()
	paths := appdirs.Paths{
		Config: filepath.Join(root, "config", "parrot"),
		Data:   filepath.Join(root, "data", "parrot"),
	}
	legacyPath := filepath.Join(paths.Data, CredentialFile)
	newPath := CredentialFilePath(paths)

	if err := os.MkdirAll(paths.Data, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyPath, []byte(`{"version":1,"credentials":{"local":{"version":1,"type":"api_key","api_key":{"key":"secret"}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	var group sync.WaitGroup
	errors := make(chan error, 100)
	for i := 0; i < 100; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			if err := MigrateLegacyCredentials(paths); err != nil {
				errors <- err
			}
		}()
	}
	group.Wait()
	close(errors)

	for err := range errors {
		t.Fatalf("MigrateLegacyCredentials() = %v", err)
	}

	if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
		t.Fatalf("legacy credential file should be gone, got err = %v", err)
	}
	got, err := os.ReadFile(newPath)
	if err != nil {
		t.Fatalf("read new credential file: %v", err)
	}
	want := `{"version":1,"credentials":{"local":{"version":1,"type":"api_key","api_key":{"key":"secret"}}}}`
	if string(got) != want {
		t.Fatalf("new credential file = %q, want %q", got, want)
	}
}

func TestOpenUsesConfigCredentialFile(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_, _ = io.WriteString(w, `{"data":[{"id":"test"}]}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer provider.Close()

	root := t.TempDir()
	configHome := filepath.Join(root, "config")
	dataHome := filepath.Join(root, "data")
	stateHome := filepath.Join(root, "state")
	cacheHome := filepath.Join(root, "cache")
	configDir := filepath.Join(configHome, "parrot")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	configuration := fmt.Sprintf(`model: local/test-model
providers:
  local:
    type: compatible
    protocol: responses
    base_url: %q
    api_key_env: PARROT_TEST_KEY
    allow_insecure_localhost: true
    models:
      test-model:
        name: Test Model
        tools: true
`, provider.URL+"/v1")
	if err := os.WriteFile(filepath.Join(configDir, "parrot.yaml"), []byte(configuration), 0o600); err != nil {
		t.Fatal(err)
	}

	// Store credentials in the legacy data location; Open should migrate them to config.
	dataDir := filepath.Join(dataHome, "parrot")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	credentials := `{"version":1,"credentials":{"local":{"version":1,"type":"api_key","api_key":{"key":"migrated-secret"}}}}`
	if err := os.WriteFile(filepath.Join(dataDir, CredentialFile), []byte(credentials), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PARROT_TEST_KEY", "")
	paths := appdirs.Overrides{Home: root, ConfigHome: configHome, DataHome: dataHome, StateHome: stateHome, CacheHome: cacheHome}
	runtime, err := Open(context.Background(), Options{CWD: root, Paths: paths, Version: "test", NonInteractive: true})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	stored, err := runtime.Credentials.Get(context.Background(), "local")
	if err != nil {
		t.Fatalf("read migrated credential: %v", err)
	}
	if stored.Type != auth.CredentialAPIKey || stored.APIKey == nil || stored.APIKey.Key.Value() != "migrated-secret" {
		t.Fatalf("migrated credential = %v, want api_key with migrated-secret", stored)
	}
	if _, err := os.Stat(filepath.Join(dataDir, CredentialFile)); !os.IsNotExist(err) {
		t.Fatalf("legacy credential file still exists after migration")
	}
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

type testSessionHierarchy map[string]agent.ChildSession

func (h testSessionHierarchy) ChildRelation(sessionID string) (string, string, bool) {
	relation, ok := h[sessionID]
	return relation.ParentSessionID, relation.TaskID, ok
}

func TestBrokerFlattensTaskAttribution(t *testing.T) {
	live := event.NewBroker(nil, nil, testSessionHierarchy{"child": {ParentSessionID: "parent", TaskID: "outer-task"}})
	parentEvents, unsubscribe := live.Subscribe("parent", 4)
	defer unsubscribe()

	delta, _ := json.Marshal(v1.MessagePartDelta{MessageID: "child-message", Kind: "text", Delta: "working"})
	live.PublishEvent(v1.Event{Type: v1.EventMessagePartDelta, SessionID: "child", Data: delta})
	direct := <-parentEvents
	if direct.Type != v1.EventMessagePartDelta || direct.SessionID != "parent" || direct.TaskID != "outer-task" || direct.Sequence != nil {
		t.Fatalf("direct projection = %#v", direct)
	}

	live.PublishEvent(v1.Event{Type: v1.EventMessagePartDelta, SessionID: "child", TaskID: "inner-task", Data: delta})
	nested := <-parentEvents
	if nested.TaskID != "inner-task" || nested.SessionID != "parent" {
		t.Fatalf("nested projection = %#v", nested)
	}
}

func TestPublishSubagentLifecycleEmitsFlatTaskEvents(t *testing.T) {
	live := event.NewBroker(nil, nil)
	parentEvents, unsubscribe := live.Subscribe("parent", 4)
	defer unsubscribe()

	publishSubagentLifecycle(live, subagent.LifecycleEvent{Kind: subagent.LifecycleStart, Task: subagent.Task{ID: "task_1", SessionID: "child", ParentSession: "parent", Agent: "explore"}})
	started := decodeTaskEvent(t, <-parentEvents)
	if started.TaskID != "task_1" || started.SessionID != "child" || started.ParentSessionID != "parent" || started.Kind != "agent" || started.Agent != "explore" {
		t.Fatalf("start = %#v", started)
	}

	publishSubagentLifecycle(live, subagent.LifecycleEvent{Kind: subagent.LifecycleFinished, Task: subagent.Task{ID: "task_1", SessionID: "child", ParentSession: "parent", Agent: "explore", Status: subagent.StatusFailed, Error: "boom"}})
	finished := decodeTaskEvent(t, <-parentEvents)
	if finished.TaskID != "task_1" || finished.SessionID != "child" || finished.Status != "failed" || finished.Error != "boom" {
		t.Fatalf("finished = %#v", finished)
	}
}

func decodeTaskEvent(t *testing.T, item v1.Event) *v1.TaskEvent {
	t.Helper()
	payload, err := v1.DecodeEventData(item)
	if err != nil {
		t.Fatal(err)
	}
	task, ok := payload.(*v1.TaskEvent)
	if !ok {
		t.Fatalf("event type = %q, payload %T, want task event", item.Type, payload)
	}
	return task
}

func TestBrokerRelaysSubagentEventsAndProgress(t *testing.T) {
	live := event.NewBroker(nil, nil, testSessionHierarchy{"child": {ParentSessionID: "parent", TaskID: "task-explore"}})
	parentEvents, unsubscribeParent := live.Subscribe("parent", 2)
	defer unsubscribeParent()
	var progress []subagent.Progress
	stop := live.ObserveTransient("child", func(item v1.Event) {
		reportSubagentEvent(func(item subagent.Progress) { progress = append(progress, item) }, item)
	})
	defer stop()

	usage, _ := json.Marshal(v1.SessionStatus{MessageID: "child-message", Kind: "usage", Usage: &v1.Usage{TotalTokens: 42}})
	live.PublishEvent(v1.Event{Type: v1.EventSessionStatus, SessionID: "child", Data: usage})
	item := <-parentEvents
	if item.Type != v1.EventSessionStatus || item.SessionID != "parent" || item.TaskID != "task-explore" {
		t.Fatalf("projection = %#v", item)
	}
	if len(progress) != 1 || progress[0].Usage.TotalTokens != 42 {
		t.Fatalf("progress = %#v", progress)
	}
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
