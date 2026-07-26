package agent

import (
	"context"
	"errors"
	"strings"

	managedtask "github.com/amirulashraf/parrot-coder/internal/task"
)

type managedTurnPolicy struct {
	maxPromptBytes int
	maxResultBytes int
	tryAcquire     func() (func(), bool, error)
	acquire        func(context.Context) (func(), error)
}

func (p managedTurnPolicy) Validate(content string) error {
	if strings.TrimSpace(content) == "" {
		return ErrInvalidChildRequest
	}
	if len(content) > p.maxPromptBytes {
		return ErrChildRequestLimit
	}
	return nil
}

func applyChildDefaults(config *UserSessionConfig) {
	if config.ObserveTurnProgress == nil {
		config.ObserveTurnProgress = func(string, func(ChildProgress)) func() { return func() {} }
	}
	if config.OnTurnProgress == nil {
		config.OnTurnProgress = func(Status) {}
	}
	if config.OnTurnComplete == nil {
		config.OnTurnComplete = func(Status) {}
	}
	if config.OnTurnLifecycle == nil {
		config.OnTurnLifecycle = func(TurnLifecycleEvent) {}
	}
	if config.MaxChildDepth <= 0 {
		config.MaxChildDepth = 4
	}
	if config.MaxChildTasks <= 0 {
		config.MaxChildTasks = 1024
	}
	if config.MaxChildPromptBytes <= 0 {
		config.MaxChildPromptBytes = 1 << 20
	}
	if config.MaxChildResultBytes <= 0 {
		config.MaxChildResultBytes = 1 << 20
	}
}

func (p managedTurnPolicy) TryAcquire() (func(), bool, error)           { return p.tryAcquire() }
func (p managedTurnPolicy) Acquire(ctx context.Context) (func(), error) { return p.acquire(ctx) }
func (p managedTurnPolicy) CapturesOutput() bool                        { return true }
func (p managedTurnPolicy) Finished(_ Status, output string, runErr error, canceled bool) (string, error, bool) {
	if canceled && errors.Is(runErr, context.Canceled) {
		runErr = ErrChildCanceled
	}
	output, outputTruncated := truncateChild(output, p.maxResultBytes)
	if runErr == nil {
		return output, nil, outputTruncated
	}
	errText, errorTruncated := truncateChild(runErr.Error(), p.maxResultBytes)
	if errorTruncated {
		runErr = errors.New(errText)
	}
	return output, runErr, outputTruncated || errorTruncated
}

type turnObserver struct{ turn *turnState }

func (o turnObserver) Wait(ctx context.Context) (Status, error) {
	if o.turn == nil {
		return Status{}, ErrChildNotFound
	}
	select {
	case <-o.turn.done:
		return cloneStatus(o.turn.result), nil
	case <-ctx.Done():
		return Status{}, ctx.Err()
	}
}

type managedChildTask struct{ child *agentSession }
type managedChildTurn struct {
	child *agentSession
	turn  *turnState
}

func (t managedChildTask) Snapshot() managedtask.Snapshot {
	return childSnapshot(t.child.statusSnapshot())
}
func (t managedChildTask) Wait(ctx context.Context) (managedtask.Completion, error) {
	return t.Observe().Wait(ctx)
}
func (t managedChildTask) Observe() managedtask.Task {
	t.child.mu.Lock()
	turn := t.child.turn
	t.child.mu.Unlock()
	return managedChildTurn{child: t.child, turn: turn}
}
func (t managedChildTask) Interrupt(ctx context.Context) (managedtask.Snapshot, error) {
	err := t.child.Interrupt(ctx)
	return childSnapshot(t.child.Status()), err
}
func (t managedChildTurn) Snapshot() managedtask.Snapshot {
	select {
	case <-t.turn.done:
		return childSnapshot(t.turn.result)
	default:
		return childSnapshot(t.child.statusSnapshot())
	}
}
func (t managedChildTurn) Wait(ctx context.Context) (managedtask.Completion, error) {
	item, err := turnObserver{turn: t.turn}.Wait(ctx)
	return managedtask.Completion{Task: childSnapshot(item), Output: item.Output, Error: item.Error}, err
}
func (t managedChildTurn) Interrupt(ctx context.Context) (managedtask.Snapshot, error) {
	t.child.mu.Lock()
	current := t.child.turn == t.turn
	t.child.mu.Unlock()
	if !current {
		return t.Snapshot(), nil
	}
	return managedChildTask{t.child}.Interrupt(ctx)
}

func childSnapshot(item Status) managedtask.Snapshot {
	return managedtask.Snapshot{ID: item.SessionID, Name: item.Name, SessionID: item.SessionID, Kind: managedtask.KindAgent, Status: string(item.State), StartedAt: item.StartedAt, Agent: item.Agent, Turn: item.Turn, Depth: item.Depth}
}
func cloneStatus(task Status) Status {
	task.Lineage = append([]string(nil), task.Lineage...)
	return task
}
func truncateChild(value string, limit int) (string, bool) {
	if len(value) <= limit {
		return value, false
	}
	return value[:limit], true
}
func validChildAgent(agent string) bool {
	if agent == "" || len(agent) > 128 {
		return false
	}
	for _, char := range agent {
		if char > 127 || !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '-' || char == '_') {
			return false
		}
	}
	return true
}
