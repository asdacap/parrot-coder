// Package app composes Parrot's long-lived application services.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/amirulashraf/parrot-coder/internal/agent"
	v1 "github.com/amirulashraf/parrot-coder/internal/api/v1"
	"github.com/amirulashraf/parrot-coder/internal/appdirs"
	"github.com/amirulashraf/parrot-coder/internal/auth"
	"github.com/amirulashraf/parrot-coder/internal/change"
	"github.com/amirulashraf/parrot-coder/internal/client"
	"github.com/amirulashraf/parrot-coder/internal/command"
	"github.com/amirulashraf/parrot-coder/internal/compaction"
	"github.com/amirulashraf/parrot-coder/internal/config"
	"github.com/amirulashraf/parrot-coder/internal/diagnostics"
	"github.com/amirulashraf/parrot-coder/internal/event"
	"github.com/amirulashraf/parrot-coder/internal/formatter"
	"github.com/amirulashraf/parrot-coder/internal/httpapi"
	"github.com/amirulashraf/parrot-coder/internal/id"
	"github.com/amirulashraf/parrot-coder/internal/lsp"
	"github.com/amirulashraf/parrot-coder/internal/mcp"
	"github.com/amirulashraf/parrot-coder/internal/mode"
	"github.com/amirulashraf/parrot-coder/internal/permission"
	"github.com/amirulashraf/parrot-coder/internal/process"
	"github.com/amirulashraf/parrot-coder/internal/project"
	"github.com/amirulashraf/parrot-coder/internal/protocol"
	"github.com/amirulashraf/parrot-coder/internal/provider"
	"github.com/amirulashraf/parrot-coder/internal/question"
	"github.com/amirulashraf/parrot-coder/internal/session"
	"github.com/amirulashraf/parrot-coder/internal/skill"
	"github.com/amirulashraf/parrot-coder/internal/snapshot"
	"github.com/amirulashraf/parrot-coder/internal/store"
	"github.com/amirulashraf/parrot-coder/internal/subagent"
	"github.com/amirulashraf/parrot-coder/internal/systemcontext"
	"github.com/amirulashraf/parrot-coder/internal/tool"
	"github.com/amirulashraf/parrot-coder/internal/transport/inproc"
	"github.com/amirulashraf/parrot-coder/internal/webfetch"
	"github.com/amirulashraf/parrot-coder/internal/workspace"
)

const (
	CredentialFile = "credentials.json"
	DatabaseFile   = "parrot.db"
)

// Options controls process-local composition. Model accepts provider/model or
// a model ID from the configured default provider.
type Options struct {
	CWD            string
	Paths          appdirs.Overrides
	Version        string
	Model          string
	Agent          string
	Mode           string
	Permission     permission.Decision
	NonInteractive bool
	AllowNoModel   bool
	HTTPClient     *http.Client
}

// App owns every long-lived dependency and the optional HTTP handler. Local
// callers use Client, whose transport invokes Handler without binding a socket.
type App struct {
	Paths            appdirs.Paths
	Project          project.Info
	WorkingDirectory string
	Config           config.Result
	Credentials      auth.Store
	Client           *Client
	Handler          http.Handler
	Backend          *httpapi.DomainBackend
	Commands         *command.Registry
	Skills           *skill.Registry
	// DefaultSelection is incomplete when Open permits model-less startup and
	// no default model is configured.
	DefaultSelection v1.SessionSelection

	db          *store.DB
	coordinator *agent.Coordinator
	compactions *compaction.Repository
	outputs     *tool.OutputStore
	mcp         *mcp.Manager
	lsp         *lsp.Manager
	closeOnce   sync.Once
	closeErr    error
}

// Client is the typed application client used by local commands. It delegates
// the public v1 contract to client.Client and adds the explicit resume action.
type Client struct {
	*client.Client
	http *http.Client
}

func (c *Client) SelectSession(ctx context.Context, sessionID, modeID, model string) error {
	_, err := c.UpdateSessionSelection(ctx, sessionID, v1.UpdateSessionSelectionRequest{Mode: modeID, Model: model})
	return err
}

func (c *Client) Resume(ctx context.Context, sessionID string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://parrot.local/api/v1/sessions/"+url.PathEscape(sessionID)+"/resume", nil)
	if err != nil {
		return err
	}
	response, err := c.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		return fmt.Errorf("resume session: HTTP %d", response.StatusCode)
	}
	return nil
}

