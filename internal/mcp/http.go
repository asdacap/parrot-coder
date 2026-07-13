package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

type httpEndpoint struct {
	config        Config
	url           *url.URL
	client        *http.Client
	lifetime      context.Context
	cancel        context.CancelFunc
	nextID        atomic.Int64
	sessionMu     sync.RWMutex
	sessionID     string
	protocol      string
	closed        atomic.Bool
	doneChannel   chan struct{}
	notifications chan Notification
}

func startHTTP(config Config) (*httpEndpoint, error) {
	u, err := url.Parse(config.URL)
	if err != nil {
		return nil, err
	}
	lifetime, cancel := context.WithCancel(context.Background())
	e := &httpEndpoint{
		config:        config,
		url:           u,
		lifetime:      lifetime,
		cancel:        cancel,
		doneChannel:   make(chan struct{}),
		notifications: make(chan Notification, 64),
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.MaxResponseHeaderBytes = maxHeaderBytes
	e.client = &http.Client{
		CheckRedirect: e.checkRedirect,
		Transport:     transport,
	}
	return e, nil
}

func (e *httpEndpoint) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if e.closed.Load() {
		return nil, &dispatchError{err: errors.New("mcp: HTTP transport is closed"), safe: true}
	}
	id := e.nextID.Add(1)
	paramsJSON, err := marshalParams(params)
	if err != nil {
		return nil, err
	}
	request := wireMessage{JSONRPC: "2.0", ID: json.RawMessage(strconv.FormatInt(id, 10)), Method: method, Params: paramsJSON}
	raw, err := e.post(ctx, request, false)
	if err != nil {
		return nil, err
	}
	var response wireMessage
	if err := json.Unmarshal(raw, &response); err != nil || response.JSONRPC != "2.0" {
		return nil, errors.New("mcp: invalid HTTP JSON-RPC response")
	}
	if response.Method != "" {
		if response.Method == "elicitation/create" {
			return nil, errors.New("mcp: elicitation is unsupported")
		}
		return nil, fmt.Errorf("mcp: server request %q is unsupported", response.Method)
	}
	var responseID int64
	if err := json.Unmarshal(response.ID, &responseID); err != nil || responseID != id {
		return nil, errors.New("mcp: HTTP response request ID mismatch")
	}
	if response.Error != nil {
		return nil, response.Error
	}
	if response.Result == nil {
		return nil, errors.New("mcp: HTTP response contains neither result nor error")
	}
	return append(json.RawMessage(nil), response.Result...), nil
}

func (e *httpEndpoint) notify(ctx context.Context, method string, params any) error {
	if e.closed.Load() {
		return errors.New("mcp: HTTP transport is closed")
	}
	paramsJSON, err := marshalParams(params)
	if err != nil {
		return err
	}
	_, err = e.post(ctx, wireMessage{JSONRPC: "2.0", Method: method, Params: paramsJSON}, true)
	return err
}

