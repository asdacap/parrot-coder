package systemcontext

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type promptTestSession struct{ model, guidance string }

func (s promptTestSession) ModelSelector() string      { return s.model }
func (s promptTestSession) ToolSystemGuidance() string { return s.guidance }

type promptTestProduct struct {
	text string
	err  error
}

func (p promptTestProduct) GetSystemPrompt(context.Context, ModelSelection) (string, error) {
	return p.text, p.err
}

type promptTestProvider struct {
	key     string
	product SystemPrompt
	err     error
}

func (p promptTestProvider) Key() string { return p.key }
func (p promptTestProvider) MaterializeSystemPrompt(AgentSession) (SystemPrompt, error) {
	return p.product, p.err
}

type promptTestResolver struct {
	resolutions map[string]ModelResolution
	err         error
	calls       int
}

func (r *promptTestResolver) resolve(selector string) (ModelResolution, error) {
	r.calls++
	return r.resolutions[selector], r.err
}

func TestCompositeValidatesMaterializesAndRendersStableOrder(t *testing.T) {
	bad := promptTestProvider{key: "invalid", product: promptTestProduct{text: "bad"}}
	valid := promptTestProvider{key: "test:a", product: promptTestProduct{text: "a"}}
	var nilProvider *ModelAugmentProvider
	for _, providers := range [][]SystemPromptProvider{{bad}, {valid, valid}, {nilProvider}} {
		if _, err := NewCompositeSystemPromptProvider("test:composite", providers...); err == nil {
			t.Fatalf("providers %#v unexpectedly accepted", providers)
		}
	}
	if _, err := NewCompositeSystemPromptProvider("invalid", valid); err == nil {
		t.Fatal("invalid composite key accepted")
	}

	composite, err := NewCompositeSystemPromptProvider("test:composite",
		promptTestProvider{key: "test:z", product: promptTestProduct{text: "z"}},
		promptTestProvider{key: "test:empty", product: promptTestProduct{text: " \n"}},
		valid,
	)
	if err != nil {
		t.Fatal(err)
	}
	product, err := composite.MaterializeSystemPrompt(promptTestSession{})
	if err != nil {
		t.Fatal(err)
	}
	text, err := product.GetSystemPrompt(context.Background(), ModelSelection{})
	if err != nil || text != "a\n\nz" {
		t.Fatalf("prompt = %q, err = %v", text, err)
	}

	failing, _ := NewCompositeSystemPromptProvider("test:failures",
		promptTestProvider{key: "test:a", product: promptTestProduct{text: "partial", err: errors.New("render")}},
		promptTestProvider{key: "test:b", product: promptTestProduct{text: "kept"}},
	)
	product, err = failing.MaterializeSystemPrompt(promptTestSession{})
	if err != nil {
		t.Fatal(err)
	}
	text, err = product.GetSystemPrompt(context.Background(), ModelSelection{})
	if text != "kept" || err == nil || !strings.Contains(err.Error(), "test:a: render") {
		t.Fatalf("partial prompt = %q, err = %v", text, err)
	}

	invalidProduct, _ := NewCompositeSystemPromptProvider("test:invalid-product",
		promptTestProvider{key: "test:a", err: errors.New("create")},
		promptTestProvider{key: "test:b"},
	)
	if product, err = invalidProduct.MaterializeSystemPrompt(promptTestSession{}); product != nil || err == nil || !strings.Contains(err.Error(), "test:a: create") || !strings.Contains(err.Error(), "test:b: nil system prompt") {
		t.Fatalf("product = %#v, err = %v", product, err)
	}
}

func TestRegistryProductKeepsSourcesLivePerMaterialization(t *testing.T) {
	source := NewModelAliasesSource(nil)
	registry, err := NewRegistry(source)
	if err != nil {
		t.Fatal(err)
	}
	first, err := registry.MaterializeSystemPrompt(promptTestSession{model: "p/first"})
	if err != nil {
		t.Fatal(err)
	}
	source.Set([]ModelAlias{{Name: "later", ModelString: "p/later", Usage: "live"}})
	text, err := first.GetSystemPrompt(context.Background(), ModelSelection{})
	if err != nil || !strings.Contains(text, "- later: p/later — live") {
		t.Fatalf("live prompt = %q, err = %v", text, err)
	}
	second, err := registry.MaterializeSystemPrompt(promptTestSession{model: "p/second"})
	if err != nil || first == second {
		t.Fatalf("fresh product = %t, err = %v", first != second, err)
	}
}