// Open resolves the current project and XDG paths, opens durable state, and
// constructs all application services. It never opens a network listener.
func Open(ctx context.Context, options Options) (_ *App, err error) {
	started := time.Now()
	diagnostics.Event("app_open_started", "allow_no_model", options.AllowNoModel, "non_interactive", options.NonInteractive)
	defer func() {
		attributes := []any{"duration_ms", time.Since(started).Milliseconds(), "status", "success"}
		if err != nil {
			attributes[3] = "error"
			attributes = append(attributes, "error_type", diagnostics.ErrorType(err))
			diagnostics.Error("app_open_finished", attributes...)
			return
		}
		diagnostics.Event("app_open_finished", attributes...)
	}()
	cwd := options.CWD
	if cwd == "" {
		cwd, err = os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("app: working directory: %w", err)
		}
	}
	cwd, err = filepath.Abs(cwd)
	if err != nil {
		return nil, fmt.Errorf("app: working directory: %w", err)
	}
	if canonical, evaluateErr := filepath.EvalSymlinks(cwd); evaluateErr == nil {
		cwd = canonical
	}
	info, err := project.Resolve(ctx, cwd)
	if err != nil {
		return nil, fmt.Errorf("app: project: %w", err)
	}
	paths, err := appdirs.ResolveAndEnsure(options.Paths)
	if err != nil {
		return nil, fmt.Errorf("app: paths: %w", err)
	}
	loaded, err := config.Load(config.Options{ConfigDir: paths.Config, ProjectRoot: info.Root, CWD: cwd})
	if err != nil {
		return nil, fmt.Errorf("app: config: %w", err)
	}
	if err := validateConfigTrust(loaded); err != nil {
		return nil, err
	}
	credentials := auth.NewFileStore(filepath.Join(paths.Data, CredentialFile))
	providers, err := BuildProviders(ctx, loaded.Config, credentials, options.HTTPClient)
	if err != nil {
		return nil, err
	}
	providerID, modelID := "", ""
	if !options.AllowNoModel || loaded.Config.DefaultModel != "" || options.Model != "" {
		providerID, modelID, err = selectModel(loaded.Config.DefaultModel, options.Model, providers)
		if err != nil {
			return nil, err
		}
	}
	modes, err := mode.NewRegistry()
	if err != nil {
		return nil, fmt.Errorf("app: agents: %w", err)
	}
	agentID := options.Mode
	if agentID == "" {
		agentID = options.Agent
	}
	if agentID == "" {
		agentID = mode.BuildID
	}
	if _, err := modes.Get(agentID); err != nil {
		return nil, fmt.Errorf("app: %w", err)
	}
	taskAgents, err := agent.NewRegistry(agent.Subagents()...)
	if err != nil {
		return nil, fmt.Errorf("app: subagents: %w", err)
	}
	providerRegistry, err := agent.NewProviderRegistry(providers...)
	if err != nil {
		return nil, fmt.Errorf("app: providers: %w", err)
	}
	if providerID != "" {
		_, _, err = providerRegistry.Resolve(providerID, modelID)
	}
	if err != nil {
		return nil, fmt.Errorf("app: default model: %w", err)
	}

	db, err := store.Open(ctx, filepath.Join(paths.State, DatabaseFile))
	if err != nil {
		return nil, fmt.Errorf("app: store: %w", err)
	}
	defaultSelection := session.Selection{Agent: agentID, Provider: providerID, Model: modelID}
	result := &App{
		Paths: paths, Project: info, WorkingDirectory: cwd, Config: loaded, Credentials: credentials, db: db,
		DefaultSelection: v1.SessionSelection{Agent: agentID, Provider: providerID, Model: modelID},
	}
	defer func() {
		if err != nil {
			_ = result.Close()
		}
	}()
	if _, err := db.SQL().ExecContext(ctx, `INSERT INTO project(id,root_path,created_at) VALUES(?,?,?)
		ON CONFLICT(id) DO UPDATE SET root_path=excluded.root_path`, info.ID, info.Root, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return nil, fmt.Errorf("app: persist project: %w", err)
	}
	repository := event.NewRepository(db)
	live := event.NewBroker()
	sessions := session.NewService(db, repository)
	if providerID == "" && options.AllowNoModel {
		selected, selectionErr := sessions.LatestSelection(ctx, info.ID)
		switch {
		case selectionErr == nil:
			if _, restoredModel, resolveErr := providerRegistry.Resolve(selected.Provider, selected.Model); resolveErr == nil && (selected.Variant == "" || modelHasVariant(restoredModel, selected.Variant)) {
				providerID, modelID = selected.Provider, selected.Model
				defaultSelection.Provider, defaultSelection.Model, defaultSelection.Variant = providerID, modelID, selected.Variant
				result.DefaultSelection.Provider, result.DefaultSelection.Model, result.DefaultSelection.Variant = providerID, modelID, selected.Variant
			}
		case !errors.Is(selectionErr, session.ErrNotFound):
			return nil, fmt.Errorf("app: restore model selection: %w", selectionErr)
		}
	}
	todos := session.NewTodoService(db, repository)
	ws, err := workspace.New(info.Root)
	if err != nil {
		return nil, fmt.Errorf("app: workspace: %w", err)
	}
	skills, err := skill.Discover(skill.Options{GlobalConfig: paths.Config, ProjectRoot: info.Root, CWD: cwd})
	if err != nil {
		return nil, fmt.Errorf("app: skills: %w", err)
	}
	commands, err := command.Discover(command.Options{GlobalConfig: paths.Config, ProjectRoot: info.Root, CWD: cwd, Workspace: info.Root})
	if err != nil {
		return nil, fmt.Errorf("app: commands: %w", err)
	}
	result.Skills, result.Commands = skills, commands
	outputs, err := tool.NewOutputStore(tool.OutputConfig{Directory: filepath.Join(paths.Cache, "outputs"), PreviewBytes: 32 << 10, PreviewLines: 400, PerOutput: 64 << 20, Total: 256 << 20, Retention: 7 * 24 * time.Hour})
	if err != nil {
		return nil, fmt.Errorf("app: outputs: %w", err)
	}
	result.outputs = outputs
	changes := change.NewService(change.Config{})
	snapshots := snapshot.NewService(db, snapshot.Config{})
	processes, err := process.NewRunner(process.Config{Workspace: ws, OutputStore: tool.NewProcessOutputStore(outputs)})
	if err != nil {
		return nil, fmt.Errorf("app: process: %w", err)
	}
	var questionHandler question.Prompter
	if options.NonInteractive {
		questionHandler = questionPrompter{}
	}
	questions := question.NewBroker(questionHandler)
	policy := tool.DefaultReadOnlyPolicy()
	if options.Permission != "" {
		policy.Default = options.Permission
	}
	permissions := permission.NewBroker(policy, options.NonInteractive, nil)
	mcpConfigs, err := buildMCPConfigs(loaded.Config.MCP)
	if err != nil {
		return nil, err
	}
	var mcpManager *mcp.Manager
	var mcpDefinitions []mcp.ToolDefinition
	if len(mcpConfigs) != 0 {
		mcpManager, err = mcp.NewManager(mcpConfigs)
		if err != nil {
			return nil, fmt.Errorf("app: MCP config: %w", err)
		}
		result.mcp = mcpManager
		for _, item := range mcpConfigs {
			if item.Enabled {
				if err := mcpManager.Start(ctx, item.Name); err != nil {
					return nil, fmt.Errorf("app: MCP server %q startup: %w", item.Name, err)
				}
			}
		}
		mcpDefinitions, err = mcpManager.DiscoverTools(ctx)
		if err != nil {
			return nil, fmt.Errorf("app: MCP tool discovery: %w", err)
		}
	}
	lspConfigs, lspLanguages, err := buildLSPConfigs(loaded.Config.LSP, info.Root)
	if err != nil {
		return nil, err
	}
	var lspManager *lsp.Manager
	var lspClient tool.LSPClientFunc
	if len(lspConfigs) != 0 {
		lspManager, err = lsp.NewManager(lspConfigs)
		if err != nil {
			return nil, fmt.Errorf("app: LSP config: %w", err)
		}
		result.lsp = lspManager
		lspClient = func(ctx context.Context, name string) (tool.LSPClient, error) { return lspManager.Client(ctx, name) }
	}
	formatterRegistry, err := buildFormatters(loaded.Config.Formatters, info.Root)
	if err != nil {
		return nil, err
	}
	web := webfetch.New(webfetch.Config{AllowPrivate: loaded.Config.WebFetch.AllowPrivate})
	subagentExecutor := &appSubagentExecutor{sessions: sessions, project: info, providers: providerRegistry, defaultSelection: defaultSelection, live: live}
	subagents := subagent.NewManager(subagentExecutor, subagent.Config{OnProgress: func(task subagent.Task) {
		data, _ := json.Marshal(v1.TaskProgress{TaskID: task.ID, ToolCallID: task.ToolCallID, Agent: task.Agent, Status: string(task.Status), Usage: v1.Usage{InputTokens: task.Usage.InputTokens, OutputTokens: task.Usage.OutputTokens, TotalTokens: task.Usage.TotalTokens, ReasoningTokens: task.Usage.ReasoningTokens, CachedInputTokens: task.Usage.CachedInputTokens}, ToolUses: task.ToolUses})
		live.PublishEvent(v1.Event{Type: v1.EventTaskProgress, SessionID: task.ParentSession, Data: data})
	}})
	tools := tool.NewRegistry()
	for _, builtin := range []tool.Tool{tool.NewReadTool(tool.ReadConfig{}), tool.NewGlobTool(tool.GlobConfig{}), tool.NewGrepTool(tool.GrepConfig{}), tool.NewReadOutputTool(1 << 20)} {
		if err := tools.Register(builtin); err != nil {
			return nil, fmt.Errorf("app: register tool: %w", err)
		}
	}
	if err := tool.RegisterPhase6(tools, tool.Phase6Services{Changes: changes, Snapshots: snapshots, Processes: processes, Todos: todos, Questions: questions}); err != nil {
		return nil, fmt.Errorf("app: register tools: %w", err)
	}
	agentLookup := func(id string) (bool, error) {
		profile, err := combinedProfileResolver{modes: modes, agents: taskAgents}.GetProfile(id)
		return profile.ReadOnly, err
	}
	if err := tool.RegisterPhase9(tools, tool.Phase9Services{
		Skills: skills, MCP: mcpManager, MCPTools: mcpDefinitions, WebFetch: web,
		LSP: tool.LSPToolConfig{Client: lspClient, Languages: lspLanguages}, Formatters: formatterRegistry,
		Changes: changes, Snapshots: snapshots, Subagents: subagents, Agents: agentLookup,
	}); err != nil {
		return nil, fmt.Errorf("app: register phase 9 tools: %w", err)
	}
	toolSnapshot := tools.Materialize()
	guidance, _ := json.Marshal(toolSnapshot.Definitions())
	sources, err := systemcontext.Builtins(systemcontext.BuiltinOptions{
		AgentPrompt: "You are Parrot Coder, a local coding agent.", ToolGuidance: string(guidance),
		Skills:    skillMetadata(skills),
		ConfigDir: paths.Config, ProjectRoot: info.Root, WorkingDirectory: cwd, ProjectID: info.ID,
	})
	if err != nil {
		return nil, fmt.Errorf("app: context sources: %w", err)
	}
	contextRegistry, err := systemcontext.NewRegistry(sources...)
	if err != nil {
		return nil, fmt.Errorf("app: context registry: %w", err)
	}
	contexts := systemcontext.Manager{Registry: contextRegistry, Store: sessions}
	compactionRepository := compaction.NewRepository(db, repository)
	compactionService, err := compaction.NewService(compactionRepository,
		compaction.ProviderSummarizer{Providers: providerRegistry},
		compactionContextObserver{manager: contexts}, compaction.Config{})
	if err != nil {
		return nil, fmt.Errorf("app: compaction: %w", err)
	}
	result.compactions = compactionRepository
	runner, err := agent.NewRunner(agent.RunnerConfig{
		Sessions: sessions, Contexts: contexts, Profiles: combinedProfileResolver{modes: modes, agents: taskAgents}, Providers: providerRegistry,
		ToolSnapshot: func() tool.Snapshot { return toolSnapshot },
		ToolExecutor: func(snapshot tool.Snapshot) tool.Executor {
			return tool.Executor{Snapshot: snapshot, Permissions: permissions}
		},
		Workspace: ws, Outputs: outputs, Live: live, Compactor: compactionService,
		ToolPanicLogger: toolPanicLogger(),
	})
	if err != nil {
		return nil, fmt.Errorf("app: runner: %w", err)
	}
	drainer := statusDrainer{runner: runner, live: live}
	coordinator := agent.NewCoordinator(drainer, drainer)
	subagentExecutor.coordinator = coordinator
	result.coordinator = coordinator
	backend := &httpapi.DomainBackend{
		Version: options.Version, Sessions: sessions, Coordinator: coordinator, Agents: taskAgents, Modes: modes,
		Providers: providers, Permissions: permissions, Questions: questions, Todos: todos, Snapshots: snapshots,
		Workspace: ws, Events: repository, Live: live, DefaultSelection: defaultSelection,
		ProviderResolver: providerRegistry,
	}
	backend.CompactSessionFunc = func(ctx context.Context, sessionID string) (v1.Compaction, error) {
		for _, active := range coordinator.Active() {
			if active.SessionID == sessionID {
				return v1.Compaction{}, httpapi.ErrConflict
			}
		}
		selected, err := sessions.Get(ctx, sessionID)
		if err != nil {
			if errors.Is(err, session.ErrNotFound) {
				return v1.Compaction{}, httpapi.ErrNotFound
			}
			return v1.Compaction{}, err
		}
		_, model, err := providerRegistry.Resolve(selected.Provider, selected.Model)
		if err != nil {
			return v1.Compaction{}, err
		}
		profile, err := modes.GetProfile(selected.Agent)
		if err != nil {
			return v1.Compaction{}, err
		}
		definitions := make([]protocol.ToolDefinition, 0)
		for _, definition := range toolSnapshot.Definitions() {
			if profile.AllowsTool(definition.ID) {
				definitions = append(definitions, protocol.ToolDefinition{Name: definition.ID, Description: definition.Description, InputSchema: definition.Schema})
			}
		}
		instructions := profile.Prompt
		if len(profile.HardRules) > 0 {
			instructions += "\n\nHard rules:\n- " + strings.Join(profile.HardRules, "\n- ")
		}
		item, err := compactionService.Compact(ctx, compaction.Request{SessionID: sessionID, ProviderID: selected.Provider, Model: model, Instructions: instructions, Tools: definitions, Force: true})
		return v1.Compaction{Status: item.Status, AttemptID: item.AttemptID, RecordID: item.RecordID, SourceEpochID: item.SourceEpochID, TargetEpochID: item.TargetEpochID, HistoryCutoff: item.HistoryCutoff, Reason: item.Reason}, err
	}
	result.Backend = backend
	composed := &compositionBackend{DomainBackend: backend}
	apiServer := httpapi.New(composed, httpapi.Config{Logger: httpapi.LoggerFunc(func(_ context.Context, record httpapi.LogRecord) {
		diagnostics.Event("http_request",
			"request_id", record.RequestID, "method", record.Method, "path", record.Path,
			"status", record.Status, "duration_ms", record.Duration.Milliseconds(), "error_ref", record.ErrorRef,
		)
	})})
	handler := resumeHandler{next: apiServer, sessions: sessions, coordinator: coordinator, live: live}
	transport := inproc.New(handler)
	typed, err := client.New("http://parrot.local", transport)
	if err != nil {
		return nil, fmt.Errorf("app: client: %w", err)
	}
	result.Handler = handler
	result.Client = &Client{Client: typed, http: &http.Client{Transport: transport}}
	if _, err := result.Maintain(ctx); err != nil {
		return nil, fmt.Errorf("app: startup maintenance: %w", err)
	}
	return result, nil
}

