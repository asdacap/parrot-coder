package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

type Manager struct {
	mu            sync.RWMutex
	servers       map[string]*managedServer
	toolTargets   map[string]toolTarget
	notifications chan Notification
	closed        bool
}

type managedServer struct {
	mu        sync.Mutex
	config    Config
	state     State
	client    *protocolClient
	startedAt time.Time
	lastError string
	manager   *Manager
}

type toolTarget struct {
	server string
	tool   string
}

func NewManager(configs []Config) (*Manager, error) {
	m := &Manager{
		servers:       make(map[string]*managedServer, len(configs)),
		toolTargets:   make(map[string]toolTarget),
		notifications: make(chan Notification, 128),
	}
	for _, input := range configs {
		config, err := input.validated()
		if err != nil {
			return nil, err
		}
		if _, exists := m.servers[config.Name]; exists {
			return nil, fmt.Errorf("mcp: duplicate server name %q", config.Name)
		}
		m.servers[config.Name] = &managedServer{config: config, state: StateStopped, manager: m}
	}
	return m, nil
}

// StartAll starts every enabled server. Independent servers start concurrently.
func (m *Manager) StartAll(ctx context.Context) error {
	m.mu.RLock()
	if m.closed {
		m.mu.RUnlock()
		return errors.New("mcp: manager is closed")
	}
	names := make([]string, 0, len(m.servers))
	for name, server := range m.servers {
		if server.config.Enabled {
			names = append(names, name)
		}
	}
	m.mu.RUnlock()
	sort.Strings(names)
	var wg sync.WaitGroup
	errs := make(chan error, len(names))
	for _, name := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := m.Start(ctx, name); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	var result []error
	for err := range errs {
		result = append(result, err)
	}
	return errors.Join(result...)
}

