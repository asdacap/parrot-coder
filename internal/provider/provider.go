// Package provider connects provider-neutral requests to model HTTP APIs.
package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/amirulashraf/parrot-coder/internal/diagnostics"
	"github.com/amirulashraf/parrot-coder/internal/protocol"
)

const (
	headerRetryInitialDelay = 2 * time.Second
	headerRetryMaximumDelay = 30 * time.Second
	overloadMaxRetries      = 5
)

// Stream is a provider response consumed one canonical event at a time.
type Stream interface {
	Next(context.Context) (protocol.Event, error)
	Close() error
}

// ResponseError preserves the structured failure returned inside an otherwise
// successful provider response stream.
type ResponseError struct {
	Type    string
	Code    string
	Message string
}

func (e *ResponseError) Error() string {
	message := e.Message
	if message == "" {
		message = "provider error"
	}
	if e.Type != "" {
		message += " (" + e.Type + ")"
	}
	if e.Code != "" {
		message += " [" + e.Code + "]"
	}
	return message
}

// IsUsageLimitError reports permanent account/quota exhaustion from structured
// provider fields. HTTP status and message text are intentionally ignored so
// transient rate limiting cannot suspend autonomous work.
func IsUsageLimitError(err error) bool {
	if err == nil {
		return false
	}
	switch current := err.(type) {
	case *HTTPError:
		if usageLimitValue(current.Type) || usageLimitValue(current.Code) {
			return true
		}
	case *ResponseError:
		if usageLimitValue(current.Type) || usageLimitValue(current.Code) {
			return true
		}
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, child := range joined.Unwrap() {
			if IsUsageLimitError(child) {
				return true
			}
		}
		return false
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return IsUsageLimitError(wrapped.Unwrap())
	}
	return false
}

func usageLimitValue(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "usage_limit_reached", "usage_limit_exceeded", "insufficient_quota", "billing_hard_limit_reached":
		return true
	default:
		return false
	}
}

// IsEngineOverloadedError reports a transient provider failure. HTTP 503
// failures are always retryable; other HTTP and in-stream errors are recognized
// from structured fields or a known retryable provider message.
func IsEngineOverloadedError(err error) bool {
	var kind, code, message string
	var httpErr *HTTPError
	if errors.As(err, &httpErr) {
		if httpErr.StatusCode == http.StatusServiceUnavailable {
			return true
		}
		kind, code, message = httpErr.Type, httpErr.Code, httpErr.Message
	} else {
		var responseErr *ResponseError
		if !errors.As(err, &responseErr) {
			return false
		}
		kind, code, message = responseErr.Type, responseErr.Code, responseErr.Message
	}
	return overloadValue(kind) || overloadValue(code) || retryableProviderMessage(message)
}

func overloadValue(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "engine_overloaded_error", "service_unavailable_error", "server_is_overloaded":
		return true
	default:
		return false
	}
}

func retryableProviderMessage(message string) bool {
	const prefix = "an error occurred while processing your request. you can retry your request, or contact us through our help center at help.openai.com if the error persists. please include the request id "
	const suffix = " in your message."

	message = strings.ToLower(strings.TrimSpace(message))
	if !strings.HasPrefix(message, prefix) || !strings.HasSuffix(message, suffix) {
		return false
	}
	return strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(message, prefix), suffix)) != ""
}

// RetryNotice reports an automatic retry of a transient provider failure so
// callers can surface progress while a turn waits.
type RetryNotice struct {
	Provider   string
	Model      string
	Attempt    int
	MaxRetries int
	Delay      time.Duration
}

func (n RetryNotice) String() string {
	return fmt.Sprintf("Provider error: %s servers are overloaded. Retrying at the provider level in %s (attempt %d/%d).", n.Provider, n.Delay, n.Attempt, n.MaxRetries)
}

type overloadRetryBudget struct {
	attempts int
}

func (b *overloadRetryBudget) take() (int, bool) {
	if b.attempts >= overloadMaxRetries {
		return b.attempts, false
	}
	b.attempts++
	return b.attempts, true
}

type redactingStream struct {
	Stream
	secrets []string
}

func (s *redactingStream) Next(ctx context.Context) (protocol.Event, error) {
	event, err := s.Stream.Next(ctx)
	if err != nil {
		if redacted := redact(err.Error(), s.secrets); redacted != err.Error() {
			return protocol.Event{}, redactedError{message: redacted, err: err}
		}
		return protocol.Event{}, err
	}
	event.Text = redact(event.Text, s.secrets)
	if event.ToolInput != nil {
		copy := *event.ToolInput
		copy.ID = redact(copy.ID, s.secrets)
		copy.Name = redact(copy.Name, s.secrets)
		copy.Delta = redact(copy.Delta, s.secrets)
		event.ToolInput = &copy
	}
	if event.ToolCall != nil {
		copy := *event.ToolCall
		copy.ID = redact(copy.ID, s.secrets)
		copy.Name = redact(copy.Name, s.secrets)
		input, redactErr := redactJSON(copy.Input, s.secrets)
		if redactErr != nil {
			return protocol.Event{}, redactErr
		}
		copy.Input = input
		event.ToolCall = &copy
	}
	if event.ProviderError != nil {
		copy := *event.ProviderError
		copy.Type = redact(copy.Type, s.secrets)
		copy.Code = redact(copy.Code, s.secrets)
		copy.Message = redact(copy.Message, s.secrets)
		event.ProviderError = &copy
	}
	return event, nil
}