// Project configuration may select models and override model metadata, but it
// cannot introduce endpoints, credentials, private-network access, or local
// executables. Those capabilities require a file in the user's global config.
func validateConfigTrust(loaded config.Result) error {
	kinds := make(map[string]config.SourceKind, len(loaded.Sources))
	for _, source := range loaded.Sources {
		kinds[source.Path] = source.Kind
	}
	for field, source := range loaded.Provenance {
		if kinds[source] != config.SourceProject {
			continue
		}
		restricted := strings.HasPrefix(field, "mcp.") ||
			strings.HasPrefix(field, "lsp.") ||
			strings.HasPrefix(field, "formatters.") ||
			field == "web_fetch.allow_private"
		if strings.HasPrefix(field, "providers.") {
			parts := strings.Split(field, ".")
			restricted = len(parts) < 4 || parts[2] != "models"
		}
		if restricted {
			return fmt.Errorf("app: config field %q in project source %q requires global configuration", field, source)
		}
	}
	return nil
}

type compactionContextObserver struct{ manager systemcontext.Manager }

func (o compactionContextObserver) ObserveFull(ctx context.Context) (compaction.FullContext, error) {
	observed, err := o.manager.ObserveFull(ctx)
	return compaction.FullContext{Baseline: observed.Baseline, Sources: observed.Sources}, err
}

