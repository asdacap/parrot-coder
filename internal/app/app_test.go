package app

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
	"sync/atomic"
	"testing"
	"time"

	v1 "github.com/amirulashraf/parrot-coder/internal/api/v1"
	"github.com/amirulashraf/parrot-coder/internal/appdirs"
	"github.com/amirulashraf/parrot-coder/internal/compaction"
	"github.com/amirulashraf/parrot-coder/internal/config"
	"github.com/amirulashraf/parrot-coder/internal/event"
	"github.com/amirulashraf/parrot-coder/internal/project"
	"github.com/amirulashraf/parrot-coder/internal/store"
	"github.com/amirulashraf/parrot-coder/internal/tool"
)

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

func TestMaintainCleansOnlyManagedArtifacts(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(root, "maintenance.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	now := time.Now().UTC()
	stamp := now.Format(time.RFC3339Nano)
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO project(id,root_path,created_at) VALUES('prj',?,?)`, []any{root, stamp}},
		{`INSERT INTO session(id,project_id,title,created_at,updated_at) VALUES('ses','prj','test',?,?)`, []any{stamp, stamp}},
		{`INSERT INTO session_context_epoch(id,session_id,ordinal,baseline,sources_json,history_cutoff,created_at) VALUES('ctx','ses',0,'','{}',0,?)`, []any{stamp}},
		{`INSERT INTO compaction_attempt(id,session_id,source_epoch_id,covered_from_sequence,covered_to_sequence,history_cutoff,provider_id,model_id,forced,status,created_at) VALUES('cmpa','ses','ctx',0,0,1,'p','m',0,'active',?)`, []any{stamp}},
		{`INSERT INTO snapshot_blob(hash,data,size) VALUES('referenced',X'01',1),('orphan',X'02',1)`, nil},
		{`INSERT INTO snapshot_transaction(id,workspace,session_id,position,created_at) VALUES('txn',?,'ses',1,?)`, []any{root, stamp}},
		{`INSERT INTO snapshot_file(transaction_id,ordinal,path,before_exists,before_mode,before_hash,before_blob_hash,after_exists,after_mode) VALUES('txn',0,?,1,420,'referenced','referenced',0,0)`, []any{filepath.Join(root, "file")}},
	}
	for _, statement := range statements {
		if _, err := db.SQL().ExecContext(ctx, statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}

	outputDir := filepath.Join(root, "outputs")
	outputs, err := tool.NewOutputStore(tool.OutputConfig{Directory: outputDir, PreviewBytes: 16, PreviewLines: 4, PerOutput: 1024, Total: 4096, Retention: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
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
	staleTemp := filepath.Join(root, ".parrot-snapshot-stale")
	freshTemp := filepath.Join(root, ".parrot-snapshot-fresh")
	for path, modified := range map[string]time.Time{staleTemp: old, freshTemp: now} {
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, modified, modified); err != nil {
			t.Fatal(err)
		}
	}

	application := &App{Project: project.Info{ID: "prj", Root: root}, db: db, outputs: outputs, compactions: compaction.NewRepository(db, event.NewRepository(db))}
	report, err := application.Maintain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if report.OutputsRemoved != 2 || report.TemporaryFilesRemoved != 1 || report.SnapshotBlobsPruned != 1 || report.CompactionAttemptsRepaired != 1 {
		t.Fatalf("report = %#v", report)
	}
	for _, path := range []string{filepath.Join(outputDir, ".parrot-output-fresh"), filepath.Join(outputDir, "unmanaged"), freshTemp} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("preserved file %s: %v", path, err)
		}
	}
	var blobs int
	if err := db.SQL().QueryRowContext(ctx, `SELECT COUNT(*) FROM snapshot_blob WHERE hash='referenced'`).Scan(&blobs); err != nil || blobs != 1 {
		t.Fatalf("referenced blobs = %d, %v", blobs, err)
	}
	var attemptStatus string
	if err := db.SQL().QueryRowContext(ctx, `SELECT status FROM compaction_attempt WHERE id='cmpa'`).Scan(&attemptStatus); err != nil || attemptStatus != "interrupted" {
		t.Fatalf("attempt status = %q, %v", attemptStatus, err)
	}
	second, err := application.Maintain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if second != (MaintenanceReport{}) {
		t.Fatalf("second maintenance was not a no-op: %#v", second)
	}
}
