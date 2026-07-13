package mcp

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
	"testing"
	"time"
)

func TestHTTPHandshakeDiscoveryCallAndLists(t *testing.T) {
	t.Parallel()
	server := newHTTPMCPServer(t)
	defer server.Close()
	manager := newHTTPManager(t, server.URL, Config{MaxOutputBytes: 4096})
	defer manager.Close()

	definitions, err := manager.DiscoverTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(definitions) != 2 {
		t.Fatalf("definitions = %#v", definitions)
	}
	var echoName string
	for _, definition := range definitions {
		if definition.Tool == "echo" {
			echoName = definition.Name
			if string(definition.InputSchema) != `{"type":"object","properties":{"value":{"type":"string"}}}` {
				t.Fatalf("schema changed: %s", definition.InputSchema)
			}
		}
	}
	result, err := manager.CallTool(context.Background(), echoName, json.RawMessage(`{"value":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Content) != 1 || result.Content[0].Text != "hello" || string(result.StructuredContent) != `{"echo":"hello"}` {
		t.Fatalf("result = %#v", result)
	}
	select {
	case notification := <-manager.Notifications():
		if notification.Method != "notifications/progress" {
			t.Fatalf("notification = %#v", notification)
		}
	case <-time.After(time.Second):
		t.Fatal("SSE notification was not forwarded")
	}
	prompts, err := manager.ListPrompts(context.Background(), "http")
	if err != nil || len(prompts) != 1 || prompts[0].Name != "review" {
		t.Fatalf("prompts = %#v, %v", prompts, err)
	}
	resources, err := manager.ListResources(context.Background(), "http")
	if err != nil || len(resources) != 1 || resources[0].URI != "file:///readme" {
		t.Fatalf("resources = %#v, %v", resources, err)
	}
	status, err := manager.Status("http")
	if err != nil || !status.Healthy || status.ProtocolVersion != ProtocolVersion || status.ServerName != "fixture" {
		t.Fatalf("status = %#v, %v", status, err)
	}
}

func TestHTTPSSEApplicationErrorTimeoutAndLimits(t *testing.T) {
	t.Parallel()
	server := newHTTPMCPServer(t)
	defer server.Close()
	manager := newHTTPManager(t, server.URL, Config{CallTimeout: 50 * time.Millisecond, MaxOutputBytes: 32, MaxMessageBytes: 2048})
	defer manager.Close()
	definitions, err := manager.DiscoverTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	name := definitions[0].Name
	for _, definition := range definitions {
		if definition.Tool == "echo" {
			name = definition.Name
		}
	}

	result, err := manager.CallTool(context.Background(), name, json.RawMessage(`{"value":"application-error"}`))
	var applicationErr *ApplicationError
	if !errors.As(err, &applicationErr) || !result.IsError {
		t.Fatalf("application result = %#v, %v", result, err)
	}
	result, err = manager.CallTool(context.Background(), name, json.RawMessage(`{"value":"abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyz"}`))
	if err != nil || !result.Truncated {
		t.Fatalf("bounded result = %#v, %v", result, err)
	}
	_, err = manager.CallTool(context.Background(), name, json.RawMessage(`{"value":"timeout"}`))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout error = %v", err)
	}

	large := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "999999")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer large.Close()
	limited := newHTTPManager(t, large.URL, Config{MaxMessageBytes: 1024, MaxOutputBytes: 128})
	defer limited.Close()
	if err := limited.Start(context.Background(), "http"); err == nil || !strings.Contains(err.Error(), "exceeds byte limit") {
		t.Fatalf("oversized response error = %v", err)
	}
}

func TestHTTPRedirectAndConfigSecurity(t *testing.T) {
	t.Parallel()
	destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer destination.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	manager := newHTTPManager(t, redirect.URL, Config{})
	defer manager.Close()
	if err := manager.Start(context.Background(), "http"); err == nil || !strings.Contains(err.Error(), "cross-origin") {
		t.Fatalf("redirect error = %v", err)
	}

	invalid := []Config{
		{Name: "x", Transport: TransportHTTP, Enabled: true, URL: "http://example.com", AllowInsecureLocalhost: true},
		{Name: "x", Transport: TransportHTTP, Enabled: true, URL: "https://user:pass@example.com"},
		{Name: "x", Transport: TransportHTTP, Enabled: true, URL: "https://example.com", Headers: map[string]string{"Host": "evil"}},
		{Name: "x", Transport: TransportHTTP, Enabled: true, URL: "https://example.com", Headers: map[string]string{"X-Test": "ok\r\nevil"}},
	}
	for _, config := range invalid {
		if _, err := NewManager([]Config{config}); err == nil {
			t.Fatalf("invalid config accepted: %#v", config)
		}
	}
}

func newHTTPManager(t *testing.T, url string, override Config) *Manager {
	t.Helper()
	config := Config{
		Name:                   "http",
		Transport:              TransportHTTP,
		Enabled:                true,
		URL:                    url,
		AllowInsecureLocalhost: true,
		StartupTimeout:         time.Second,
		CallTimeout:            time.Second,
		MaxMessageBytes:        1 << 20,
		MaxOutputBytes:         64 << 10,
	}
	if override.CallTimeout != 0 {
		config.CallTimeout = override.CallTimeout
	}
	if override.MaxMessageBytes != 0 {
		config.MaxMessageBytes = override.MaxMessageBytes
	}
	if override.MaxOutputBytes != 0 {
		config.MaxOutputBytes = override.MaxOutputBytes
	}
	manager, err := NewManager([]Config{config})
	if err != nil {
		t.Fatal(err)
	}
	return manager
}

func newHTTPMCPServer(t *testing.T) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	initialized := false
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			t.Error(err)
			return
		}
		var request wireMessage
		if err := json.Unmarshal(data, &request); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		if request.Method == "notifications/initialized" {
			if r.Header.Get("Mcp-Protocol-Version") != ProtocolVersion {
				t.Errorf("protocol header = %q", r.Header.Get("Mcp-Protocol-Version"))
			}
			mu.Lock()
			initialized = true
			mu.Unlock()
			w.WriteHeader(http.StatusAccepted)
			return
		}
		mu.Lock()
		ready := initialized
		mu.Unlock()
		if request.Method != "initialize" && !ready {
			t.Errorf("%s arrived before initialized notification", request.Method)
		}
		if request.Method != "initialize" && r.Header.Get("Mcp-Protocol-Version") != ProtocolVersion {
			t.Errorf("protocol header = %q", r.Header.Get("Mcp-Protocol-Version"))
		}
		var result any
		switch request.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "session-1")
			result = map[string]any{"protocolVersion": ProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "fixture", "version": "1.0"}}
		case "tools/list":
			var params struct {
				Cursor string `json:"cursor"`
			}
			_ = json.Unmarshal(request.Params, &params)
			if params.Cursor == "" {
				result = map[string]any{
					"tools": []any{map[string]any{
						"name":        "echo",
						"description": "Echo",
						"inputSchema": json.RawMessage(`{"type":"object","properties":{"value":{"type":"string"}}}`),
					}},
					"nextCursor": "page-2",
				}
			} else {
				result = map[string]any{"tools": []any{map[string]any{"name": "second", "inputSchema": map[string]any{"type": "object"}}}}
			}
		case "prompts/list":
			result = map[string]any{"prompts": []any{map[string]any{"name": "review"}}}
		case "resources/list":
			result = map[string]any{"resources": []any{map[string]any{"uri": "file:///readme", "name": "readme"}}}
		case "tools/call":
			var params struct {
				Arguments struct {
					Value string `json:"value"`
				} `json:"arguments"`
			}
			_ = json.Unmarshal(request.Params, &params)
			if params.Arguments.Value == "timeout" {
				time.Sleep(200 * time.Millisecond)
			}
			result = map[string]any{
				"content":           []any{map[string]any{"type": "text", "text": params.Arguments.Value}},
				"structuredContent": map[string]any{"echo": params.Arguments.Value},
				"isError":           params.Arguments.Value == "application-error",
			}
		default:
			writeHTTPRPC(t, w, wireMessage{JSONRPC: "2.0", ID: request.ID, Error: &RPCError{Code: -32601, Message: "not found"}})
			return
		}
		response := wireMessage{JSONRPC: "2.0", ID: request.ID}
		response.Result, _ = json.Marshal(result)
		if request.Method == "tools/call" {
			w.Header().Set("Content-Type", "text/event-stream")
			notification, _ := json.Marshal(wireMessage{JSONRPC: "2.0", Method: "notifications/progress", Params: json.RawMessage(`{"progress":1}`)})
			raw, _ := json.Marshal(response)
			_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\nevent: message\ndata: %s\n\n", notification, raw)
			return
		}
		writeHTTPRPC(t, w, response)
	}))
}

func writeHTTPRPC(t *testing.T, w http.ResponseWriter, response wireMessage) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(response); err != nil {
		t.Errorf("encode response: %v", err)
	}
}
