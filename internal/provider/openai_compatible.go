package provider

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/amirulashraf/parrot-coder/internal/auth"
	"github.com/amirulashraf/parrot-coder/internal/protocol"
	"github.com/amirulashraf/parrot-coder/internal/protocol/chatcompletions"
	"github.com/amirulashraf/parrot-coder/internal/protocol/responses"
)

// CompatibleProtocol selects an OpenAI-compatible wire format.
type CompatibleProtocol string

const (
	ProtocolResponses       CompatibleProtocol = "responses"
	ProtocolChatCompletions CompatibleProtocol = "chat-completions"
)

// OpenAICompatibleOptions configures an API-key authenticated compatible API.
type OpenAICompatibleOptions struct {
	ID                     string
	BaseURL                string
	Protocol               CompatibleProtocol
	APIKey                 auth.Secret
	Headers                map[string]string
	AllowInsecureLocalhost bool
	Models                 []Model
	HTTPClient             *http.Client
	HeaderTimeout          time.Duration
}

// OpenAICompatible is an explicitly configured compatible provider.
type OpenAICompatible struct {
	id            string
	endpoint      *url.URL
	protocol      CompatibleProtocol
	apiKey        auth.Secret
	headers       http.Header
	models        []Model
	client        *http.Client
	headerTimeout time.Duration
}

// NewOpenAICompatible validates options before retaining any credentials.
func NewOpenAICompatible(options OpenAICompatibleOptions) (*OpenAICompatible, error) {
	if strings.TrimSpace(options.ID) == "" {
		return nil, errors.New("provider: compatible provider ID is required")
	}
	if options.APIKey.Value() == "" {
		return nil, errors.New("provider: compatible provider API key is required")
	}
	if options.HeaderTimeout < 0 {
		return nil, errors.New("provider: header timeout cannot be negative")
	}
	endpointName := ""
	switch options.Protocol {
	case ProtocolResponses:
		endpointName = "responses"
	case ProtocolChatCompletions:
		endpointName = "chat/completions"
	default:
		return nil, errors.New("provider: protocol must be responses or chat-completions")
	}
	endpoint, err := endpointURL(options.BaseURL, endpointName, options.AllowInsecureLocalhost)
	if err != nil {
		return nil, err
	}
	headers, err := validatedHeaders(options.Headers)
	if err != nil {
		return nil, err
	}
	return &OpenAICompatible{
		id: options.ID, endpoint: endpoint, protocol: options.Protocol, apiKey: options.APIKey,
		headers: headers, models: cloneModels(options.Models), client: secureClient(options.HTTPClient, endpoint),
		headerTimeout: options.HeaderTimeout,
	}, nil
}

func (p *OpenAICompatible) ID() string      { return p.id }
func (p *OpenAICompatible) Models() []Model { return cloneModels(p.models) }

func (p *OpenAICompatible) Stream(ctx context.Context, request protocol.Request) (Stream, error) {
	var body []byte
	var err error
	var parser streamParser
	switch p.protocol {
	case ProtocolResponses:
		body, err = responses.EncodeRequest(request)
		parser = func(reader io.Reader, limit int) Stream { return responses.NewParser(reader, limit) }
	case ProtocolChatCompletions:
		body, err = chatcompletions.EncodeRequest(request)
		parser = func(reader io.Reader, limit int) Stream { return chatcompletions.NewParser(reader, limit) }
	}
	if err != nil {
		return nil, err
	}
	headers := p.headers.Clone()
	headers.Set("Authorization", "Bearer "+p.apiKey.Value())
	return startStream(ctx, p.client, p.endpoint, body, headers, []string{p.apiKey.Value()}, p.headerTimeout, parser)
}
