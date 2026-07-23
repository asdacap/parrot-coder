package provider

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/amirulashraf/parrot-coder/internal/protocol"
)

// scriptStream replays a fixed sequence of events and errors.
type scriptStream struct {
	steps  []scriptStep
	closed bool
}

type scriptStep struct {
	event protocol.Event
	err   error
}

func (s *scriptStream) Next(context.Context) (protocol.Event, error) {
	if len(s.steps) == 0 {
		return protocol.Event{}, io.EOF
	}
	step := s.steps[0]
	s.steps = s.steps[1:]
	return step.event, step.err
}

func (s *scriptStream) Close() error {
	s.closed = true
	return nil
}

func TestStreamWithRetry(t *testing.T) {
	broken := errors.New("connection reset")
	text := protocol.Event{Type: protocol.EventTextDelta, Text: "hi"}
	finish := protocol.Event{Type: protocol.EventFinish, FinishReason: protocol.FinishStop}

	t.Run("reconnects when the stream drops before any event", func(t *testing.T) {
		dropped := &scriptStream{steps: []scriptStep{{err: broken}}}
		client := &retryProvider{fn: func(call int) (Stream, error) {
			if call == 1 {
				return dropped, nil
			}
			return &scriptStream{steps: []scriptStep{{event: text}, {event: finish}}}, nil
		}}
		stream, err := streamWithRetry(context.Background(), client, protocol.Request{}, time.Millisecond, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer stream.Close()
		event, err := stream.Next(context.Background())
		if err != nil || event.Text != "hi" {
			t.Fatalf("event = %#v, err = %v", event, err)
		}
		if client.calls.Load() != 2 || !dropped.closed {
			t.Fatalf("calls = %d, dropped.closed = %t", client.calls.Load(), dropped.closed)
		}
	})

	t.Run("does not reconnect after an event was emitted", func(t *testing.T) {
		client := &retryProvider{fn: func(int) (Stream, error) {
			return &scriptStream{steps: []scriptStep{{event: text}, {err: broken}}}, nil
		}}
		stream, err := streamWithRetry(context.Background(), client, protocol.Request{}, time.Millisecond, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer stream.Close()
		if _, err := stream.Next(context.Background()); err != nil {
			t.Fatal(err)
		}
		if _, err := stream.Next(context.Background()); !errors.Is(err, broken) || client.calls.Load() != 1 {
			t.Fatalf("calls = %d, err = %v", client.calls.Load(), err)
		}
	})

	t.Run("gives up after the retry budget", func(t *testing.T) {
		client := &retryProvider{fn: func(int) (Stream, error) {
			return &scriptStream{steps: []scriptStep{{err: broken}}}, nil
		}}
		stream, err := streamWithRetry(context.Background(), client, protocol.Request{}, time.Millisecond, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer stream.Close()
		if _, err := stream.Next(context.Background()); !errors.Is(err, broken) {
			t.Fatalf("err = %v", err)
		}
		if want := int32(1 + streamMaxRetries); client.calls.Load() != want {
			t.Fatalf("calls = %d, want %d", client.calls.Load(), want)
		}
	})

	t.Run("does not retry permanent failures", func(t *testing.T) {
		for _, testCase := range []struct {
			name string
			err  error
		}{
			{"usage limit", &HTTPError{StatusCode: http.StatusTooManyRequests, Code: "insufficient_quota"}},
			{"bad request", &HTTPError{StatusCode: http.StatusBadRequest, Code: "context_length_exceeded"}},
			{"unauthorized", &HTTPError{StatusCode: http.StatusUnauthorized}},
		} {
			t.Run(testCase.name, func(t *testing.T) {
				client := &retryProvider{fn: func(int) (Stream, error) {
					return &scriptStream{steps: []scriptStep{{err: testCase.err}}}, nil
				}}
				stream, err := streamWithRetry(context.Background(), client, protocol.Request{}, time.Millisecond, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer stream.Close()
				if _, err := stream.Next(context.Background()); !errors.Is(err, testCase.err) || client.calls.Load() != 1 {
					t.Fatalf("calls = %d, err = %v", client.calls.Load(), err)
				}
			})
		}
	})

	t.Run("surfaces reopen failures", func(t *testing.T) {
		openErr := &HTTPError{StatusCode: http.StatusForbidden}
		client := &retryProvider{fn: func(call int) (Stream, error) {
			if call == 1 {
				return &scriptStream{steps: []scriptStep{{err: broken}}}, nil
			}
			return nil, openErr
		}}
		stream, err := streamWithRetry(context.Background(), client, protocol.Request{}, time.Millisecond, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer stream.Close()
		if _, err := stream.Next(context.Background()); !errors.Is(err, broken) || !errors.Is(err, openErr) {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("passes through a clean empty stream", func(t *testing.T) {
		client := &retryProvider{fn: func(int) (Stream, error) { return &scriptStream{}, nil }}
		stream, err := streamWithRetry(context.Background(), client, protocol.Request{}, time.Millisecond, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer stream.Close()
		if _, err := stream.Next(context.Background()); !errors.Is(err, io.EOF) || client.calls.Load() != 1 {
			t.Fatalf("calls = %d, err = %v", client.calls.Load(), err)
		}
	})
}

func TestStreamWithRetryHandlesStructuredOverloadEvents(t *testing.T) {
	overload := protocol.Event{Type: protocol.EventProviderError, ProviderError: &protocol.ProviderError{
		Type: "service_unavailable_error", Code: "server_is_overloaded", Message: "The service is temporarily overloaded",
	}}
	text := protocol.Event{Type: protocol.EventTextDelta, Text: "recovered"}

	for _, bookkeeping := range []protocol.Event{
		{Type: protocol.EventUsage, Usage: &protocol.Usage{InputTokens: 1}},
		{Type: protocol.EventRouterMetadata, RouterMetadata: &protocol.RouterMetadata{ProviderName: "upstream"}},
	} {
		t.Run("discards failed "+string(bookkeeping.Type), func(t *testing.T) {
			first := &scriptStream{steps: []scriptStep{{event: bookkeeping}, {event: overload}}}
			client := &retryProvider{fn: func(call int) (Stream, error) {
				if call == 1 {
					return first, nil
				}
				return &scriptStream{steps: []scriptStep{{event: text}}}, nil
			}}
			var notices []RetryNotice
			stream, err := streamWithRetry(context.Background(), client, protocol.Request{Model: "m"}, 0, func(notice RetryNotice) {
				notices = append(notices, notice)
			})
			if err != nil {
				t.Fatal(err)
			}
			retrying := stream.(*retryStream)
			retrying.overloadInitialDelay, retrying.overloadMaximumDelay = 0, 0
			event, err := stream.Next(context.Background())
			if err != nil || event.Text != text.Text || client.calls.Load() != 2 || !first.closed || len(notices) != 1 || notices[0].Attempt != 1 {
				t.Fatalf("event = %#v, calls = %d, closed = %t, notices = %#v, err = %v", event, client.calls.Load(), first.closed, notices, err)
			}
		})
	}

	t.Run("shares the opening and streamed overload budget", func(t *testing.T) {
		client := &retryProvider{fn: func(int) (Stream, error) {
			return &scriptStream{steps: []scriptStep{{event: overload}}}, nil
		}}
		stream, err := streamWithRetry(context.Background(), client, protocol.Request{}, 0, nil)
		if err != nil {
			t.Fatal(err)
		}
		retrying := stream.(*retryStream)
		retrying.overloadInitialDelay, retrying.overloadMaximumDelay = 0, 0
		retrying.overloadBudget.attempts = overloadMaxRetries - 1
		event, err := stream.Next(context.Background())
		if err != nil || event.Type != protocol.EventProviderError || client.calls.Load() != 2 || retrying.overloadBudget.attempts != overloadMaxRetries {
			t.Fatalf("event = %#v, calls = %d, attempts = %d, err = %v", event, client.calls.Load(), retrying.overloadBudget.attempts, err)
		}
	})

	t.Run("does not retry after visible output", func(t *testing.T) {
		client := &retryProvider{fn: func(int) (Stream, error) {
			return &scriptStream{steps: []scriptStep{{event: text}, {event: overload}}}, nil
		}}
		stream, err := streamWithRetry(context.Background(), client, protocol.Request{}, 0, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := stream.Next(context.Background()); err != nil {
			t.Fatal(err)
		}
		event, err := stream.Next(context.Background())
		if err != nil || event.Type != protocol.EventProviderError || client.calls.Load() != 1 {
			t.Fatalf("event = %#v, calls = %d, err = %v", event, client.calls.Load(), err)
		}
	})
}

func TestRetryableStreamError(t *testing.T) {
	for _, testCase := range []struct {
		name string
		err  error
		want bool
	}{
		{"io error", errors.New("connection reset"), true},
		{"rate limited", &HTTPError{StatusCode: http.StatusTooManyRequests}, true},
		{"overloaded", &HTTPError{StatusCode: http.StatusServiceUnavailable}, true},
		{"usage limit", &HTTPError{StatusCode: http.StatusTooManyRequests, Code: "usage_limit_reached"}, false},
		{"bad request", &HTTPError{StatusCode: http.StatusBadRequest}, false},
		{"unauthorized", &HTTPError{StatusCode: http.StatusUnauthorized}, false},
		{"not found", &HTTPError{StatusCode: http.StatusNotFound}, false},
		{"wrapped 500", errors.Join(errors.New("read"), &HTTPError{StatusCode: http.StatusInternalServerError}), true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := retryableStreamError(testCase.err); got != testCase.want {
				t.Fatalf("retryableStreamError(%v) = %t, want %t", testCase.err, got, testCase.want)
			}
		})
	}
}
