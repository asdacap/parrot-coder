package systemcontext

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// ModelSelection is the immutable model identity used by one provider request.
type ModelSelection struct {
	RequestedSelector string
	CanonicalBase     string
	CanonicalSelector string
}

// SystemPrompt is a prompt materialized for one agent session.
type SystemPrompt interface {
	GetSystemPrompt(context.Context, ModelSelection) (string, error)
}

// AgentSession is the session capability needed to materialize system prompts.
// Tool guidance is already materialized so this package need not depend on tools.
type AgentSession interface {
	ModelSelector() string
	ToolSystemGuidance() string
}

// SystemPromptProvider creates a fresh prompt product for an agent session.
type SystemPromptProvider interface {
	Key() string
	MaterializeSystemPrompt(AgentSession) (SystemPrompt, error)
}

// CompositeSystemPromptProvider is an immutable keyed provider composition.
type CompositeSystemPromptProvider struct {
	key       string
	providers []keyedSystemPromptProvider
}

type keyedSystemPromptProvider struct {
	key      string
	provider SystemPromptProvider
}

func NewCompositeSystemPromptProvider(key string, providers ...SystemPromptProvider) (*CompositeSystemPromptProvider, error) {
	if !validKey(key) {
		return nil, errors.New("systemcontext: composite requires a stable namespaced key")
	}
	ordered := make([]keyedSystemPromptProvider, 0, len(providers))
	seen := make(map[string]struct{}, len(providers))
	for _, provider := range providers {
		if isNil(provider) {
			return nil, errors.New("systemcontext: provider requires a stable namespaced key")
		}
		providerKey := provider.Key()
		if !validKey(providerKey) {
			return nil, errors.New("systemcontext: provider requires a stable namespaced key")
		}
		if _, exists := seen[providerKey]; exists {
			return nil, fmt.Errorf("systemcontext: duplicate provider %q", providerKey)
		}
		seen[providerKey] = struct{}{}
		ordered = append(ordered, keyedSystemPromptProvider{key: providerKey, provider: provider})
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].key < ordered[j].key })
	return &CompositeSystemPromptProvider{key: key, providers: ordered}, nil
}

func (p *CompositeSystemPromptProvider) Key() string { return p.key }

func (p *CompositeSystemPromptProvider) MaterializeSystemPrompt(session AgentSession) (SystemPrompt, error) {
	if p == nil {
		return nil, errors.New("systemcontext: composite provider is unavailable")
	}
	products := make([]keyedSystemPrompt, 0, len(p.providers))
	var failures []error
	for _, registered := range p.providers {
		prompt, err := registered.provider.MaterializeSystemPrompt(session)
		if err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", registered.key, err))
			continue
		}
		if isNil(prompt) {
			failures = append(failures, fmt.Errorf("%s: nil system prompt", registered.key))
			continue
		}
		products = append(products, keyedSystemPrompt{key: registered.key, prompt: prompt})
	}
	if err := errors.Join(failures...); err != nil {
		return nil, err
	}
	return compositeSystemPrompt{prompts: products}, nil
}

type keyedSystemPrompt struct {
	key    string
	prompt SystemPrompt
}

type compositeSystemPrompt struct{ prompts []keyedSystemPrompt }

func (p compositeSystemPrompt) GetSystemPrompt(ctx context.Context, model ModelSelection) (string, error) {
	sections := make([]string, 0, len(p.prompts))
	var failures []error
	for _, product := range p.prompts {
		text, err := product.prompt.GetSystemPrompt(ctx, model)
		if err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", product.key, err))
			continue
		}
		if strings.TrimSpace(text) != "" {
			sections = append(sections, text)
		}
	}
	return strings.Join(sections, "\n\n"), errors.Join(failures...)
}

type staticSystemPrompt string

func (p staticSystemPrompt) GetSystemPrompt(context.Context, ModelSelection) (string, error) {
	return string(p), nil
}

func isNil(value any) bool {
	if value == nil {
		return true
	}
	kind := reflect.ValueOf(value).Kind()
	return (kind == reflect.Chan || kind == reflect.Func || kind == reflect.Interface || kind == reflect.Map || kind == reflect.Pointer || kind == reflect.Slice) && reflect.ValueOf(value).IsNil()
}

