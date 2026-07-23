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
// fails before delivering client-visible output. A retry cannot duplicate
// output, so a stream that already emitted output is never restarted: its
// error passes through unchanged. Usage and router metadata are bookkeeping
// and do not make a retry unsafe. notify, when set, observes each automatic
// retry of a transient engine-overload rejection.
func StreamWithRetry(ctx context.Context, client Provider, request protocol.Request, notify func(RetryNotice)) (Stream, error) {
	return streamWithRetry(ctx, client, request, streamRetryBaseDelay, notify)
}

func streamWithRetry(ctx context.Context, client Provider, request protocol.Request, baseDelay time.Duration, notify func(RetryNotice)) (Stream, error) {
	budget := &overloadRetryBudget{}
	stream, err := streamWithHeaderRetryBudget(ctx, client, request, headerRetryInitialDelay, headerRetryMaximumDelay, notify, budget)
	if err != nil {
		return nil, err
	}
	return &retryStream{
		client: client, request: request, stream: stream, remaining: streamMaxRetries, baseDelay: baseDelay,
		notify: notify, overloadBudget: budget, overloadInitialDelay: headerRetryInitialDelay, overloadMaximumDelay: headerRetryMaximumDelay,
	}, nil
}

type retryStream struct {
	client               Provider
	request              protocol.Request
	stream               Stream
	remaining            int
	attempt              int
	outputEmitted        bool
	buffered             []protocol.Event
	queued               []protocol.Event
	baseDelay            time.Duration
	notify               func(RetryNotice)
	overloadBudget       *overloadRetryBudget
	overloadInitialDelay time.Duration
	overloadMaximumDelay time.Duration
}

func (s *retryStream) Next(ctx context.Context) (protocol.Event, error) {
	for {
		if len(s.queued) > 0 {
			event := s.queued[0]
			s.queued = s.queued[1:]
			return event, nil
		}
		event, err := s.stream.Next(ctx)
		if err == nil {
			if !s.outputEmitted && (event.Type == protocol.EventUsage || event.Type == protocol.EventRouterMetadata) {
				s.bufferBookkeeping(event)
				continue
			}
			if !s.outputEmitted && event.Type == protocol.EventProviderError && event.ProviderError != nil {
				responseErr := &ResponseError{Type: event.ProviderError.Type, Code: event.ProviderError.Code, Message: event.ProviderError.Message}
				if IsEngineOverloadedError(responseErr) {
					if retryErr := s.retryOverload(ctx, responseErr); retryErr == nil {
						s.buffered = nil
						continue
					} else if !errors.Is(retryErr, responseErr) {
						return protocol.Event{}, retryErr
					}
				}
			}
			s.outputEmitted = true
			if len(s.buffered) > 0 {
				s.queued = append(s.buffered, event)
				s.buffered = nil
				continue
			}
			return event, nil
		}
		if s.outputEmitted || s.remaining == 0 || ctx.Err() != nil || !retryableStreamError(err) {
			return protocol.Event{}, err
		}
		// A clean EOF before any event is a completed empty response, not a
		// dropped stream; both wire parsers convert premature EOF into an
		// error, so only a custom Stream can reach this.
		if errors.Is(err, io.EOF) {
			if len(s.buffered) > 0 {
				s.queued = s.buffered
				s.buffered = nil
				continue
			}
			return protocol.Event{}, err
		}
		s.buffered = nil
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
		reopened, openErr := streamWithHeaderRetryBudget(ctx, s.client, s.request, s.overloadInitialDelay, s.overloadMaximumDelay, s.notify, s.overloadBudget)
		if openErr != nil {
			return protocol.Event{}, errors.Join(err, openErr)
		}
		s.stream = reopened
	}
}

func (s *retryStream) bufferBookkeeping(event protocol.Event) {
	for i := range s.buffered {
		if s.buffered[i].Type == event.Type {
			s.buffered[i] = event
			return
		}
	}
	s.buffered = append(s.buffered, event)
}

func (s *retryStream) retryOverload(ctx context.Context, responseErr *ResponseError) error {
	attempt, retry := s.overloadBudget.take()
	if !retry {
		return responseErr
	}
	delay := s.overloadInitialDelay << min(attempt-1, 4)
	if delay > s.overloadMaximumDelay {
		delay = s.overloadMaximumDelay
	}
	notice := RetryNotice{Provider: s.client.ID(), Model: s.request.Model, Attempt: attempt, MaxRetries: overloadMaxRetries, Delay: delay}
	diagnostics.Warn("provider_overloaded_retry",
		"provider", s.client.ID(), "model", s.request.Model, "attempt", attempt, "max_retries", overloadMaxRetries,
		"retry_delay_ms", delay.Milliseconds(), "error", responseErr,
	)
	if s.notify != nil {
		s.notify(notice)
	}
	_ = s.stream.Close()
	if err := waitForRetry(ctx, delay); err != nil {
		return err
	}
	reopened, err := streamWithHeaderRetryBudget(ctx, s.client, s.request, s.overloadInitialDelay, s.overloadMaximumDelay, s.notify, s.overloadBudget)
	if err != nil {
		return err
	}
	s.stream = reopened
	return nil
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
