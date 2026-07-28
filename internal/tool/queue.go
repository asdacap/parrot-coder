package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/amirulashraf/parrot-coder/internal/queue"
)

type QueueService interface {
	Create(name, description string) (queue.Info, error)
	Push(name string, items []string, direction queue.Direction) (queue.Info, error)
	TakeContext(context.Context, string, int, queue.Direction) ([]string, queue.Info, error)
	TryTake(string, int, queue.Direction) ([]string, queue.Info, bool, error)
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
	Name         string          `json:"name"`
	Description  string          `json:"description,omitempty"`
	Items        []string        `json:"items,omitempty"`
	Count        *int            `json:"count,omitempty"`
	Direction    queue.Direction `json:"direction,omitempty"`
	Enabled      *bool           `json:"enabled,omitempty"`
	YieldAfterMS *int64          `json:"yield_after_ms,omitempty"`
}

const (
	defaultQueueTakeYieldAfter = 30 * time.Second
	queueTakePollInterval      = 10 * time.Millisecond
	maxQueueTakeYieldAfterMS   = int64(^uint64(0)>>1) / int64(time.Millisecond)
)

func (t *QueueTool) ID() string { return t.Kind }

func (t *QueueTool) Presentation() Presentation {
	if t.Kind == "queue_push" || t.Kind == "queue_take" {
		return Presentation{Label: LabelSpec{Fields: []LabelField{{Names: []string{"name"}}}}}
	}
	return t.BasePresentation.Presentation()
}

func (t *QueueTool) Descriptor() Descriptor {
	descriptor := Descriptor{ID: t.ID(), Description: t.Description(), Schema: t.JSONSchema(), Presentation: t.Presentation()}
	if t.Kind == "queue_create" {
		descriptor.SystemPromptGuidance = "When a task have many work item, use queue and multiple worker subagent. Publisher and consumer can be spawned at the same time."
	}
	return descriptor
}

func (t *QueueTool) Description() string {
	switch t.Kind {
	case "queue_create":
		return "Explicitly create a persistent queue shared by agents in the current user session. Queue names may contain lowercase ASCII letters, numbers, and hyphens."
	case "queue_push":
		return "Push strings onto an existing shared user-session queue. Direction defaults to back."
	case "queue_take":
		return "Remove and return strings from an existing shared user-session queue. If the queue is empty, wait until an item is available or yield_after_ms elapses. Count defaults to one, direction defaults to front, and yield_after_ms defaults to 30000. The queue does not need to fill count before returning."
	case "queue_listen":
		return "Enable or disable idle notification delivery from an existing shared user-session queue. Listening remains enabled after each FIFO delivery."
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
	properties := `"name":{"type":"string","pattern":"^[a-z0-9]+(-[a-z0-9]+)*$","description":"Lowercase ASCII letters and numbers, optionally separated by hyphens."}`
	required := `"name"`
	switch t.Kind {
	case "queue_create":
		properties += `,"description":{"type":"string","description":"Optional queue description."}`
	case "queue_push":
		properties += `,"items":{"type":"array","items":{"type":"string"},"description":"String items to enqueue."},"direction":{"type":"string","enum":["front","back"],"description":"Queue end; defaults to back."}`
		required += `,"items"`
	case "queue_take":
		properties += `,"count":{"type":"integer","minimum":1,"description":"Maximum number of items to take; defaults to one."},"direction":{"type":"string","enum":["front","back"],"description":"Queue end; defaults to front."},"yield_after_ms":{"type":"integer","minimum":0,"description":"Wait this many milliseconds for at least one item before yielding; defaults to 30000."}`
	case "queue_listen":
		properties += `,"enabled":{"type":"boolean","description":"Whether to listen to this queue; defaults to true."}`
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
	if input.YieldAfterMS != nil && (*input.YieldAfterMS < 0 || *input.YieldAfterMS > maxQueueTakeYieldAfterMS) {
		return Plan{}, fmt.Errorf("%s: yield_after_ms must be between 0 and %d", t.ID(), maxQueueTakeYieldAfterMS)
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
	var items []string
	var err error
	switch t.Kind {
	case "queue_create":
		info, err = t.Store.Create(input.Name, input.Description)
	case "queue_push":
		info, err = t.Store.Push(input.Name, input.Items, input.Direction)
	case "queue_take":
		count := 1
		if input.Count != nil {
			count = *input.Count
		}
		yieldAfter := defaultQueueTakeYieldAfter
		if input.YieldAfterMS != nil {
			yieldAfter = time.Duration(*input.YieldAfterMS) * time.Millisecond
		}
		items, info, err = t.take(ctx, input.Name, count, input.Direction, yieldAfter)
	case "queue_info":
		info, err = t.Store.Get(input.Name)
	case "queue_listen":
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
		Items *[]string `json:"items,omitempty"`
	}{Info: info}
	if t.Kind == "queue_take" {
		if items == nil {
			items = []string{}
		}
		output.Items = &items
	}
	data, err := json.Marshal(output)
	if err != nil {
		return Result{}, err
	}
	text := string(data)
	return Result{Text: text, ModelText: text, Metadata: map[string]any{"queue": info.Name, "size": info.Size}}, nil
}

func (t *QueueTool) take(ctx context.Context, name string, count int, direction queue.Direction, yieldAfter time.Duration) ([]string, queue.Info, error) {
	waitCtx, cancel := context.WithTimeout(ctx, yieldAfter)
	defer cancel()
	lastInfo := queue.Info{Name: name}
	for {
		items, info, err := t.Store.TakeContext(waitCtx, name, count, direction)
		if err == nil || (!errors.Is(err, queue.ErrEmpty) && !errors.Is(err, context.DeadlineExceeded)) {
			return items, info, err
		}
		if errors.Is(err, queue.ErrEmpty) {
			lastInfo = info
		}
		if waitCtx.Err() != nil {
			return t.finalTake(ctx, name, count, direction, lastInfo)
		}
		timer := time.NewTimer(queueTakePollInterval)
		select {
		case <-waitCtx.Done():
			timer.Stop()
			return t.finalTake(ctx, name, count, direction, lastInfo)
		case <-timer.C:
		}
	}
}

func (t *QueueTool) finalTake(ctx context.Context, name string, count int, direction queue.Direction, lastInfo queue.Info) ([]string, queue.Info, error) {
	if err := ctx.Err(); err != nil {
		return nil, queue.Info{}, err
	}
	items, info, acquired, err := t.Store.TryTake(name, count, direction)
	if err == nil && acquired {
		return items, info, nil
	}
	if err != nil && !errors.Is(err, queue.ErrEmpty) {
		return nil, info, err
	}
	if errors.Is(err, queue.ErrEmpty) {
		lastInfo = info
	}
	return nil, lastInfo, nil
}
