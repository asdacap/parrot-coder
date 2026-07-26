package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/amirulashraf/parrot-coder/internal/protocol"
	"github.com/amirulashraf/parrot-coder/internal/provider"
)

// idProvider is a minimal provider whose ID and catalog are test-controlled, so
// the registry can be exercised with several distinct providers.
type idProvider struct {
	id     string
	models []provider.Model
}

func (p idProvider) ID() string               { return p.id }
func (p idProvider) Models() []provider.Model { return p.models }
func (p idProvider) Stream(context.Context, protocol.Request) (provider.Stream, error) {
	return nil, errors.New("idProvider does not stream")
}

func models(ids ...string) []provider.Model {
	out := make([]provider.Model, len(ids))
	for i, id := range ids {
		out[i] = provider.Model{ID: id}
	}
	return out
}

func modelWithVariants(id string, variants ...string) provider.Model {
	model := provider.Model{ID: id}
	for _, variant := range variants {
		model.Capabilities.Variants = append(model.Capabilities.Variants, provider.Variant{Name: variant})
	}
	return model
}

func providerIDs(list []provider.Provider) []string {
	ids := make([]string, len(list))
	for i, item := range list {
		ids[i] = item.ID()
	}
	return ids
}

// TestProviderRegistryReplaceAndList covers the hot-reload contract in one
// parameterized pass: Replace swaps the registered set so Resolve and List
// reflect it, a rejected replacement leaves the previous set intact, and
// Resolve keeps reporting the latest provider after a swap.
func TestProviderRegistryReplaceAndList(t *testing.T) {
	one := idProvider{"alpha", models("a1")}
	two := idProvider{"beta", models("b1", "b2")}
	three := idProvider{"gamma", models("g1")}
	for _, testCase := range []struct {
		name      string
		replace   []provider.Provider
		wantErr   bool
		wantIDs   []string
		wantModel string // resolved from the first provider, empty model picks first
	}{
		{name: "swap to a new set", replace: []provider.Provider{two, three}, wantIDs: []string{"beta", "gamma"}, wantModel: "b1"},
		{name: "swap back to single", replace: []provider.Provider{one}, wantIDs: []string{"alpha"}, wantModel: "a1"},
		{name: "empty replacement clears the registry", replace: nil, wantIDs: nil},
		{name: "nil provider is rejected", replace: []provider.Provider{nil}, wantErr: true, wantIDs: []string{"alpha"}, wantModel: "a1"},
		{name: "empty ID is rejected", replace: []provider.Provider{idProvider{"", nil}}, wantErr: true, wantIDs: []string{"alpha"}, wantModel: "a1"},
		{name: "duplicate ID is rejected", replace: []provider.Provider{one, idProvider{"alpha", nil}}, wantErr: true, wantIDs: []string{"alpha"}, wantModel: "a1"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			registry, err := NewProviderRegistry(one)
			if err != nil {
				t.Fatal(err)
			}
			if err := registry.Replace(testCase.replace); (err != nil) != testCase.wantErr {
				t.Fatalf("Replace err = %v, want error = %t", err, testCase.wantErr)
			}
			if got := providerIDs(registry.List()); !equalStrings(got, testCase.wantIDs) {
				t.Fatalf("List = %v, want %v", got, testCase.wantIDs)
			}
			if testCase.wantModel != "" {
				selector := registry.List()[0].ID() + "/" + testCase.wantModel
				if _, model, variant, err := registry.Resolve(selector); err != nil || model.ID != testCase.wantModel || variant != nil {
					t.Fatalf("Resolve err = %v, model = %q, variant = %v, want model %q without variant", err, model.ID, variant, testCase.wantModel)
				}
			}
		})
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestProviderRegistryResolve(t *testing.T) {
	registry, err := NewProviderRegistry(
		idProvider{"catalog", []provider.Model{
			modelWithVariants("plain", "high"),
			modelWithVariants("vendor/slash-model", "low"),
			modelWithVariants("ambiguous/model", "high"),
			modelWithVariants("ambiguous/model/high"),
		}},
	)
	if err != nil {
		t.Fatal(err)
	}

	for _, testCase := range []struct {
		name        string
		selector    string
		wantModel   string
		wantVariant string
		wantErr     bool
	}{
		{name: "base model", selector: "catalog/plain", wantModel: "plain"},
		{name: "model variant", selector: "catalog/plain/high", wantModel: "plain", wantVariant: "high"},
		{name: "slash model ID", selector: "catalog/vendor/slash-model", wantModel: "vendor/slash-model"},
		{name: "slash model ID variant", selector: "catalog/vendor/slash-model/low", wantModel: "vendor/slash-model", wantVariant: "low"},
		{name: "exact model wins over variant suffix", selector: "catalog/ambiguous/model/high", wantModel: "ambiguous/model/high"},
		{name: "empty selector", selector: "", wantErr: true},
		{name: "missing model", selector: "catalog", wantErr: true},
		{name: "empty provider", selector: "/plain", wantErr: true},
		{name: "empty model", selector: "catalog/", wantErr: true},
		{name: "empty model segment", selector: "catalog/vendor//slash-model", wantErr: true},
		{name: "trailing slash", selector: "catalog/plain/", wantErr: true},
		{name: "unknown provider", selector: "missing/plain", wantErr: true},
		{name: "unknown model", selector: "catalog/missing", wantErr: true},
		{name: "unknown variant", selector: "catalog/plain/low", wantErr: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			client, model, variant, err := registry.Resolve(testCase.selector)
			if (err != nil) != testCase.wantErr {
				t.Fatalf("Resolve(%q) err = %v, want error = %t", testCase.selector, err, testCase.wantErr)
			}
			if testCase.wantErr {
				if client != nil || model.ID != "" || variant != nil {
					t.Fatalf("Resolve(%q) = (%v, %#v, %v), want zero values on error", testCase.selector, client, model, variant)
				}
				return
			}
			if client == nil || client.ID() != "catalog" || model.ID != testCase.wantModel {
				t.Fatalf("Resolve(%q) = provider %v, model %q; want catalog/%s", testCase.selector, client, model.ID, testCase.wantModel)
			}
			if testCase.wantVariant == "" {
				if variant != nil {
					t.Fatalf("Resolve(%q) variant = %#v, want nil", testCase.selector, variant)
				}
			} else if variant == nil || variant.Name != testCase.wantVariant {
				t.Fatalf("Resolve(%q) variant = %#v, want %q", testCase.selector, variant, testCase.wantVariant)
			}
		})
	}
}

// TestProviderRegistryConcurrentReload exercises the read/write locking under
// the race detector: many goroutines resolve while a writer swaps providers,
// and neither panics nor deadlocks.
func TestProviderRegistryConcurrentReload(t *testing.T) {
	t.Parallel()
	registry, err := NewProviderRegistry(idProvider{"alpha", models("a1")})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			selector := fmt.Sprintf("p%d/m%d", i%4, i%4)
			for range 200 {
				_, _, _, _ = registry.Resolve(selector)
			}
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 200 {
			_ = registry.Replace([]provider.Provider{
				idProvider{"p0", models("m0")},
				idProvider{"p1", models("m1")},
				idProvider{"p2", models("m2")},
				idProvider{"p3", models("m3")},
			})
		}
	}()
	wg.Wait()
	if ids := providerIDs(registry.List()); len(ids) != 4 {
		t.Fatalf("List after reload = %v, want 4 providers", ids)
	}
}
