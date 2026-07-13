package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type DeviceAuthorization struct {
	DeviceAuthID    Secret
	UserCode        Secret
	VerificationURL string
	Interval        time.Duration
	ExpiresAt       time.Time
}

func (d DeviceAuthorization) String() string {
	return fmt.Sprintf("device authorization (url %s, codes [REDACTED])", d.VerificationURL)
}

func (c *OpenAI) StartDeviceAuthorization(ctx context.Context) (DeviceAuthorization, error) {
	body := strings.NewReader(`{"client_id":"` + OpenAIClientID + `"}`)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.issuer()+"/api/accounts/deviceauth/usercode", body)
	if err != nil {
		return DeviceAuthorization{}, errors.New("auth: create device authorization request")
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.do(request)
	if err != nil {
		if ctx.Err() != nil {
			return DeviceAuthorization{}, ctx.Err()
		}
		return DeviceAuthorization{}, errors.New("auth: device authorization request failed")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return DeviceAuthorization{}, fmt.Errorf("auth: device authorization returned HTTP %d", response.StatusCode)
	}
	var value struct {
		DeviceAuthID string `json:"device_auth_id"`
		UserCode     string `json:"user_code"`
		Interval     any    `json:"interval"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := decodeLimitedJSON(response.Body, &value); err != nil || value.DeviceAuthID == "" || value.UserCode == "" {
		return DeviceAuthorization{}, errors.New("auth: invalid device authorization response")
	}
	interval := parseInterval(value.Interval)
	if interval < time.Second {
		interval = 5 * time.Second
	}
	expires := time.Duration(value.ExpiresIn) * time.Second
	if expires <= 0 || expires > c.timeout() {
		expires = c.timeout()
	}
	return DeviceAuthorization{
		DeviceAuthID: Secret(value.DeviceAuthID), UserCode: Secret(value.UserCode),
		VerificationURL: c.issuer() + "/codex/device", Interval: interval, ExpiresAt: c.now().Add(expires),
	}, nil
}

func parseInterval(value any) time.Duration {
	var seconds int64
	switch value := value.(type) {
	case string:
		fmt.Sscan(value, &seconds)
	case float64:
		seconds = int64(value)
	case json.Number:
		seconds, _ = value.Int64()
	}
	return time.Duration(seconds) * time.Second
}

func (c *OpenAI) AwaitDeviceAuthorization(ctx context.Context, device DeviceAuthorization) (OAuthCredential, error) {
	deadline := device.ExpiresAt
	if deadline.IsZero() || deadline.After(c.now().Add(c.timeout())) {
		deadline = c.now().Add(c.timeout())
	}
	pollCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	for {
		credential, pending, err := c.pollDevice(pollCtx, device)
		if err != nil {
			return OAuthCredential{}, err
		}
		if !pending {
			return credential, nil
		}
		margin := 3 * time.Second
		if c != nil && c.PollingSafetyMargin != 0 {
			margin = c.PollingSafetyMargin
		}
		if err := c.sleep(pollCtx, device.Interval+margin); err != nil {
			return OAuthCredential{}, err
		}
	}
}

func (c *OpenAI) pollDevice(ctx context.Context, device DeviceAuthorization) (OAuthCredential, bool, error) {
	payload, _ := json.Marshal(map[string]string{"device_auth_id": device.DeviceAuthID.Value(), "user_code": device.UserCode.Value()})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.issuer()+"/api/accounts/deviceauth/token", strings.NewReader(string(payload)))
	if err != nil {
		return OAuthCredential{}, false, errors.New("auth: create device polling request")
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.do(request)
	if err != nil {
		if ctx.Err() != nil {
			return OAuthCredential{}, false, ctx.Err()
		}
		return OAuthCredential{}, false, errors.New("auth: device polling request failed")
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusForbidden || response.StatusCode == http.StatusNotFound {
		return OAuthCredential{}, true, nil
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return OAuthCredential{}, false, fmt.Errorf("auth: device polling returned HTTP %d", response.StatusCode)
	}
	var value struct {
		AuthorizationCode Secret `json:"authorization_code"`
		CodeVerifier      Secret `json:"code_verifier"`
	}
	if err := decodeLimitedJSON(response.Body, &value); err != nil || value.AuthorizationCode == "" || value.CodeVerifier == "" {
		return OAuthCredential{}, false, errors.New("auth: invalid device polling response")
	}
	credential, err := c.exchange(ctx, value.AuthorizationCode, c.issuer()+"/deviceauth/callback", value.CodeVerifier)
	return credential, false, err
}
