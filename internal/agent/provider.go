package agent

import (
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/amirulashraf/parrot-coder/internal/provider"
)

type ProviderResolver interface {
	Resolve(providerID, modelID string) (provider.Provider, provider.Model, error)
}

// ProviderLister reports the registered providers in deterministic order. The
// registry implements it so a live reload is the single source of truth for
// model and usage listings, instead of a snapshot captured at startup.
type ProviderLister interface {
	List() []provider.Provider
}

type ProviderRegistry struct {
	mu        sync.RWMutex
	providers map[string]provider.Provider
}

func NewProviderRegistry(providers ...provider.Provider) (*ProviderRegistry, error) {
	r := &ProviderRegistry{providers: make(map[string]provider.Provider)}
	if err := r.load(providers); err != nil {
		return nil, err
	}
	return r, nil
}

// Replace swaps the registered providers atomically. Validation mirrors
// NewProviderRegistry: a nil provider, an empty ID, or a duplicate fails
// without touching the existing set, so a failed reload leaves the running
// providers intact.
func (r *ProviderRegistry) Replace(providers []provider.Provider) error {
	next := make(map[string]provider.Provider, len(providers))
	for _, item := range providers {
		if item == nil || item.ID() == "" {
			return errors.New("agent: provider and provider ID are required")
		}
		if _, exists := next[item.ID()]; exists {
			return fmt.Errorf("agent: duplicate provider %q", item.ID())
		}
		next[item.ID()] = item
	}
	r.mu.Lock()
	r.providers = next
	r.mu.Unlock()
	return nil
}

// load validates and registers providers without holding the write lock,
// because it runs during construction before the registry is published.
func (r *ProviderRegistry) load(providers []provider.Provider) error {
	for _, item := range providers {
		if item == nil || item.ID() == "" {
			return errors.New("agent: provider and provider ID are required")
		}
		if _, exists := r.providers[item.ID()]; exists {
			return fmt.Errorf("agent: duplicate provider %q", item.ID())
		}
		r.providers[item.ID()] = item
	}
	return nil
}

// List returns the registered providers sorted by ID for deterministic output.
func (r *ProviderRegistry) List() []provider.Provider {
	r.mu.RLock()
	ids := make([]string, 0, len(r.providers))
	for id := range r.providers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]provider.Provider, 0, len(ids))
	for _, id := range ids {
		out = append(out, r.providers[id])
	}
	r.mu.RUnlock()
	return out
}

func (r *ProviderRegistry) Resolve(providerID, modelID string) (provider.Provider, provider.Model, error) {
	r.mu.RLock()
	ids := make([]string, 0, len(r.providers))
	for id := range r.providers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if providerID == "" && len(ids) > 0 {
		providerID = ids[0]
	}
	selected := r.providers[providerID]
	r.mu.RUnlock()
	if selected == nil {
		return nil, provider.Model{}, fmt.Errorf("agent: unknown provider %q", providerID)
	}
	models := selected.Models()
	sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
	if modelID == "" && len(models) > 0 {
		modelID = models[0].ID
	}
	for _, model := range models {
		if model.ID == modelID {
			return selected, model, nil
		}
	}
	return nil, provider.Model{}, fmt.Errorf("agent: unknown model %q for provider %q", modelID, providerID)
}
