package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestContentLengthFraming(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	writer := NewFramedWriter(&shortWriter{writer: &output, max: 3}, 1024)
	want := map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"ok": true}}
	if err := writer.Write(want); err != nil {
		t.Fatal(err)
	}
	reader := NewFramedReader(&oneByteReader{reader: bytes.NewReader(output.Bytes())}, 1024)
	raw, err := reader.Read()
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got["jsonrpc"] != "2.0" || got["id"] != float64(1) {
		t.Fatalf("decoded message = %#v", got)
	}
	if _, err := reader.Read(); !errors.Is(err, io.EOF) {
		t.Fatalf("second read error = %v", err)
	}
}

func TestFramingRejectsMaliciousSizesAndHeaders(t *testing.T) {
	t.Parallel()
	tests := []string{
		"Content-Length: 999999999999999999999\r\n\r\n",
		"Content-Length: 2\r\nContent-Length: 2\r\n\r\n{}",
		"Content-Length: -1\r\n\r\n",
		"X: " + strings.Repeat("a", maxFrameHeaderBytes) + "\r\n\r\n",
		"Content-Length: 9\r\n\r\n{}",
	}
	for _, input := range tests {
		if _, err := NewFramedReader(strings.NewReader(input), 8).Read(); err == nil {
			t.Fatalf("malicious frame accepted: %.40q", input)
		}
	}
	if err := NewFramedWriter(io.Discard, 2).Write(map[string]any{"large": true}); err == nil {
		t.Fatal("oversized outgoing frame accepted")
	}
}

func TestRPCDispatchWriteHonorsCancellation(t *testing.T) {
	t.Parallel()
	input, serverOutput := io.Pipe()
	output := &blockingWriter{started: make(chan struct{}, 2), release: make(chan struct{})}
	peer := newRPCPeer("blocked", input, output, 1024)
	defer serverOutput.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := peer.call(ctx, "tools/list", map[string]any{})
			errs <- err
		}()
	}
	select {
	case <-output.started:
	case <-time.After(time.Second):
		t.Fatal("first write did not start")
	}
	for range 2 {
		if err := <-errs; !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("call error = %v", err)
		}
	}
	select {
	case <-output.started:
		t.Fatal("a cancelled queued request was dispatched")
	default:
	}
	close(output.release)
}

type shortWriter struct {
	writer io.Writer
	max    int
}

func (w *shortWriter) Write(value []byte) (int, error) {
	if len(value) > w.max {
		value = value[:w.max]
	}
	return w.writer.Write(value)
}

type oneByteReader struct{ reader io.Reader }

func (r *oneByteReader) Read(value []byte) (int, error) {
	if len(value) > 1 {
		value = value[:1]
	}
	return r.reader.Read(value)
}

type blockingWriter struct {
	started chan struct{}
	release chan struct{}
}

func (w *blockingWriter) Write(value []byte) (int, error) {
	w.started <- struct{}{}
	<-w.release
	return len(value), nil
}
