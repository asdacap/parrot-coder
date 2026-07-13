package agent

import (
	"errors"
	"fmt"
	"sort"

	"github.com/amirulashraf/parrot-coder/internal/provider"
)

type ProviderResolver interface {
	Resolve(providerID, modelID string) (provider.Provider, provider.Model, error)
}

type ProviderRegistry struct {
	providers map[string]provider.Provider
}

func NewProviderRegistry(providers ...provider.Provider) (*ProviderRegistry, error) {
	r := &ProviderRegistry{providers: make(map[string]provider.Provider)}
	for _, item := range providers {
		if item == nil || item.ID() == "" {
			return nil, errors.New("agent: provider and provider ID are required")
		}
		if _, exists := r.providers[item.ID()]; exists {
			return nil, fmt.Errorf("agent: duplicate provider %q", item.ID())
		}
		r.providers[item.ID()] = item
	}
	return r, nil
}

func (r *ProviderRegistry) Resolve(providerID, modelID string) (provider.Provider, provider.Model, error) {
	ids := make([]string, 0, len(r.providers))
	for id := range r.providers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if providerID == "" && len(ids) > 0 {
		providerID = ids[0]
	}
	selected := r.providers[providerID]
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
