// Package app composes Parrot's long-lived application services.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	"github.com/amirulashraf/parrot-coder/internal/config"
	"github.com/amirulashraf/parrot-coder/internal/event"
	"github.com/amirulashraf/parrot-coder/internal/httpapi"
	"github.com/amirulashraf/parrot-coder/internal/permission"
	"github.com/amirulashraf/parrot-coder/internal/process"
	"github.com/amirulashraf/parrot-coder/internal/project"
	"github.com/amirulashraf/parrot-coder/internal/provider"
	"github.com/amirulashraf/parrot-coder/internal/question"
	"github.com/amirulashraf/parrot-coder/internal/session"
	"github.com/amirulashraf/parrot-coder/internal/snapshot"
	"github.com/amirulashraf/parrot-coder/internal/store"
	"github.com/amirulashraf/parrot-coder/internal/systemcontext"
	"github.com/amirulashraf/parrot-coder/internal/tool"
	"github.com/amirulashraf/parrot-coder/internal/transport/inproc"
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
	Permission     permission.Decision
	NonInteractive bool
	AllowNoModel   bool
	HTTPClient     *http.Client
}

// App owns every long-lived dependency and the optional HTTP handler. Local
// callers use Client, whose transport invokes Handler without binding a socket.
type App struct {
	Paths       appdirs.Paths
	Project     project.Info
	Config      config.Result
	Credentials auth.Store
	Client      *Client
	Handler     http.Handler
	Backend     *httpapi.DomainBackend

	db          *store.DB
	coordinator *agent.Coordinator
	closeOnce   sync.Once
	closeErr    error
}

// Client is the typed application client used by local commands. It delegates
// the public v1 contract to client.Client and adds the explicit resume action.
type Client struct {
	*client.Client
	http *http.Client
}

func (c *Client) SelectSession(ctx context.Context, sessionID, agentID, model string) error {
	body, err := json.Marshal(struct {
		Agent string `json:"agent,omitempty"`
		Model string `json:"model,omitempty"`
	}{agentID, model})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://parrot.local/api/v1/sessions/"+url.PathEscape(sessionID)+"/selection", strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", v1.MediaTypeJSON)
	response, err := c.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		return fmt.Errorf("select session model: HTTP %d", response.StatusCode)
	}
	return nil
}

