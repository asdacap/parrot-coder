// Package systemcontext observes provider context for the system prompt.
package systemcontext

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
)

type Source interface {
	Key() string
	Observe(context.Context) (Observation, error)
}

type Observation struct {
	Available bool
	Text      string
	Path      string
}

type Registry struct {
	mu      sync.RWMutex
	sources map[string]Source
}

func (*Registry) Key() string { return "runtime:source-context" }

// MaterializeSystemPrompt binds this source registry to a session while
// retaining live source observation when the returned product is rendered.
func (r *Registry) MaterializeSystemPrompt(AgentSession) (SystemPrompt, error) {
	if r == nil {
		return nil, errors.New("systemcontext: registry is unavailable")
	}
	return &registrySystemPrompt{registry: r}, nil
}

type registrySystemPrompt struct{ registry *Registry }

func (p *registrySystemPrompt) GetSystemPrompt(ctx context.Context, _ ModelSelection) (string, error) {
	return p.registry.GetSystemPrompt(ctx, ModelSelection{})
}

func NewRegistry(sources ...Source) (*Registry, error) {
	r := &Registry{sources: make(map[string]Source)}
	for _, source := range sources {
		if err := r.Register(source); err != nil {
			return nil, err
		}
	}
	return r, nil
}

func (r *Registry) Register(source Source) error {
	if isNil(source) || !validKey(source.Key()) {
		return errors.New("systemcontext: source requires a stable namespaced key")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.sources[source.Key()]; exists {
		return fmt.Errorf("systemcontext: duplicate source %q", source.Key())
	}
	r.sources[source.Key()] = source
	return nil
}

func validKey(key string) bool {
	namespace, name, ok := strings.Cut(key, ":")
	return ok && namespace != "" && name != "" && !strings.ContainsAny(key, "\r\n\t ")
}

// GetSystemPrompt observes every source and renders available context in stable
// key order.
func (r *Registry) GetSystemPrompt(ctx context.Context, _ ModelSelection) (string, error) {
	if r == nil {
		return "", errors.New("systemcontext: registry is unavailable")
	}
	r.mu.RLock()
	keys := make([]string, 0, len(r.sources))
	sources := make(map[string]Source, len(r.sources))
	for key, source := range r.sources {
		keys = append(keys, key)
		sources[key] = source
	}
	r.mu.RUnlock()
	sort.Strings(keys)

	type result struct {
		key         string
		observation Observation
		err         error
	}
	results := make(chan result, len(keys))
	var wg sync.WaitGroup
	for _, key := range keys {
		wg.Add(1)
		go func(key string, source Source) {
			defer wg.Done()
			observation, err := source.Observe(ctx)
			results <- result{key: key, observation: observation, err: err}
		}(key, sources[key])
	}
	wg.Wait()
	close(results)

	observed := make(map[string]result, len(keys))
	for result := range results {
		observed[result.key] = result
	}
	sections := make([]string, 0, len(keys))
	failures := make([]error, 0)
	for _, key := range keys {
		result := observed[key]
		if result.err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", key, result.err))
			continue
		}
		if !result.observation.Available {
			failures = append(failures, fmt.Errorf("%s: source unavailable", key))
			continue
		}
		if strings.TrimSpace(result.observation.Text) != "" {
			sections = append(sections, result.observation.Text)
		}
	}
	return strings.Join(sections, "\n\n"), errors.Join(failures...)
}
