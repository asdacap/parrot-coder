package httpapi

import (
	"context"
	"testing"

	"github.com/amirulashraf/parrot-coder/internal/agent"
	v1 "github.com/amirulashraf/parrot-coder/internal/api/v1"
	"github.com/amirulashraf/parrot-coder/internal/provider"
)

// listProvider is a stubProvider that also reports a model catalog, so
// ListModels has something to list during the reload-routing test.
type listProvider struct {
	stubProvider
	models []provider.Model
}

func (p listProvider) Models() []provider.Model { return p.models }

// minimalResolver satisfies agent.ProviderResolver without implementing
// ProviderLister, exercising the fallback to the Providers slice.
type minimalResolver struct{}

func (minimalResolver) Resolve(string) (provider.Provider, provider.Model, *provider.Variant, error) {
	return nil, provider.Model{}, nil, nil
}

func modelNames(items []v1.Model) []string {
	names := make([]string, len(items))
	for i, item := range items {
		names[i] = item.Provider + "/" + item.ID
	}
	return names
}

// listModels wraps ListModels so the test can chain .Items while still failing
// on an unexpected error.
func listModels(t *testing.T, backend *DomainBackend) []v1.Model {
	t.Helper()
	out, err := backend.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return out.Items
}

// TestListModelsRoutesThroughRegistry verifies that when the backend's
// ProviderResolver is the live registry, ListModels reflects a hot reload via
// Replace; and that a resolver which does not implement ProviderLister falls
// back to the Providers snapshot captured at construction.
func TestListModelsRoutesThroughRegistry(t *testing.T) {
	alpha := listProvider{stubProvider: stubProvider{id: "alpha"}, models: []provider.Model{{ID: "a1"}}}
	beta := listProvider{stubProvider: stubProvider{id: "beta"}, models: []provider.Model{{ID: "b1"}, {ID: "b2"}}}
	registry, err := agent.NewProviderRegistry(alpha)
	if err != nil {
		t.Fatal(err)
	}
	backend := &DomainBackend{ProviderResolver: registry}
	if got := modelNames(listModels(t, backend)); len(got) != 1 || got[0] != "alpha/a1" {
		t.Fatalf("ListModels before reload = %v, want [alpha/a1]", got)
	}
	if err := registry.Replace([]provider.Provider{beta}); err != nil {
		t.Fatal(err)
	}
	if got := modelNames(listModels(t, backend)); len(got) != 2 || got[0] != "beta/b1" || got[1] != "beta/b2" {
		t.Fatalf("ListModels after reload = %v, want [beta/b1 beta/b2]", got)
	}
	// A resolver without ProviderLister keeps using the startup snapshot.
	fallback := &DomainBackend{Providers: []provider.Provider{alpha}, ProviderResolver: minimalResolver{}}
	if got := modelNames(listModels(t, fallback)); len(got) != 1 || got[0] != "alpha/a1" {
		t.Fatalf("fallback ListModels = %v, want [alpha/a1]", got)
	}
}
