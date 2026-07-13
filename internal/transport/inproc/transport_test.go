package inproc

import (
	"context"
	"io"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRoundTripStreamsAfterFlush(t *testing.T) {
	exited := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(exited)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	client := &http.Client{Transport: New(handler)}
	response, err := client.Get("http://inproc/events")
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 200 || response.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("response = %#v", response)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("handler did not exit after abandoned body")
	}
}

func TestRoundTripBodyAndPanicRecovery(t *testing.T) {
	tests := []struct {
		name    string
		handler http.Handler
		status  int
		body    string
	}{
		{"body", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(201); _, _ = io.WriteString(w, "created") }), 201, "created"},
		{"panic", http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("secret") }), 500, "internal server error\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := &http.Client{Transport: New(test.handler)}
			response, err := client.Get("http://inproc/test")
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			data, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != test.status || string(data) != test.body {
				t.Fatalf("status/body = %d %q", response.StatusCode, data)
			}
			if strings.Contains(string(data), "secret") {
				t.Fatal("panic value leaked")
			}
		})
	}
}

func TestRequestCancellationBeforeHeaders(t *testing.T) {
	handlerExited := make(chan struct{})
	handler := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		defer close(handlerExited)
		<-r.Context().Done()
	})
	client := &http.Client{Transport: New(handler)}
	ctx, cancel := context.WithCancel(context.Background())
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://inproc/wait", nil)
	done := make(chan error, 1)
	go func() { _, err := client.Do(request); done <- err }()
	cancel()
	if err := <-done; !errorsIsCanceled(err) {
		t.Fatalf("error = %v", err)
	}
	select {
	case <-handlerExited:
	case <-time.After(time.Second):
		t.Fatal("handler did not observe cancellation")
	}
}

func TestAbandonedStreamStress(t *testing.T) {
	baseline := runtime.NumGoroutine()
	var active atomic.Int64
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		active.Add(1)
		defer active.Add(-1)
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		for {
			select {
			case <-r.Context().Done():
				return
			default:
				if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
					return
				}
				w.(http.Flusher).Flush()
			}
		}
	})
	client := &http.Client{Transport: New(handler)}
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			response, err := client.Get("http://inproc/events")
			if err == nil {
				_ = response.Body.Close()
			}
		}()
	}
	wg.Wait()
	deadline := time.Now().Add(2 * time.Second)
	for active.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if active.Load() != 0 {
		t.Fatalf("%d handlers remain", active.Load())
	}
	runtime.GC()
	if current := runtime.NumGoroutine(); current > baseline+10 {
		t.Fatalf("goroutines grew from %d to %d", baseline, current)
	}
}

func errorsIsCanceled(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "context canceled") || strings.Contains(err.Error(), "request canceled"))
}
