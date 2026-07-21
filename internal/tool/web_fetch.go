package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/amirulashraf/parrot-coder/internal/permission"
	"github.com/amirulashraf/parrot-coder/internal/webfetch"
	"net/http"
	"net/url"
	"strings"
)

type WebFetchTool struct {
	BasePresentation
	Service *webfetch.Service
}

func NewWebFetchTool(service *webfetch.Service) *WebFetchTool { return &WebFetchTool{Service: service} }
func (*WebFetchTool) ID() string                              { return "web_fetch" }
func (*WebFetchTool) Presentation() Presentation {
	return Presentation{
		Muted: true,
		Label: LabelSpec{Fields: []LabelField{{Names: []string{"url"}}}},
	}
}

func (*WebFetchTool) Description() string {
	return "Fetch bounded HTTP or HTTPS text with GET or HEAD after exact network permission review."
}
func (*WebFetchTool) DescribeRequest(raw json.RawMessage) (string, error) {
	var input webFetchInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return "", err
	}
	normalized, err := normalizeFetch(input)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Fetch %s with %s", normalized.URL, normalized.Method), nil
}
func (*WebFetchTool) JSONSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"url":{"type":"string"},"method":{"type":"string"}},"required":["url"],"additionalProperties":false}`)
}

type webFetchInput struct {
	URL    string `json:"url"`
	Method string `json:"method"`
}

func (t *WebFetchTool) Plan(_ context.Context, raw json.RawMessage, _ CallContext) (Plan, error) {
	if t.Service == nil {
		return Plan{}, errors.New("web_fetch: service is required")
	}
	var input webFetchInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return Plan{}, err
	}
	normalized, err := normalizeFetch(input)
	if err != nil {
		return Plan{}, err
	}
	review, _ := json.Marshal(webfetch.PermissionReview{URL: normalized.URL, Method: normalized.Method})
	request, err := permission.NewRequest(t.ID(), raw, []permission.Resource{{Kind: "network", Identifier: normalized.URL, Operation: normalized.Method}}, review)
	if err != nil {
		return Plan{}, err
	}
	return NewPlan(t.ID(), raw, []permission.Request{request}, review, webFetchPlan{Input: input, Normalized: normalized})
}
func (t *WebFetchTool) Execute(ctx context.Context, plan Plan, call CallContext) (Result, error) {
	planned, ok := plan.Data.(webFetchPlan)
	if !ok {
		return Result{}, errors.New("web_fetch: incompatible plan")
	}
	revalidated, err := normalizeFetch(planned.Input)
	if err != nil || revalidated != planned.Normalized {
		return Result{}, errors.New("web_fetch: request changed after planning")
	}
	response, err := t.Service.Fetch(ctx, webfetch.Request{URL: planned.Normalized.URL, Method: planned.Normalized.Method})
	if err != nil {
		return Result{}, err
	}
	return Result{Text: response.Text, ModelText: modelText(response.Text), Metadata: map[string]any{"final_url": response.FinalURL, "status": response.Status, "content_type": response.ContentType, "truncated": response.Truncated}}, nil
}

type webFetchPlan struct {
	Input      webFetchInput
	Normalized webFetchInput
}

func normalizeFetch(input webFetchInput) (webFetchInput, error) {
	method := strings.ToUpper(strings.TrimSpace(input.Method))
	if method == "" {
		method = http.MethodGet
	}
	if method != http.MethodGet && method != http.MethodHead {
		return webFetchInput{}, errors.New("web_fetch: only GET and HEAD are supported")
	}
	raw := strings.TrimSpace(input.URL)
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Hostname() == "" || parsed.User != nil || parsed.Opaque != "" || parsed.Scheme != "http" && parsed.Scheme != "https" {
		return webFetchInput{}, errors.New("web_fetch: URL must be absolute HTTP or HTTPS without user information")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	parsed.Host = strings.ToLower(parsed.Host)
	parsed.Fragment = ""
	return webFetchInput{URL: parsed.String(), Method: method}, nil
}
