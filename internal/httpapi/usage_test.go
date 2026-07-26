package httpapi

import (
	"context"
	"errors"
	"testing"

	"github.com/amirulashraf/parrot-coder/internal/protocol"
	"github.com/amirulashraf/parrot-coder/internal/provider"
	"github.com/amirulashraf/parrot-coder/internal/session"
)

type stubProvider struct{ id string }

func (p stubProvider) ID() string { return p.id }
func (p stubProvider) Models() []provider.Model {
	return []provider.Model{{ID: "model"}}
}
func (p stubProvider) Stream(context.Context, protocol.Request) (provider.Stream, error) {
	return nil, errors.New("not implemented")
}

type stubUsageProvider struct {
	stubProvider
	plan string
}

func (p stubUsageProvider) Usage(context.Context) (provider.SubscriptionUsage, error) {
	return provider.SubscriptionUsage{PlanType: p.plan}, nil
}

func TestSubscriptionUsageSelectsProvider(t *testing.T) {
	chatgpt := stubUsageProvider{stubProvider: stubProvider{id: "chatgpt"}, plan: "plus"}
	kimi := stubUsageProvider{stubProvider: stubProvider{id: "kimi"}, plan: "coding"}
	plain := stubProvider{id: "local"}
	for _, testCase := range []struct {
		name      string
		providers []provider.Provider
		selection string
		wantID    string
		wantPlan  string
		wantErr   bool
	}{
		{
			name: "prefers the default selection's provider", providers: []provider.Provider{chatgpt, kimi},
			selection: "kimi/model", wantID: "kimi", wantPlan: "coding",
		},
		{
			name: "falls back to the first reporter", providers: []provider.Provider{plain, chatgpt, kimi},
			selection: "local/model", wantID: "chatgpt", wantPlan: "plus",
		},
		{
			name: "errors when no provider reports usage", providers: []provider.Provider{plain},
			selection: "local/model", wantErr: true,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			backend := &DomainBackend{Providers: testCase.providers, DefaultSelection: session.Selection{Model: testCase.selection}}
			usage, err := backend.SubscriptionUsage(context.Background())
			if testCase.wantErr {
				if err == nil {
					t.Fatalf("usage = %#v, want an error", usage)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if usage.Provider != testCase.wantID || usage.PlanType != testCase.wantPlan {
				t.Fatalf("usage = %#v", usage)
			}
		})
	}
}
