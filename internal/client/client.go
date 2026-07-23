// Package client implements the typed v1 HTTP client.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"

	v1 "github.com/amirulashraf/parrot-coder/internal/api/v1"
)

const defaultMaxResponseBytes int64 = 4 << 20

type Client struct {
	baseURL          *url.URL
	http             *http.Client
	MaxResponseBytes int64
	MaxEventBytes    int
}

type APIError struct {
	Problem v1.Problem
}

func (e *APIError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("parrot API: %s (%s)", e.Problem.Title, e.Problem.Code)
}

func New(baseURL string, transport http.RoundTripper) (*Client, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, errors.New("client: absolute base URL is required")
	}
	if transport == nil {
		transport = http.DefaultTransport
	}
	return &Client{baseURL: parsed, http: &http.Client{Transport: transport}, MaxResponseBytes: defaultMaxResponseBytes, MaxEventBytes: 1 << 20}, nil
}

func (c *Client) Health(ctx context.Context) (v1.Health, error) {
	var out v1.Health
	err := c.do(ctx, http.MethodGet, "/api/v1/health", nil, http.StatusOK, &out)
	return out, err
}

func (c *Client) Runtime(ctx context.Context) (v1.Runtime, error) {
	var out v1.Runtime
	err := c.do(ctx, http.MethodGet, "/api/v1/runtime", nil, http.StatusOK, &out)
	return out, err
}

func (c *Client) Sessions(ctx context.Context) (v1.SessionList, error) {
	return c.SessionsPage(ctx, "", 0)
}

func (c *Client) SessionsPage(ctx context.Context, cursor string, limit int) (v1.SessionList, error) {
	var out v1.SessionList
	err := c.do(ctx, http.MethodGet, "/api/v1/sessions"+pageQuery(cursor, limit), nil, http.StatusOK, &out)
	return out, err
}

func (c *Client) CreateSession(ctx context.Context, request v1.CreateSessionRequest) (v1.Session, error) {
	var out v1.Session
	err := c.do(ctx, http.MethodPost, "/api/v1/sessions", request, http.StatusCreated, &out)
	return out, err
}

func (c *Client) ClaimSession(ctx context.Context, request v1.ClaimSessionRequest) (v1.ClaimSessionResponse, error) {
	var out v1.ClaimSessionResponse
	err := c.do(ctx, http.MethodPost, "/api/v1/interactive-sessions/claim", request, http.StatusOK, &out)
	return out, err
}

func (c *Client) Session(ctx context.Context, id string) (v1.Session, error) {
	var out v1.Session
	err := c.do(ctx, http.MethodGet, sessionPath(id), nil, http.StatusOK, &out)
	return out, err
}

func (c *Client) UpdateSessionSelection(ctx context.Context, id string, request v1.UpdateSessionSelectionRequest) (v1.SessionSelection, error) {
	var out v1.SessionSelection
	err := c.do(ctx, http.MethodPut, sessionPath(id)+"/selection", request, http.StatusOK, &out)
	return out, err
}

func (c *Client) DeleteSession(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, sessionPath(id), nil, http.StatusNoContent, nil)
}

// Messages returns the session's complete message history. The server caps each
// page (100 items), and callers treat the result as authoritative — a truncated
// first page hides the active tail of a long session — so follow the cursor and
// aggregate every page before returning.
func (c *Client) Messages(ctx context.Context, id string) (v1.MessageList, error) {
	var out v1.MessageList
	for cursor := ""; ; {
		page, err := c.MessagesPage(ctx, id, cursor, 0)
		if err != nil {
			return v1.MessageList{}, err
		}
		out.Items = append(out.Items, page.Items...)
		if page.NextCursor == "" {
			return out, nil
		}
		cursor = page.NextCursor
	}
}

func (c *Client) MessagesPage(ctx context.Context, id, cursor string, limit int) (v1.MessageList, error) {
	var out v1.MessageList
	err := c.do(ctx, http.MethodGet, sessionPath(id)+"/messages"+pageQuery(cursor, limit), nil, http.StatusOK, &out)
	return out, err
}

