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

// ModelListDecoder parses a raw model list response into provider models.
// The decoded models describe only what the catalog reports; declared and
// preset metadata is overlaid separately by the merge step. A nil decoder
// falls back to DecodeStandardModels, which understands the plain OpenAI
// /v1/models format.
type ModelListDecoder func(data []byte) ([]Model, error)

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
	// ModelListDecoder parses the endpoint's model list response. A nil value
	// selects DecodeStandardModels. Provider-specific decoders (such as
	// DecodeOpenRouterModels or DecodeKimiModels) parse vendor extensions a
	// standard list cannot express.
	ModelListDecoder ModelListDecoder
	HTTPClient       *http.Client
	HeaderTimeout    time.Duration
	// ProviderPreferences is forwarded as the top-level "provider" object of
	// each request body. OpenAI-compatible routers such as OpenRouter use it
	// to steer routing and fallback behavior. It is opaque JSON validated as
	// an object at request time, so the provider stays neutral to any one
	// vendor's schema.
	ProviderPreferences json.RawMessage
}

// OpenAICompatible is an explicitly configured compatible provider.
type OpenAICompatible struct {
	id                  string
	endpoint            *url.URL
	modelsEndpoint      *url.URL
	protocol            CompatibleProtocol
	apiKey              auth.Secret
	headers             http.Header
	declared            []Model
	defaults            []Model
	decoder             ModelListDecoder
	modelsMu            sync.RWMutex
	models              []Model
	client              *http.Client
	stream              *http.Client
	headerTimeout       time.Duration
	providerPreferences json.RawMessage
	responses           responseChain
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
	decoder := options.ModelListDecoder
	if decoder == nil {
		decoder = DecodeStandardModels
	}
	return &OpenAICompatible{
		id: options.ID, endpoint: endpoint, modelsEndpoint: modelsEndpoint, protocol: options.Protocol, apiKey: options.APIKey,
		headers: headers, declared: declared, defaults: defaults, decoder: decoder,
		client: secureClient(options.HTTPClient, endpoint, false),
		stream: secureClient(options.HTTPClient, endpoint, true),
		// Until a catalog is fetched the defaults stand in for one, so an
		// offline start still has models to select.
		models:              mergeModels(nil, declared, defaults),
		headerTimeout:       options.HeaderTimeout,
		providerPreferences: append(json.RawMessage(nil), options.ProviderPreferences...),
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
	fetched, err := p.decoder(data)
	if err != nil {
		return err
	}
	p.modelsMu.Lock()
	p.models = mergeModels(fetched, p.declared, p.defaults)
	p.modelsMu.Unlock()
	return nil
}

// DecodeStandardModels parses a standard OpenAI-format model list. Only the ID
// is standard; the remaining fields are vendor extensions used when present.
func DecodeStandardModels(data []byte) ([]Model, error) {
	var wire struct {
		Data []struct {
			ID              string `json:"id"`
			Name            string `json:"name"`
			ContextWindow   int    `json:"context_window"`
			ContextLength   int    `json:"context_length"`
			MaxOutputTokens int    `json:"max_output_tokens"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return nil, fmt.Errorf("provider: decode models response: %w", err)
	}
	seen := make(map[string]struct{}, len(wire.Data))
	models := make([]Model, 0, len(wire.Data))
	for _, item := range wire.Data {
		if item.ID == "" {
			continue
		}
		if _, duplicate := seen[item.ID]; duplicate {
			continue
		}
		seen[item.ID] = struct{}{}
		contextWindow := item.ContextWindow
		if contextWindow == 0 {
			contextWindow = item.ContextLength
		}
		name := item.Name
		if name == "" {
			name = item.ID
		}
		models = append(models, Model{
			ID: item.ID, Name: name,
			ContextWindow:   contextWindow,
			MaxOutputTokens: item.MaxOutputTokens,
			Capabilities:    Capabilities{Tools: true, Output: []string{"text"}},
		})
	}
	if len(models) == 0 {
		return nil, errors.New("provider: models response contains no usable models")
	}
	return models, nil
}

// mergeModels builds the catalog from what the endpoint serves, describing each
// entry with the best metadata available: a declared model first, then a
// built-in default, since a context window, output limit, or reasoning variant
// may be absent from a standard model list. Each decoder already fills what its
// catalog reports, so a fetched entry stands as-is when nothing else describes
// it; declared or preset metadata wins, with the catalog filling only the gaps.
//
// Declared models are always included, because the user asserted they exist.
// Defaults are only descriptions: one whose ID the endpoint does not serve is
// dropped, so built-in guesses cannot invent models. A nil fetched list means no
// catalog has been loaded yet, and the defaults stand in for one.
func mergeModels(fetched []Model, declared, defaults []Model) []Model {
	describe := make(map[string]Model, len(declared)+len(defaults))
	for _, item := range defaults {
		describe[item.ID] = item
	}
	for _, item := range declared {
		describe[item.ID] = item
	}
	if fetched == nil {
		fetched = make([]Model, 0, len(describe))
		for id := range describe {
			fetched = append(fetched, Model{ID: id})
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
			// The decoder already produced a complete model from the catalog;
			// nothing else describes it, so it stands as-is.
			model = item
		} else {
			// Known metadata wins; the catalog only fills gaps the declaration
			// or preset left open.
			if model.Name == "" {
				model.Name = item.Name
			}
			if model.ContextWindow == 0 {
				model.ContextWindow = item.ContextWindow
			}
			if model.MaxOutputTokens == 0 {
				model.MaxOutputTokens = item.MaxOutputTokens
			}
			if model.InputPrice == 0 {
				model.InputPrice = item.InputPrice
			}
			if model.OutputPrice == 0 {
				model.OutputPrice = item.OutputPrice
			}
			if len(model.Capabilities.Variants) == 0 {
				model.Capabilities.Variants = item.Capabilities.Variants
			}
		}
		if model.Name == "" {
			model.Name = model.ID
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

// orderedEfforts returns the supported reasoning efforts with the default
// effort first, so the first variant is the fallback a fresh selection lands
// on. An empty default leaves the order untouched.
func orderedEfforts(efforts []string, defaultEffort string) []string {
	if len(efforts) == 0 {
		return nil
	}
	result := append([]string(nil), efforts...)
	if defaultEffort == "" {
		return result
	}
	for i, effort := range result {
		if effort == defaultEffort {
			if i == 0 {
				return result
			}
			result[0], result[i] = result[i], result[0]
			return result
		}
	}
	return result
}

// effortVariants builds reasoning variants from a list of effort levels,
// ordered with the default first.
func effortVariants(efforts []string, defaultEffort string) []Variant {
	ordered := orderedEfforts(efforts, defaultEffort)
	variants := make([]Variant, 0, len(ordered))
	for _, effort := range ordered {
		variants = append(variants, Variant{Name: effort, ReasoningEffort: effort})
	}
	return variants
}

func (p *OpenAICompatible) Stream(ctx context.Context, request protocol.Request) (Stream, error) {
	fullRequest := request
	if p.protocol == ProtocolResponses {
		request = p.responses.prepare(request)
	}
	request.ProviderPreferences = p.providerPreferences
	request.IncludeRouterMetadata = len(p.providerPreferences) > 0
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
	stream, err := startStream(ctx, p.stream, p.endpoint, body, headers, []string{p.apiKey.Value()}, p.headerTimeout, parser)
	if err != nil {
		return nil, err
	}
	if p.protocol == ProtocolResponses {
		stream = p.responses.observe(stream, fullRequest)
	}
	return stream, nil
}
