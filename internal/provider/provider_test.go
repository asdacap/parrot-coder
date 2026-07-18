package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amirulashraf/parrot-coder/internal/auth"
	"github.com/amirulashraf/parrot-coder/internal/protocol"
)

func TestOpenAICompatibleResponsesWireAndPath(t *testing.T) {
	var path string
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		path = request.URL.Path
		if request.Header.Get("Authorization") != "Bearer secret" || request.Header.Get("X-Tenant") != "acme" {
			t.Errorf("headers = %#v", request.Header)
		}
		body, _ := io.ReadAll(request.Body)
		if !strings.Contains(string(body), `"model":"model-a"`) || !strings.Contains(string(body), `"stream":true`) {
			t.Errorf("body = %s", body)
		}
		response.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(response, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n"+
			"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
	}))
	defer server.Close()

	value, err := NewOpenAICompatible(OpenAICompatibleOptions{
		ID: "local", BaseURL: server.URL + "/v1/", Protocol: ProtocolResponses,
		APIKey: auth.Secret("secret"), Headers: map[string]string{"X-Tenant": "acme"},
		AllowInsecureLocalhost: true, HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := value.Stream(context.Background(), protocol.Request{Model: "model-a"})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	event, err := stream.Next(context.Background())
	if err != nil || event.Type != protocol.EventTextDelta || event.Text != "hello" {
		t.Fatalf("event = %#v, err = %v", event, err)
	}
	if path != "/v1/responses" {
		t.Fatalf("path = %q", path)
	}
}

func TestOpenAICompatibleChatCompletionsRootPathAndDecode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/chat/completions" {
			t.Errorf("path = %q", request.URL.Path)
		}
		response.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(response, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\n"+
			"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	value, err := NewOpenAICompatible(OpenAICompatibleOptions{
		ID: "local", BaseURL: server.URL, Protocol: ProtocolChatCompletions,
		APIKey: "key", AllowInsecureLocalhost: true, HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := value.Stream(context.Background(), protocol.Request{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	event, err := stream.Next(context.Background())
	if err != nil || event.Text != "hi" {
		t.Fatalf("event = %#v, err = %v", event, err)
	}
}

func TestOpenAICompatibleURLAndHeaderPolicy(t *testing.T) {
	base := OpenAICompatibleOptions{ID: "p", Protocol: ProtocolResponses, APIKey: "key"}
	for _, raw := range []string{"http://example.com/v1", "ftp://localhost/v1", "https://user@example.com/v1", "https://example.com/v1?q=1"} {
		options := base
		options.BaseURL = raw
		options.AllowInsecureLocalhost = true
		if _, err := NewOpenAICompatible(options); err == nil {
			t.Errorf("accepted base URL %q", raw)
		}
	}
	options := base
	options.BaseURL = "http://localhost:1234/v1"
	if _, err := NewOpenAICompatible(options); err == nil {
		t.Fatal("accepted loopback HTTP without opt-in")
	}
	options.AllowInsecureLocalhost = true
	if _, err := NewOpenAICompatible(options); err != nil {
		t.Fatalf("rejected opted-in loopback: %v", err)
	}
	for _, name := range []string{"Authorization", "cookie", "HOST", "Proxy-Authorization"} {
		options.Headers = map[string]string{name: "unsafe"}
		if _, err := NewOpenAICompatible(options); err == nil {
			t.Errorf("accepted header %q", name)
		}
	}
}

func TestOpenAICompatibleHeaderTimeout(t *testing.T) {
	startedRequest := make(chan struct{})
	releaseRequest := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		close(startedRequest)
		select {
		case <-request.Context().Done():
		case <-releaseRequest:
		}
	}))
	defer server.Close()
	defer close(releaseRequest)
	value, err := NewOpenAICompatible(OpenAICompatibleOptions{
		ID: "local", BaseURL: server.URL, Protocol: ProtocolResponses, APIKey: "key",
		AllowInsecureLocalhost: true, HTTPClient: server.Client(), HeaderTimeout: 25 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	done := make(chan error, 1)
	go func() {
		_, streamErr := value.Stream(context.Background(), protocol.Request{Model: "m"})
		done <- streamErr
	}()
	<-startedRequest
	requestStarted := time.Now()
	err = <-done
	var timeoutErr *HeaderTimeoutError
	if !errors.As(err, &timeoutErr) || timeoutErr.Timeout != 25*time.Millisecond {
		t.Fatalf("error = %#v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("header timeout took %s", elapsed)
	}
	if elapsed := time.Since(requestStarted); elapsed < 15*time.Millisecond {
		t.Fatalf("header timeout fired too early after %s", elapsed)
	}
}

type retryProvider struct {
	calls atomic.Int32
	fn    func(int) (Stream, error)
}

func (*retryProvider) ID() string      { return "retry" }
func (*retryProvider) Models() []Model { return nil }
func (p *retryProvider) Stream(context.Context, protocol.Request) (Stream, error) {
	call := int(p.calls.Add(1))
	return p.fn(call)
}

type testStream struct{}

func (*testStream) Next(context.Context) (protocol.Event, error) { return protocol.Event{}, io.EOF }
func (*testStream) Close() error                                 { return nil }

func TestStreamWithHeaderRetryRetriesTimeoutThenSucceeds(t *testing.T) {
	want := &testStream{}
	client := &retryProvider{fn: func(call int) (Stream, error) {
		if call == 1 {
			return nil, &HeaderTimeoutError{Timeout: time.Second}
		}
		return want, nil
	}}
	got, err := streamWithHeaderRetry(context.Background(), client, protocol.Request{}, time.Millisecond, 2*time.Millisecond)
	if err != nil || got != want || client.calls.Load() != 2 {
		t.Fatalf("stream = %v, calls = %d, err = %v", got, client.calls.Load(), err)
	}
}

func TestStreamWithHeaderRetryStopsOnCancellation(t *testing.T) {
	called := make(chan struct{})
	client := &retryProvider{fn: func(int) (Stream, error) {
		select {
		case <-called:
		default:
			close(called)
		}
		return nil, &HeaderTimeoutError{Timeout: time.Second}
	}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := streamWithHeaderRetry(ctx, client, protocol.Request{}, time.Hour, time.Hour)
		done <- err
	}()
	<-called
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) || client.calls.Load() != 1 {
		t.Fatalf("calls = %d, err = %v", client.calls.Load(), err)
	}
}

func TestOpenAICompatibleHeaderTimeoutStopsAfterHeaders(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "text/event-stream")
		response.(http.Flusher).Flush()
		<-release
		_, _ = io.WriteString(response, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
	}))
	defer server.Close()
	value, err := NewOpenAICompatible(OpenAICompatibleOptions{
		ID: "local", BaseURL: server.URL, Protocol: ProtocolResponses, APIKey: "key",
		AllowInsecureLocalhost: true, HTTPClient: server.Client(), HeaderTimeout: 25 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := value.Stream(context.Background(), protocol.Request{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	unblock()
	defer stream.Close()
	if _, err := stream.Next(context.Background()); err != nil {
		t.Fatalf("stream after delayed body = %v", err)
	}
}

func TestOpenAICompatibleCallerCancellationWins(t *testing.T) {
	started := make(chan struct{})
	releaseRequest := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		close(started)
		select {
		case <-request.Context().Done():
		case <-releaseRequest:
		}
	}))
	defer server.Close()
	defer close(releaseRequest)
	value, err := NewOpenAICompatible(OpenAICompatibleOptions{
		ID: "local", BaseURL: server.URL, Protocol: ProtocolResponses, APIKey: "key",
		AllowInsecureLocalhost: true, HTTPClient: server.Client(), HeaderTimeout: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, streamErr := value.Stream(ctx, protocol.Request{Model: "m"})
		done <- streamErr
	}()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
}

func TestOpenAICompatibleRejectsNegativeHeaderTimeout(t *testing.T) {
	_, err := NewOpenAICompatible(OpenAICompatibleOptions{
		ID: "local", BaseURL: "http://localhost:1234/v1", Protocol: ProtocolResponses, APIKey: "key",
		AllowInsecureLocalhost: true, HeaderTimeout: -time.Millisecond,
	})
	if err == nil || !strings.Contains(err.Error(), "header timeout") {
		t.Fatalf("error = %v", err)
	}
}

func TestOpenAICompatibleBlocksCrossOriginRedirectAndToken(t *testing.T) {
	var attackerRequests atomic.Int32
	attacker := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		attackerRequests.Add(1)
	}))
	defer attacker.Close()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		http.Redirect(response, request, attacker.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	value, err := NewOpenAICompatible(OpenAICompatibleOptions{
		ID: "local", BaseURL: server.URL, Protocol: ProtocolResponses, APIKey: "super-secret",
		AllowInsecureLocalhost: true, HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = value.Stream(context.Background(), protocol.Request{Model: "m"})
	if err == nil || strings.Contains(err.Error(), "super-secret") {
		t.Fatalf("error = %v", err)
	}
	if attackerRequests.Load() != 0 {
		t.Fatal("redirect reached the other origin")
	}
}

func TestNonSuccessErrorIsStructuredBoundedAndRedacted(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(response, `{"error":{"type":"auth_error","code":"bad_key","message":"bad secret-token`+strings.Repeat("x", 2000)+`"}}`)
	}))
	defer server.Close()
	value, err := NewOpenAICompatible(OpenAICompatibleOptions{
		ID: "local", BaseURL: server.URL, Protocol: ProtocolResponses, APIKey: "secret-token",
		AllowInsecureLocalhost: true, HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = value.Stream(context.Background(), protocol.Request{Model: "m"})
	var providerError *HTTPError
	if !errors.As(err, &providerError) || providerError.StatusCode != http.StatusUnauthorized || providerError.Code != "bad_key" {
		t.Fatalf("error = %#v", err)
	}
	if strings.Contains(err.Error(), "secret-token") || len(providerError.Message) > 1024 {
		t.Fatalf("unsafe error = %v", err)
	}
}

func TestUsageLimitClassificationUsesOnlyStructuredPermanentCodes(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"http type", &HTTPError{StatusCode: 429, Type: "usage_limit_reached"}, true},
		{"http code", &HTTPError{Code: " insufficient_quota "}, true},
		{"stream code", &ResponseError{Code: "BILLING_HARD_LIMIT_REACHED"}, true},
		{"wrapped", fmt.Errorf("request: %w", &ResponseError{Code: "usage_limit_exceeded"}), true},
		{"joined second branch", errors.Join(errors.New("first"), &HTTPError{Code: "insufficient_quota"}), true},
		{"plain 429", &HTTPError{StatusCode: 429}, false},
		{"transient rate limit", &HTTPError{StatusCode: 429, Code: "rate_limit_exceeded"}, false},
		{"message text", &ResponseError{Message: "usage limit reached"}, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := IsUsageLimitError(test.err); got != test.want {
				t.Fatalf("IsUsageLimitError(%v) = %t, want %t", test.err, got, test.want)
			}
		})
	}
}