func TestToolGuidanceProviderUsesMaterializedSessionGuidance(t *testing.T) {
	provider := ToolGuidanceProvider{}
	for _, test := range []struct{ guidance string }{{"sandbox-specific guidance"}, {""}} {
		prompt, err := provider.MaterializeSystemPrompt(promptTestSession{guidance: test.guidance})
		if err != nil {
			t.Fatal(err)
		}
		text, err := prompt.GetSystemPrompt(context.Background(), ModelSelection{})
		if err != nil || text != test.guidance {
			t.Fatalf("guidance = %q, err = %v", text, err)
		}
	}
	if _, err := provider.MaterializeSystemPrompt(nil); err == nil {
		t.Fatal("nil session accepted")
	}
}

func TestModelAugmentProviderPrecedenceAndExplicitEmpty(t *testing.T) {
	empty, alias := "", "alias guidance"
	resolver := &promptTestResolver{resolutions: map[string]ModelResolution{
		"alias":          {CanonicalBase: "p/m", CanonicalSelector: "p/m/high"},
		"disabled-alias": {CanonicalBase: "p/m", CanonicalSelector: "p/m/high"},
		"nil-alias":      {CanonicalBase: "p/m", CanonicalSelector: "p/m/high"},
		"p/m/high":       {CanonicalBase: "p/m", CanonicalSelector: "p/m/high"},
		"p/m/low":        {CanonicalBase: "p/m", CanonicalSelector: "p/m/low"},
		"p/m/medium":     {CanonicalBase: "p/m", CanonicalSelector: "p/m/medium"},
		"p/other":        {CanonicalBase: "p/other", CanonicalSelector: "p/other"},
	}}
	provider, err := NewModelAugmentProvider("model:augment", map[string]string{
		"p/m/high": "variant guidance",
		"p/m/low":  "",
		"p/m":      "base guidance",
	}, map[string]*string{
		"alias":          &alias,
		"disabled-alias": &empty,
		"nil-alias":      nil,
	}, resolver.resolve)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct{ selector, want string }{
		{"alias", "alias guidance"},
		{"disabled-alias", ""},
		{"nil-alias", "variant guidance"},
		{"p/m/high", "variant guidance"},
		{"p/m/low", ""},
		{"p/m/medium", "base guidance"},
		{"p/other", ""},
	}
	for _, test := range tests {
		prompt, err := provider.MaterializeSystemPrompt(promptTestSession{model: test.selector})
		if err != nil {
			t.Fatalf("%s: %v", test.selector, err)
		}
		text, err := prompt.GetSystemPrompt(context.Background(), ModelSelection{})
		if err != nil || text != test.want {
			t.Fatalf("%s = %q, err = %v; want %q", test.selector, text, err, test.want)
		}
	}
	// Explicit alias pointers, including explicit empty, do not need resolution.
	if resolver.calls != len(tests)-2 {
		t.Fatalf("resolver calls = %d, want %d", resolver.calls, len(tests)-2)
	}

	// A materialized product retains the session capability rather than a model
	// snapshot, so model changes between turns affect subsequent renders.
	session := &promptTestSession{model: "p/m/high"}
	prompt, err := provider.MaterializeSystemPrompt(session)
	if err != nil {
		t.Fatal(err)
	}
	first, err := prompt.GetSystemPrompt(context.Background(), ModelSelection{})
	if err != nil {
		t.Fatal(err)
	}
	session.model = "p/m/medium"
	second, err := prompt.GetSystemPrompt(context.Background(), ModelSelection{})
	if err != nil || first != "variant guidance" || second != "base guidance" {
		t.Fatalf("model-changing prompt = %q then %q, err = %v", first, second, err)
	}

	canonical := map[string]string{"p/m": "original"}
	overrides := map[string]*string{"alias": &alias}
	immutable, err := NewModelAugmentProvider("model:immutable", canonical, overrides, resolver.resolve)
	if err != nil {
		t.Fatal(err)
	}
	canonical["p/m"] = "mutated"
	alias = "mutated"
	prompt, _ = immutable.MaterializeSystemPrompt(promptTestSession{model: "alias"})
	text, _ := prompt.GetSystemPrompt(context.Background(), ModelSelection{})
	if text != "alias guidance" {
		t.Fatalf("constructor input mutation changed prompt: %q", text)
	}
}