// Start lazily starts one enabled server and performs the MCP handshake.
func (m *Manager) Start(ctx context.Context, name string) error {
	m.mu.RLock()
	closed := m.closed
	m.mu.RUnlock()
	if closed {
		return errors.New("mcp: manager is closed")
	}
	server, err := m.server(name)
	if err != nil {
		return err
	}
	if !server.config.Enabled {
		return fmt.Errorf("mcp: server %q is disabled", name)
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	m.mu.RLock()
	closed = m.closed
	m.mu.RUnlock()
	if closed {
		return errors.New("mcp: manager is closed")
	}
	if server.state == StateRunning && server.client != nil {
		return nil
	}
	return server.startLocked(ctx)
}

func (s *managedServer) startLocked(ctx context.Context) error {
	s.state = StateStarting
	s.lastError = ""
	s.startedAt = time.Time{}
	if err := ctx.Err(); err != nil {
		s.state = StateFailed
		s.lastError = err.Error()
		return err
	}
	startCtx, cancel := context.WithTimeout(ctx, s.config.StartupTimeout)
	defer cancel()
	var transport endpoint
	var err error
	switch s.config.Transport {
	case TransportStdio:
		transport, err = startStdio(s.config)
	case TransportHTTP:
		transport, err = startHTTP(s.config)
	default:
		err = errors.New("mcp: invalid transport")
	}
	if err == nil {
		var client *protocolClient
		client, err = initialize(startCtx, s.config, transport)
		if err == nil {
			s.client = client
			s.state = StateRunning
			s.startedAt = time.Now().UTC()
			go s.monitor(client)
			return nil
		}
	}
	if transport != nil {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = transport.close(cleanupCtx)
		cleanupCancel()
	}
	s.client = nil
	s.state = StateFailed
	s.lastError = err.Error()
	return err
}

func (s *managedServer) monitor(client *protocolClient) {
	for {
		select {
		case notification := <-client.endpoint.notificationChannel():
			select {
			case s.manager.notifications <- notification:
			default:
			}
		case <-client.endpoint.done():
			s.mu.Lock()
			if s.client == client {
				s.client = nil
				s.state = StateFailed
				s.lastError = "mcp: transport closed unexpectedly"
			}
			s.mu.Unlock()
			return
		}
	}
}

func (m *Manager) Restart(ctx context.Context, name string) error {
	if err := m.Stop(ctx, name); err != nil {
		return err
	}
	return m.Start(ctx, name)
}

func (m *Manager) Stop(ctx context.Context, name string) error {
	server, err := m.server(name)
	if err != nil {
		return err
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.client == nil {
		server.state = StateStopped
		server.lastError = ""
		server.startedAt = time.Time{}
		return nil
	}
	server.state = StateStopping
	client := server.client
	err = client.endpoint.close(ctx)
	server.client = nil
	server.state = StateStopped
	server.lastError = ""
	server.startedAt = time.Time{}
	return err
}

func (m *Manager) StopAll(ctx context.Context) error {
	m.mu.RLock()
	names := make([]string, 0, len(m.servers))
	for name := range m.servers {
		names = append(names, name)
	}
	m.mu.RUnlock()
	sort.Strings(names)
	var errs []error
	for _, name := range names {
		if err := m.Stop(ctx, name); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (m *Manager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	m.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return m.StopAll(ctx)
}

func (m *Manager) Status(name string) (Status, error) {
	server, err := m.server(name)
	if err != nil {
		return Status{}, err
	}
	return server.status(), nil
}

// Health returns nil only while the named server has a live initialized
// transport.
func (m *Manager) Health(name string) error {
	status, err := m.Status(name)
	if err != nil {
		return err
	}
	if status.Healthy {
		return nil
	}
	if status.LastError != "" {
		return errors.New(status.LastError)
	}
	return fmt.Errorf("mcp: server %q is %s", name, status.State)
}

func (m *Manager) Statuses() []Status {
	m.mu.RLock()
	servers := make([]*managedServer, 0, len(m.servers))
	for _, server := range m.servers {
		servers = append(servers, server)
	}
	m.mu.RUnlock()
	result := make([]Status, 0, len(servers))
	for _, server := range servers {
		result = append(result, server.status())
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

func (s *managedServer) status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	status := Status{
		Name:      s.config.Name,
		Transport: s.config.Transport,
		State:     s.state,
		Healthy:   s.state == StateRunning && s.client != nil,
		StartedAt: s.startedAt,
		LastError: s.lastError,
	}
	if s.client != nil {
		status.ProtocolVersion = s.client.protocol
		status.ServerName = s.client.serverInfo.Name
		status.ServerVersion = s.client.serverInfo.Version
		status.PID = s.client.endpoint.pid()
	}
	return status
}

func (m *Manager) Notifications() <-chan Notification { return m.notifications }

func (m *Manager) ListTools(ctx context.Context, serverName string) ([]Tool, error) {
	var result []Tool
	err := m.withClient(ctx, serverName, func(client *protocolClient) error {
		var err error
		result, err = client.listTools(ctx)
		return err
	})
	return result, err
}

func (m *Manager) ListPrompts(ctx context.Context, serverName string) ([]Prompt, error) {
	var result []Prompt
	err := m.withClient(ctx, serverName, func(client *protocolClient) error {
		var err error
		result, err = client.listPrompts(ctx)
		return err
	})
	return result, err
}

func (m *Manager) ListResources(ctx context.Context, serverName string) ([]Resource, error) {
	var result []Resource
	err := m.withClient(ctx, serverName, func(client *protocolClient) error {
		var err error
		result, err = client.listResources(ctx)
		return err
	})
	return result, err
}

// DiscoverTools returns all enabled server tools under stable namespaced names.
func (m *Manager) DiscoverTools(ctx context.Context) ([]ToolDefinition, error) {
	m.mu.RLock()
	names := make([]string, 0, len(m.servers))
	for name, server := range m.servers {
		if server.config.Enabled {
			names = append(names, name)
		}
	}
	m.mu.RUnlock()
	sort.Strings(names)
	type discovered struct {
		server string
		tool   Tool
		base   string
	}
	var all []discovered
	for _, name := range names {
		tools, err := m.ListTools(ctx, name)
		if err != nil {
			return nil, fmt.Errorf("mcp: discover tools from %s: %w", name, err)
		}
		for _, tool := range tools {
			all = append(all, discovered{server: name, tool: tool, base: "mcp_" + namespacePart(name) + "_" + namespacePart(tool.Name)})
		}
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].base != all[j].base {
			return all[i].base < all[j].base
		}
		if all[i].server != all[j].server {
			return all[i].server < all[j].server
		}
		return all[i].tool.Name < all[j].tool.Name
	})
	counts := make(map[string]int)
	for _, item := range all {
		counts[item.base]++
	}
	definitions := make([]ToolDefinition, 0, len(all))
	targets := make(map[string]toolTarget, len(all))
	for _, item := range all {
		name := item.base
		if counts[item.base] > 1 {
			name += "_" + stableSuffix(item.server, item.tool.Name)
		}
		if _, duplicate := targets[name]; duplicate {
			return nil, fmt.Errorf("mcp: server %s returned duplicate tool %q", item.server, item.tool.Name)
		}
		targets[name] = toolTarget{server: item.server, tool: item.tool.Name}
		definitions = append(definitions, ToolDefinition{
			Name:         name,
			Description:  item.tool.Description,
			InputSchema:  append(json.RawMessage(nil), item.tool.InputSchema...),
			OutputSchema: append(json.RawMessage(nil), item.tool.OutputSchema...),
			Server:       item.server,
			Tool:         item.tool.Name,
		})
	}
	m.mu.Lock()
	if !m.closed {
		m.toolTargets = targets
	}
	m.mu.Unlock()
	return definitions, nil
}

func (m *Manager) CallTool(ctx context.Context, namespacedName string, arguments json.RawMessage) (ToolResult, error) {
	m.mu.RLock()
	target, ok := m.toolTargets[namespacedName]
	m.mu.RUnlock()
	if !ok {
		if _, err := m.DiscoverTools(ctx); err != nil {
			return ToolResult{}, err
		}
		m.mu.RLock()
		target, ok = m.toolTargets[namespacedName]
		m.mu.RUnlock()
	}
	if !ok {
		return ToolResult{}, fmt.Errorf("mcp: unknown tool %q", namespacedName)
	}
	if len(arguments) == 0 {
		arguments = json.RawMessage(`{}`)
	}
	var result ToolResult
	err := m.withClient(ctx, target.server, func(client *protocolClient) error {
		var err error
		result, err = client.callTool(ctx, target.tool, arguments)
		return err
	})
	return result, err
}

func (m *Manager) withClient(ctx context.Context, name string, operation func(*protocolClient) error) error {
	if err := m.Start(ctx, name); err != nil {
		return err
	}
	server, err := m.server(name)
	if err != nil {
		return err
	}
	client := server.currentClient()
	if client == nil {
		return errors.New("mcp: server stopped during call")
	}
	err = operation(client)
	var dispatched *dispatchError
	if !errors.As(err, &dispatched) || !dispatched.safe {
		return err
	}
	// Exactly one reconnect is allowed, and only when the transport proved no
	// request bytes were dispatched.
	if restartErr := m.Restart(ctx, name); restartErr != nil {
		return errors.Join(err, restartErr)
	}
	client = server.currentClient()
	if client == nil {
		return err
	}
	return operation(client)
}

func (s *managedServer) currentClient() *protocolClient {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.client
}

func (m *Manager) server(name string) (*managedServer, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	server := m.servers[name]
	if server == nil {
		return nil, fmt.Errorf("mcp: unknown server %q", name)
	}
	return server, nil
}

func namespacePart(value string) string {
	var output strings.Builder
	lastUnderscore := false
	for _, r := range strings.ToLower(value) {
		valid := r >= 'a' && r <= 'z' || r >= '0' && r <= '9'
		if valid {
			output.WriteRune(r)
			lastUnderscore = false
		} else if !lastUnderscore && output.Len() != 0 {
			output.WriteByte('_')
			lastUnderscore = true
		}
	}
	result := strings.Trim(output.String(), "_")
	if result == "" {
		return "unnamed"
	}
	return result
}

func stableSuffix(server, tool string) string {
	digest := sha256.Sum256([]byte(server + "\x00" + tool))
	return hex.EncodeToString(digest[:4])
}