func (e *httpEndpoint) post(ctx context.Context, message wireMessage, notification bool) (json.RawMessage, error) {
	body, err := json.Marshal(message)
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > e.config.MaxMessageBytes {
		return nil, errors.New("mcp: outgoing HTTP message exceeds byte limit")
	}
	requestCtx, cancel := context.WithCancel(ctx)
	stopLifetimeCancel := context.AfterFunc(e.lifetime, cancel)
	defer func() {
		stopLifetimeCancel()
		cancel()
	}()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, e.url.String(), bytes.NewReader(body))
	if err != nil {
		return nil, &dispatchError{err: err, safe: true}
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	for name, value := range e.config.Headers {
		request.Header.Set(name, value)
	}
	e.sessionMu.RLock()
	sessionID := e.sessionID
	protocol := e.protocol
	e.sessionMu.RUnlock()
	if sessionID != "" {
		request.Header.Set("Mcp-Session-Id", sessionID)
	}
	if protocol != "" {
		request.Header.Set("Mcp-Protocol-Version", protocol)
	}
	response, err := e.client.Do(request)
	if err != nil {
		// A dial failure occurs before HTTP dispatch. Other net/http failures do
		// not prove whether request bytes reached the peer.
		var operationError *net.OpError
		safe := ctx.Err() == nil && errors.As(err, &operationError) && operationError.Op == "dial"
		return nil, &dispatchError{err: fmt.Errorf("mcp: HTTP request: %w", err), safe: safe}
	}
	defer response.Body.Close()
	if value := response.Header.Get("Mcp-Session-Id"); value != "" {
		if len(value) > 1024 || strings.ContainsAny(value, "\r\n\x00") {
			return nil, errors.New("mcp: invalid HTTP session ID")
		}
		e.sessionMu.Lock()
		e.sessionID = value
		e.sessionMu.Unlock()
	}
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return nil, errors.New("mcp: server requires authorization; OAuth is unsupported")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("mcp: HTTP server returned %s", response.Status)
	}
	if notification && response.StatusCode == http.StatusAccepted {
		return nil, nil
	}
	if response.ContentLength > e.config.MaxMessageBytes {
		return nil, errors.New("mcp: HTTP response exceeds byte limit")
	}
	data, err := readBounded(response.Body, e.config.MaxMessageBytes)
	if err != nil {
		return nil, err
	}
	if notification && len(bytes.TrimSpace(data)) == 0 {
		return nil, nil
	}
	contentType := response.Header.Get("Content-Type")
	mediaType, _, mediaTypeErr := mime.ParseMediaType(contentType)
	if contentType != "" && mediaTypeErr != nil {
		return nil, errors.New("mcp: malformed HTTP response content type")
	}
	if mediaType == "text/event-stream" {
		events, decodeErr := decodeHTTPSSE(data, e.config.MaxMessageBytes)
		if decodeErr != nil {
			return nil, decodeErr
		}
		for _, event := range events {
			var candidate wireMessage
			if err := json.Unmarshal(event, &candidate); err != nil {
				return nil, errors.New("mcp: SSE event is not a JSON-RPC message")
			}
			if candidate.Method != "" && len(candidate.ID) == 0 {
				if candidate.JSONRPC != "2.0" {
					return nil, errors.New("mcp: invalid SSE JSON-RPC notification")
				}
				item := Notification{Server: e.config.Name, Method: candidate.Method, Params: append(json.RawMessage(nil), candidate.Params...)}
				select {
				case e.notifications <- item:
				default:
				}
				continue
			}
			if len(candidate.ID) != 0 {
				return event, nil
			}
		}
		if notification {
			return nil, nil
		}
		return nil, errors.New("mcp: SSE response contains no JSON-RPC response")
	}
	if mediaType != "" && mediaType != "application/json" && mediaType != "application/json-rpc" {
		return nil, fmt.Errorf("mcp: unsupported HTTP response content type %q", mediaType)
	}
	if !json.Valid(data) {
		return nil, errors.New("mcp: HTTP response is not valid JSON")
	}
	return json.RawMessage(data), nil
}

func (e *httpEndpoint) checkRedirect(request *http.Request, via []*http.Request) error {
	if len(via) >= 3 {
		return errors.New("mcp: too many HTTP redirects")
	}
	if request.URL.User != nil {
		return errors.New("mcp: redirect with embedded credentials is forbidden")
	}
	if request.Method != http.MethodPost {
		return errors.New("mcp: redirect changed the JSON-RPC request method")
	}
	if !sameOrigin(e.url, request.URL) {
		return errors.New("mcp: cross-origin or scheme-changing redirect is forbidden")
	}
	return nil
}

func sameOrigin(a, b *url.URL) bool {
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Host, b.Host)
}

func (e *httpEndpoint) close(context.Context) error {
	if e.closed.CompareAndSwap(false, true) {
		e.cancel()
		close(e.doneChannel)
		if transport, ok := e.client.Transport.(*http.Transport); ok {
			transport.CloseIdleConnections()
		}
	}
	return nil
}

func (e *httpEndpoint) setProtocolVersion(version string) {
	e.sessionMu.Lock()
	e.protocol = version
	e.sessionMu.Unlock()
}

func (e *httpEndpoint) done() <-chan struct{}                    { return e.doneChannel }
func (e *httpEndpoint) notificationChannel() <-chan Notification { return e.notifications }
func (e *httpEndpoint) pid() int                                 { return 0 }

func readBounded(reader io.Reader, max int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, max+1))
	if err != nil {
		return nil, fmt.Errorf("mcp: read response: %w", err)
	}
	if int64(len(data)) > max {
		return nil, errors.New("mcp: response exceeds byte limit")
	}
	return data, nil
}

func decodeHTTPSSE(data []byte, max int64) ([]json.RawMessage, error) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	bufferSize := int(max)
	if bufferSize < 4096 {
		bufferSize = 4096
	}
	scanner.Buffer(make([]byte, 4096), bufferSize)
	var eventData []string
	var events []json.RawMessage
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			if len(eventData) != 0 {
				raw := []byte(strings.Join(eventData, "\n"))
				if !json.Valid(raw) {
					return nil, errors.New("mcp: SSE event data is not valid JSON")
				}
				events = append(events, json.RawMessage(raw))
				eventData = nil
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			eventData = append(eventData, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, errors.New("mcp: SSE event exceeds byte limit")
	}
	if len(eventData) != 0 {
		raw := []byte(strings.Join(eventData, "\n"))
		if !json.Valid(raw) {
			return nil, errors.New("mcp: SSE event data is not valid JSON")
		}
		events = append(events, json.RawMessage(raw))
	}
	if len(events) == 0 {
		return nil, errors.New("mcp: SSE response contains no JSON-RPC event")
	}
	return events, nil
}
