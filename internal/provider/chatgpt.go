package provider

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/amirulashraf/parrot-coder/internal/auth"
	"github.com/amirulashraf/parrot-coder/internal/protocol"
	"github.com/amirulashraf/parrot-coder/internal/protocol/responses"
)

const chatGPTEndpoint = "https://chatgpt.com/backend-api/codex/responses"
const chatGPTModelsEndpoint = "https://chatgpt.com/backend-api/codex/models"
const chatGPTUsageEndpoint = "https://chatgpt.com/backend-api/wham/usage"
const chatGPTHeaderTimeout = 10 * time.Second

// modelsRefreshTimeout bounds a startup model-catalog fetch for any provider.
const modelsRefreshTimeout = 5 * time.Second
const chatGPTModelsClientVersion = "0.144.5"
const maxModelCatalogBytes = 16 << 20

// OAuthTokenSource is implemented by auth.TokenSource.
type OAuthTokenSource interface {
	Token(context.Context) (auth.OAuthCredential, error)
}

// ChatGPTOptions configures the fixed ChatGPT subscription provider.
type ChatGPTOptions struct {
	TokenSource OAuthTokenSource
	HTTPClient  *http.Client
}

// ChatGPT uses subscription OAuth credentials only with the compiled endpoint.
type ChatGPT struct {
	tokens    OAuthTokenSource
	client    *http.Client
	endpoint  *url.URL
	sessionID string
	modelsMu  sync.RWMutex
	models    []Model
}

// SubscriptionUsage is the quota information exposed by the ChatGPT Codex
// backend. Percentages are amounts already used, in the range 0 through 100.
type SubscriptionUsage struct {
	PlanType        string
	PrimaryWindow   *UsageWindow
	SecondaryWindow *UsageWindow
	Credits         *UsageCredits
}

type UsageWindow struct {
	UsedPercent        float64
	ResetAt            time.Time
	LimitWindowSeconds int64
}

type UsageCredits struct {
	HasCredits bool
	Balance    string
}

// NewChatGPT creates a subscription provider. Its endpoint and extra headers
// are deliberately not configurable.
func NewChatGPT(options ChatGPTOptions) (*ChatGPT, error) {
	if options.TokenSource == nil {
		return nil, errors.New("provider: ChatGPT token source is required")
	}
	endpoint, _ := url.Parse(chatGPTEndpoint)
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return nil, errors.New("provider: create ChatGPT session ID")
	}
	return &ChatGPT{
		tokens: options.TokenSource, endpoint: endpoint, sessionID: hex.EncodeToString(value[:]),
		client: secureClient(options.HTTPClient, endpoint), models: cloneModels(chatGPTModels),
	}, nil
}

func (p *ChatGPT) ID() string { return "chatgpt" }

func (p *ChatGPT) Models() []Model {
	p.modelsMu.RLock()
	defer p.modelsMu.RUnlock()
	return cloneModels(p.models)
}

// RefreshModels replaces the bundled model catalog with the current catalog
// returned by the ChatGPT Codex backend. The bundled catalog remains active if
// this method returns an error.
func (p *ChatGPT) RefreshModels(ctx context.Context) error {
	requestCtx, cancel := context.WithTimeout(ctx, modelsRefreshTimeout)
	defer cancel()
	credential, err := p.tokens.Token(requestCtx)
	if err != nil {
		return err
	}
	if credential.AccessToken.Value() == "" {
		return errors.New("provider: ChatGPT credential requires an access token")
	}
	endpoint, _ := url.Parse(chatGPTModelsEndpoint)
	query := endpoint.Query()
	// This declares compatibility with the Codex model catalog schema consumed
	// below; Parrot's own release version is unrelated to Codex client versions.
	query.Set("client_version", chatGPTModelsClientVersion)
	endpoint.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return fmt.Errorf("provider: create models request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+credential.AccessToken.Value())
	if credential.AccountID != "" {
		request.Header.Set("ChatGPT-Account-Id", credential.AccountID)
	}
	request.Header.Set("originator", "parrot")
	request.Header.Set("User-Agent", "parrot")
	request.Header.Set("Accept", "application/json")
	response, err := p.client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("provider: send models request: " + redact(err.Error(), []string{credential.AccessToken.Value()}))
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return parseHTTPError(response, []string{credential.AccessToken.Value()})
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxModelCatalogBytes+1))
	if err != nil {
		return fmt.Errorf("provider: read models response: %w", err)
	}
	if len(data) > maxModelCatalogBytes {
		return errors.New("provider: models response exceeds byte limit")
	}
	models, err := decodeChatGPTModels(data)
	if err != nil {
		return err
	}
	p.modelsMu.Lock()
	p.models = models
	p.modelsMu.Unlock()
	return nil
}

