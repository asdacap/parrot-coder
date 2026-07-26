package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/amirulashraf/parrot-coder/internal/diagnostics"
	"github.com/amirulashraf/parrot-coder/internal/id"
	"github.com/amirulashraf/parrot-coder/internal/session"
)

// NotificationSession receives a completed child-agent turn.
type NotificationSession interface {
	Send(context.Context, string, string) (session.Admission, error)
}

type activeNotification struct {
	parent string
	cancel context.CancelFunc
	done   chan struct{}
}

type notificationKey struct {
	sessionID string
	turn      int
}

// CompletionNotifier delivers completed child-agent turns to their parents.
type CompletionNotifier struct {
	mu     sync.Mutex
	ctx    context.Context
	cancel context.CancelFunc
	lookup func(string) (NotificationSession, bool)
	active map[notificationKey]*activeNotification
	paused map[string]int
	closed bool
	wg     sync.WaitGroup
}

func NewCompletionNotifier() *CompletionNotifier {
	ctx, cancel := context.WithCancel(context.Background())
	return &CompletionNotifier{
		ctx: ctx, cancel: cancel,
		active: make(map[notificationKey]*activeNotification),
		paused: make(map[string]int),
	}
}

func (n *CompletionNotifier) SetLookup(lookup func(string) (NotificationSession, bool)) {
	n.mu.Lock()
	n.lookup = lookup
	n.mu.Unlock()
}

// Notify asynchronously delivers one terminal turn to its direct parent.
func (n *CompletionNotifier) Notify(task Status) {
	if n == nil || task.ParentSession == "" {
		return
	}
	key := notificationKey{sessionID: task.SessionID, turn: task.Turn}
	n.mu.Lock()
	if n.closed || n.lookup == nil || n.paused[task.ParentSession] > 0 || n.active[key] != nil {
		n.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(n.ctx)
	active := &activeNotification{parent: task.ParentSession, cancel: cancel, done: make(chan struct{})}
	n.active[key] = active
	n.wg.Add(1)
	lookup := n.lookup
	n.mu.Unlock()

	go func() {
		defer func() {
			cancel()
			n.mu.Lock()
			if n.active[key] == active {
				delete(n.active, key)
			}
			close(active.done)
			n.mu.Unlock()
			n.wg.Done()
		}()
		session, ok := lookup(task.ParentSession)
		if !ok || session == nil {
			diagnostics.Warn("agent_task_notification_unavailable", "session_id", task.ParentSession, "task_id", task.SessionID)
			return
		}
		sendCtx, sendCancel := context.WithTimeout(ctx, 5*time.Second)
		defer sendCancel()
		messageID, err := id.New("msg")
		if err == nil {
			_, err = session.Send(sendCtx, messageID, completionNotification(task))
		}
		if err != nil && !errors.Is(err, context.Canceled) {
			diagnostics.Error("agent_task_notification_failed", "session_id", task.ParentSession, "task_id", task.SessionID, "error_type", diagnostics.ErrorType(err))
		}
	}()
}

func completionNotification(task Status) string {
	content := fmt.Sprintf("Agent task notification: task %s", task.SessionID)
	if task.Name != "" {
		content += fmt.Sprintf(" (%s)", task.Name)
	}
	content += fmt.Sprintf(" finished with status %s.", task.State)
	if task.Output != "" {
		content += "\n\n" + task.Output
	}
	if task.NoFinalMessage {
		content += "\n\nThe agent turn ended without a final assistant message."
	}
	if task.Error != "" {
		content += "\n\nError: " + task.Error
	}
	return content
}

// SuspendSession prevents and cancels deliveries into one parent session.
func (n *CompletionNotifier) SuspendSession(ctx context.Context, sessionID string) error {
	if n == nil {
		return nil
	}
	n.mu.Lock()
	n.paused[sessionID]++
	active := make([]*activeNotification, 0)
	for _, notification := range n.active {
		if notification.parent == sessionID {
			active = append(active, notification)
		}
	}
	n.mu.Unlock()
	for _, notification := range active {
		notification.cancel()
		select {
		case <-notification.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (n *CompletionNotifier) ResumeSession(sessionID string) {
	if n == nil {
		return
	}
	n.mu.Lock()
	if n.paused[sessionID] <= 1 {
		delete(n.paused, sessionID)
	} else {
		n.paused[sessionID]--
	}
	n.mu.Unlock()
}

func (n *CompletionNotifier) Close(ctx context.Context) error {
	if n == nil {
		return nil
	}
	n.mu.Lock()
	if !n.closed {
		n.closed = true
		n.cancel()
	}
	n.mu.Unlock()
	done := make(chan struct{})
	go func() {
		n.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
