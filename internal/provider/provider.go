// Package provider connects provider-neutral requests to model HTTP APIs.
package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/amirulashraf/parrot-coder/internal/protocol"
)

// Stream is a provider response consumed one canonical event at a time.
type Stream interface {
	Next(context.Context) (protocol.Event, error)
	Close() error
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

// Capabilities describes optional model behavior callers may rely on.
type Capabilities struct {
	Tools     bool
	Reasoning bool
	Output    []string
	Variants  map[string]Variant
}

// Variant contains the request options selected by a named model variant.
type Variant struct {
	ReasoningEffort string
}

// Model is provider model metadata used for selection and request planning.
type Model struct {
	ID              string
	Name            string
	ContextWindow   int
	MaxOutputTokens int
	Capabilities    Capabilities
}

func cloneModels(models []Model) []Model {
	result := make([]Model, len(models))
	copy(result, models)
	for i := range result {
		result[i].Capabilities.Output = append([]string(nil), result[i].Capabilities.Output...)
		if variants := result[i].Capabilities.Variants; variants != nil {
			result[i].Capabilities.Variants = make(map[string]Variant, len(variants))
			for name, variant := range variants {
				result[i].Capabilities.Variants[name] = variant
			}
		}
	}
	return result
}
