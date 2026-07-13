package sse

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

type chunkReader struct {
	data []byte
	size int
}

func (r *chunkReader) Read(buffer []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	count := min(len(r.data), r.size, len(buffer))
	copy(buffer, r.data[:count])
	r.data = r.data[count:]
	return count, nil
}

func TestDecoderFieldsCRLFCommentsAndSplitReads(t *testing.T) {
	fixture := ": heartbeat\r\nevent: update\r\nid: evt_1\r\ndata: first\r\ndata: second\r\n\r\n"
	decoder := NewDecoder(&chunkReader{data: []byte(fixture), size: 2}, 1024)
	event, err := decoder.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if event.Event != "update" || event.ID != "evt_1" || event.Data != "first\nsecond" {
		t.Fatalf("unexpected event: %#v", event)
	}
	if _, err := decoder.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("Next after fixture = %v, want EOF", err)
	}
}

func TestDecoderAllowsRecordsBeyondScannerLimit(t *testing.T) {
	data := strings.Repeat("x", 128<<10)
	decoder := NewDecoder(strings.NewReader("data: "+data+"\n\n"), 256<<10)
	event, err := decoder.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if event.Data != data {
		t.Fatalf("data length = %d, want %d", len(event.Data), len(data))
	}
}

func TestDecoderRejectsOversizedEvent(t *testing.T) {
	decoder := NewDecoder(strings.NewReader("data: 123456789\n\n"), 10)
	if _, err := decoder.Next(context.Background()); !errors.Is(err, ErrEventTooLarge) {
		t.Fatalf("Next error = %v, want ErrEventTooLarge", err)
	}
}

func TestDecoderCancellationUnblocksReadCloser(t *testing.T) {
	reader, writer := io.Pipe()
	defer writer.Close()
	decoder := NewDecoder(reader, 1024)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := decoder.Next(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Next error = %v, want context deadline", err)
	}
}

func TestCommentRecordsDoNotConsumeFollowingEventLimit(t *testing.T) {
	decoder := NewDecoder(strings.NewReader(": 123456\n\ndata: ok\n\n"), 12)
	event, err := decoder.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if event.Data != "ok" {
		t.Fatalf("data = %q", event.Data)
	}
}