func TestNonSuccessErrorWithoutStructuredMessageFallsBackToBody(t *testing.T) {
	for _, testCase := range []struct{ name, body, want string }{
		{"plain text", "upstream rejected the request", "upstream rejected the request"},
		{"unknown json shape", `{"detail":"model \"m\" is unknown"}`, `{"detail":"model \"m\" is unknown"}`},
		{"multiline body", "invalid request\nfield: model", "invalid requestfield: model"},
		{"empty body", "", http.StatusText(http.StatusBadRequest)},
		{"only a secret", "secret-token", "[REDACTED]"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				response.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(response, testCase.body)
			}))
			defer server.Close()
			value, err := NewOpenAICompatible(OpenAICompatibleOptions{
				ID: "local", BaseURL: server.URL, Protocol: ProtocolResponses, APIKey: "secret-token",
				AllowInsecureLocalhost: true, HTTPClient: server.Client(),
			})
			if err != nil {
				t.Fatal(err)
			}
			_, err = value.Stream(context.Background(), protocol.Request{Model: "m"})
			var providerError *HTTPError
			if !errors.As(err, &providerError) || providerError.Message != testCase.want {
				t.Fatalf("error = %#v", err)
			}
		})
	}
}

func TestStreamEventsAndErrorsRedactCredential(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(response, "data: {\"type\":\"error\",\"error\":{\"type\":\"auth\",\"code\":\"bad\",\"message\":\"echo secret-token\"}}\n\n")
	}))
	defer server.Close()
	value, err := NewOpenAICompatible(OpenAICompatibleOptions{
		ID: "local", BaseURL: server.URL, Protocol: ProtocolResponses, APIKey: "secret-token",
		AllowInsecureLocalhost: true, HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := value.Stream(context.Background(), protocol.Request{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	event, err := stream.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if event.ProviderError == nil || strings.Contains(event.ProviderError.Message, "secret-token") || !strings.Contains(event.ProviderError.Message, "[REDACTED]") {
		t.Fatalf("provider error = %#v", event.ProviderError)
	}
}

func TestToolInputRedactionPreservesJSONTypes(t *testing.T) {
	raw, err := redactJSON(json.RawMessage(`{"number":123,"text":"value-123"}`), []string{"123"})
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(raw) {
		t.Fatalf("redacted input is invalid JSON: %s", raw)
	}
	var value struct {
		Number int    `json:"number"`
		Text   string `json:"text"`
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	if value.Number != 123 || value.Text != "value-[REDACTED]" {
		t.Fatalf("redacted value = %#v", value)
	}
}

func TestBoundedStreamReportsOverflowInsteadOfEOF(t *testing.T) {
	source := io.NopCloser(strings.NewReader("12345"))
	reader := &boundedReadCloser{reader: source, closer: source, remaining: 4}
	data, err := io.ReadAll(reader)
	if !errors.Is(err, ErrStreamTooLarge) || string(data) != "1234" {
		t.Fatalf("read = %q, %v", data, err)
	}
}

func TestStreamNextCancellationClosesResponse(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "text/event-stream")
		response.(http.Flusher).Flush()
		close(started)
		<-request.Context().Done()
	}))
	defer server.Close()
	value, err := NewOpenAICompatible(OpenAICompatibleOptions{
		ID: "local", BaseURL: server.URL, Protocol: ProtocolResponses, APIKey: "key",
		AllowInsecureLocalhost: true, HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := value.Stream(context.Background(), protocol.Request{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := stream.Next(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Next error = %v", err)
	}
}

type fixedTokenSource struct{ value auth.OAuthCredential }

func (source fixedTokenSource) Token(context.Context) (auth.OAuthCredential, error) {
	return source.value, nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestChatGPTFixedEndpointHeadersAndModels(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.String() != chatGPTEndpoint {
			t.Errorf("URL = %s", request.URL)
		}
		want := map[string]string{
			"Authorization": "Bearer oauth-token", "Chatgpt-Account-Id": "account-1",
			"Originator": "parrot", "User-Agent": "parrot",
		}
		for name, value := range want {
			if got := request.Header.Get(name); got != value {
				t.Errorf("header %s = %q, want %q", name, got, value)
			}
		}
		if request.Header.Get("session-id") == "" {
			t.Error("session-id header is empty")
		}
		return &http.Response{
			StatusCode: http.StatusOK, Header: make(http.Header),
			Body:    io.NopCloser(strings.NewReader("data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n")),
			Request: request,
		}, nil
	})}
	value, err := NewChatGPT(ChatGPTOptions{
		TokenSource: fixedTokenSource{auth.OAuthCredential{AccessToken: "oauth-token", AccountID: "account-1", ExpiresAt: time.Now().Add(time.Hour)}},
		HTTPClient:  client,
	})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := value.Stream(context.Background(), protocol.Request{Model: "gpt-5.4"})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	event, err := stream.Next(context.Background())
	if err != nil || event.Text != "ok" {
		t.Fatalf("event = %#v, err = %v", event, err)
	}
	models := value.Models()
	if len(models) == 0 || !models[0].Capabilities.Tools || !models[0].Capabilities.Reasoning {
		t.Fatalf("models = %#v", models)
	}
	models[0].Capabilities.Output[0] = "mutated"
	models[0].Capabilities.Variants[0].Name = "mutated"
	got := value.Models()[0].Capabilities
	if got.Output[0] == "mutated" || got.Variants[0].Name == "mutated" {
		t.Fatal("Models exposed mutable provider metadata")
	}
}

func TestChatGPTRefreshModels(t *testing.T) {
	var requests atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet || request.URL.Scheme+"://"+request.URL.Host+request.URL.Path != chatGPTModelsEndpoint || request.URL.Query().Get("client_version") != chatGPTModelsClientVersion {
			t.Errorf("request = %s %s", request.Method, request.URL)
		}
		wantHeaders := map[string]string{
			"Authorization": "Bearer oauth-token", "Chatgpt-Account-Id": "account-1",
			"Originator": "parrot", "User-Agent": "parrot", "Accept": "application/json",
		}
		for name, want := range wantHeaders {
			if got := request.Header.Get(name); got != want {
				t.Errorf("header %s = %q, want %q", name, got, want)
			}
		}
		body := `{"models":[
			{"slug":"gpt-valid","display_name":"Valid","visibility":"list","context_window":272000,"supported_reasoning_levels":[]},
			{"slug":"gpt-invalid","display_name":"Invalid","visibility":"list","context_window":null,"supported_reasoning_levels":[]}
		]}`
		if requests.Add(1) == 2 {
			body = `{"models":[
				{"slug":"gpt-remote","display_name":"GPT Remote","visibility":"list","context_window":272000,"supported_reasoning_levels":[{"effort":"low"},{"effort":"high"},{"effort":"high"}]},
				{"slug":"gpt-max-only","display_name":"","visibility":"list","context_window":null,"max_context_window":400000,"supported_reasoning_levels":[]},
				{"slug":"gpt-hidden","display_name":"Hidden","visibility":"hide","context_window":500000,"supported_reasoning_levels":[]},
				{"slug":"gpt-remote","display_name":"Duplicate","visibility":"list","context_window":1,"supported_reasoning_levels":[]}
			]}`
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
	})}
	value, err := NewChatGPT(ChatGPTOptions{
		TokenSource: fixedTokenSource{auth.OAuthCredential{AccessToken: "oauth-token", AccountID: "account-1", ExpiresAt: time.Now().Add(time.Hour)}},
		HTTPClient:  client,
	})
	if err != nil {
		t.Fatal(err)
	}
	fallback := value.Models()
	if err := value.RefreshModels(context.Background()); err == nil || len(value.Models()) != len(fallback) {
		t.Fatalf("failed refresh error = %v, models = %#v", err, value.Models())
	}
	if err := value.RefreshModels(context.Background()); err != nil {
		t.Fatal(err)
	}
	models := value.Models()
	if len(models) != 2 || models[0].ID != "gpt-remote" || models[0].ContextWindow != 272000 || models[0].MaxOutputTokens != 0 || models[0].Name != "GPT Remote" || !models[0].Capabilities.Tools || !models[0].Capabilities.Reasoning || len(models[0].Capabilities.Variants) != 2 {
		t.Fatalf("remote model = %#v", models)
	}
	if models[1].ID != "gpt-max-only" || models[1].Name != "gpt-max-only" || models[1].ContextWindow != 400000 || models[1].Capabilities.Reasoning {
		t.Fatalf("max-only model = %#v", models[1])
	}
	models[0].Capabilities.Variants[0].Name = "mutated"
	if value.Models()[0].Capabilities.Variants[0].Name == "mutated" {
		t.Fatal("Models exposed mutable refreshed metadata")
	}
}

