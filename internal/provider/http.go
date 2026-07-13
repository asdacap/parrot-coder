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

// HTTPError is a bounded, safe representation of a non-success response.
type HTTPError struct {
	StatusCode int
	Type       string
	Code       string
	Message    string
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

func secureClient(source *http.Client, target *url.URL) *http.Client {
	if source == nil {
		source = http.DefaultClient
	}
	client := *source
	if client.Timeout == 0 || client.Timeout > requestTimeout {
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

func startStream(ctx context.Context, client *http.Client, endpoint *url.URL, body []byte, headers http.Header, secrets []string, parser streamParser) (Stream, error) {
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
	response, err := client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("provider: send request: " + redact(err.Error(), secrets))
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		defer response.Body.Close()
		return nil, parseHTTPError(response, secrets)
	}
	if response.Body == nil {
		return nil, errors.New("provider: response has no body")
	}
	bounded := &boundedReadCloser{Reader: io.LimitReader(response.Body, maxStreamBytes), Closer: response.Body}
	return parser(bounded, maxEventBytes), nil
}

type boundedReadCloser struct {
	io.Reader
	io.Closer
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

func redact(value string, secrets []string) string {
	for _, secret := range secrets {
		if secret != "" {
			value = strings.ReplaceAll(value, secret, "[REDACTED]")
		}
	}
	return value
}
