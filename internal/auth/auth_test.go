package auth

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestFileStoreRoundTripModesAndMalformed(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	path := filepath.Join(dir, "credentials.json")
	store := NewFileStore(path)
	ctx := context.Background()
	api := NewAPIKeyCredential("api-secret")
	if err := store.Put(ctx, "z", api); err != nil {
		t.Fatal(err)
	}
	oauth := NewOAuthCredential(OAuthCredential{
		AccessToken: "access-secret", RefreshToken: "refresh-secret",
		ExpiresAt: time.Now().Add(time.Hour), AccountID: "account",
	})
	if err := store.Put(ctx, "a", oauth); err != nil {
		t.Fatal(err)
	}
	names, err := store.List(ctx)
	if err != nil || fmt.Sprint(names) != "[a z]" {
		t.Fatalf("List() = %v, %v", names, err)
	}
	got, err := store.Get(ctx, "z")
	if err != nil || got.APIKey.Key.Value() != "api-secret" {
		t.Fatalf("Get() = %v, %v", got, err)
	}
	for _, check := range []struct {
		path string
		mode os.FileMode
	}{{dir, 0o700}, {path, 0o600}} {
		info, err := os.Stat(check.path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != check.mode {
			t.Fatalf("mode of %s = %o, want %o", check.path, info.Mode().Perm(), check.mode)
		}
	}
	if strings.Contains(fmt.Sprint(api, oauth, api.APIKey.Key, oauth.OAuth.AccessToken), "secret") {
		t.Fatal("String formatting exposed a secret")
	}
	if err := store.Delete(ctx, "z"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, "z"); !errors.Is(err, ErrCredentialNotFound) {
		t.Fatalf("Get deleted error = %v", err)
	}

	if err := os.WriteFile(path, []byte(`{"version":1,"credentials":{"bad":{"version":1,"type":"oauth"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.List(ctx); err == nil {
		t.Fatal("malformed store was accepted")
	}
	if err := store.Put(ctx, "new", api); err == nil {
		t.Fatal("Put overwrote malformed store")
	}
}

func TestAuthorizationURL(t *testing.T) {
	client := &OpenAI{Issuer: "https://issuer.example/base"}
	raw, err := client.AuthorizationURL(OpenAICallbackURL, "challenge", "state")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Path != "/base/oauth/authorize" {
		t.Fatalf("path = %q", parsed.Path)
	}
	want := map[string]string{
		"response_type": "code", "client_id": OpenAIClientID, "redirect_uri": OpenAICallbackURL,
		"scope": "openid profile email offline_access", "code_challenge": "challenge",
		"code_challenge_method": "S256", "state": "state", "originator": "opencode",
		"id_token_add_organizations": "true", "codex_cli_simplified_flow": "true",
	}
	for key, value := range want {
		if parsed.Query().Get(key) != value {
			t.Errorf("query %s = %q, want %q", key, parsed.Query().Get(key), value)
		}
	}
}

func TestBrowserLoginExchangeAndCleanup(t *testing.T) {
	var exchanged atomic.Bool
	issuer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/oauth/token" {
			http.NotFound(response, request)
			return
		}
		if err := request.ParseForm(); err != nil {
			t.Error(err)
		}
		if request.Form.Get("code") != "browser-code" || request.Form.Get("redirect_uri") != OpenAICallbackURL {
			t.Errorf("unexpected exchange form")
		}
		verifier := request.Form.Get("code_verifier")
		if verifier == "" {
			t.Error("missing verifier")
		}
		exchanged.Store(true)
		io.WriteString(response, `{"access_token":"access","refresh_token":"refresh","expires_in":3600}`)
	}))
	defer issuer.Close()

	client := &OpenAI{Issuer: issuer.URL, HTTPClient: issuer.Client(), LoginTimeout: time.Second}
	client.OpenBrowser = func(raw string) error {
		authorize, err := url.Parse(raw)
		if err != nil {
			return err
		}
		challenge := authorize.Query().Get("code_challenge")
		state := authorize.Query().Get("state")
		if challenge == "" || state == "" || authorize.Query().Get("redirect_uri") != OpenAICallbackURL {
			return errors.New("invalid authorization URL")
		}
		response, err := http.Get(OpenAICallbackURL + "?code=browser-code&state=" + url.QueryEscape(state))
		if err != nil {
			return err
		}
		response.Body.Close()
		return nil
	}
	credential, err := client.BrowserLogin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !exchanged.Load() || credential.AccessToken.Value() != "access" {
		t.Fatalf("credential = %v", credential)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:1455")
	if err != nil {
		t.Fatalf("callback listener was not cleaned up: %v", err)
	}
	listener.Close()
}

func TestBrowserLoginStateMismatchCleansUp(t *testing.T) {
	client := &OpenAI{LoginTimeout: time.Second, OpenBrowser: func(string) error {
		response, err := http.Get(OpenAICallbackURL + "?code=do-not-log&state=wrong")
		if err == nil {
			response.Body.Close()
		}
		return err
	}}
	_, err := client.BrowserLogin(context.Background())
	if err == nil || strings.Contains(err.Error(), "do-not-log") {
		t.Fatalf("BrowserLogin error = %v", err)
	}
	listener, listenErr := net.Listen("tcp", "127.0.0.1:1455")
	if listenErr != nil {
		t.Fatalf("callback listener was not cleaned up: %v", listenErr)
	}
	listener.Close()
}

func TestDevicePendingThenExchange(t *testing.T) {
	var polls atomic.Int32
	issuer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/accounts/deviceauth/usercode":
			io.WriteString(response, `{"device_auth_id":"device-secret","user_code":"user-secret","interval":"1","expires_in":60}`)
		case "/api/accounts/deviceauth/token":
			if polls.Add(1) == 1 {
				response.WriteHeader(http.StatusForbidden)
				return
			}
			io.WriteString(response, `{"authorization_code":"authorization-secret","code_verifier":"verifier-secret"}`)
		case "/oauth/token":
			request.ParseForm()
			if request.Form.Get("redirect_uri") != "http://"+request.Host+"/deviceauth/callback" {
				t.Error("wrong device redirect")
			}
			io.WriteString(response, `{"access_token":"access","refresh_token":"refresh","expires_in":3600}`)
		default:
			http.NotFound(response, request)
		}
	}))
	defer issuer.Close()
	client := &OpenAI{Issuer: issuer.URL, HTTPClient: issuer.Client(), Sleep: func(context.Context, time.Duration) error { return nil }}
	device, err := client.StartDeviceAuthorization(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if device.VerificationURL != issuer.URL+"/codex/device" || strings.Contains(device.String(), "secret") {
		t.Fatalf("device = %v", device)
	}
	credential, err := client.AwaitDeviceAuthorization(context.Background(), device)
	if err != nil || credential.AccessToken.Value() != "access" || polls.Load() != 2 {
		t.Fatalf("AwaitDeviceAuthorization() = %v, %v; polls %d", credential, err, polls.Load())
	}
}

func TestDeviceCancellationAndExpiry(t *testing.T) {
	issuer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.WriteHeader(http.StatusForbidden)
	}))
	defer issuer.Close()
	client := &OpenAI{Issuer: issuer.URL, HTTPClient: issuer.Client(), PollingSafetyMargin: -3 * time.Second}
	device := DeviceAuthorization{DeviceAuthID: "device", UserCode: "user", Interval: time.Second, ExpiresAt: time.Now().Add(time.Minute)}
	ctx, cancel := context.WithCancel(context.Background())
	client.Sleep = func(ctx context.Context, _ time.Duration) error {
		cancel()
		<-ctx.Done()
		return ctx.Err()
	}
	if _, err := client.AwaitDeviceAuthorization(ctx, device); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}

	client.Sleep = nil
	device.ExpiresAt = time.Now().Add(20 * time.Millisecond)
	if _, err := client.AwaitDeviceAuthorization(context.Background(), device); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expiry error = %v", err)
	}
}

func TestTokenSourceRefreshRotationAndConcurrency(t *testing.T) {
	var refreshes atomic.Int32
	issuer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		refreshes.Add(1)
		request.ParseForm()
		if request.Form.Get("refresh_token") != "old-refresh" {
			t.Error("wrong refresh token")
		}
		time.Sleep(20 * time.Millisecond)
		io.WriteString(response, `{"access_token":"new-access","refresh_token":"new-refresh","expires_in":3600}`)
	}))
	defer issuer.Close()
	store := NewFileStore(filepath.Join(t.TempDir(), "auth", "credentials.json"))
	if err := store.Put(context.Background(), "openai", NewOAuthCredential(OAuthCredential{
		AccessToken: "old-access", RefreshToken: "old-refresh", ExpiresAt: time.Now().Add(time.Minute), AccountID: "old-account",
	})); err != nil {
		t.Fatal(err)
	}
	source := NewTokenSource(&OpenAI{Issuer: issuer.URL, HTTPClient: issuer.Client()}, store, "openai")
	const callers = 12
	start := make(chan struct{})
	errorsSeen := make(chan error, callers)
	var wait sync.WaitGroup
	for range callers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			value, err := source.Token(context.Background())
			if err == nil && (value.AccessToken.Value() != "new-access" || value.RefreshToken.Value() != "new-refresh") {
				err = errors.New("wrong refreshed value")
			}
			errorsSeen <- err
		}()
	}
	close(start)
	wait.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatal(err)
		}
	}
	if refreshes.Load() != 1 {
		t.Fatalf("refresh requests = %d, want 1", refreshes.Load())
	}
	stored, err := store.Get(context.Background(), "openai")
	if err != nil || stored.OAuth.RefreshToken.Value() != "new-refresh" || stored.OAuth.AccountID != "old-account" {
		t.Fatalf("stored credential = %v, %v", stored, err)
	}
}

func TestExtractAccountID(t *testing.T) {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"https://api.openai.com/auth":{"chatgpt_account_id":"account-id"}}`))
	if got := ExtractAccountID("header." + payload + ".signature"); got != "account-id" {
		t.Fatalf("ExtractAccountID() = %q", got)
	}
}
