package provider

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/amirulashraf/parrot-coder/internal/protocol"
)

func newKimi(t *testing.T, server *httptest.Server) *Kimi {
	t.Helper()
	value, err := NewKimi(OpenAICompatibleOptions{
		ID: "kimi", BaseURL: server.URL, Protocol: ProtocolChatCompletions,
		APIKey: "secret-key", AllowInsecureLocalhost: true, HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func TestKimiStreamsChatCompletionsWithBearerKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/chat/completions" {
			t.Errorf("path = %q", request.URL.Path)
		}
		if got := request.Header.Get("Authorization"); got != "Bearer secret-key" {
			t.Errorf("authorization = %q", got)
		}
		response.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(response, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	stream, err := newKimi(t, server).Stream(context.Background(), protocol.Request{Model: "kimi-k2-thinking"})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	event, err := stream.Next(context.Background())
	if err != nil || event.Text != "hi" {
		t.Fatalf("event = %#v, err = %v", event, err)
	}
}

func TestKimiUsage(t *testing.T) {
	for _, testCase := range []struct {
		name        string
		status      int
		body        string
		wantErr     string
		wantBalance string
		wantCredits bool
	}{
		{
			name: "available balance", status: http.StatusOK,
			body:        `{"code":0,"status":true,"data":{"available_balance":49.53,"cash_balance":3.0}}`,
			wantBalance: "49.53", wantCredits: true,
		},
		{
			name: "exhausted balance", status: http.StatusOK,
			body:        `{"data":{"available_balance":0}}`,
			wantBalance: "0", wantCredits: false,
		},
		{
			name: "missing balance", status: http.StatusOK,
			body: `{"code":0,"status":true}`, wantErr: "no balance",
		},
		{
			name: "http error redacts the key", status: http.StatusUnauthorized,
			body: `{"error":{"message":"invalid key secret-key"}}`, wantErr: "[REDACTED]",
		},
		{
			name: "oversized response", status: http.StatusOK,
			body: `{"data":{"note":"` + strings.Repeat("x", maxErrorBytes+1) + `"}}`, wantErr: "byte limit",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				if request.URL.Path != "/users/me/balance" {
					t.Errorf("path = %q", request.URL.Path)
				}
				response.WriteHeader(testCase.status)
				_, _ = io.WriteString(response, testCase.body)
			}))
			defer server.Close()
			usage, err := newKimi(t, server).Usage(context.Background())
			if testCase.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), testCase.wantErr) {
					t.Fatalf("err = %v, want it to contain %q", err, testCase.wantErr)
				}
				if strings.Contains(err.Error(), "secret-key") {
					t.Fatalf("error leaked the API key: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if usage.Credits == nil {
				t.Fatal("usage has no credits")
			}
			if usage.Credits.Balance != testCase.wantBalance || usage.Credits.HasCredits != testCase.wantCredits {
				t.Fatalf("credits = %#v", usage.Credits)
			}
			if usage.PrimaryWindow != nil || usage.SecondaryWindow != nil {
				t.Fatalf("Moonshot reports no rate-limit windows, got %#v and %#v", usage.PrimaryWindow, usage.SecondaryWindow)
			}
		})
	}
}
