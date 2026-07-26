package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/amirulashraf/parrot-coder/internal/queue"
)

type QueueService interface {
	Create(name, description string) (queue.Info, error)
	Push(name, item string, direction queue.Direction) (queue.Info, error)
	Take(name string, direction queue.Direction) (string, queue.Info, error)
	Get(name string) (queue.Info, error)
	List() ([]queue.Info, error)
	Monitor(name string, enabled bool) (queue.Info, error)
}

type QueueTool struct {
	BasePresentation
	Kind  string
	Store QueueService
}

type queueInput struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Item        string          `json:"item,omitempty"`
	Direction   queue.Direction `json:"direction,omitempty"`
	Enabled     *bool           `json:"enabled,omitempty"`
}

func (t *QueueTool) ID() string { return t.Kind }

func (t *QueueTool) Descriptor() Descriptor {
	return Descriptor{ID: t.ID(), Description: t.Description(), Schema: t.JSONSchema(), Presentation: t.Presentation()}
}

func (t *QueueTool) Description() string {
	switch t.Kind {
	case "queue_create":
		return "Explicitly create a persistent queue shared by agents in the current user session. Queue names must be exactly three lowercase ASCII alphanumeric words separated by hyphens."
	case "queue_push":
		return "Push a string onto an existing shared user-session queue. Direction defaults to back."
	case "queue_take":
		return "Remove and return a string from an existing shared user-session queue. Direction defaults to front."
	case "queue_monitor":
		return "Enable or disable idle notification delivery from an existing shared user-session queue. Monitoring remains enabled after each FIFO delivery."
	default:
		return "Get metadata and the current size of an existing shared user-session queue."
	}
}

func (t *QueueTool) DescribeRequest(raw json.RawMessage) (string, error) {
	var input queueInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s %s", t.Kind, input.Name), nil
}

func (t *QueueTool) JSONSchema() json.RawMessage {
	properties := `"name":{"type":"string","pattern":"^[a-z0-9]+-[a-z0-9]+-[a-z0-9]+$","description":"Exactly three lowercase ASCII alphanumeric words separated by hyphens."}`
	required := `"name"`
	switch t.Kind {
	case "queue_create":
		properties += `,"description":{"type":"string","description":"Optional queue description."}`
	case "queue_push":
		properties += `,"item":{"type":"string","description":"String item to enqueue."},"direction":{"type":"string","enum":["front","back"],"description":"Queue end; defaults to back."}`
		required += `,"item"`
	case "queue_take":
		properties += `,"direction":{"type":"string","enum":["front","back"],"description":"Queue end; defaults to front."}`
	case "queue_monitor":
		properties += `,"enabled":{"type":"boolean","description":"Whether to monitor this queue; defaults to true."}`
	}
	return json.RawMessage(`{"type":"object","properties":{` + properties + `},"required":[` + required + `],"additionalProperties":false}`)
}

func (t *QueueTool) Plan(ctx context.Context, raw json.RawMessage, _ CallContext) (Plan, error) {
	if err := ctx.Err(); err != nil {
		return Plan{}, err
	}
	var input queueInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return Plan{}, err
	}
	return NewPlan(t.ID(), raw, nil, nil, input)
}

func (t *QueueTool) Execute(ctx context.Context, plan Plan, _ CallContext) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if t.Store == nil {
		return Result{}, errors.New(t.Kind + ": queue manager is required")
	}
	input := plan.Data.(queueInput)
	var info queue.Info
	var item string
	var err error
	switch t.Kind {
	case "queue_create":
		info, err = t.Store.Create(input.Name, input.Description)
	case "queue_push":
		info, err = t.Store.Push(input.Name, input.Item, input.Direction)
	case "queue_take":
		item, info, err = t.Store.Take(input.Name, input.Direction)
	case "queue_info":
		info, err = t.Store.Get(input.Name)
	case "queue_monitor":
		enabled := true
		if input.Enabled != nil {
			enabled = *input.Enabled
		}
		info, err = t.Store.Monitor(input.Name, enabled)
	default:
		return Result{}, fmt.Errorf("unknown queue tool %q", t.Kind)
	}
	if err != nil {
		return Result{}, err
	}
	output := struct {
		queue.Info
		Item *string `json:"item,omitempty"`
	}{Info: info}
	if t.Kind == "queue_take" {
		output.Item = &item
	}
	data, err := json.Marshal(output)
	if err != nil {
		return Result{}, err
	}
	text := string(data)
	return Result{Text: text, ModelText: text, Metadata: map[string]any{"queue": info.Name, "size": info.Size}}, nil
}
