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

type ModelAlias struct {
	Name        string
	ModelString string
}

// ModelResolution preserves both the selector supplied by the caller and the
// canonical catalog identity selected by the registry. Alias is empty when the
// requested selector was already canonical.
type ModelResolution struct {
	RequestedSelector string
	Alias             string
	CanonicalBase     string
	CanonicalSelector string
	Provider          provider.Provider
	Model             provider.Model
	Variant           *provider.Variant
}

type ProviderRegistry struct {
	mu        sync.RWMutex
	providers map[string]provider.Provider
	aliases   map[string]string
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
	next, err := providerMap(providers)
	if err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if err := validateAliasTargets(r.aliases, next); err != nil {
		return err
	}
	r.providers = next
	return nil
}

// InstallAliases atomically replaces the registry aliases. Entries with an
// empty target are omitted, allowing configuration to disable an alias without
// making the entire installation invalid.
func (r *ProviderRegistry) InstallAliases(aliases []ModelAlias) error {
	next := make(map[string]string, len(aliases))
	for _, alias := range aliases {
		if alias.Name == "" {
			return errors.New("agent: alias name is required")
		}
		if _, exists := next[alias.Name]; exists {
			return fmt.Errorf("agent: duplicate alias %q", alias.Name)
		}
		next[alias.Name] = alias.ModelString
	}
	for name, target := range next {
		if target == "" {
			delete(next, name)
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if err := validateAliasTargets(next, r.providers); err != nil {
		return err
	}
	r.aliases = next
	return nil
}

func providerMap(providers []provider.Provider) (map[string]provider.Provider, error) {
	next := make(map[string]provider.Provider, len(providers))
	for _, item := range providers {
		if item == nil || item.ID() == "" {
			return nil, errors.New("agent: provider and provider ID are required")
		}
		if _, exists := next[item.ID()]; exists {
			return nil, fmt.Errorf("agent: duplicate provider %q", item.ID())
		}
		next[item.ID()] = item
	}
	return next, nil
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

// ResolveModel resolves a canonical selector or alias while preserving the
// caller-facing identity and reporting canonical base and exact selector keys.
func (r *ProviderRegistry) ResolveModel(selector string) (ModelResolution, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	requested := selector
	alias := ""
	if target, exists := r.aliases[selector]; exists {
		alias = selector
		selector = target
	}
	resolved, err := resolveProvider(r.providers, selector)
	if err != nil {
		return ModelResolution{}, err
	}
	resolved.RequestedSelector = requested
	resolved.Alias = alias
	return resolved, nil
}

// Resolve retains the original tuple API for callers which do not need model
// identity metadata.
func (r *ProviderRegistry) Resolve(selector string) (provider.Provider, provider.Model, *provider.Variant, error) {
	resolved, err := r.ResolveModel(selector)
	if err != nil {
		return nil, provider.Model{}, nil, err
	}
	return resolved.Provider, resolved.Model, resolved.Variant, nil
}

func validateAliasTargets(aliases map[string]string, providers map[string]provider.Provider) error {
	for name, target := range aliases {
		if _, exists := aliases[target]; exists {
			return fmt.Errorf("agent: alias %q targets alias %q", name, target)
		}
		if _, err := resolveProvider(providers, target); err != nil {
			return fmt.Errorf("agent: invalid target for alias %q: %w", name, err)
		}
	}
	return nil
}

func resolveProvider(providers map[string]provider.Provider, selector string) (ModelResolution, error) {
	providerID, remainder, found := strings.Cut(selector, "/")
	if !found || providerID == "" || remainder == "" || strings.HasSuffix(remainder, "/") || strings.Contains(remainder, "//") {
		return ModelResolution{}, fmt.Errorf("agent: malformed provider selector %q", selector)
	}

	selected := providers[providerID]
	if selected == nil {
		return ModelResolution{}, fmt.Errorf("agent: unknown provider %q", providerID)
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
		canonical := selected.ID() + "/" + baseModel.ID
		return ModelResolution{
			CanonicalBase: canonical, CanonicalSelector: canonical,
			Provider: selected, Model: *baseModel,
		}, nil
	}
	if len(variantMatches) == 1 {
		match := variantMatches[0]
		canonicalBase := selected.ID() + "/" + match.model.ID
		return ModelResolution{
			CanonicalBase: canonicalBase, CanonicalSelector: canonicalBase + "/" + match.variant.Name,
			Provider: selected, Model: match.model, Variant: &match.variant,
		}, nil
	}
	if len(variantMatches) > 1 {
		return ModelResolution{}, fmt.Errorf("agent: ambiguous model selector %q", selector)
	}
	return ModelResolution{}, fmt.Errorf("agent: unknown model selector %q for provider %q", remainder, providerID)
}