func decodeChatGPTModels(data []byte) ([]Model, error) {
	var response struct {
		Models []struct {
			Slug                     string `json:"slug"`
			DisplayName              string `json:"display_name"`
			Visibility               string `json:"visibility"`
			ContextWindow            int64  `json:"context_window"`
			MaxContextWindow         int64  `json:"max_context_window"`
			SupportedReasoningLevels []struct {
				Effort string `json:"effort"`
			} `json:"supported_reasoning_levels"`
		} `json:"models"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		return nil, fmt.Errorf("provider: decode models response: %w", err)
	}
	models := make([]Model, 0, len(response.Models))
	seen := make(map[string]struct{}, len(response.Models))
	for _, item := range response.Models {
		if item.Visibility != "list" {
			continue
		}
		contextWindow := item.ContextWindow
		if contextWindow == 0 {
			contextWindow = item.MaxContextWindow
		}
		if item.Slug == "" || contextWindow <= 0 || contextWindow > int64(^uint(0)>>1) {
			return nil, fmt.Errorf("provider: model catalog contains invalid listed model %q", item.Slug)
		}
		if _, exists := seen[item.Slug]; exists {
			continue
		}
		seen[item.Slug] = struct{}{}
		name := item.DisplayName
		if name == "" {
			name = item.Slug
		}
		variants := make([]Variant, 0, len(item.SupportedReasoningLevels))
		variantNames := make(map[string]struct{}, len(item.SupportedReasoningLevels))
		for _, level := range item.SupportedReasoningLevels {
			if level.Effort == "" {
				continue
			}
			if _, exists := variantNames[level.Effort]; exists {
				continue
			}
			variantNames[level.Effort] = struct{}{}
			variants = append(variants, Variant{Name: level.Effort, ReasoningEffort: level.Effort})
		}
		models = append(models, Model{
			// Codex reports its operational input window, with output headroom
			// already accounted for. Leaving MaxOutputTokens zero prevents the
			// compaction planner from reserving that headroom a second time.
			ID: item.Slug, Name: name, ContextWindow: int(contextWindow),
			Capabilities: Capabilities{Tools: true, Reasoning: len(variants) > 0, Output: []string{"text"}, Variants: variants},
		})
	}
	if len(models) == 0 {
		return nil, errors.New("provider: models response contains no usable models")
	}
	return models, nil
}

// Usage fetches current subscription rate-limit windows. This upstream
// endpoint is undocumented, so decoding deliberately tolerates extra fields.
func (p *ChatGPT) Usage(ctx context.Context) (SubscriptionUsage, error) {
	credential, err := p.tokens.Token(ctx)
	if err != nil {
		return SubscriptionUsage{}, err
	}
	if credential.AccessToken.Value() == "" {
		return SubscriptionUsage{}, errors.New("provider: ChatGPT credential requires an access token")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, chatGPTUsageEndpoint, nil)
	if err != nil {
		return SubscriptionUsage{}, fmt.Errorf("provider: create usage request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+credential.AccessToken.Value())
	if credential.AccountID != "" {
		request.Header.Set("ChatGPT-Account-Id", credential.AccountID)
	}
	request.Header.Set("User-Agent", "parrot")
	request.Header.Set("Accept", "application/json")
	response, err := p.client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return SubscriptionUsage{}, ctx.Err()
		}
		return SubscriptionUsage{}, errors.New("provider: send usage request: " + redact(err.Error(), []string{credential.AccessToken.Value()}))
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return SubscriptionUsage{}, parseHTTPError(response, []string{credential.AccessToken.Value()})
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxErrorBytes+1))
	if err != nil {
		return SubscriptionUsage{}, fmt.Errorf("provider: read usage response: %w", err)
	}
	if len(data) > maxErrorBytes {
		return SubscriptionUsage{}, errors.New("provider: usage response exceeds byte limit")
	}
	var wire struct {
		PlanType  string `json:"plan_type"`
		RateLimit struct {
			Primary   *wireUsageWindow `json:"primary_window"`
			Secondary *wireUsageWindow `json:"secondary_window"`
		} `json:"rate_limit"`
		Credits *struct {
			HasCredits bool        `json:"has_credits"`
			Balance    json.Number `json:"balance"`
		} `json:"credits"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&wire); err != nil {
		return SubscriptionUsage{}, fmt.Errorf("provider: decode usage response: %w", err)
	}
	result := SubscriptionUsage{PlanType: wire.PlanType, PrimaryWindow: mapUsageWindow(wire.RateLimit.Primary), SecondaryWindow: mapUsageWindow(wire.RateLimit.Secondary)}
	if wire.Credits != nil {
		result.Credits = &UsageCredits{HasCredits: wire.Credits.HasCredits, Balance: wire.Credits.Balance.String()}
	}
	return result, nil
}

