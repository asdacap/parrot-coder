package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	OpenAIClientID    = "app_EMoamEEZ73f0CkXaXp7hrann"
	OpenAIIssuer      = "https://auth.openai.com"
	OpenAICallbackURL = "http://localhost:1455/auth/callback"
)

type BrowserOpener func(string) error

type OpenAI struct {
	HTTPClient          *http.Client
	OpenBrowser         BrowserOpener
	Issuer              string
	Now                 func() time.Time
	Sleep               func(context.Context, time.Duration) error
	LoginTimeout        time.Duration
	PollingSafetyMargin time.Duration
}

type tokenResponse struct {
	IDToken      Secret `json:"id_token"`
	AccessToken  Secret `json:"access_token"`
	RefreshToken Secret `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
}

func (c *OpenAI) client() *http.Client {
	if c != nil && c.HTTPClient != nil {
		return c.HTTPClient
	}
	return http.DefaultClient
}

func (c *OpenAI) do(request *http.Request) (*http.Response, error) {
	client := *c.client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return client.Do(request)
}

func (c *OpenAI) issuer() string {
	if c != nil && c.Issuer != "" {
		return strings.TrimRight(c.Issuer, "/")
	}
	return OpenAIIssuer
}

func (c *OpenAI) now() time.Time {
	if c != nil && c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *OpenAI) timeout() time.Duration {
	if c != nil && c.LoginTimeout > 0 {
		return c.LoginTimeout
	}
	return 5 * time.Minute
}

func (c *OpenAI) sleep(ctx context.Context, duration time.Duration) error {
	if c != nil && c.Sleep != nil {
		return c.Sleep(ctx, duration)
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func randomURLString(size int) (string, error) {
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		return "", errors.New("auth: generate random value")
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func (c *OpenAI) AuthorizationURL(redirect, challenge, state string) (string, error) {
	issuer, err := url.Parse(c.issuer())
	if err != nil {
		return "", errors.New("auth: invalid OpenAI issuer")
	}
	issuer.Path = strings.TrimRight(issuer.Path, "/") + "/oauth/authorize"
	query := issuer.Query()
	query.Set("response_type", "code")
	query.Set("client_id", OpenAIClientID)
	query.Set("redirect_uri", redirect)
	query.Set("scope", "openid profile email offline_access")
	query.Set("code_challenge", challenge)
	query.Set("code_challenge_method", "S256")
	query.Set("id_token_add_organizations", "true")
	query.Set("codex_cli_simplified_flow", "true")
	query.Set("state", state)
	query.Set("originator", "opencode")
	issuer.RawQuery = query.Encode()
	return issuer.String(), nil
}

// BrowserLogin performs the PKCE browser flow and always closes its callback server.
func (c *OpenAI) BrowserLogin(ctx context.Context) (OAuthCredential, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:1455")
	if err != nil {
		return OAuthCredential{}, errors.New("auth: start OAuth callback listener")
	}
	verifier, err := randomURLString(32)
	if err != nil {
		listener.Close()
		return OAuthCredential{}, err
	}
	state, err := randomURLString(32)
	if err != nil {
		listener.Close()
		return OAuthCredential{}, err
	}
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	authorizationURL, err := c.AuthorizationURL(OpenAICallbackURL, challenge, state)
	if err != nil {
		listener.Close()
		return OAuthCredential{}, err
	}

	result := make(chan callbackResult, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/callback", func(response http.ResponseWriter, request *http.Request) {
		query := request.URL.Query()
		var callback callbackResult
		switch {
		case query.Get("error") != "":
			callback.err = errors.New("auth: authorization rejected")
		case query.Get("state") != state:
			callback.err = errors.New("auth: invalid OAuth state")
		case query.Get("code") == "":
			callback.err = errors.New("auth: missing authorization code")
		default:
			callback.code = Secret(query.Get("code"))
		}
		select {
		case result <- callback:
		default:
		}
		response.Header().Set("Content-Type", "text/html; charset=utf-8")
		if callback.err != nil {
			response.WriteHeader(http.StatusBadRequest)
			io.WriteString(response, "Authorization failed. You may close this window.")
			return
		}
		io.WriteString(response, "Authorization complete. You may close this window.")
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	serveDone := make(chan struct{})
	go func() {
		server.Serve(listener)
		close(serveDone)
	}()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		server.Shutdown(shutdownCtx)
		<-serveDone
	}()

	if c == nil || c.OpenBrowser == nil {
		return OAuthCredential{}, errors.New("auth: browser opener is not configured")
	}
	loginCtx, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()
	opened := make(chan error, 1)
	go func() { opened <- c.OpenBrowser(authorizationURL) }()
	select {
	case <-loginCtx.Done():
		return OAuthCredential{}, loginCtx.Err()
	case err := <-opened:
		if err != nil {
			return OAuthCredential{}, errors.New("auth: open authorization URL")
		}
	}
	select {
	case <-loginCtx.Done():
		return OAuthCredential{}, loginCtx.Err()
	case callback := <-result:
		if callback.err != nil {
			return OAuthCredential{}, callback.err
		}
		return c.exchange(loginCtx, callback.code, OpenAICallbackURL, Secret(verifier))
	}
}

type callbackResult struct {
	code Secret
	err  error
}

func (c *OpenAI) exchange(ctx context.Context, code Secret, redirect string, verifier Secret) (OAuthCredential, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code.Value()},
		"redirect_uri":  {redirect},
		"client_id":     {OpenAIClientID},
		"code_verifier": {verifier.Value()},
	}
	tokens, err := c.tokenRequest(ctx, form)
	if err != nil {
		return OAuthCredential{}, err
	}
	return c.credential(tokens, ""), nil
}

func (c *OpenAI) Refresh(ctx context.Context, current OAuthCredential) (OAuthCredential, error) {
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {current.RefreshToken.Value()},
		"client_id":     {OpenAIClientID},
	}
	tokens, err := c.tokenRequest(ctx, form)
	if err != nil {
		return OAuthCredential{}, err
	}
	return c.credential(tokens, current.AccountID), nil
}

func (c *OpenAI) tokenRequest(ctx context.Context, form url.Values) (tokenResponse, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.issuer()+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return tokenResponse{}, errors.New("auth: create token request")
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := c.do(request)
	if err != nil {
		if ctx.Err() != nil {
			return tokenResponse{}, ctx.Err()
		}
		return tokenResponse{}, errors.New("auth: token request failed")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return tokenResponse{}, fmt.Errorf("auth: token request returned HTTP %d", response.StatusCode)
	}
	var tokens tokenResponse
	if err := decodeLimitedJSON(response.Body, &tokens); err != nil {
		return tokenResponse{}, errors.New("auth: invalid token response")
	}
	if tokens.AccessToken == "" || tokens.RefreshToken == "" {
		return tokenResponse{}, errors.New("auth: incomplete token response")
	}
	return tokens, nil
}

func (c *OpenAI) credential(tokens tokenResponse, fallbackAccount string) OAuthCredential {
	expires := tokens.ExpiresIn
	if expires <= 0 {
		expires = 3600
	}
	account := ExtractAccountID(tokens.IDToken.Value())
	if account == "" {
		account = ExtractAccountID(tokens.AccessToken.Value())
	}
	if account == "" {
		account = fallbackAccount
	}
	return OAuthCredential{AccessToken: tokens.AccessToken, RefreshToken: tokens.RefreshToken, ExpiresAt: c.now().Add(time.Duration(expires) * time.Second), AccountID: account}
}

// ExtractAccountID reads unverified JWT claims for request routing only. It
// must never be used to authenticate or authorize a user.
func ExtractAccountID(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		AccountID string `json:"chatgpt_account_id"`
		Auth      struct {
			AccountID string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
		Organizations []struct {
			ID string `json:"id"`
		} `json:"organizations"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	if claims.AccountID != "" {
		return claims.AccountID
	}
	if claims.Auth.AccountID != "" {
		return claims.Auth.AccountID
	}
	if len(claims.Organizations) > 0 {
		return claims.Organizations[0].ID
	}
	return ""
}

func decodeLimitedJSON(reader io.Reader, target any) error {
	const limit = 1 << 20
	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return err
	}
	if len(data) > limit {
		return errors.New("JSON response exceeds byte limit")
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}
