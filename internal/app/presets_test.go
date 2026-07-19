package app

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/amirulashraf/parrot-coder/internal/auth"
	"github.com/amirulashraf/parrot-coder/internal/config"
	"github.com/amirulashraf/parrot-coder/internal/provider"
)

type failingTransport struct{}

func (failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("network disabled in tests")
}

// offlineClient keeps the always-present ChatGPT provider from reaching the
// network during BuildProviders; its model refresh is warn-only.
func offlineClient() *http.Client { return &http.Client{Transport: failingTransport{}} }

func storeWithKeys(t *testing.T, ids ...string) auth.Store {
	t.Helper()
	store := auth.NewFileStore(filepath.Join(t.TempDir(), "credentials.json"))
	for _, id := range ids {
		if err := store.Put(context.Background(), id, auth.NewAPIKeyCredential("test-key")); err != nil {
			t.Fatal(err)
		}
	}
	return store
}

func findProvider(providers []provider.Provider, id string) provider.Provider {
	for _, item := range providers {
		if item.ID() == id {
			return item
		}
	}
	return nil
}

func TestApplyProviderPresetFillsOnlyEmptyFields(t *testing.T) {
	timeout := 250
	for _, testCase := range []struct {
		name          string
		id            string
		item          config.Provider
		wantBaseURL   string
		wantProtocol  string
		wantAPIKeyEnv string
		wantTimeoutMS *int
		wantModelIDs  []string
	}{
		{
			name: "kimi-code defaults apply to an empty entry", id: "kimi-code",
			wantBaseURL: "https://api.kimi.com/coding/v1", wantProtocol: "chat-completions", wantAPIKeyEnv: "KIMI_API_KEY",
			wantModelIDs: []string{"kimi-k2-0905-preview", "kimi-k2-thinking", "kimi-k2-turbo-preview"},
		},
		{
			name: "user fields win over the preset", id: "kimi-api",
			item: config.Provider{
				BaseURL: "https://api.moonshot.cn/v1", Protocol: "responses", APIKeyEnv: "MY_KEY",
				HeaderTimeoutMS: &timeout, Models: map[string]config.Model{"custom": {}},
			},
			wantBaseURL: "https://api.moonshot.cn/v1", wantProtocol: "responses", wantAPIKeyEnv: "MY_KEY",
			wantTimeoutMS: &timeout, wantModelIDs: []string{"custom"},
		},
		{
			name: "openai keeps its ten second header timeout and nothing else", id: "openai",
			item:          config.Provider{BaseURL: "https://api.openai.com/v1"},
			wantBaseURL:   "https://api.openai.com/v1",
			wantTimeoutMS: func() *int { value := 10000; return &value }(),
		},
		{
			name: "unknown providers are untouched", id: "whatever",
			item:        config.Provider{BaseURL: "https://example.com/v1"},
			wantBaseURL: "https://example.com/v1",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			got := applyProviderPreset(testCase.id, testCase.item)
			if got.BaseURL != testCase.wantBaseURL || got.Protocol != testCase.wantProtocol || got.APIKeyEnv != testCase.wantAPIKeyEnv {
				t.Fatalf("got %#v", got)
			}
			switch {
			case testCase.wantTimeoutMS == nil && got.HeaderTimeoutMS != nil:
				t.Fatalf("header timeout = %d, want none", *got.HeaderTimeoutMS)
			case testCase.wantTimeoutMS != nil && (got.HeaderTimeoutMS == nil || *got.HeaderTimeoutMS != *testCase.wantTimeoutMS):
				t.Fatalf("header timeout = %v, want %d", got.HeaderTimeoutMS, *testCase.wantTimeoutMS)
			}
			if len(got.Models) != len(testCase.wantModelIDs) {
				t.Fatalf("models = %#v, want %v", got.Models, testCase.wantModelIDs)
			}
			for _, id := range testCase.wantModelIDs {
				if _, ok := got.Models[id]; !ok {
					t.Fatalf("models %#v missing %q", got.Models, id)
				}
			}
		})
	}
}

func TestBuildProvidersUsesPresetsForUnconfiguredProviders(t *testing.T) {
	ctx := context.Background()
	cfg := config.Config{}

	built, err := BuildProviders(ctx, cfg, storeWithKeys(t), offlineClient())
	if err != nil {
		t.Fatal(err)
	}
	if findProvider(built, "kimi-code") != nil {
		t.Fatal("kimi-code was built without a credential")
	}

	built, err = BuildProviders(ctx, cfg, storeWithKeys(t, "kimi-code", "kimi-api"), offlineClient())
	if err != nil {
		t.Fatal(err)
	}
	code := findProvider(built, "kimi-code")
	if code == nil {
		t.Fatal("kimi-code was not built from a stored credential alone")
	}
	if len(code.Models()) != len(providerPresets["kimi-code"].Models) {
		t.Fatalf("models = %#v", code.Models())
	}
	// The subscription endpoint has no balance route, so it must not claim to
	// report usage; the pay-as-you-go platform API does.
	if _, ok := code.(provider.UsageReporter); ok {
		t.Fatal("kimi-code reports subscription usage, but its endpoint has no balance route")
	}
	api := findProvider(built, "kimi-api")
	if api == nil {
		t.Fatal("kimi-api was not built from a stored credential alone")
	}
	if _, ok := api.(provider.UsageReporter); !ok {
		t.Fatal("kimi-api does not report its balance")
	}
}

func TestBuildProvidersRejectsConfiguredProviderWithoutCredential(t *testing.T) {
	cfg := config.Config{Providers: map[string]config.Provider{"kimi-code": {}}}
	if _, err := BuildProviders(context.Background(), cfg, storeWithKeys(t), offlineClient()); err == nil {
		t.Fatal("accepted a configured provider with no API key")
	}
}

func TestBuildProvidersAppliesEnvironmentKeyFromPreset(t *testing.T) {
	t.Setenv("MOONSHOT_API_KEY", "from-env")
	built, err := BuildProviders(context.Background(), config.Config{}, storeWithKeys(t), offlineClient())
	if err != nil {
		t.Fatal(err)
	}
	if findProvider(built, "kimi-api") == nil {
		t.Fatal("kimi-api was not built from MOONSHOT_API_KEY")
	}
}
