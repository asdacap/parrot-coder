package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	v1 "github.com/amirulashraf/parrot-coder/internal/api/v1"
)

func TestAPIErrorPreservesProblem(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", v1.MediaTypeProblem)
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"type":"https://parrot.invalid/problems/session-not-found","title":"Session not found","status":404,"detail":"missing","code":"session_not_found","request_id":"req_test"}`)
	}))
	defer server.Close()
	client, err := New(server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Session(context.Background(), "missing")
	var apiError *APIError
	if !errors.As(err, &apiError) {
		t.Fatalf("error = %T %v", err, err)
	}
	if apiError.Problem.Code != "session_not_found" || apiError.Problem.RequestID != "req_test" {
		t.Fatalf("problem = %#v", apiError.Problem)
	}
}

func TestResponseLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", v1.MediaTypeJSON)
		_, _ = io.WriteString(w, strings.Repeat(" ", 100)+`{"status":"ok"}`)
	}))
	defer server.Close()
	client, _ := New(server.URL, nil)
	client.MaxResponseBytes = 16
	if _, err := client.Health(context.Background()); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("error = %v", err)
	}
}

func TestMessagesAggregatesEveryPage(t *testing.T) {
	// The active assistant sits at the tail of a long session; a truncated first
	// page would hide it and desync the terminal's stream handoff. Serve two
	// pages and require the client to follow the cursor and return both.
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.RawQuery)
		w.Header().Set("Content-Type", v1.MediaTypeJSON)
		if r.URL.Query().Get("cursor") == "" {
			_, _ = io.WriteString(w, `{"items":[{"id":"first","role":"assistant","status":"complete"}],"next_cursor":"c2"}`)
			return
		}
		_, _ = io.WriteString(w, `{"items":[{"id":"active","role":"assistant","status":"active"}]}`)
	}))
	defer server.Close()
	client, _ := New(server.URL, nil)
	list, err := client.Messages(context.Background(), "session")
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 2 || list.Items[0].ID != "first" || list.Items[1].ID != "active" || list.NextCursor != "" {
		t.Fatalf("aggregated = %#v", list)
	}
	if len(paths) != 2 || strings.Contains(paths[0], "cursor") || !strings.Contains(paths[1], "cursor=c2") {
		t.Fatalf("requested pages = %#v", paths)
	}
}

func TestSSEDecoderMultilineHeartbeatAndEOF(t *testing.T) {
	input := ": heartbeat\n\nid: evt_1\nevent: message.part.delta\ndata: {\"id\":\"evt_1\",\ndata: \"type\":\"message.part.delta\",\"data\":{}}"
	decoder := NewSSEDecoder(strings.NewReader(input), 1024)
	item, err := decoder.Next()
	if err != nil {
		t.Fatal(err)
	}
	if item.ID != "evt_1" || item.Type != v1.EventMessagePartDelta {
		t.Fatalf("event = %#v", item)
	}
	if _, err := decoder.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("EOF error = %v", err)
	}
}

func TestEventsCancellationAndNoReconnect(t *testing.T) {
	requests := make(chan struct{}, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- struct{}{}
		w.Header().Set("Content-Type", v1.MediaTypeSSE)
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	client, _ := New(server.URL, nil)
	ctx, cancel := context.WithCancel(context.Background())
	stream, err := client.Events(ctx, "ses_test", nil)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	_ = stream.Close()
	select {
	case <-requests:
	case <-time.After(time.Second):
		t.Fatal("request did not start")
	}
	time.Sleep(20 * time.Millisecond)
	select {
	case <-requests:
		t.Fatal("client automatically reconnected")
	default:
	}
}

func TestSSEDecoderBound(t *testing.T) {
	decoder := NewSSEDecoder(strings.NewReader("data: "+strings.Repeat("x", 100)+"\n\n"), 32)
	if _, err := decoder.Next(); err == nil {
		t.Fatal("oversized event was accepted")
	}
}

func ExampleAPIError() {
	err := &APIError{Problem: v1.Problem{Title: "Conflict", Code: "conflict"}}
	fmt.Println(err)
	// Output: parrot API: Conflict (conflict)
}