// ToolGuidanceProvider emits the guidance materialized by the target session.
type ToolGuidanceProvider struct{}

func (ToolGuidanceProvider) Key() string { return "runtime:tool-system-guidance" }
func (ToolGuidanceProvider) MaterializeSystemPrompt(session AgentSession) (SystemPrompt, error) {
	if isNil(session) {
		return nil, errors.New("systemcontext: agent session is unavailable")
	}
	return staticSystemPrompt(session.ToolSystemGuidance()), nil
}

// ModelResolution contains only the canonical lookup keys needed by prompt
// augmentation. A richer model registry can adapt to this shape with a closure.
type ModelResolution struct {
	CanonicalBase     string
	CanonicalSelector string
}

// ModelResolver is the narrow resolver capability required by augmentation.
type ModelResolver func(string) (ModelResolution, error)

func (r ModelResolver) ResolveModel(selector string) (ModelResolution, error) { return r(selector) }

// ModelAugmentProvider selects optional prompt text for the session model.
type ModelAugmentProvider struct {
	key            string
	canonical      map[string]string
	aliasOverrides map[string]*string
	resolver       ModelResolver
}

func NewModelAugmentProvider(key string, canonical map[string]string, aliasOverrides map[string]*string, resolver ModelResolver) (*ModelAugmentProvider, error) {
	if !validKey(key) {
		return nil, errors.New("systemcontext: model augment provider requires a stable namespaced key")
	}
	if isNil(resolver) {
		return nil, errors.New("systemcontext: model resolver is unavailable")
	}
	canonicalCopy := make(map[string]string, len(canonical))
	for selector, text := range canonical {
		if selector == "" {
			return nil, errors.New("systemcontext: model augment canonical selector is empty")
		}
		canonicalCopy[selector] = text
	}
	aliasCopy := make(map[string]*string, len(aliasOverrides))
	for alias, text := range aliasOverrides {
		if alias == "" {
			return nil, errors.New("systemcontext: model augment alias is empty")
		}
		if text != nil {
			value := *text
			aliasCopy[alias] = &value
		} else {
			aliasCopy[alias] = nil
		}
	}
	return &ModelAugmentProvider{key: key, canonical: canonicalCopy, aliasOverrides: aliasCopy, resolver: resolver}, nil
}

func (p *ModelAugmentProvider) Key() string { return p.key }
func (p *ModelAugmentProvider) MaterializeSystemPrompt(session AgentSession) (SystemPrompt, error) {
	if p == nil || isNil(session) {
		return nil, errors.New("systemcontext: model augment session is unavailable")
	}
	return modelAugmentSystemPrompt{provider: p, session: session}, nil
}

type modelAugmentSystemPrompt struct {
	provider *ModelAugmentProvider
	session  AgentSession
}

func (p modelAugmentSystemPrompt) GetSystemPrompt(_ context.Context, model ModelSelection) (string, error) {
	if model.RequestedSelector == "" {
		model.RequestedSelector = p.session.ModelSelector()
	}
	if override, exists := p.provider.aliasOverrides[model.RequestedSelector]; exists && override != nil {
		return *override, nil
	}
	if model.CanonicalSelector != "" {
		if text, exists := p.provider.canonical[model.CanonicalSelector]; exists {
			return text, nil
		}
	}
	if model.CanonicalBase != "" {
		if text, exists := p.provider.canonical[model.CanonicalBase]; exists {
			return text, nil
		}
	}
	resolved, err := p.provider.resolver.ResolveModel(model.RequestedSelector)
	if err != nil {
		return "", err
	}
	if resolved.CanonicalSelector != "" {
		if text, exists := p.provider.canonical[resolved.CanonicalSelector]; exists {
			return text, nil
		}
	}
	if resolved.CanonicalBase != "" {
		if text, exists := p.provider.canonical[resolved.CanonicalBase]; exists {
			return text, nil
		}
	}
	return "", nil
}
