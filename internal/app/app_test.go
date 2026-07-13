package app

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	v1 "github.com/amirulashraf/parrot-coder/internal/api/v1"
	"github.com/amirulashraf/parrot-coder/internal/appdirs"
)

func TestCompositionEndToEndInProcess(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Errorf("provider path = %q", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-secret" {
			t.Errorf("provider authorization = %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello from provider\"}\n\n"+
			"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
	}))
	defer provider.Close()

	root := t.TempDir()
	configHome := filepath.Join(root, "config")
	dataHome := filepath.Join(root, "data")
	stateHome := filepath.Join(root, "state")
	cacheHome := filepath.Join(root, "cache")
	if err := os.MkdirAll(filepath.Join(configHome, "parrot"), 0o700); err != nil {
		t.Fatal(err)
	}
	configuration := fmt.Sprintf(`{
  "model": "local/test-model",
  "providers": {
    "local": {
      "type": "compatible",
      "protocol": "responses",
      "base_url": %q,
      "api_key_env": "PARROT_TEST_KEY",
      "allow_insecure_localhost": true,
      "models": {"test-model": {"name": "Test Model", "tools": true}}
    }
  }
}`, provider.URL+"/v1")
	if err := os.WriteFile(filepath.Join(configHome, "parrot", "parrot.jsonc"), []byte(configuration), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PARROT_TEST_KEY", "test-secret")

	runtime, err := Open(context.Background(), Options{
		CWD:     root,
		Paths:   appdirs.Overrides{Home: root, ConfigHome: configHome, DataHome: dataHome, StateHome: stateHome, CacheHome: cacheHome},
		Version: "test", NonInteractive: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	created, err := runtime.Client.CreateSession(context.Background(), v1.CreateSessionRequest{ProjectID: runtime.Project.ID, Title: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if created.Provider != "local" || created.Model != "test-model" || created.Agent != "build" {
		t.Fatalf("created selection = %#v", created)
	}
	after := int64(^uint64(0) >> 1)
	events, err := runtime.Client.Events(context.Background(), created.ID, &after)
	if err != nil {
		t.Fatal(err)
	}
	defer events.Close()
	connected, err := events.Next()
	if err != nil || connected.Type != v1.EventServerConnected {
		t.Fatalf("connected = %#v, %v", connected, err)
	}
	if _, err := runtime.Client.Prompt(context.Background(), created.ID, v1.PromptRequest{MessageID: "msg_test", Content: "hello", Delivery: "steer"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(5 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for idle")
		default:
		}
		item, err := events.Next()
		if err != nil {
			t.Fatal(err)
		}
		if item.Type == v1.EventSessionStatus {
			payload, err := v1.DecodeEventData(item)
			if err != nil {
				t.Fatal(err)
			}
			if payload.(*v1.SessionStatus).Kind == "idle" {
				break
			}
		}
	}
	messages, err := runtime.Client.Messages(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := messages.Items[len(messages.Items)-1]; got.Role != "assistant" || got.Content != "hello from provider" || got.Status != "complete" {
		t.Fatalf("last message = %#v", got)
	}
}
