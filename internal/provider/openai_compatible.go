package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/amirulashraf/parrot-coder/internal/auth"
	"github.com/amirulashraf/parrot-coder/internal/protocol"
	"github.com/amirulashraf/parrot-coder/internal/protocol/chatcompletions"
	"github.com/amirulashraf/parrot-coder/internal/protocol/responses"
)

// CompatibleProtocol selects an OpenAI-compatible wire format.
type CompatibleProtocol string

const (
	ProtocolResponses       CompatibleProtocol = "responses"
	ProtocolChatCompletions CompatibleProtocol = "chat-completions"
)

// OpenAICompatibleOptions configures an API-key authenticated compatible API.
type OpenAICompatibleOptions struct {
	ID                     string
	BaseURL                string
	Protocol               CompatibleProtocol
	APIKey                 auth.Secret
	Headers                map[string]string
	AllowInsecureLocalhost bool
	// Models are declared by the user and always remain selectable, even when
	// the endpoint does not list them.
	Models []Model
	// ModelDefaults supply metadata a model list cannot express, such as a
	// context window or reasoning variants. They describe models rather than
	// declaring them: a default whose ID the endpoint does not serve is dropped
	// once a catalog is fetched, and only survives as an offline fallback.
	ModelDefaults []Model
	HTTPClient    *http.Client
	HeaderTimeout time.Duration
}

// OpenAICompatible is an explicitly configured compatible provider.
type OpenAICompatible struct {
	id             string
	endpoint       *url.URL
	modelsEndpoint *url.URL
	protocol       CompatibleProtocol
	apiKey         auth.Secret
	headers        http.Header
	declared       []Model
	defaults       []Model
	modelsMu       sync.RWMutex
	models         []Model
	client         *http.Client
	headerTimeout  time.Duration
}

// NewOpenAICompatible validates options before retaining any credentials.
func NewOpenAICompatible(options OpenAICompatibleOptions) (*OpenAICompatible, error) {
	if strings.TrimSpace(options.ID) == "" {
		return nil, errors.New("provider: compatible provider ID is required")
	}
	if options.APIKey.Value() == "" {
		return nil, errors.New("provider: compatible provider API key is required")
	}
	if options.HeaderTimeout < 0 {
		return nil, errors.New("provider: header timeout cannot be negative")
	}
	endpointName := ""
	switch options.Protocol {
	case ProtocolResponses:
		endpointName = "responses"
	case ProtocolChatCompletions:
		endpointName = "chat/completions"
	default:
		return nil, errors.New("provider: protocol must be responses or chat-completions")
	}
	endpoint, err := endpointURL(options.BaseURL, endpointName, options.AllowInsecureLocalhost)
	if err != nil {
		return nil, err
	}
	modelsEndpoint, err := endpointURL(options.BaseURL, "models", options.AllowInsecureLocalhost)
	if err != nil {
		return nil, err
	}
	headers, err := validatedHeaders(options.Headers)
	if err != nil {
		return nil, err
	}
	declared, defaults := cloneModels(options.Models), cloneModels(options.ModelDefaults)
	return &OpenAICompatible{
		id: options.ID, endpoint: endpoint, modelsEndpoint: modelsEndpoint, protocol: options.Protocol, apiKey: options.APIKey,
		headers: headers, declared: declared, defaults: defaults, client: secureClient(options.HTTPClient, endpoint),
		// Until a catalog is fetched the defaults stand in for one, so an
		// offline start still has models to select.
		models:        mergeModels(nil, declared, defaults),
		headerTimeout: options.HeaderTimeout,
	}, nil
}

func (p *OpenAICompatible) ID() string { return p.id }

func (p *OpenAICompatible) Models() []Model {
	p.modelsMu.RLock()
	defer p.modelsMu.RUnlock()
	return cloneModels(p.models)
}

// RefreshModels replaces the catalog with the models the endpoint serves,
// keeping the configured metadata as an overlay. The configured catalog stays
// active if this method returns an error.
func (p *OpenAICompatible) RefreshModels(ctx context.Context) error {
	requestCtx, cancel := context.WithTimeout(ctx, modelsRefreshTimeout)
	defer cancel()
	secrets := []string{p.apiKey.Value()}
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, p.modelsEndpoint.String(), nil)
	if err != nil {
		return fmt.Errorf("provider: create models request: %w", err)
	}
	request.Header = p.headers.Clone()
	request.Header.Set("Authorization", "Bearer "+p.apiKey.Value())
	request.Header.Set("Accept", "application/json")
	response, err := p.client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("provider: send models request: " + redact(err.Error(), secrets))
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return parseHTTPError(response, secrets)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxModelCatalogBytes+1))
	if err != nil {
		return fmt.Errorf("provider: read models response: %w", err)
	}
	if len(data) > maxModelCatalogBytes {
		return errors.New("provider: models response exceeds byte limit")
	}
	fetched, err := decodeCompatibleModels(data)
	if err != nil {
		return err
	}
	p.modelsMu.Lock()
	p.models = mergeModels(fetched, p.declared, p.defaults)
	p.modelsMu.Unlock()
	return nil
}

