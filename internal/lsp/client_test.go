package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLSPHelperProcess(t *testing.T) {
	if os.Getenv("PARROT_LSP_HELPER") != "1" {
		return
	}
	marker := os.Getenv("PARROT_LSP_MARKER")
	appendMarker(marker, "start\n")
	reader := bufio.NewReader(os.Stdin)
	var writes sync.Mutex
	write := func(value any) {
		writes.Lock()
		defer writes.Unlock()
		frame, _ := EncodeFrame(value)
		_, _ = os.Stdout.Write(frame)
	}
	for {
		body, err := readFrame(reader, 1<<20)
		if err != nil {
			os.Exit(0)
		}
		var message rpcMessage
		if json.Unmarshal(body, &message) != nil {
			os.Exit(2)
		}
		switch message.Method {
		case "initialize":
			write(map[string]any{"jsonrpc": "2.0", "id": message.ID, "result": map[string]any{"capabilities": map[string]any{}}})
		case "textDocument/didOpen":
			var params struct {
				TextDocument struct {
					URI DocumentURI `json:"uri"`
				} `json:"textDocument"`
			}
			_ = json.Unmarshal(message.Params, &params)
			write(map[string]any{"jsonrpc": "2.0", "method": "textDocument/publishDiagnostics", "params": map[string]any{
				"uri":         params.TextDocument.URI,
				"diagnostics": []map[string]any{{"message": "one", "range": Range{}}, {"message": "two", "range": Range{}}, {"message": "three", "range": Range{}}},
			}})
		case "textDocument/hover":
			write(map[string]any{"jsonrpc": "2.0", "id": message.ID, "result": map[string]any{"contents": "hover"}})
		case "workspace/symbol":
			write(map[string]any{"jsonrpc": "2.0", "id": message.ID, "result": []any{}})
		case "test/slow":
			// Deliberately leave the request pending so the client must cancel it.
		case "$/cancelRequest":
			appendMarker(marker, "cancel\n")
		case "test/crash":
			os.Exit(12)
		case "shutdown":
			write(map[string]any{"jsonrpc": "2.0", "id": message.ID, "result": nil})
		case "exit":
			appendMarker(marker, "exit\n")
			os.Exit(0)
		}
	}
}

func appendMarker(path, value string) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err == nil {
		_, _ = io.WriteString(file, value)
		_ = file.Close()
	}
}

func newFakeClient(t *testing.T, maxDiagnostics int) (*Client, string, string) {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "file.go")
	if err := os.WriteFile(path, []byte("package p\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "marker")
	client, err := NewClient(context.Background(), Config{
		Name: "fake", Command: executable, Args: []string{"-test.run=TestLSPHelperProcess"}, Workspace: root,
		Environment: map[string]string{"PARROT_LSP_HELPER": "1", "PARROT_LSP_MARKER": marker},
		Timeout:     time.Second, MaxDiagnostics: maxDiagnostics,
	})
	if err != nil {
		t.Fatal(err)
	}
	return client, path, marker
}

func TestLifecycleDiagnosticsCancellationCrashAndRace(t *testing.T) {
	client, path, marker := newFakeClient(t, 2)
	if err := client.DidOpen(context.Background(), path, "go", "package p\n"); err != nil {
		t.Fatal(err)
	}
	uri, _ := FileURI(filepath.Dir(path), path)
	deadline := time.Now().Add(time.Second)
	for len(client.Diagnostics(uri)) != 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := client.Diagnostics(uri); len(got) != 2 || got[0].Message != "one" {
		t.Fatalf("diagnostics = %#v", got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	err := client.Call(ctx, "test/slow", nil, nil)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("slow request error = %v", err)
	}
	waitMarker(t, marker, "cancel\n")

	err = client.Call(context.Background(), "test/crash", nil, nil)
	if err == nil {
		t.Fatal("crashed request unexpectedly succeeded")
	}
	hover, err := client.Hover(context.Background(), path, Position{})
	if err != nil || hover == nil {
		t.Fatalf("hover after restart = %#v, %v", hover, err)
	}
	waitMarkerCount(t, marker, "start\n", 2)

	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := client.Symbols(context.Background(), "x")
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestFramingAndCanonicalURI(t *testing.T) {
	frame, err := EncodeFrame(map[string]string{"value": "hello"})
	if err != nil {
		t.Fatal(err)
	}
	body, err := readFrame(bufio.NewReaderSize(bytesReader(frame), 4), 100)
	if err != nil || string(body) != `{"value":"hello"}` {
		t.Fatalf("frame = %q, %v", body, err)
	}
	if _, err := readFrame(bufio.NewReader(bytesReader([]byte("Content-Length: 101\r\n\r\n"))), 100); err == nil {
		t.Fatal("oversized frame accepted")
	}
	if _, err := readFrame(bufio.NewReader(strings.NewReader("Content-Length: 1\r\nContent-Length: 1\r\n\r\n{}")), 100); err == nil {
		t.Fatal("duplicate content length accepted")
	}
	oversizedHeaders := strings.Repeat("X: y\r\n", 2000) + "Content-Length: 0\r\n\r\n"
	if _, err := readFrame(bufio.NewReader(strings.NewReader(oversizedHeaders)), 100); err == nil {
		t.Fatal("oversized aggregate headers accepted")
	}
	root := t.TempDir()
	path := filepath.Join(root, "a b.go")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	uri, err := FileURI(root, path)
	if err != nil {
		t.Fatal(err)
	}
	canonicalPath, _ := filepath.EvalSymlinks(path)
	if got, err := PathFromURI(root, uri); err != nil || got != canonicalPath {
		t.Fatalf("URI round trip = %q, %v", got, err)
	}
	outside := filepath.Join(filepath.Dir(root), "outside")
	if err := os.WriteFile(outside, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := FileURI(root, outside); err == nil {
		t.Fatal("outside path accepted")
	}
}

type byteReader struct{ data []byte }

func bytesReader(data []byte) *byteReader { return &byteReader{data: data} }
func (r *byteReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

func waitMarker(t *testing.T, path, wanted string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		data, _ := os.ReadFile(path)
		if strings.Contains(string(data), wanted) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	data, _ := os.ReadFile(path)
	t.Fatal(fmt.Sprintf("marker %q does not contain prefix %q", data, wanted))
}

func waitMarkerCount(t *testing.T, path, wanted string, count int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		data, _ := os.ReadFile(path)
		if strings.Count(string(data), wanted) >= count {
			return
		}
		time.Sleep(time.Millisecond)
	}
	data, _ := os.ReadFile(path)
	t.Fatalf("marker %q contains fewer than %d instances of %q", data, count, wanted)
}
