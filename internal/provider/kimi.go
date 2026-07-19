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

	"github.com/amirulashraf/parrot-coder/internal/auth"
)

// Kimi is the Moonshot subscription provider. It streams over the same
// OpenAI-compatible transport as any configured provider and additionally
// reports the account balance backing a Kimi subscription.
type Kimi struct {
	*OpenAICompatible
	apiKey  auth.Secret
	client  *http.Client
	balance *url.URL
}

// NewKimi creates a Moonshot provider. Options are validated by
// NewOpenAICompatible before any credential is retained.
func NewKimi(options OpenAICompatibleOptions) (*Kimi, error) {
	compatible, err := NewOpenAICompatible(options)
	if err != nil {
		return nil, err
	}
	balance, err := endpointURL(options.BaseURL, "users/me/balance", options.AllowInsecureLocalhost)
	if err != nil {
		return nil, err
	}
	return &Kimi{
		OpenAICompatible: compatible, apiKey: options.APIKey,
		client: secureClient(options.HTTPClient, balance), balance: balance,
	}, nil
}

// Usage reports the balance remaining on the Moonshot account. Moonshot exposes
// a balance rather than rate-limit windows, so both usage windows are nil and
// only Credits is populated. Decoding tolerates extra fields.
func (p *Kimi) Usage(ctx context.Context) (SubscriptionUsage, error) {
	secrets := []string{p.apiKey.Value()}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, p.balance.String(), nil)
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
		Data *struct {
			AvailableBalance json.Number `json:"available_balance"`
		} `json:"data"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&wire); err != nil {
		return SubscriptionUsage{}, fmt.Errorf("provider: decode usage response: %w", err)
	}
	if wire.Data == nil {
		return SubscriptionUsage{}, errors.New("provider: usage response has no balance")
	}
	available, err := wire.Data.AvailableBalance.Float64()
	if err != nil {
		return SubscriptionUsage{}, fmt.Errorf("provider: decode usage balance: %w", err)
	}
	return SubscriptionUsage{Credits: &UsageCredits{HasCredits: available > 0, Balance: wire.Data.AvailableBalance.String()}}, nil
}