func (c *Client) Todos(ctx context.Context, id string) (v1.TodoList, error) {
	var out v1.TodoList
	err := c.do(ctx, http.MethodGet, sessionPath(id)+"/todos", nil, http.StatusOK, &out)
	return out, err
}

func (c *Client) Goal(ctx context.Context, id string) (v1.Goal, error) {
	var out v1.Goal
	err := c.do(ctx, http.MethodGet, sessionPath(id)+"/goal", nil, http.StatusOK, &out)
	return out, err
}

func (c *Client) PutGoal(ctx context.Context, id string, request v1.PutGoalRequest) (v1.Goal, error) {
	var out v1.Goal
	err := c.do(ctx, http.MethodPut, sessionPath(id)+"/goal", request, http.StatusOK, &out)
	return out, err
}

func (c *Client) DeleteGoal(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, sessionPath(id)+"/goal", nil, http.StatusNoContent, nil)
}

// Prompt returns after durable admission (202), not model completion.
func (c *Client) Prompt(ctx context.Context, id string, request v1.PromptRequest) (v1.PromptAccepted, error) {
	var out v1.PromptAccepted
	err := c.do(ctx, http.MethodPost, sessionPath(id)+"/prompts", request, http.StatusAccepted, &out)
	return out, err
}

func (c *Client) Interrupt(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, sessionPath(id)+"/interrupt", nil, http.StatusNoContent, nil)
}

func (c *Client) Compact(ctx context.Context, id string) (v1.Compaction, error) {
	var out v1.Compaction
	err := c.do(ctx, http.MethodPost, sessionPath(id)+"/compact", nil, http.StatusAccepted, &out)
	return out, err
}

func (c *Client) Permissions(ctx context.Context, id string) (v1.PermissionList, error) {
	var out v1.PermissionList
	err := c.do(ctx, http.MethodGet, sessionPath(id)+"/permissions", nil, http.StatusOK, &out)
	return out, err
}

func (c *Client) ReplyPermission(ctx context.Context, sessionID, requestID string, reply v1.PermissionReply) error {
	path := sessionPath(sessionID) + "/permissions/" + url.PathEscape(requestID) + "/reply"
	return c.do(ctx, http.MethodPost, path, reply, http.StatusNoContent, nil)
}

func (c *Client) Questions(ctx context.Context, id string) (v1.QuestionList, error) {
	var out v1.QuestionList
	err := c.do(ctx, http.MethodGet, sessionPath(id)+"/questions", nil, http.StatusOK, &out)
	return out, err
}

func (c *Client) ReplyQuestion(ctx context.Context, sessionID, requestID string, reply v1.QuestionReply) error {
	path := sessionPath(sessionID) + "/questions/" + url.PathEscape(requestID) + "/reply"
	return c.do(ctx, http.MethodPost, path, reply, http.StatusNoContent, nil)
}

func (c *Client) Models(ctx context.Context) (v1.ModelList, error) {
	var out v1.ModelList
	err := c.do(ctx, http.MethodGet, "/api/v1/models", nil, http.StatusOK, &out)
	return out, err
}

func (c *Client) ModelInfo(ctx context.Context, provider string, model string) (v1.Model, error) {
	var out v1.Model
	path := "/api/v1/models/" + url.PathEscape(provider) + "/" + url.PathEscape(model)
	err := c.do(ctx, http.MethodGet, path, nil, http.StatusOK, &out)
	return out, err
}

func (c *Client) SubscriptionUsage(ctx context.Context) (v1.SubscriptionUsage, error) {
	var out v1.SubscriptionUsage
	err := c.do(ctx, http.MethodGet, "/api/v1/usage", nil, http.StatusOK, &out)
	return out, err
}

func (c *Client) Agents(ctx context.Context) (v1.AgentList, error) {
	var out v1.AgentList
	err := c.do(ctx, http.MethodGet, "/api/v1/agents", nil, http.StatusOK, &out)
	return out, err
}

