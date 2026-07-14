package compaction

import (
	"context"
	"errors"
	"io"
	"strings"

	"github.com/amirulashraf/parrot-coder/internal/protocol"
	"github.com/amirulashraf/parrot-coder/internal/provider"
)

type ProviderResolver interface {
	Resolve(string, string) (provider.Provider, provider.Model, error)
}

type ProviderSummarizer struct {
	Providers ProviderResolver
	MaxEvents int
	MaxBytes  int
}

func (s ProviderSummarizer) Summarize(ctx context.Context, request SummaryRequest) (SummaryResult, error) {
	if s.Providers == nil {
		return SummaryResult{}, errors.New("compaction: provider resolver is unavailable")
	}
	client, model, err := s.Providers.Resolve(request.ProviderID, request.ModelID)
	if err != nil {
		return SummaryResult{}, err
	}
	maxEvents, maxBytes := s.MaxEvents, s.MaxBytes
	if maxEvents <= 0 {
		maxEvents = 10000
	}
	if maxBytes <= 0 {
		maxBytes = 1 << 20
	}
	stream, err := provider.StreamWithHeaderRetry(ctx, client, protocol.Request{Model: model.ID, Instructions: request.Prompt, Messages: request.Messages, Tools: nil})
	if err != nil {
		return SummaryResult{}, err
	}
	defer stream.Close()
	var text strings.Builder
	var usage protocol.Usage
	for count := 0; ; count++ {
		item, err := stream.Next(ctx)
		if errors.Is(err, io.EOF) {
			return SummaryResult{Summary: text.String(), Usage: usage}, nil
		}
		if err != nil {
			return SummaryResult{}, err
		}
		if count >= maxEvents {
			return SummaryResult{}, errors.New("compaction: summary stream event limit exceeded")
		}
		switch item.Type {
		case protocol.EventTextDelta:
			if text.Len()+len(item.Text) > maxBytes {
				return SummaryResult{}, errors.New("compaction: summary stream exceeds configured bound")
			}
			text.WriteString(item.Text)
		case protocol.EventUsage:
			if item.Usage != nil {
				usage = *item.Usage
			}
		case protocol.EventProviderError:
			if item.ProviderError != nil && item.ProviderError.Message != "" {
				return SummaryResult{}, errors.New(item.ProviderError.Message)
			}
			return SummaryResult{}, errors.New("compaction: provider error")
		}
	}
}