func (c *Client) Resume(ctx context.Context, sessionID string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://parrot.local/api/v1/sessions/"+sessionID+"/resume", nil)
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
	agents, err := agent.NewRegistry()
	if err != nil {
		return nil, fmt.Errorf("app: agents: %w", err)
	}
	agentID := options.Agent
	if agentID == "" {
		agentID = agent.BuildID
	}
	if _, err := agents.Get(agentID); err != nil {
		return nil, fmt.Errorf("app: %w", err)
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
	result := &App{Paths: paths, Project: info, Config: loaded, Credentials: credentials, db: db}
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
	todos := session.NewTodoService(db)
	ws, err := workspace.New(info.Root)
	if err != nil {
		return nil, fmt.Errorf("app: workspace: %w", err)
	}
	outputs, err := tool.NewOutputStore(tool.OutputConfig{Directory: filepath.Join(paths.Cache, "outputs"), PreviewBytes: 32 << 10, PreviewLines: 400, PerOutput: 64 << 20, Total: 256 << 20, Retention: 7 * 24 * time.Hour})
	if err != nil {
		return nil, fmt.Errorf("app: outputs: %w", err)
	}
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
	tools := tool.NewRegistry()
	for _, builtin := range []tool.Tool{tool.NewReadTool(tool.ReadConfig{}), tool.NewGlobTool(tool.GlobConfig{}), tool.NewGrepTool(tool.GrepConfig{}), tool.NewReadOutputTool(1 << 20)} {
		if err := tools.Register(builtin); err != nil {
			return nil, fmt.Errorf("app: register tool: %w", err)
		}
	}
	if err := tool.RegisterPhase6(tools, tool.Phase6Services{Changes: changes, Snapshots: snapshots, Processes: processes, Todos: todos, Questions: questions}); err != nil {
		return nil, fmt.Errorf("app: register tools: %w", err)
	}
	toolSnapshot := tools.Materialize()
	guidance, _ := json.Marshal(toolSnapshot.Definitions())
	sources, err := systemcontext.Builtins(systemcontext.BuiltinOptions{
		AgentPrompt: "You are Parrot Coder, a local coding agent.", ToolGuidance: string(guidance),
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
	runner, err := agent.NewRunner(agent.RunnerConfig{
		Sessions: sessions, Contexts: contexts, Agents: agents, Providers: providerRegistry,
		ToolSnapshot: func() tool.Snapshot { return toolSnapshot },
		ToolExecutor: func(snapshot tool.Snapshot) tool.Executor {
			return tool.Executor{Snapshot: snapshot, Permissions: permissions}
		},
		Workspace: ws, Outputs: outputs, Live: live,
	})
	if err != nil {
		return nil, fmt.Errorf("app: runner: %w", err)
	}
	coordinator := agent.NewCoordinator(statusDrainer{runner: runner, live: live})
	result.coordinator = coordinator
	backend := &httpapi.DomainBackend{
		Version: options.Version, Sessions: sessions, Coordinator: coordinator, Agents: agents,
		Providers: providers, Permissions: permissions, Questions: questions, Snapshots: snapshots,
		Workspace: ws, Events: repository, Live: live,
	}
	result.Backend = backend
	composed := &compositionBackend{DomainBackend: backend, sessions: sessions, selection: session.Selection{Agent: agentID, Provider: providerID, Model: modelID}}
	apiServer := httpapi.New(composed, httpapi.Config{})
	handler := resumeHandler{next: apiServer, sessions: sessions, coordinator: coordinator, agents: agents, providers: providerRegistry, live: live}
	transport := inproc.New(handler)
	typed, err := client.New("http://parrot.local", transport)
	if err != nil {
		return nil, fmt.Errorf("app: client: %w", err)
	}
	result.Handler = handler
	result.Client = &Client{Client: typed, http: &http.Client{Transport: transport}}
	return result, nil
}

func (a *App) Close() error {
	if a == nil {
		return nil
	}
	a.closeOnce.Do(func() {
		if a.coordinator != nil {
			for _, active := range a.coordinator.Active() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				a.closeErr = errors.Join(a.closeErr, a.coordinator.Interrupt(ctx, active.SessionID))
				cancel()
			}
		}
		if a.db != nil {
			a.closeErr = errors.Join(a.closeErr, a.db.Close())
		}
	})
	return a.closeErr
}

type compositionBackend struct {
	*httpapi.DomainBackend
	sessions  *session.Service
	selection session.Selection
}

func (b *compositionBackend) CreateSession(ctx context.Context, request v1.CreateSessionRequest) (v1.Session, error) {
	item, err := b.DomainBackend.CreateSession(ctx, request)
	if err != nil {
		return item, err
	}
	if err := b.sessions.SetSelection(ctx, item.ID, b.selection); err != nil {
		_ = b.sessions.Delete(ctx, item.ID)
		return v1.Session{}, err
	}
	return b.DomainBackend.GetSession(ctx, item.ID)
}

func (b *compositionBackend) Wake(sessionID string) {
	data, _ := json.Marshal(v1.SessionStatus{Kind: "running"})
	b.Live.PublishEvent(v1.Event{Type: v1.EventSessionStatus, SessionID: sessionID, Data: data})
	b.Coordinator.Wake(sessionID)
}

type statusDrainer struct {
	runner *agent.Runner
	live   *event.Broker
}

func (d statusDrainer) Drain(ctx context.Context, sessionID string) error {
	err := d.runner.Drain(ctx, sessionID)
	status := v1.SessionStatus{Kind: "idle"}
	if err != nil && !errors.Is(err, context.Canceled) {
		status.Kind = "error"
		status.ErrorCode = "runner_error"
	}
	data, _ := json.Marshal(status)
	d.live.PublishEvent(v1.Event{Type: v1.EventSessionStatus, SessionID: sessionID, Data: data})
	return err
}

type questionPrompter struct{}

func (p questionPrompter) Prompt(context.Context, question.Pending) (question.Response, error) {
	return question.Response{}, question.ErrRejected
}

type resumeHandler struct {
	next        http.Handler
	sessions    *session.Service
	coordinator *agent.Coordinator
	agents      *agent.Registry
	providers   *agent.ProviderRegistry
	live        *event.Broker
}

func (h resumeHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
	if r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, prefix) && strings.HasSuffix(r.URL.Path, "/selection") {
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, prefix), "/selection")
		if id == "" || strings.Contains(id, "/") {
			http.NotFound(w, r)
			return
		}
		var request struct {
			Agent string `json:"agent"`
			Model string `json:"model"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
		decoder.DisallowUnknownFields()
		if r.Header.Get("Content-Type") != v1.MediaTypeJSON || decoder.Decode(&request) != nil {
			http.Error(w, "invalid selection", http.StatusBadRequest)
			return
		}
		current, err := h.sessions.Get(r.Context(), id)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		selection := session.Selection{Agent: current.Agent, Provider: current.Provider, Model: current.Model}
		if request.Agent != "" {
			selection.Agent = request.Agent
		}
		if request.Model != "" {
			if providerID, modelID, ok := strings.Cut(request.Model, "/"); ok {
				selection.Provider, selection.Model = providerID, modelID
			} else {
				selection.Model = request.Model
			}
		}
		if _, err := h.agents.Get(selection.Agent); err != nil {
			http.Error(w, "unknown agent", http.StatusBadRequest)
			return
		}
		if _, _, err := h.providers.Resolve(selection.Provider, selection.Model); err != nil {
			http.Error(w, "unknown model", http.StatusBadRequest)
			return
		}
		if err := h.sessions.SetSelection(r.Context(), id, selection); err != nil {
			http.Error(w, "selection failed", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
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
		data, _ := json.Marshal(v1.SessionStatus{Kind: "running"})
		h.live.PublishEvent(v1.Event{Type: v1.EventSessionStatus, SessionID: id, Data: data})
		h.coordinator.Wake(id)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	h.next.ServeHTTP(w, r)
}

// BuildProviders creates configured provider clients. Environment credentials
// take precedence over credentials stored under the provider ID.
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
			models = append(models, provider.Model{ID: modelID, Name: name, ContextWindow: model.Context, MaxOutputTokens: model.MaxTokens, Capabilities: provider.Capabilities{Tools: model.Tools, Reasoning: model.Reasoning, Output: append([]string(nil), model.Output...)}})
		}
		compatible, createErr := provider.NewOpenAICompatible(provider.OpenAICompatibleOptions{
			ID: id, BaseURL: item.BaseURL, Protocol: provider.CompatibleProtocol(item.Protocol), APIKey: auth.Secret(key),
			Headers: item.Headers, AllowInsecureLocalhost: item.AllowInsecureLocalhost, Models: models, HTTPClient: httpClient,
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
