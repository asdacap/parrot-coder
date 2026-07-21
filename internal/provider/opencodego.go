package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/amirulashraf/parrot-coder/internal/auth"
)

// OpenCodeGo backs the opencode-go provider. It streams through the same
// OpenAI-compatible transport as any configured provider and additionally
// reports subscription usage from the /zen/go/v1/usage endpoint.
type OpenCodeGo struct {
	*OpenAICompatible
	apiKey auth.Secret
	client *http.Client
	usage  *url.URL
}

// NewOpenCodeGo creates an OpenCode Go provider. Options are validated by
// NewOpenAICompatible before any credential is retained.
func NewOpenCodeGo(options OpenAICompatibleOptions) (*OpenCodeGo, error) {
	compatible, err := NewOpenAICompatible(options)
	if err != nil {
		return nil, err
	}
	usage, err := endpointURL(options.BaseURL, "usage", options.AllowInsecureLocalhost)
	if err != nil {
		return nil, err
	}
	return &OpenCodeGo{
		OpenAICompatible: compatible, apiKey: options.APIKey,
		client: secureClient(options.HTTPClient, usage, false), usage: usage,
	}, nil
}

// Usage reports the subscription usage (rolling, weekly, monthly windows)
// from the OpenCode Go /zen/go/v1/usage endpoint. The response shape is
// defined by the upstream PR #16513. Parsing tolerates extra fields.
func (p *OpenCodeGo) Usage(ctx context.Context) (SubscriptionUsage, error) {
	secrets := []string{p.apiKey.Value()}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, p.usage.String(), nil)
	if err != nil {
		return SubscriptionUsage{}, fmt.Errorf("provider: create usage request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+p.apiKey.Value())
	request.Header.Set("Accept", "application/json")
	response, err := p.client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return SubscriptionUsage{}, ctx.Err()
		}
		return SubscriptionUsage{}, errors.New("provider: send usage request: " + redact(err.Error(), secrets))
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return SubscriptionUsage{}, parseHTTPError(response, secrets)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxErrorBytes+1))
	if err != nil {
		return SubscriptionUsage{}, fmt.Errorf("provider: read usage response: %w", err)
	}
	if len(data) > maxErrorBytes {
		return SubscriptionUsage{}, errors.New("provider: usage response exceeds byte limit")
	}
	var wire struct {
		UseBalance   bool             `json:"useBalance"`
		RollingUsage *json.RawMessage `json:"rollingUsage"`
		WeeklyUsage  *json.RawMessage `json:"weeklyUsage"`
		MonthlyUsage *json.RawMessage `json:"monthlyUsage"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&wire); err != nil {
		return SubscriptionUsage{}, fmt.Errorf("provider: decode usage response: %w", err)
	}
	result := SubscriptionUsage{}
	// Extract a primary window from rolling usage (the most granular window).
	if wire.RollingUsage != nil {
		if window, err := decodeOpenCodeGoWindow(*wire.RollingUsage); err == nil {
			result.PrimaryWindow = window
		}
	}
	// Extract a secondary window from weekly usage.
	if wire.WeeklyUsage != nil {
		if window, err := decodeOpenCodeGoWindow(*wire.WeeklyUsage); err == nil {
			// If rolling was not available, promote weekly to primary.
			if result.PrimaryWindow == nil {
				result.PrimaryWindow = window
			} else {
				result.SecondaryWindow = window
			}
		}
	}
	// Fall back to monthly when neither rolling nor weekly had a usable window.
	if result.PrimaryWindow == nil && wire.MonthlyUsage != nil {
		if window, err := decodeOpenCodeGoWindow(*wire.MonthlyUsage); err == nil {
			result.PrimaryWindow = window
		}
	}
	// Populate credits when the plan uses balance-based billing.
	if wire.UseBalance && result.PrimaryWindow != nil {
		remaining := 100 - result.PrimaryWindow.UsedPercent
		if remaining < 0 {
			remaining = 0
		}
		result.Credits = &UsageCredits{
			HasCredits: true,
			Balance:    fmt.Sprintf("%.0f%% remaining", remaining),
		}
	}
	return result, nil
}

// openCodeGoWindowWire matches the per-window object the upstream endpoint
// returns (rollingUsage, weeklyUsage, monthlyUsage). The Subscription analysis
// functions produce fields with camelCase names.
type openCodeGoWindowWire struct {
	UsedPercent      json.Number `json:"usedPercent"`
	RemainingPercent json.Number `json:"remainingPercent,omitempty"`
	Limit            json.Number `json:"limit"`
	Used             json.Number `json:"used,omitempty"`
	Window           json.Number `json:"window,omitempty"`
	ResetAt          string      `json:"resetAt"`
}

func decodeOpenCodeGoWindow(raw json.RawMessage) (*UsageWindow, error) {
	var w openCodeGoWindowWire
	if err := json.Unmarshal(raw, &w); err != nil {
		return nil, err
	}
	usedPercent, _ := w.UsedPercent.Float64()
	// If only remaining was sent, compute used from remaining.
	if usedPercent == 0 {
		if remaining, _ := w.RemainingPercent.Float64(); remaining > 0 {
			usedPercent = 100 - remaining
			if usedPercent < 0 {
				usedPercent = 0
			} else if usedPercent > 100 {
				usedPercent = 100
			}
		}
	}
	var resetAt time.Time
	if w.ResetAt != "" {
		var parseErr error
		resetAt, parseErr = time.Parse(time.RFC3339, w.ResetAt)
		if parseErr != nil {
			// Try ISO 8601 without timezone.
			resetAt, parseErr = time.Parse("2006-01-02T15:04:05", w.ResetAt)
			if parseErr != nil {
				return nil, fmt.Errorf("provider: parse resetAt: %w", parseErr)
			}
		}
	}
	var windowSec int64
	if w.Window.String() != "" {
		windowSec, _ = w.Window.Int64()
	}
	return &UsageWindow{
		UsedPercent:        usedPercent,
		ResetAt:            resetAt,
		LimitWindowSeconds: windowSec,
	}, nil
}
