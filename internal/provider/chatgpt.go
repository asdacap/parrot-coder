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
	"time"

	"github.com/amirulashraf/parrot-coder/internal/auth"
	"github.com/amirulashraf/parrot-coder/internal/protocol"
	"github.com/amirulashraf/parrot-coder/internal/protocol/responses"
)

const chatGPTEndpoint = "https://chatgpt.com/backend-api/codex/responses"
const chatGPTUsageEndpoint = "https://chatgpt.com/backend-api/wham/usage"
const chatGPTHeaderTimeout = 10 * time.Second

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
		client: secureClient(options.HTTPClient, endpoint),
	}, nil
}

func (p *ChatGPT) ID() string { return "chatgpt" }

func (p *ChatGPT) Models() []Model { return cloneModels(chatGPTModels) }

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
	variants := map[string]Variant{
		"low": {ReasoningEffort: "low"}, "medium": {ReasoningEffort: "medium"},
		"high": {ReasoningEffort: "high"}, "xhigh": {ReasoningEffort: "xhigh"},
	}
	for i := range chatGPTModels {
		chatGPTModels[i].Capabilities.Variants = variants
		chatGPTModels[i].Capabilities.VariantOrder = []string{"low", "medium", "high", "xhigh"}
	}
}
