package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/amirulashraf/parrot-coder/internal/diagnostics"
)

const (
	maxRequestBytes = 4 << 20
	maxErrorBytes   = 64 << 10
	maxEventBytes   = 4 << 20
	maxStreamBytes  = 64 << 20
	requestTimeout  = 10 * time.Minute
)

var forbiddenHeaders = map[string]struct{}{
	"authorization":       {},
	"cookie":              {},
	"host":                {},
	"proxy-authorization": {},
}

var ErrStreamTooLarge = errors.New("provider: response stream exceeds byte limit")

// HeaderTimeoutError reports that a provider did not return response headers
// within the configured deadline. The response body is not covered by this
// timeout.
type HeaderTimeoutError struct {
	Timeout time.Duration
}

func (e *HeaderTimeoutError) Error() string {
	return fmt.Sprintf("provider: response headers timed out after %s", e.Timeout)
}

// HTTPError is a bounded, safe representation of a non-success response.
type HTTPError struct {
	StatusCode int
	Type       string
	Code       string
	Message    string
	Response   string
}

func (e *HTTPError) Error() string {
	message := "provider request failed with HTTP " + strconv.Itoa(e.StatusCode)
	if e.Type != "" {
		message += " (" + e.Type + ")"
	}
	if e.Code != "" {
		message += " [" + e.Code + "]"
	}
	if e.Message != "" {
		message += ": " + e.Message
	}
	if e.StatusCode == http.StatusInternalServerError && e.Response != "" && e.Response != e.Message {
		message += "\nResponse: " + e.Response
	}
	return message
}

type streamParser func(io.Reader, int) Stream

func endpointURL(raw, endpoint string, allowInsecureLocalhost bool) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("provider: invalid base URL: %w", err)
	}
	if parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("provider: base URL must contain only a scheme, host, and path")
	}
	switch parsed.Scheme {
	case "https":
	case "http":
		if !allowInsecureLocalhost || !isLoopbackHost(parsed.Hostname()) {
			return nil, errors.New("provider: HTTP is allowed only for loopback hosts with allow_insecure_localhost")
		}
	default:
		return nil, errors.New("provider: base URL must use HTTPS")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/" + endpoint
	parsed.RawPath = ""
	return parsed, nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func validatedHeaders(headers map[string]string) (http.Header, error) {
	result := make(http.Header, len(headers))
	for name, value := range headers {
		canonical := http.CanonicalHeaderKey(name)
		if canonical == "" || strings.TrimSpace(name) != name || !validHeaderName(name) {
			return nil, fmt.Errorf("provider: invalid configured header name %q", name)
		}
		if _, denied := forbiddenHeaders[strings.ToLower(canonical)]; denied {
			return nil, fmt.Errorf("provider: configured header %q is not allowed", canonical)
		}
		if strings.ContainsAny(value, "\r\n") {
			return nil, fmt.Errorf("provider: configured header %q has an invalid value", canonical)
		}
		result.Set(canonical, value)
	}
	return result, nil
}

func validHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for _, character := range name {
		if !(character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", character)) {
			return false
		}
	}
	return true
}

// secureClient hardens a provider HTTP client. Non-streaming requests are
// bounded by requestTimeout; streaming requests are not, because
// http.Client.Timeout also bounds reading the response body and would cut off
// long SSE streams. Streams are instead bounded by the header timeout, caller
// cancellation, and the maxStreamBytes read limit.
func secureClient(source *http.Client, target *url.URL, streaming bool) *http.Client {
	if source == nil {
		source = http.DefaultClient
	}
	client := *source
	if streaming {
		client.Timeout = 0
	} else if client.Timeout == 0 || client.Timeout > requestTimeout {
		client.Timeout = requestTimeout
	}
	previous := source.CheckRedirect
	client.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if !sameOrigin(request.URL, target) {
			return errors.New("provider: cross-origin redirect refused")
		}
		if previous != nil {
			return previous(request, via)
		}
		if len(via) >= 10 {
			return errors.New("provider: too many redirects")
		}
		return nil
	}
	return &client
}

func sameOrigin(left, right *url.URL) bool {
	return strings.EqualFold(left.Scheme, right.Scheme) && strings.EqualFold(left.Host, right.Host)
}