type MaintenanceReport struct {
	OutputsRemoved             int
	TemporaryFilesRemoved      int
	SnapshotBlobsPruned        int64
	CompactionAttemptsRepaired int64
}

// Maintain performs bounded, idempotent cleanup. Durable events, messages,
// context epochs, and compaction records are never deleted.
func (a *App) Maintain(ctx context.Context) (MaintenanceReport, error) {
	started := time.Now()
	diagnostics.Event("maintenance_started")
	var report MaintenanceReport
	var maintenanceErr error
	if a.outputs != nil {
		removed, err := a.outputs.Maintain(time.Now(), 24*time.Hour, 2000)
		report.OutputsRemoved = removed
		maintenanceErr = errors.Join(maintenanceErr, err)
	}
	if a.Project.Root != "" {
		removed, err := cleanupSnapshotTemps(ctx, a.Project.Root, time.Now(), 24*time.Hour, 10000, 1000)
		report.TemporaryFilesRemoved = removed
		maintenanceErr = errors.Join(maintenanceErr, err)
	}
	if a.db != nil {
		result, err := a.db.SQL().ExecContext(ctx, `DELETE FROM snapshot_blob WHERE hash IN (
			SELECT b.hash FROM snapshot_blob b
			WHERE NOT EXISTS (SELECT 1 FROM snapshot_file f WHERE f.before_blob_hash=b.hash OR f.after_blob_hash=b.hash)
			ORDER BY b.hash LIMIT 1000
		)`)
		if err == nil {
			report.SnapshotBlobsPruned, err = result.RowsAffected()
		}
		maintenanceErr = errors.Join(maintenanceErr, err)
	}
	if a.compactions != nil {
		repaired, err := a.compactions.InterruptAbandoned(ctx, 1000, "process restarted")
		report.CompactionAttemptsRepaired = repaired
		maintenanceErr = errors.Join(maintenanceErr, err)
	}
	attributes := []any{
		"duration_ms", time.Since(started).Milliseconds(),
		"outputs_removed", report.OutputsRemoved,
		"temporary_files_removed", report.TemporaryFilesRemoved,
		"snapshot_blobs_pruned", report.SnapshotBlobsPruned,
		"compaction_attempts_repaired", report.CompactionAttemptsRepaired,
	}
	if maintenanceErr != nil {
		diagnostics.Error("maintenance_finished", append(attributes, "status", "error", "error_type", diagnostics.ErrorType(maintenanceErr))...)
	} else {
		diagnostics.Event("maintenance_finished", append(attributes, "status", "success")...)
	}
	return report, maintenanceErr
}