func TestChatGPTSubscriptionUsage(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.String() != chatGPTUsageEndpoint || request.Method != http.MethodGet {
			t.Errorf("request = %s %s", request.Method, request.URL)
		}
		if request.Header.Get("Authorization") != "Bearer oauth-token" || request.Header.Get("ChatGPT-Account-Id") != "account-1" {
			t.Errorf("headers = %#v", request.Header)
		}
		body := `{"plan_type":"plus","rate_limit":{"primary_window":{"used_percent":27.5,"reset_at":1893456000,"limit_window_seconds":18000},"secondary_window":{"used_percent":4,"reset_at":1894060800,"limit_window_seconds":604800}},"credits":{"has_credits":true,"balance":12.50},"unknown":true}`
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
	})}
	value, err := NewChatGPT(ChatGPTOptions{TokenSource: fixedTokenSource{auth.OAuthCredential{AccessToken: "oauth-token", AccountID: "account-1", ExpiresAt: time.Now().Add(time.Hour)}}, HTTPClient: client})
	if err != nil {
		t.Fatal(err)
	}
	usage, err := value.Usage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if usage.PlanType != "plus" || usage.PrimaryWindow == nil || usage.PrimaryWindow.UsedPercent != 27.5 || usage.SecondaryWindow == nil || usage.Credits == nil || usage.Credits.Balance != "12.50" {
		t.Fatalf("usage = %#v", usage)
	}
}
