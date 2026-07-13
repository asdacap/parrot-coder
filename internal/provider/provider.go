// Package provider connects provider-neutral requests to model HTTP APIs.
package provider

import (
	"context"

	"github.com/amirulashraf/parrot-coder/internal/protocol"
)

// Stream is a provider response consumed one canonical event at a time.
type Stream interface {
	Next(context.Context) (protocol.Event, error)
	Close() error
}

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
	}
	return result
}
