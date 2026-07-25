package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/amirulashraf/parrot-coder/internal/queue"
)

type QueueService interface {
	Create(sessionID, name, description string) (queue.Info, error)
	Push(sessionID, name, item string, direction queue.Direction) (queue.Info, error)
	Take(sessionID, name string, direction queue.Direction) (string, queue.Info, error)
	Get(sessionID, name string) (queue.Info, error)
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
}

func (t *QueueTool) ID() string { return t.Kind }

func (t *QueueTool) Descriptor() Descriptor {
	return Descriptor{ID: t.ID(), Description: t.Description(), Schema: t.JSONSchema(), Presentation: t.Presentation()}
}

func (t *QueueTool) Description() string {
	switch t.Kind {
	case "queue_create":
		return "Explicitly create a session-scoped persistent queue. Queue names must be exactly three lowercase ASCII alphanumeric words separated by hyphens."
	case "queue_push":
		return "Push a string onto an existing session queue. Direction defaults to back."
	case "queue_take":
		return "Remove and return a string from an existing session queue. Direction defaults to front."
	default:
		return "Get metadata and the current size of an existing session queue."
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
	}
	return json.RawMessage(`{"type":"object","properties":{` + properties + `},"required":[` + required + `],"additionalProperties":false}`)
}

func (t *QueueTool) Plan(_ context.Context, raw json.RawMessage, _ CallContext) (Plan, error) {
	var input queueInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return Plan{}, err
	}
	return NewPlan(t.ID(), raw, nil, nil, input)
}

func (t *QueueTool) Execute(_ context.Context, plan Plan, call CallContext) (Result, error) {
	if t.Store == nil || call.SessionID == "" {
		return Result{}, errors.New(t.Kind + ": store and session are required")
	}
	input := plan.Data.(queueInput)
	var info queue.Info
	var item string
	var err error
	switch t.Kind {
	case "queue_create":
		info, err = t.Store.Create(call.SessionID, input.Name, input.Description)
	case "queue_push":
		info, err = t.Store.Push(call.SessionID, input.Name, input.Item, input.Direction)
	case "queue_take":
		item, info, err = t.Store.Take(call.SessionID, input.Name, input.Direction)
	case "queue_info":
		info, err = t.Store.Get(call.SessionID, input.Name)
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