func startStream(ctx context.Context, client *http.Client, endpoint *url.URL, body []byte, headers http.Header, secrets []string, headerTimeout time.Duration, parser streamParser) (stream Stream, err error) {
	started := time.Now()
	responseStatus := 0
	diagnostics.Event("provider_http_headers_started",
		"host", endpoint.Hostname(), "path", endpoint.EscapedPath(), "request_bytes", len(body), "header_timeout_ms", headerTimeout.Milliseconds(),
	)
	defer func() {
		attributes := []any{
			"host", endpoint.Hostname(), "path", endpoint.EscapedPath(), "status", responseStatus,
			"duration_ms", time.Since(started).Milliseconds(),
		}
		if err != nil {
			diagnostics.Error("provider_http_headers_finished", append(attributes, "result", "error", "error_type", diagnostics.ErrorType(err))...)
		} else {
			diagnostics.Event("provider_http_headers_finished", append(attributes, "result", "success")...)
		}
	}()
	if len(body) > maxRequestBytes {
		return nil, fmt.Errorf("provider: request exceeds %d bytes", maxRequestBytes)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("provider: create request: %w", err)
	}
	request.Header = headers.Clone()
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream")
	stopHeaderTimer := func() {}
	cancelHeaderContext := func() {}
	if headerTimeout > 0 {
		requestCtx, cancel := context.WithCancelCause(ctx)
		timeoutErr := &HeaderTimeoutError{Timeout: headerTimeout}
		fired := make(chan struct{})
		timer := time.AfterFunc(headerTimeout, func() {
			cancel(timeoutErr)
			close(fired)
		})
		stopHeaderTimer = func() {
			if !timer.Stop() {
				<-fired
			}
		}
		cancelHeaderContext = func() { cancel(nil) }
		request = request.WithContext(requestCtx)
	}
	response, err := client.Do(request)
	stopHeaderTimer()
	cause := context.Cause(request.Context())
	if err != nil {
		cancelHeaderContext()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var timeoutErr *HeaderTimeoutError
		if errors.As(cause, &timeoutErr) {
			return nil, timeoutErr
		}
		return nil, errors.New("provider: send request: " + redact(err.Error(), secrets))
	}
	responseStatus = response.StatusCode
	if ctx.Err() != nil {
		response.Body.Close()
		cancelHeaderContext()
		return nil, ctx.Err()
	}
	var timeoutErr *HeaderTimeoutError
	if errors.As(cause, &timeoutErr) {
		response.Body.Close()
		cancelHeaderContext()
		return nil, timeoutErr
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		defer cancelHeaderContext()
		defer response.Body.Close()
		return nil, parseHTTPError(response, secrets)
	}
	if response.Body == nil {
		cancelHeaderContext()
		return nil, errors.New("provider: response has no body")
	}
	bounded := &boundedReadCloser{reader: response.Body, closer: response.Body, remaining: maxStreamBytes, onClose: cancelHeaderContext}
	return &redactingStream{Stream: parser(bounded, maxEventBytes), secrets: append([]string(nil), secrets...)}, nil
}

type boundedReadCloser struct {
	reader    io.Reader
	closer    io.Closer
	remaining int64
	onClose   func()
}

func (r *boundedReadCloser) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		var extra [1]byte
		n, err := r.reader.Read(extra[:])
		if n != 0 {
			return 0, ErrStreamTooLarge
		}
		return 0, err
	}
	if int64(len(p)) > r.remaining+1 {
		p = p[:r.remaining+1]
	}
	n, err := r.reader.Read(p)
	if int64(n) > r.remaining {
		n = int(r.remaining)
		r.remaining = 0
		return n, ErrStreamTooLarge
	}
	r.remaining -= int64(n)
	return n, err
}

func (r *boundedReadCloser) Close() error {
	err := r.closer.Close()
	if r.onClose != nil {
		r.onClose()
	}
	return err
}

func parseHTTPError(response *http.Response, secrets []string) error {
	limited := io.LimitReader(response.Body, maxErrorBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return &HTTPError{StatusCode: response.StatusCode, Message: "unable to read provider error"}
	}
	if len(data) > maxErrorBytes {
		data = data[:maxErrorBytes]
	}
	var envelope struct {
		Error   json.RawMessage `json:"error"`
		Type    string          `json:"type"`
		Code    any             `json:"code"`
		Message string          `json:"message"`
	}
	_ = json.Unmarshal(data, &envelope)
	if len(envelope.Error) != 0 {
		var nested struct {
			Type    string `json:"type"`
			Code    any    `json:"code"`
			Message string `json:"message"`
		}
		if json.Unmarshal(envelope.Error, &nested) == nil {
			envelope.Type, envelope.Code, envelope.Message = nested.Type, nested.Code, nested.Message
		}
	}
	result := &HTTPError{StatusCode: response.StatusCode}
	result.Type = sanitize(envelope.Type, secrets, 128)
	if envelope.Code != nil {
		result.Code = sanitize(fmt.Sprint(envelope.Code), secrets, 128)
	}
	result.Message = sanitize(envelope.Message, secrets, 1024)
	if response.StatusCode == http.StatusInternalServerError {
		result.Response = sanitizeResponse(string(data), secrets)
	}
	if result.Message == "" {
		result.Message = sanitize(string(data), secrets, 1024)
	}
	if result.Message == "" {
		result.Message = http.StatusText(response.StatusCode)
	}
	return result
}

func sanitize(value string, secrets []string, limit int) string {
	value = redact(value, secrets)
	value = strings.Map(func(character rune) rune {
		if unicode.IsControl(character) && character != '\t' {
			return -1
		}
		return character
	}, value)
	value = strings.TrimSpace(value)
	if len(value) <= limit {
		return value
	}
	value = value[:limit]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func sanitizeResponse(value string, secrets []string) string {
	value = redact(value, secrets)
	value = strings.Map(func(character rune) rune {
		if unicode.IsControl(character) && character != '\t' && character != '\n' && character != '\r' {
			return -1
		}
		return character
	}, value)
	return strings.TrimSpace(value)
}

func redact(value string, secrets []string) string {
	for _, secret := range secrets {
		if secret != "" {
			value = strings.ReplaceAll(value, secret, "[REDACTED]")
		}
	}
	return value
}
