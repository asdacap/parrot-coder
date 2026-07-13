// Package inproc runs an HTTP handler without opening a network socket while
// preserving streaming response semantics.
package inproc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
)

type Transport struct {
	Handler http.Handler
}

func New(handler http.Handler) *Transport { return &Transport{Handler: handler} }

func (t *Transport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request == nil || request.URL == nil {
		return nil, errors.New("inproc: request and URL are required")
	}
	if t == nil || t.Handler == nil {
		return nil, errors.New("inproc: handler is required")
	}
	if request.Body == nil {
		request.Body = http.NoBody
	}
	ctx, cancel := context.WithCancel(request.Context())
	request = request.Clone(ctx)
	reader, writer := io.Pipe()
	responses := make(chan *http.Response, 1)
	done := make(chan struct{})
	rw := &responseWriter{header: make(http.Header), writer: writer, request: request, responses: responses}

	go func() {
		defer close(done)
		defer request.Body.Close()
		defer func() {
			if recovered := recover(); recovered != nil {
				wasCommitted := rw.committed()
				if !wasCommitted {
					rw.Header().Set("Content-Type", "text/plain; charset=utf-8")
					rw.WriteHeader(http.StatusInternalServerError)
					_, _ = io.WriteString(rw, "internal server error\n")
					_ = writer.Close()
					return
				}
				_ = writer.CloseWithError(fmt.Errorf("inproc: handler panic"))
				return
			}
			if !rw.committed() {
				rw.WriteHeader(http.StatusOK)
			}
			_ = writer.Close()
		}()
		t.Handler.ServeHTTP(rw, request)
	}()

	// Cancellation must also unblock a handler currently writing to the pipe.
	go func() {
		select {
		case <-ctx.Done():
			_ = writer.CloseWithError(ctx.Err())
		case <-done:
		}
	}()

	select {
	case response := <-responses:
		response.Body = &responseBody{reader: reader, cancel: cancel}
		return response, nil
	case <-ctx.Done():
		cancel()
		_ = reader.CloseWithError(ctx.Err())
		_ = writer.CloseWithError(ctx.Err())
		return nil, ctx.Err()
	}
}

func (t *Transport) CloseIdleConnections() {}

type responseWriter struct {
	mu        sync.Mutex
	header    http.Header
	status    int
	writer    *io.PipeWriter
	request   *http.Request
	responses chan<- *http.Response
}

func (w *responseWriter) Header() http.Header { return w.header }

func (w *responseWriter) WriteHeader(status int) {
	w.mu.Lock()
	if w.status != 0 {
		w.mu.Unlock()
		return
	}
	w.status = status
	header := w.header.Clone()
	w.mu.Unlock()
	w.responses <- &http.Response{
		Status:        fmt.Sprintf("%d %s", status, http.StatusText(status)),
		StatusCode:    status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        header,
		ContentLength: -1,
		Request:       w.request,
	}
}

func (w *responseWriter) Write(data []byte) (int, error) {
	if !w.committed() {
		w.WriteHeader(http.StatusOK)
	}
	return w.writer.Write(data)
}

func (w *responseWriter) Flush() {
	if !w.committed() {
		w.WriteHeader(http.StatusOK)
	}
}

func (w *responseWriter) committed() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.status != 0
}

type responseBody struct {
	reader *io.PipeReader
	cancel context.CancelFunc
	once   sync.Once
}

func (b *responseBody) Read(data []byte) (int, error) { return b.reader.Read(data) }

func (b *responseBody) Close() error {
	var err error
	b.once.Do(func() {
		b.cancel()
		err = b.reader.CloseWithError(context.Canceled)
	})
	return err
}