func TestModelAugmentProviderUsesRequestModelSelectionBeforeResolvingSessionModel(t *testing.T) {
	alias := "alias guidance"
	resolver := &promptTestResolver{resolutions: map[string]ModelResolution{
		"session-selector": {CanonicalBase: "p/m", CanonicalSelector: "p/m/medium"},
	}}
	provider, err := NewModelAugmentProvider("model:selection", map[string]string{
		"p/m":      "base guidance",
		"p/m/high": "variant guidance",
	}, map[string]*string{"xhigh_llm": &alias}, resolver.resolve)
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name      string
		model     ModelSelection
		want      string
		wantCalls int
	}{
		{
			name:  "requested alias overrides canonical model",
			model: ModelSelection{RequestedSelector: "xhigh_llm", CanonicalBase: "p/m", CanonicalSelector: "p/m/high"},
			want:  "alias guidance",
		},
		{
			name:  "canonical variant avoids resolution",
			model: ModelSelection{RequestedSelector: "unresolved", CanonicalBase: "p/m", CanonicalSelector: "p/m/high"},
			want:  "variant guidance",
		},
		{
			name:  "canonical base avoids resolution",
			model: ModelSelection{RequestedSelector: "unresolved", CanonicalBase: "p/m", CanonicalSelector: "p/other"},
			want:  "base guidance",
		},
		{
			name:      "empty request resolves materialized session selector",
			model:     ModelSelection{},
			want:      "base guidance",
			wantCalls: 1,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			resolver.calls = 0
			prompt, err := provider.MaterializeSystemPrompt(promptTestSession{model: "session-selector"})
			if err != nil {
				t.Fatal(err)
			}
			text, err := prompt.GetSystemPrompt(context.Background(), test.model)
			if err != nil || text != test.want || resolver.calls != test.wantCalls {
				t.Fatalf("text = %q, err = %v, resolver calls = %d; want %q, %d", text, err, resolver.calls, test.want, test.wantCalls)
			}
		})
	}
}

func TestModelAugmentProviderValidatesAndPropagatesResolutionErrors(t *testing.T) {
	resolver := &promptTestResolver{err: errors.New("unknown selector")}
	for _, test := range []struct {
		key       string
		canonical map[string]string
		aliases   map[string]*string
		resolver  ModelResolver
	}{
		{key: "invalid", resolver: resolver.resolve},
		{key: "model:test", canonical: map[string]string{"": "bad"}, resolver: resolver.resolve},
		{key: "model:test", aliases: map[string]*string{"": nil}, resolver: resolver.resolve},
		{key: "model:test"},
	} {
		if _, err := NewModelAugmentProvider(test.key, test.canonical, test.aliases, test.resolver); err == nil {
			t.Fatalf("invalid provider %#v accepted", test)
		}
	}
	provider, _ := NewModelAugmentProvider("model:test", nil, nil, resolver.resolve)
	prompt, err := provider.MaterializeSystemPrompt(promptTestSession{model: "missing"})
	if err != nil {
		t.Fatal(err)
	}
	if text, err := prompt.GetSystemPrompt(context.Background(), ModelSelection{}); text != "" || err == nil || !strings.Contains(err.Error(), "unknown selector") {
		t.Fatalf("text = %q, err = %v", text, err)
	}
}