var errMaintenanceBound = errors.New("app: maintenance traversal bound reached")

func cleanupSnapshotTemps(ctx context.Context, root string, now time.Time, staleAfter time.Duration, visitLimit, removeLimit int) (int, error) {
	visited, removed := 0, 0
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		visited++
		if visited > visitLimit {
			return errMaintenanceBound
		}
		if entry.IsDir() || removed >= removeLimit || !strings.HasPrefix(entry.Name(), ".parrot-snapshot-") {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || now.Sub(info.ModTime()) <= staleAfter {
			return nil
		}
		if err := os.Remove(path); err != nil {
			return err
		}
		removed++
		return nil
	})
	if errors.Is(err, errMaintenanceBound) {
		return removed, nil
	}
	return removed, err
}

// toolPanicLogger routes the original tool-goroutine stack into the process
// diagnostics log. The diagnostics package makes this a no-op when process
// diagnostics could not be initialized.
func toolPanicLogger() func(context.Context, string, string, any, []byte) {
	return func(_ context.Context, sessionID, toolName string, recovered any, stack []byte) {
		diagnostics.PanicWithStack("agent_tool_call", recovered, stack,
			"session_id", sessionID, "tool", toolName,
		)
	}
}

func (a *App) Close() error {
	if a == nil {
		return nil
	}
	a.closeOnce.Do(func() {
		started := time.Now()
		activeSessions := 0
		if a.coordinator != nil {
			active := a.coordinator.Active()
			activeSessions = len(active)
			diagnostics.Event("app_close_started", "active_sessions", activeSessions)
			for _, active := range active {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				a.closeErr = errors.Join(a.closeErr, a.coordinator.Interrupt(ctx, active.SessionID))
				cancel()
			}
		} else {
			diagnostics.Event("app_close_started", "active_sessions", activeSessions)
		}
		if a.mcp != nil {
			a.closeErr = errors.Join(a.closeErr, a.mcp.Close())
		}
		if a.lsp != nil {
			a.closeErr = errors.Join(a.closeErr, a.lsp.Close())
		}
		if a.db != nil {
			a.closeErr = errors.Join(a.closeErr, a.db.Close())
		}
		attributes := []any{"active_sessions", activeSessions, "duration_ms", time.Since(started).Milliseconds()}
		if a.closeErr != nil {
			diagnostics.Error("app_close_finished", append(attributes, "status", "error", "error_type", diagnostics.ErrorType(a.closeErr))...)
		} else {
			diagnostics.Event("app_close_finished", append(attributes, "status", "success")...)
		}
	})
	return a.closeErr
}

type compositionBackend struct {
	*httpapi.DomainBackend
}

func (b *compositionBackend) Wake(sessionID string) {
	b.Coordinator.Wake(sessionID)
}

type statusDrainer struct {
	runner agent.Drainer
	live   *event.Broker
}

func (d statusDrainer) Drain(ctx context.Context, sessionID string) error {
	return d.runner.Drain(ctx, sessionID)
}

func (d statusDrainer) LifecycleStarted(sessionID string) {
	diagnostics.Event("session_run_started", "session_id", sessionID)
	data, _ := json.Marshal(v1.SessionStatus{Kind: "running"})
	d.live.PublishEvent(v1.Event{Type: v1.EventSessionStatus, SessionID: sessionID, Data: data})
}

