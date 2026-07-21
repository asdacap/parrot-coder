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
				if _, model, err := registry.Resolve("", ""); err != nil || model.ID != testCase.wantModel {
					t.Fatalf("Resolve err = %v, model = %q, want %q", err, model.ID, testCase.wantModel)
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
			id := fmt.Sprintf("p%d", i%4)
			for range 200 {
				_, _, _ = registry.Resolve(id, "")
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
