package app

import (
	"context"
	"testing"

	"github.com/amirulashraf/parrot-coder/internal/agent"
	"github.com/amirulashraf/parrot-coder/internal/auth"
	"github.com/amirulashraf/parrot-coder/internal/config"
)

// TestReloadProvidersPicksUpNewCredential verifies that ReloadProviders rebuilds
// the live registry from the credential store, so a credential added after
// Open (as /auth login does) makes its provider available without a restart.
// It also checks that a build failure leaves the existing providers in place.
func TestReloadProvidersPicksUpNewCredential(t *testing.T) {
	ctx := context.Background()
	store := storeWithKeys(t)
	client := offlineClient()

	// Initial build: only the always-present ChatGPT provider exists, because no
	// preset credential is stored yet.
	initial, err := BuildProviders(ctx, config.Config{}, store, client)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := agent.NewProviderRegistry(initial...)
	if err != nil {
		t.Fatal(err)
	}
	app := &App{
		Config:      config.Result{Config: config.Config{}},
		Credentials: store,
		providers:   registry,
		httpClient:  client,
	}

	if findProvider(registry.List(), "openrouter") != nil {
		t.Fatal("openrouter present before its credential was stored")
	}

	// Simulate /auth login openrouter by storing its key, then reload.
	if err := store.Put(ctx, "openrouter", auth.NewAPIKeyCredential("test-key")); err != nil {
		t.Fatal(err)
	}
	if err := app.ReloadProviders(ctx); err != nil {
		t.Fatalf("ReloadProviders after adding credential: %v", err)
	}
	if findProvider(registry.List(), "openrouter") == nil {
		t.Fatal("openrouter missing from registry after reload")
	}

	// A configured provider with no credential makes the rebuild fail; the
	// running providers must survive so the chat keeps working.
	app.Config = config.Result{Config: config.Config{Providers: map[string]config.Provider{"kimi-code": {}}}}
	if err := app.ReloadProviders(ctx); err == nil {
		t.Fatal("reload accepted a configured provider with no credential")
	}
	if findProvider(registry.List(), "openrouter") == nil {
		t.Fatal("failed reload dropped the existing providers")
	}
}

// TestReloadProvidersUnavailable guards the nil-registry path so a caller on an
// App that was not fully opened reports a clear error instead of panicking.
func TestReloadProvidersUnavailable(t *testing.T) {
	app := &App{Config: config.Result{Config: config.Config{}}, Credentials: storeWithKeys(t), httpClient: offlineClient()}
	if err := app.ReloadProviders(context.Background()); err == nil {
		t.Fatal("ReloadProviders on an App without a provider registry should fail")
	}
}