// compatibleModel is one entry of an OpenAI-format model list. Only the ID is
// standard; the remaining fields are vendor extensions used when present.
type compatibleModel struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	ContextWindow   int    `json:"context_window"`
	ContextLength   int    `json:"context_length"`
	MaxOutputTokens int    `json:"max_output_tokens"`
}

func (m compatibleModel) contextWindow() int {
	if m.ContextWindow > 0 {
		return m.ContextWindow
	}
	return m.ContextLength
}

func decodeCompatibleModels(data []byte) ([]compatibleModel, error) {
	var wire struct {
		Data []compatibleModel `json:"data"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return nil, fmt.Errorf("provider: decode models response: %w", err)
	}
	seen := make(map[string]struct{}, len(wire.Data))
	models := make([]compatibleModel, 0, len(wire.Data))
	for _, item := range wire.Data {
		if item.ID == "" {
			continue
		}
		if _, duplicate := seen[item.ID]; duplicate {
			continue
		}
		seen[item.ID] = struct{}{}
		models = append(models, item)
	}
	if len(models) == 0 {
		return nil, errors.New("provider: models response contains no usable models")
	}
	return models, nil
}

// mergeModels builds the catalog from what the endpoint serves, describing each
// entry with the best metadata available: a declared model first, then a
// built-in default, since a context window, output limit, or reasoning variant
// cannot be discovered from an OpenAI-format model list.
//
// Declared models are always included, because the user asserted they exist.
// Defaults are only descriptions: one whose ID the endpoint does not serve is
// dropped, so built-in guesses cannot invent models. A nil fetched list means no
// catalog has been loaded yet, and the defaults stand in for one.
func mergeModels(fetched []compatibleModel, declared, defaults []Model) []Model {
	describe := make(map[string]Model, len(declared)+len(defaults))
	for _, item := range defaults {
		describe[item.ID] = item
	}
	for _, item := range declared {
		describe[item.ID] = item
	}
	if fetched == nil {
		fetched = make([]compatibleModel, 0, len(describe))
		for id := range describe {
			fetched = append(fetched, compatibleModel{ID: id})
		}
	}
	result := make([]Model, 0, len(fetched)+len(declared))
	listed := make(map[string]struct{}, len(fetched))
	for _, item := range fetched {
		if _, duplicate := listed[item.ID]; duplicate {
			continue
		}
		listed[item.ID] = struct{}{}
		model, described := describe[item.ID]
		if !described {
			// Tools, reasoning, and output are reported but never gate a
			// request, so assuming the common case costs nothing when wrong.
			model = Model{ID: item.ID, Capabilities: Capabilities{Tools: true, Output: []string{"text"}}}
		}
		if model.Name == "" {
			model.Name = item.Name
		}
		if model.Name == "" {
			model.Name = item.ID
		}
		if model.ContextWindow == 0 {
			model.ContextWindow = item.contextWindow()
		}
		if model.MaxOutputTokens == 0 {
			model.MaxOutputTokens = item.MaxOutputTokens
		}
		model.Capabilities.Reasoning = model.Capabilities.Reasoning || len(model.Capabilities.Variants) > 0
		result = append(result, model)
	}
	for _, item := range declared {
		if _, ok := listed[item.ID]; !ok {
			result = append(result, item)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return cloneModels(result)
}

func (p *OpenAICompatible) Stream(ctx context.Context, request protocol.Request) (Stream, error) {
	var body []byte
	var err error
	var parser streamParser
	switch p.protocol {
	case ProtocolResponses:
		body, err = responses.EncodeRequest(request)
		parser = func(reader io.Reader, limit int) Stream { return responses.NewParser(reader, limit) }
	case ProtocolChatCompletions:
		body, err = chatcompletions.EncodeRequest(request)
		parser = func(reader io.Reader, limit int) Stream { return chatcompletions.NewParser(reader, limit) }
	}
	if err != nil {
		return nil, err
	}
	headers := p.headers.Clone()
	headers.Set("Authorization", "Bearer "+p.apiKey.Value())
	return startStream(ctx, p.client, p.endpoint, body, headers, []string{p.apiKey.Value()}, p.headerTimeout, parser)
}
