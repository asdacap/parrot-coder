package lsp

import (
	"context"
	"errors"
	"sync"
)

// Manager lazily starts a configured set of language servers.
type Manager struct {
	mu      sync.Mutex
	configs map[string]Config
	clients map[string]*Client
	closed  bool
}

func NewManager(configs []Config) (*Manager, error) {
	m := &Manager{configs: make(map[string]Config), clients: make(map[string]*Client)}
	for _, config := range configs {
		normalized, err := config.normalized()
		if err != nil {
			return nil, err
		}
		if _, exists := m.configs[normalized.Name]; exists {
			return nil, errors.New("lsp: duplicate server name")
		}
		m.configs[normalized.Name] = normalized
	}
	return m, nil
}

func (m *Manager) Client(ctx context.Context, name string) (*Client, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, ErrClosed
	}
	if client := m.clients[name]; client != nil {
		return client, nil
	}
	config, ok := m.configs[name]
	if !ok {
		return nil, errors.New("lsp: unknown server")
	}
	client, err := NewClient(ctx, config)
	if err != nil {
		return nil, err
	}
	m.clients[name] = client
	return client, nil
}

func (m *Manager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	clients := make([]*Client, 0, len(m.clients))
	for _, client := range m.clients {
		clients = append(clients, client)
	}
	m.mu.Unlock()
	var errs []error
	for _, client := range clients {
		if err := client.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