func (d statusDrainer) LifecycleComplete(sessionID string, err error) {
	status := v1.SessionStatus{Kind: "idle"}
	if err == context.Canceled {
		status.Kind = "interrupted"
	} else if err != nil {
		status.Kind = "error"
		status.ErrorCode = "runner_error"
	}
	data, _ := json.Marshal(status)
	d.live.PublishEvent(v1.Event{Type: v1.EventSessionStatus, SessionID: sessionID, Data: data})
	attributes := []any{"session_id", sessionID, "status", status.Kind}
	if err != nil {
		attributes = append(attributes, "error_type", diagnostics.ErrorType(err))
		if err != context.Canceled {
			diagnostics.Error("session_run_finished", attributes...)
			return
		}
	}
	diagnostics.Event("session_run_finished", attributes...)
}

type questionPrompter struct{}

func (p questionPrompter) Prompt(context.Context, question.Pending) (question.Response, error) {
	return question.Response{}, question.ErrRejected
}

type resumeHandler struct {
	next        http.Handler
	sessions    *session.Service
	coordinator *agent.Coordinator
	live        *event.Broker
}

func (h resumeHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if recovered := recover(); recovered != nil {
			diagnostics.Panic("application_http_handler", recovered)
			http.Error(w, "internal server error", http.StatusInternalServerError)
		}
	}()
	const prefix = "/api/v1/sessions/"
	if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, prefix) && strings.HasSuffix(r.URL.Path, "/events") && r.URL.Query().Get("after") == "" {
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, prefix), "/events")
		if id != "" && !strings.Contains(id, "/") {
			latest, err := h.sessions.LatestSequence(r.Context(), id)
			if err == nil && latest >= 1000 {
				clone := r.Clone(r.Context())
				query := clone.URL.Query()
				query.Set("after", strconv.FormatInt(latest-1000, 10))
				clone.URL.RawQuery = query.Encode()
				r = clone
			}
		}
	}
	if r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, prefix) && strings.HasSuffix(r.URL.Path, "/resume") {
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, prefix), "/resume")
		if id == "" || strings.Contains(id, "/") {
			http.NotFound(w, r)
			return
		}
		if _, err := h.sessions.Get(r.Context(), id); err != nil {
			http.NotFound(w, r)
			return
		}
		h.coordinator.Wake(id)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	h.next.ServeHTTP(w, r)
}

// BuildProviders creates configured provider clients. Environment credentials
// take precedence over credentials stored under the provider ID.
func modelHasVariant(model provider.Model, name string) bool {
	_, ok := model.Capabilities.Variants[name]
	return ok
}

func BuildProviders(ctx context.Context, cfg config.Config, credentials auth.Store, httpClient *http.Client) ([]provider.Provider, error) {
	openAI := &auth.OpenAI{HTTPClient: httpClient}
	tokens := auth.NewTokenSource(openAI, credentials, "chatgpt")
	chatgpt, err := provider.NewChatGPT(provider.ChatGPTOptions{TokenSource: tokens, HTTPClient: httpClient})
	if err != nil {
		return nil, fmt.Errorf("app: ChatGPT provider: %w", err)
	}
	result := []provider.Provider{chatgpt}
	ids := make([]string, 0, len(cfg.Providers))
	for id := range cfg.Providers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		item := cfg.Providers[id]
		if item.HeaderTimeoutMS != nil && *item.HeaderTimeoutMS < 0 {
			return nil, fmt.Errorf("app: provider %q header timeout cannot be negative", id)
		}
		const maxDurationMilliseconds = int64(^uint64(0)>>1) / int64(time.Millisecond)
		if item.HeaderTimeoutMS != nil && int64(*item.HeaderTimeoutMS) > maxDurationMilliseconds {
			return nil, fmt.Errorf("app: provider %q header timeout is too large", id)
		}
		headerTimeout := time.Duration(0)
		if item.HeaderTimeoutMS != nil {
			headerTimeout = time.Duration(*item.HeaderTimeoutMS) * time.Millisecond
		} else if id == "openai" {
			headerTimeout = 10 * time.Second
		}
		if item.Type != "" && item.Type != "compatible" && item.Type != "openai-compatible" {
			return nil, fmt.Errorf("app: provider %q has unsupported type %q", id, item.Type)
		}
		key := ""
		if item.APIKeyEnv != "" {
			key = os.Getenv(item.APIKeyEnv)
		}
		if key == "" {
			stored, getErr := credentials.Get(ctx, id)
			if getErr == nil && stored.Type == auth.CredentialAPIKey && stored.APIKey != nil {
				key = stored.APIKey.Key.Value()
			} else if getErr != nil && !errors.Is(getErr, auth.ErrCredentialNotFound) {
				return nil, fmt.Errorf("app: provider %q credential: %w", id, getErr)
			}
		}
		if key == "" {
			where := "credential store"
			if item.APIKeyEnv != "" {
				where = "environment variable " + item.APIKeyEnv + " or credential store entry " + id
			}
			return nil, fmt.Errorf("app: provider %q requires an API key from %s", id, where)
		}
		models := make([]provider.Model, 0, len(item.Models))
		modelIDs := make([]string, 0, len(item.Models))
		for modelID := range item.Models {
			modelIDs = append(modelIDs, modelID)
		}
		sort.Strings(modelIDs)
		for _, modelID := range modelIDs {
			model := item.Models[modelID]
			name := model.Name
			if name == "" {
				name = modelID
			}
			variants := make(map[string]provider.Variant, len(model.Variants))
			for variantName, variant := range model.Variants {
				variants[variantName] = provider.Variant{ReasoningEffort: variant.ReasoningEffort}
			}
			models = append(models, provider.Model{ID: modelID, Name: name, ContextWindow: model.Context, MaxOutputTokens: model.MaxTokens, Capabilities: provider.Capabilities{Tools: model.Tools, Reasoning: model.Reasoning, Output: append([]string(nil), model.Output...), Variants: variants}})
		}
		compatible, createErr := provider.NewOpenAICompatible(provider.OpenAICompatibleOptions{
			ID: id, BaseURL: item.BaseURL, Protocol: provider.CompatibleProtocol(item.Protocol), APIKey: auth.Secret(key),
			Headers: item.Headers, AllowInsecureLocalhost: item.AllowInsecureLocalhost, Models: models, HTTPClient: httpClient,
			HeaderTimeout: headerTimeout,
		})
		if createErr != nil {
			return nil, fmt.Errorf("app: provider %q: %w", id, createErr)
		}
		result = append(result, compatible)
	}
	return result, nil
}

