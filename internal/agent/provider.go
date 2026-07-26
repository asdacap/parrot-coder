package agent

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/amirulashraf/parrot-coder/internal/provider"
)

type ProviderResolver interface {
	Resolve(selector string) (provider.Provider, provider.Model, *provider.Variant, error)
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

func (r *ProviderRegistry) Resolve(selector string) (provider.Provider, provider.Model, *provider.Variant, error) {
	providerID, remainder, found := strings.Cut(selector, "/")
	if !found || providerID == "" || remainder == "" || strings.HasSuffix(remainder, "/") || strings.Contains(remainder, "//") {
		return nil, provider.Model{}, nil, fmt.Errorf("agent: malformed provider selector %q", selector)
	}

	r.mu.RLock()
	selected := r.providers[providerID]
	r.mu.RUnlock()
	if selected == nil {
		return nil, provider.Model{}, nil, fmt.Errorf("agent: unknown provider %q", providerID)
	}

	var baseModel *provider.Model
	type variantMatch struct {
		model   provider.Model
		variant provider.Variant
	}
	var variantMatches []variantMatch
	for _, model := range selected.Models() {
		if model.ID == remainder {
			match := model
			baseModel = &match
		}
		for _, variant := range model.Capabilities.Variants {
			if model.ID+"/"+variant.Name == remainder {
				variantMatches = append(variantMatches, variantMatch{model: model, variant: variant})
			}
		}
	}

	// Model IDs may contain slash-delimited components which also happen to be
	// variant names. An exact catalog model is authoritative; only interpret a
	// suffix as a variant when no exact model exists.
	if baseModel != nil {
		return selected, *baseModel, nil, nil
	}
	if len(variantMatches) == 1 {
		match := variantMatches[0]
		return selected, match.model, &match.variant, nil
	}
	if len(variantMatches) > 1 {
		return nil, provider.Model{}, nil, fmt.Errorf("agent: ambiguous model selector %q", selector)
	}
	return nil, provider.Model{}, nil, fmt.Errorf("agent: unknown model selector %q for provider %q", remainder, providerID)
}
