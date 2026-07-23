// Package status provides runtime information which agents can query without
// placing it in the system prompt.
package status

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
)

type Query struct {
	SessionID string
	Agent     string
	Provider  string
	Model     string
	Variant   string
}

type Observation struct {
	Available bool
	Text      string
}

type Provider interface {
	Key() string
	Observe(context.Context, Query) (Observation, error)
}

type Static struct {
	ProviderKey string
	Text        string
}

func (s Static) Key() string { return s.ProviderKey }
func (s Static) Observe(context.Context, Query) (Observation, error) {
	return Observation{Available: strings.TrimSpace(s.Text) != "", Text: s.Text}, nil
}

type Selection struct{}

func (Selection) Key() string { return "runtime:selection" }
func (Selection) Observe(_ context.Context, query Query) (Observation, error) {
	lines := []string{"Active profile: " + query.Agent, "Model: " + query.Provider + "/" + query.Model}
	if query.Variant != "" {
		lines = append(lines, "Variant: "+query.Variant)
	}
	return Observation{Available: true, Text: strings.Join(lines, "\n")}, nil
}

type Registry struct {
	mu        sync.RWMutex
	providers map[string]Provider
}

func NewRegistry(providers ...Provider) (*Registry, error) {
	r := &Registry{providers: make(map[string]Provider, len(providers))}
	for _, provider := range providers {
		if err := r.Register(provider); err != nil {
			return nil, err
		}
	}
	return r, nil
}

func (r *Registry) Register(provider Provider) error {
	if provider == nil || !validKey(provider.Key()) {
		return errors.New("status: provider requires a stable namespaced key")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.providers[provider.Key()]; exists {
		return fmt.Errorf("status: duplicate provider %q", provider.Key())
	}
	r.providers[provider.Key()] = provider
	return nil
}

func validKey(key string) bool {
	namespace, name, ok := strings.Cut(key, ":")
	return ok && namespace != "" && name != "" && !strings.ContainsAny(key, "\r\n\t ")
}

// Observe evaluates all providers on every call and renders them by key.
func (r *Registry) Observe(ctx context.Context, query Query, profile Provider) (string, error) {
	if r == nil {
		return "", errors.New("status: registry is unavailable")
	}
	r.mu.RLock()
	providers := make(map[string]Provider, len(r.providers)+1)
	for key, provider := range r.providers {
		providers[key] = provider
	}
	r.mu.RUnlock()
	if profile != nil {
		if !validKey(profile.Key()) {
			return "", errors.New("status: profile provider requires a stable namespaced key")
		}
		if _, exists := providers[profile.Key()]; exists {
			return "", fmt.Errorf("status: duplicate provider %q", profile.Key())
		}
		providers[profile.Key()] = profile
	}
	keys := make([]string, 0, len(providers))
	for key := range providers {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	sections := make([]string, 0, len(keys))
	for _, key := range keys {
		observation, err := providers[key].Observe(ctx, query)
		if err != nil {
			return "", fmt.Errorf("%s: %w", key, err)
		}
		if observation.Available && strings.TrimSpace(observation.Text) != "" {
			sections = append(sections, observation.Text)
		}
	}
	return strings.Join(sections, "\n\n"), nil
}