func selectModel(configured, override string, providers []provider.Provider) (string, string, error) {
	value := override
	if value == "" {
		value = configured
	}
	if value == "" {
		return "", "", errors.New("app: no default model configured; set model to provider/model in parrot.jsonc or pass --model")
	}
	providerID, modelID, found := strings.Cut(value, "/")
	if !found {
		defaultProvider, _, ok := strings.Cut(configured, "/")
		if !ok || defaultProvider == "" {
			return "", "", fmt.Errorf("app: model %q must include its provider as provider/model", value)
		}
		providerID, modelID = defaultProvider, value
	}
	if providerID == "" || modelID == "" {
		return "", "", fmt.Errorf("app: invalid model selection %q; expected provider/model", value)
	}
	registry, err := agent.NewProviderRegistry(providers...)
	if err != nil {
		return "", "", err
	}
	if _, _, err := registry.Resolve(providerID, modelID); err != nil {
		return "", "", err
	}
	return providerID, modelID, nil
}

func buildMCPConfigs(configs map[string]config.MCP) ([]mcp.Config, error) {
	names := sortedKeys(configs)
	result := make([]mcp.Config, 0, len(names))
	for _, name := range names {
		item := configs[name]
		if item.StartupTimeoutMS < 0 || item.CallTimeoutMS < 0 {
			return nil, fmt.Errorf("app: MCP server %q timeout integers cannot be negative", name)
		}
		if item.Transport == string(mcp.TransportStdio) {
			if err := requireExecutable("MCP server "+strconv.Quote(name)+" command", item.Command); err != nil {
				return nil, err
			}
		}
		mapped := mcp.Config{
			Name: name, Transport: mcp.Transport(item.Transport), Enabled: item.Enabled,
			Command: item.Command, Args: append([]string(nil), item.Args...), Env: cloneMap(item.Env), Cwd: item.CWD,
			URL: item.URL, Headers: cloneMap(item.Headers), AllowInsecureLocalhost: item.AllowInsecureLocalhost,
			StartupTimeout: time.Duration(item.StartupTimeoutMS) * time.Millisecond,
			CallTimeout:    time.Duration(item.CallTimeoutMS) * time.Millisecond,
		}
		if err := mapped.Validate(); err != nil {
			return nil, fmt.Errorf("app: MCP server %q config: %w", name, err)
		}
		result = append(result, mapped)
	}
	return result, nil
}

func buildLSPConfigs(configs map[string]config.LSP, root string) ([]lsp.Config, map[string]map[string]string, error) {
	names := sortedKeys(configs)
	result := make([]lsp.Config, 0, len(names))
	languages := make(map[string]map[string]string, len(names))
	for _, name := range names {
		item := configs[name]
		if item.TimeoutMS < 0 {
			return nil, nil, fmt.Errorf("app: LSP server %q timeout integer cannot be negative", name)
		}
		if err := requireExecutable("LSP server "+strconv.Quote(name)+" command", item.Command); err != nil {
			return nil, nil, err
		}
		mapping := make(map[string]string)
		for extension, language := range item.Languages {
			extension = normalizeExtension(extension)
			if extension == "" || language == "" {
				return nil, nil, fmt.Errorf("app: LSP server %q has an empty extension or language", name)
			}
			mapping[extension] = language
		}
		for _, extension := range item.Extensions {
			extension = normalizeExtension(extension)
			if extension == "" {
				return nil, nil, fmt.Errorf("app: LSP server %q has an empty extension", name)
			}
			if mapping[extension] == "" {
				mapping[extension] = strings.TrimPrefix(extension, ".")
			}
		}
		languages[name] = mapping
		result = append(result, lsp.Config{Name: name, Command: item.Command, Args: append([]string(nil), item.Args...), Workspace: root, Environment: cloneMap(item.Env), Timeout: time.Duration(item.TimeoutMS) * time.Millisecond})
	}
	return result, languages, nil
}

func buildFormatters(configs map[string]config.Formatter, root string) (*formatter.Registry, error) {
	if len(configs) == 0 {
		return nil, nil
	}
	items := make([]formatter.Formatter, 0, len(configs))
	for _, name := range sortedKeys(configs) {
		item := configs[name]
		if len(item.Command) == 0 {
			return nil, fmt.Errorf("app: formatter %q command argv is required", name)
		}
		if err := requireExecutable("formatter "+strconv.Quote(name)+" command", item.Command[0]); err != nil {
			return nil, err
		}
		items = append(items, formatter.Formatter{Name: name, Extensions: append([]string(nil), item.Extensions...), Command: append([]string(nil), item.Command...), Mode: formatter.Mode(item.Mode)})
	}
	registry, err := formatter.NewRegistry(formatter.Config{Workspace: root}, items...)
	if err != nil {
		return nil, fmt.Errorf("app: formatter config: %w", err)
	}
	return registry, nil
}