type wireUsageWindow struct {
	UsedPercent        float64 `json:"used_percent"`
	ResetAt            int64   `json:"reset_at"`
	LimitWindowSeconds int64   `json:"limit_window_seconds"`
}

func mapUsageWindow(window *wireUsageWindow) *UsageWindow {
	if window == nil {
		return nil
	}
	return &UsageWindow{UsedPercent: window.UsedPercent, ResetAt: time.Unix(window.ResetAt, 0).UTC(), LimitWindowSeconds: window.LimitWindowSeconds}
}

func (p *ChatGPT) Stream(ctx context.Context, request protocol.Request) (Stream, error) {
	credential, err := p.tokens.Token(ctx)
	if err != nil {
		return nil, err
	}
	if credential.AccessToken.Value() == "" {
		return nil, errors.New("provider: ChatGPT credential requires an access token")
	}
	body, err := responses.EncodeRequest(request)
	if err != nil {
		return nil, err
	}
	headers := make(http.Header)
	headers.Set("Authorization", "Bearer "+credential.AccessToken.Value())
	if credential.AccountID != "" {
		headers.Set("ChatGPT-Account-Id", credential.AccountID)
	}
	headers.Set("originator", "parrot")
	headers.Set("User-Agent", "parrot")
	headers.Set("session-id", p.sessionID)
	parser := func(reader io.Reader, limit int) Stream { return responses.NewParser(reader, limit) }
	return startStream(ctx, p.client, p.endpoint, body, headers, []string{credential.AccessToken.Value()}, chatGPTHeaderTimeout, parser)
}

var chatGPTModels = []Model{
	{
		ID: "gpt-5.4", Name: "GPT-5.4", ContextWindow: 400000, MaxOutputTokens: 128000,
		Capabilities: Capabilities{Tools: true, Reasoning: true, Output: []string{"text"}},
	},
	{
		ID: "gpt-5.4-mini", Name: "GPT-5.4 Mini", ContextWindow: 400000, MaxOutputTokens: 128000,
		Capabilities: Capabilities{Tools: true, Reasoning: true, Output: []string{"text"}},
	},
	{
		ID: "gpt-5.5", Name: "GPT-5.5", ContextWindow: 400000, MaxOutputTokens: 128000,
		Capabilities: Capabilities{Tools: true, Reasoning: true, Output: []string{"text"}},
	},
	{
		ID: "gpt-5.6-sol", Name: "GPT-5.6 Sol", ContextWindow: 500000, MaxOutputTokens: 128000,
		Capabilities: Capabilities{Tools: true, Reasoning: true, Output: []string{"text"}},
	},
}

func init() {
	variants := []Variant{
		{Name: "low", ReasoningEffort: "low"}, {Name: "medium", ReasoningEffort: "medium"},
		{Name: "high", ReasoningEffort: "high"}, {Name: "xhigh", ReasoningEffort: "xhigh"},
	}
	for i := range chatGPTModels {
		chatGPTModels[i].Capabilities.Variants = variants
	}
}
