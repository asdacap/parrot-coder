package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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
	if value.Models()[0].Capabilities.Output[0] == "mutated" {
		t.Fatal("Models exposed mutable provider metadata")
	}
}
