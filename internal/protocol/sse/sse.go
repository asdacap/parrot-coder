// Package sse decodes Server-Sent Events used by provider streaming APIs.
package sse

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
)

const DefaultMaxEventBytes = 4 << 20

var ErrEventTooLarge = errors.New("sse: event exceeds maximum size")

// Event is one decoded SSE record.
type Event struct {
	Event string
	Data  string
	ID    string
}

type result struct {
	event Event
	err   error
}

// Decoder incrementally decodes SSE records. It supports records and lines of
// arbitrary length, bounded only by maxEventBytes.
type Decoder struct {
	reader io.Reader
	closer io.Closer
	max    int

	once      sync.Once
	closeOnce sync.Once
	results   chan result
	done      chan struct{}
}

// NewDecoder creates a decoder. A non-positive limit uses
// DefaultMaxEventBytes.
func NewDecoder(reader io.Reader, maxEventBytes int) *Decoder {
	if maxEventBytes <= 0 {
		maxEventBytes = DefaultMaxEventBytes
	}
	d := &Decoder{reader: reader, max: maxEventBytes, results: make(chan result), done: make(chan struct{})}
	if closer, ok := reader.(io.Closer); ok {
		d.closer = closer
	}
	return d
}

// Next returns the next event. If ctx is canceled while a source read is
// blocked, an io.ReadCloser source is closed to unblock it.
func (d *Decoder) Next(ctx context.Context) (Event, error) {
	d.once.Do(func() { go d.run() })
	select {
	case <-ctx.Done():
		d.close()
		return Event{}, ctx.Err()
	case item, ok := <-d.results:
		if !ok {
			return Event{}, io.EOF
		}
		return item.event, item.err
	}
}

// Close stops decoding and closes the underlying source when possible.
func (d *Decoder) Close() error {
	d.close()
	return nil
}

func (d *Decoder) close() {
	d.closeOnce.Do(func() {
		close(d.done)
		if d.closer != nil {
			_ = d.closer.Close()
		}
	})
}

func (d *Decoder) run() {
	defer close(d.results)
	reader := bufio.NewReader(d.reader)
	var event Event
	var data []string
	size := 0
	hasFields := false

	emit := func() bool {
		if !hasFields {
			size = 0
			return true
		}
		event.Data = strings.Join(data, "\n")
		select {
		case d.results <- result{event: event}:
			event = Event{}
			data = nil
			size = 0
			hasFields = false
			return true
		case <-d.done:
			return false
		}
	}

	for {
		var line []byte
		var err error
		for {
			fragment, readErr := reader.ReadSlice('\n')
			size += len(fragment)
			if size > d.max {
				d.send(result{err: fmt.Errorf("%w: limit %d", ErrEventTooLarge, d.max)})
				return
			}
			line = append(line, fragment...)
			if errors.Is(readErr, bufio.ErrBufferFull) {
				continue
			}
			err = readErr
			break
		}
		text := strings.TrimSuffix(string(line), "\n")
		text = strings.TrimSuffix(text, "\r")
		if text == "" {
			if !emit() {
				return
			}
		} else if text[0] != ':' {
			name, value, found := strings.Cut(text, ":")
			if found && strings.HasPrefix(value, " ") {
				value = value[1:]
			}
			switch name {
			case "event":
				event.Event = value
				hasFields = true
			case "data":
				data = append(data, value)
				hasFields = true
			case "id":
				if !strings.ContainsRune(value, '\x00') {
					event.ID = value
				}
				hasFields = true
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				if !emit() {
					return
				}
				return
			}
			d.send(result{err: fmt.Errorf("sse: read: %w", err)})
			return
		}
	}
}

func (d *Decoder) send(item result) {
	select {
	case d.results <- item:
	case <-d.done:
	}
}
