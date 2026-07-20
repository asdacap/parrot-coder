package provider

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/amirulashraf/parrot-coder/internal/diagnostics"
	"github.com/amirulashraf/parrot-coder/internal/protocol"
)

// Stream retries reconnect a dropped response stream, bounded so a provider
// that never completes does not block a turn forever. The budget mirrors
// codex's stream_max_retries default.
const (
	streamMaxRetries     = 5
	streamRetryBaseDelay = 200 * time.Millisecond
)

// StreamWithRetry starts a provider stream and reconnects when the stream
// fails before delivering any event. A retry cannot duplicate client-visible
// output, so a stream that already emitted an event is never restarted: its
// error passes through unchanged.
func StreamWithRetry(ctx context.Context, client Provider, request protocol.Request) (Stream, error) {
	return streamWithRetry(ctx, client, request, streamRetryBaseDelay)
}

func streamWithRetry(ctx context.Context, client Provider, request protocol.Request, baseDelay time.Duration) (Stream, error) {
	stream, err := StreamWithHeaderRetry(ctx, client, request)
	if err != nil {
		return nil, err
	}
	return &retryStream{client: client, request: request, stream: stream, remaining: streamMaxRetries, baseDelay: baseDelay}, nil
}

type retryStream struct {
	client    Provider
	request   protocol.Request
	stream    Stream
	remaining int
	attempt   int
	emitted   bool
	baseDelay time.Duration
}

func (s *retryStream) Next(ctx context.Context) (protocol.Event, error) {
	for {
		event, err := s.stream.Next(ctx)
		if err == nil {
			s.emitted = true
			return event, nil
		}
		if s.emitted || s.remaining == 0 || ctx.Err() != nil || !retryableStreamError(err) {
			return protocol.Event{}, err
		}
		// A clean EOF before any event is a completed empty response, not a
		// dropped stream; both wire parsers convert premature EOF into an
		// error, so only a custom Stream can reach this.
		if errors.Is(err, io.EOF) {
			return protocol.Event{}, err
		}
		s.remaining--
		s.attempt++
		delay := s.baseDelay << min(s.attempt-1, 7)
		diagnostics.Warn("provider_stream_retry",
			"provider", s.client.ID(), "model", s.request.Model,
			"attempt", s.attempt, "max_retries", streamMaxRetries,
			"retry_delay_ms", delay.Milliseconds(), "error", err,
		)
		_ = s.stream.Close()
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return protocol.Event{}, ctx.Err()
		case <-timer.C:
		}
		reopened, openErr := streamWithHeaderRetry(ctx, s.client, s.request, headerRetryInitialDelay, headerRetryMaximumDelay)
		if openErr != nil {
			return protocol.Event{}, errors.Join(err, openErr)
		}
		s.stream = reopened
	}
}

func (s *retryStream) Close() error { return s.stream.Close() }

// retryableStreamError reports whether a failed stream is worth reconnecting.
// Permanent account exhaustion and rejected requests are not, mirroring the
// retryable classification codex applies to sampling requests.
func retryableStreamError(err error) bool {
	if IsUsageLimitError(err) {
		return false
	}
	var httpErr *HTTPError
	if errors.As(err, &httpErr) {
		return httpErr.StatusCode == http.StatusTooManyRequests || httpErr.StatusCode >= http.StatusInternalServerError
	}
	return true
}
