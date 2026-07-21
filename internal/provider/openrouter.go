package provider

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
)

// DecodeOpenRouterModels parses an OpenRouter model list. OpenRouter extends the
// standard OpenAI /v1/models format with a pricing object (prompt, completion)
// and a reasoning object (supported_efforts, default_effort) and a top_provider
// object (max_completion_tokens), which a plain model list cannot express.
// Reasoning efforts become variants so /effort works without configuration.
func DecodeOpenRouterModels(data []byte) ([]Model, error) {
	var wire struct {
		Data []struct {
			ID            string `json:"id"`
			Name          string `json:"name"`
			ContextLength int    `json:"context_length"`
			Pricing       *struct {
				Prompt     string `json:"prompt"`
				Completion string `json:"completion"`
			} `json:"pricing,omitempty"`
			TopProvider *struct {
				MaxCompletionTokens int `json:"max_completion_tokens,omitempty"`
			} `json:"top_provider,omitempty"`
			Reasoning *struct {
				SupportedEfforts []string `json:"supported_efforts,omitempty"`
				DefaultEffort    string   `json:"default_effort,omitempty"`
			} `json:"reasoning,omitempty"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return nil, fmt.Errorf("provider: decode models response: %w", err)
	}
	seen := make(map[string]struct{}, len(wire.Data))
	models := make([]Model, 0, len(wire.Data))
	for _, item := range wire.Data {
		if item.ID == "" {
			continue
		}
		if _, duplicate := seen[item.ID]; duplicate {
			continue
		}
		seen[item.ID] = struct{}{}
		name := item.Name
		if name == "" {
			name = item.ID
		}
		maxTokens := 0
		if item.TopProvider != nil {
			maxTokens = item.TopProvider.MaxCompletionTokens
		}
		var variants []Variant
		if item.Reasoning != nil {
			variants = effortVariants(item.Reasoning.SupportedEfforts, item.Reasoning.DefaultEffort)
		}
		inputPrice, outputPrice := 0.0, 0.0
		if item.Pricing != nil {
			inputPrice, _ = strconv.ParseFloat(item.Pricing.Prompt, 64)
			outputPrice, _ = strconv.ParseFloat(item.Pricing.Completion, 64)
		}
		models = append(models, Model{
			ID: item.ID, Name: name,
			ContextWindow:   item.ContextLength,
			MaxOutputTokens: maxTokens,
			InputPrice:      inputPrice,
			OutputPrice:     outputPrice,
			Capabilities: Capabilities{
				Tools: true, Output: []string{"text"}, Variants: variants,
			},
		})
	}
	if len(models) == 0 {
		return nil, errors.New("provider: models response contains no usable models")
	}
	return models, nil
}