func (c *Client) TurnCompletion(ctx context.Context, sessionID, messageID string) (v1.TurnCompletion, error) {
	var out v1.TurnCompletion
	err := c.do(ctx, http.MethodGet, "/api/v1/sessions/"+url.PathEscape(sessionID)+"/turn-completion?message_id="+url.QueryEscape(messageID), nil, http.StatusOK, &out)
	return out, err
}

func (c *Client) Modes(ctx context.Context) (v1.ModeList, error) {
	var out v1.ModeList
	err := c.do(ctx, http.MethodGet, "/api/v1/modes", nil, http.StatusOK, &out)
	return out, err
}

func (c *Client) Tools(ctx context.Context) (v1.ToolList, error) {
	var out v1.ToolList
	err := c.do(ctx, http.MethodGet, "/api/v1/tools", nil, http.StatusOK, &out)
	return out, err
}

func (c *Client) OpenAPI(ctx context.Context) (json.RawMessage, error) {
	var out json.RawMessage
	err := c.do(ctx, http.MethodGet, "/openapi.json", nil, http.StatusOK, &out)
	return out, err
}

func (c *Client) Events(ctx context.Context, sessionID string, after *int64) (*EventStream, error) {
	path := sessionPath(sessionID) + "/events"
	if after != nil {
		path += "?after=" + strconv.FormatInt(*after, 10)
	}
	request, err := c.request(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", v1.MediaTypeSSE)
	response, err := c.http.Do(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		defer response.Body.Close()
		return nil, c.responseError(response)
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || mediaType != v1.MediaTypeSSE {
		response.Body.Close()
		return nil, errors.New("client: event response is not text/event-stream")
	}
	max := c.MaxEventBytes
	if max <= 0 {
		max = 1 << 20
	}
	return &EventStream{body: response.Body, decoder: NewSSEDecoder(response.Body, max)}, nil
}

func (c *Client) do(ctx context.Context, method, path string, body any, expected int, target any) error {
	request, err := c.request(ctx, method, path, body)
	if err != nil {
		return err
	}
	response, err := c.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != expected {
		return c.responseError(response)
	}
	if target == nil {
		_, err := readBounded(response.Body, c.responseLimit())
		return err
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || mediaType != v1.MediaTypeJSON {
		return errors.New("client: response is not application/json")
	}
	data, err := readBounded(response.Body, c.responseLimit())
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("client: decode response: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errors.New("client: trailing response data")
	}
	return nil
}

func (c *Client) request(ctx context.Context, method, path string, body any) (*http.Request, error) {
	relative, err := url.Parse(path)
	if err != nil {
		return nil, err
	}
	endpoint := c.baseURL.ResolveReference(relative)
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(data)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint.String(), reader)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", v1.MediaTypeJSON)
	if body != nil {
		request.Header.Set("Content-Type", v1.MediaTypeJSON)
	}
	return request, nil
}

func (c *Client) responseError(response *http.Response) error {
	data, err := readBounded(response.Body, c.responseLimit())
	if err != nil {
		return err
	}
	mediaType, _, mediaErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if mediaErr != nil || mediaType != v1.MediaTypeProblem {
		return fmt.Errorf("client: unexpected HTTP status %d", response.StatusCode)
	}
	var item v1.Problem
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&item); err != nil {
		return errors.New("client: invalid problem response")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errors.New("client: trailing problem response data")
	}
	if item.Status != response.StatusCode || item.Code == "" {
		return errors.New("client: inconsistent problem response")
	}
	return &APIError{Problem: item}
}

func (c *Client) responseLimit() int64 {
	if c.MaxResponseBytes <= 0 {
		return defaultMaxResponseBytes
	}
	return c.MaxResponseBytes
}

func readBounded(reader io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("client: response exceeds configured limit")
	}
	return data, nil
}

func sessionPath(id string) string { return "/api/v1/sessions/" + url.PathEscape(id) }

func pageQuery(cursor string, limit int) string {
	values := make(url.Values)
	if cursor != "" {
		values.Set("cursor", cursor)
	}
	if limit > 0 {
		values.Set("limit", strconv.Itoa(limit))
	}
	if len(values) == 0 {
		return ""
	}
	return "?" + values.Encode()
}