func redactJSON(raw json.RawMessage, secrets []string) (json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("provider: redact tool input: %w", err)
	}
	encoded, err := json.Marshal(redactJSONValue(value, secrets))
	if err != nil {
		return nil, fmt.Errorf("provider: redact tool input: %w", err)
	}
	return encoded, nil
}

func redactJSONValue(value any, secrets []string) any {
	switch current := value.(type) {
	case string:
		return redact(current, secrets)
	case []any:
		for i := range current {
			current[i] = redactJSONValue(current[i], secrets)
		}
	case map[string]any:
		clean := make(map[string]any, len(current))
		for key, child := range current {
			clean[redact(key, secrets)] = redactJSONValue(child, secrets)
		}
		return clean
	}
	return value
}

type redactedError struct {
	message string
	err     error
}

func (e redactedError) Error() string { return e.message }
func (e redactedError) Unwrap() error { return e.err }

// Provider starts model turns and describes the models it exposes.
type Provider interface {
	ID() string
	Models() []Model
	Stream(context.Context, protocol.Request) (Stream, error)
}

// UsageReporter is implemented by providers whose account exposes subscription
// quota or balance. Providers that cannot report usage simply omit the method.
type UsageReporter interface {
	Usage(context.Context) (SubscriptionUsage, error)
}

// StreamWithHeaderRetry starts a provider stream, retries response-header
// timeouts until cancellation, and retries transient overload rejections up to
// five times. A retry cannot duplicate client-visible partial output,
// although the provider may have processed an earlier request whose response
// headers never reached the client.
func StreamWithHeaderRetry(ctx context.Context, client Provider, request protocol.Request) (Stream, error) {
	return streamWithHeaderRetry(ctx, client, request, headerRetryInitialDelay, headerRetryMaximumDelay, nil)
}

func streamWithHeaderRetry(ctx context.Context, client Provider, request protocol.Request, initialDelay, maximumDelay time.Duration, notify func(RetryNotice)) (Stream, error) {
	return streamWithHeaderRetryBudget(ctx, client, request, initialDelay, maximumDelay, notify, &overloadRetryBudget{})
}

func streamWithHeaderRetryBudget(ctx context.Context, client Provider, request protocol.Request, initialDelay, maximumDelay time.Duration, notify func(RetryNotice), budget *overloadRetryBudget) (Stream, error) {
	for timeoutAttempt := 0; ; {
		stream, err := client.Stream(ctx, request)
		var timeoutErr *HeaderTimeoutError
		overloaded := IsEngineOverloadedError(err)
		if err == nil || !errors.As(err, &timeoutErr) && !overloaded {
			return stream, err
		}
		attempt := timeoutAttempt + 1
		if overloaded {
			var retry bool
			attempt, retry = budget.take()
			if !retry {
				return nil, err
			}
		} else {
			timeoutAttempt++
		}
		delay := initialDelay << min(attempt-1, 4)
		if delay > maximumDelay {
			delay = maximumDelay
		}
		if overloaded {
			notice := RetryNotice{Provider: client.ID(), Model: request.Model, Attempt: attempt, MaxRetries: overloadMaxRetries, Delay: delay}
			diagnostics.Warn("provider_overloaded_retry",
				"provider", client.ID(), "model", request.Model, "attempt", attempt, "max_retries", overloadMaxRetries,
				"retry_delay_ms", delay.Milliseconds(), "error", err,
			)
			if notify != nil {
				notify(notice)
			}
		} else {
			diagnostics.Warn("provider_header_retry",
				"provider", client.ID(), "model", request.Model, "attempt", attempt,
				"header_timeout_ms", timeoutErr.Timeout.Milliseconds(), "retry_delay_ms", delay.Milliseconds(),
			)
		}
		if err := waitForRetry(ctx, delay); err != nil {
			return nil, err
		}
	}
}

func waitForRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	select {
	case <-ctx.Done():
		if !timer.Stop() {
			<-timer.C
		}
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Capabilities describes optional model behavior callers may rely on.
type Capabilities struct {
	Tools     bool
	Reasoning bool
	Output    []string
	Variants  []Variant
}

// Variant contains the request options selected by a named model variant.
type Variant struct {
	Name            string
	ReasoningEffort string
}

func (c Capabilities) Variant(name string) (Variant, bool) {
	for _, variant := range c.Variants {
		if variant.Name == name {
			return variant, true
		}
	}
	return Variant{}, false
}

// Model is provider model metadata used for selection and request planning.
type Model struct {
	ID              string
	Name            string
	ContextWindow   int
	MaxOutputTokens int
	Capabilities    Capabilities
	InputPrice      float64 // USD per input token
	OutputPrice     float64 // USD per output token
}

func cloneModels(models []Model) []Model {
	result := make([]Model, len(models))
	copy(result, models)
	for i := range result {
		result[i].Capabilities.Output = append([]string(nil), result[i].Capabilities.Output...)
		result[i].Capabilities.Variants = append([]Variant(nil), result[i].Capabilities.Variants...)
	}
	return result
}
