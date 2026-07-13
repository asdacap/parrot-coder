package provider

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/url"

	"github.com/amirulashraf/parrot-coder/internal/auth"
	"github.com/amirulashraf/parrot-coder/internal/protocol"
	"github.com/amirulashraf/parrot-coder/internal/protocol/responses"
)

const chatGPTEndpoint = "https://chatgpt.com/backend-api/codex/responses"

// OAuthTokenSource is implemented by auth.TokenSource.
type OAuthTokenSource interface {
	Token(context.Context) (auth.OAuthCredential, error)
}

// ChatGPTOptions configures the fixed ChatGPT subscription provider.
type ChatGPTOptions struct {
	TokenSource OAuthTokenSource
	HTTPClient  *http.Client
}

// ChatGPT uses subscription OAuth credentials only with the compiled endpoint.
type ChatGPT struct {
	tokens    OAuthTokenSource
	client    *http.Client
	endpoint  *url.URL
	sessionID string
}

// NewChatGPT creates a subscription provider. Its endpoint and extra headers
// are deliberately not configurable.
func NewChatGPT(options ChatGPTOptions) (*ChatGPT, error) {
	if options.TokenSource == nil {
		return nil, errors.New("provider: ChatGPT token source is required")
	}
	endpoint, _ := url.Parse(chatGPTEndpoint)
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return nil, errors.New("provider: create ChatGPT session ID")
	}
	return &ChatGPT{
		tokens: options.TokenSource, endpoint: endpoint, sessionID: hex.EncodeToString(value[:]),
		client: secureClient(options.HTTPClient, endpoint),
	}, nil
}

func (p *ChatGPT) ID() string { return "chatgpt" }

func (p *ChatGPT) Models() []Model { return cloneModels(chatGPTModels) }

func (p *ChatGPT) Stream(ctx context.Context, request protocol.Request) (Stream, error) {
	credential, err := p.tokens.Token(ctx)
	if err != nil {
		return nil, err
	}
	if credential.AccessToken.Value() == "" {
		return nil, errors.New("provider: ChatGPT credential requires an access token")
	}
	body, err := responses.EncodeRequest(request)
	if err != nil {
		return nil, err
	}
	headers := make(http.Header)
	headers.Set("Authorization", "Bearer "+credential.AccessToken.Value())
	if credential.AccountID != "" {
		headers.Set("ChatGPT-Account-Id", credential.AccountID)
	}
	headers.Set("originator", "parrot")
	headers.Set("User-Agent", "parrot")
	headers.Set("session-id", p.sessionID)
	parser := func(reader io.Reader, limit int) Stream { return responses.NewParser(reader, limit) }
	return startStream(ctx, p.client, p.endpoint, body, headers, []string{credential.AccessToken.Value()}, parser)
}

var chatGPTModels = []Model{
	{
		ID: "gpt-5.4", Name: "GPT-5.4", ContextWindow: 400000, MaxOutputTokens: 128000,
		Capabilities: Capabilities{Tools: true, Reasoning: true, Output: []string{"text"}},
	},
	{
		ID: "gpt-5.4-mini", Name: "GPT-5.4 Mini", ContextWindow: 400000, MaxOutputTokens: 128000,
		Capabilities: Capabilities{Tools: true, Reasoning: true, Output: []string{"text"}},
	},
	{
		ID: "gpt-5.5", Name: "GPT-5.5", ContextWindow: 400000, MaxOutputTokens: 128000,
		Capabilities: Capabilities{Tools: true, Reasoning: true, Output: []string{"text"}},
	},
	{
		ID: "gpt-5.6-sol", Name: "GPT-5.6 Sol", ContextWindow: 500000, MaxOutputTokens: 128000,
		Capabilities: Capabilities{Tools: true, Reasoning: true, Output: []string{"text"}},
	},
}