func requireExecutable(label, path string) error {
	if path == "" || !filepath.IsAbs(path) {
		return fmt.Errorf("app: %s must be an absolute executable path", label)
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("app: %s: %w", label, err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("app: %s is not an executable regular file", label)
	}
	return nil
}

func sortedKeys[T any](values map[string]T) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func cloneMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	result := make(map[string]string, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func normalizeExtension(extension string) string {
	extension = strings.ToLower(strings.TrimSpace(extension))
	if extension != "" && !strings.HasPrefix(extension, ".") {
		extension = "." + extension
	}
	return extension
}

func skillMetadata(registry *skill.Registry) string {
	items := registry.List()
	if len(items) == 0 {
		return ""
	}
	var output strings.Builder
	output.WriteString("Available skills (load one with the skill tool):")
	for _, item := range items {
		fmt.Fprintf(&output, "\n- %s: %s", item.Name, item.Description)
	}
	return output.String()
}

type appSubagentExecutor struct {
	sessions         *session.Service
	coordinator      *agent.Coordinator
	project          project.Info
	providers        *agent.ProviderRegistry
	defaultSelection session.Selection
	live             *event.Broker
}

func (e *appSubagentExecutor) Execute(ctx context.Context, execution subagent.Execution) (string, error) {
	if e.coordinator == nil {
		return "", errors.New("app: subagent coordinator is unavailable")
	}
	parent, err := e.sessions.Get(ctx, execution.ParentSession)
	if err != nil {
		return "", fmt.Errorf("app: subagent parent session: %w", err)
	}
	if parent.ProjectID != e.project.ID {
		return "", errors.New("app: subagent parent belongs to another project")
	}
	selection := e.defaultSelection
	if parent.Provider != "" && parent.Model != "" {
		selection.Provider, selection.Model = parent.Provider, parent.Model
	}
	selection.Agent = execution.Request.Agent
	if execution.Request.Model != "" {
		if providerID, modelID, found := strings.Cut(execution.Request.Model, "/"); found {
			selection.Provider, selection.Model = providerID, modelID
		} else {
			selection.Model = execution.Request.Model
		}
	}
	if selection.Provider == "" || selection.Model == "" {
		return "", errors.New("app: subagent has no default model")
	}
	if _, _, err := e.providers.Resolve(selection.Provider, selection.Model); err != nil {
		return "", fmt.Errorf("app: subagent model: %w", err)
	}
	title := "Subtask " + execution.TaskID + " [" + execution.Request.Agent + "]"
	child, err := e.sessions.CreateSelected(ctx, session.CreateParams{ProjectID: parent.ProjectID, Title: title}, selection)
	if err != nil {
		return "", err
	}
	messageID, err := id.New("msg")
	if err != nil {
		return "", err
	}
	if _, err := e.sessions.Admit(ctx, child.ID, session.AdmitParams{MessageID: messageID, Content: execution.Request.Prompt, Delivery: session.DeliverySteer}); err != nil {
		return "", err
	}
	var progressDone chan struct{}
	stopProgress := func() {}
	if e.live != nil && execution.ReportProgress != nil {
		events, unsubscribe := e.live.Subscribe(child.ID, 128)
		progressDone = make(chan struct{})
		stop := make(chan struct{})
		stopProgress = func() { close(stop); unsubscribe(); <-progressDone }
		go func() {
			defer close(progressDone)
			for {
				select {
				case item, ok := <-events:
					if !ok {
						return
					}
					reportSubagentEvent(execution.ReportProgress, item)
				case <-stop:
					return
				}
			}
		}()
		defer stopProgress()
	}
	if err := e.coordinator.Resume(ctx, child.ID); err != nil {
		if ctx.Err() != nil {
			cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = e.coordinator.Interrupt(cleanup, child.ID)
			cancel()
		}
		return "", err
	}
	messages, err := e.sessions.ListMessages(ctx, child.ID)
	if err != nil {
		return "", err
	}
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != "assistant" {
			continue
		}
		if messages[i].Error != "" {
			return messages[i].Content, errors.New(messages[i].Error)
		}
		return messages[i].Content, nil
	}
	return "", errors.New("app: subagent produced no assistant output")
}

func reportSubagentEvent(report func(subagent.Progress), item v1.Event) {
	switch item.Type {
	case v1.EventSessionStatus:
		var status v1.SessionStatus
		if json.Unmarshal(item.Data, &status) != nil {
			return
		}
		if status.Kind == "tool_call_complete" {
			report(subagent.Progress{ToolUses: 1})
		} else if status.Kind == "usage" && status.Usage != nil {
			report(subagent.Progress{Usage: subagent.Usage{InputTokens: status.Usage.InputTokens, OutputTokens: status.Usage.OutputTokens, TotalTokens: status.Usage.TotalTokens, ReasoningTokens: status.Usage.ReasoningTokens, CachedInputTokens: status.Usage.CachedInputTokens}})
		}
	}
}

type combinedProfileResolver struct {
	modes  *mode.Registry
	agents *agent.Registry
}

func (r combinedProfileResolver) GetProfile(id string) (agent.Profile, error) {
	if profile, err := r.modes.GetProfile(id); err == nil {
		return profile, nil
	}
	return r.agents.Get(id)
}
