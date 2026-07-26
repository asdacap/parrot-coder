package agent

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/amirulashraf/parrot-coder/internal/session"
)

type notificationAgentSession struct {
	mu       sync.Mutex
	messages []string
	sent     chan struct{}
}

func (s *notificationAgentSession) Send(_ context.Context, _, message string) (session.Admission, error) {
	s.mu.Lock()
	s.messages = append(s.messages, message)
	s.mu.Unlock()
	select {
	case s.sent <- struct{}{}:
	default:
	}
	return session.Admission{}, nil
}

func TestCompletionNotifierDeliversTerminalTurnToDirectParent(t *testing.T) {
	parent := &notificationAgentSession{sent: make(chan struct{}, 1)}
	notifier := NewCompletionNotifier()
	notifier.SetLookup(func(id string) (NotificationSession, bool) {
		return parent, id == "parent"
	})
	notifier.Notify(Status{SessionID: "child", ParentSession: "parent", Name: "worker", Turn: 2, State: StatusSucceeded, Output: "done"})
	select {
	case <-parent.sent:
	case <-time.After(time.Second):
		t.Fatal("notification was not delivered")
	}
	parent.mu.Lock()
	message := strings.Join(parent.messages, "\n")
	parent.mu.Unlock()
	for _, expected := range []string{"child", "worker", "succeeded", "done"} {
		if !strings.Contains(message, expected) {
			t.Fatalf("notification %q does not contain %q", message, expected)
		}
	}
	if err := notifier.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestCompletionNotifierSkipsUnavailableAndSuspendedParents(t *testing.T) {
	parent := &notificationAgentSession{sent: make(chan struct{}, 1)}
	notifier := NewCompletionNotifier()
	notifier.SetLookup(func(id string) (NotificationSession, bool) { return parent, id == "parent" })
	if err := notifier.SuspendSession(context.Background(), "parent"); err != nil {
		t.Fatal(err)
	}
	notifier.Notify(Status{SessionID: "paused-child", ParentSession: "parent", Turn: 1, State: StatusSucceeded})
	notifier.Notify(Status{SessionID: "deleted-child", ParentSession: "deleted", Turn: 1, State: StatusSucceeded})
	notifier.ResumeSession("parent")
	if err := notifier.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	parent.mu.Lock()
	defer parent.mu.Unlock()
	if len(parent.messages) != 0 {
		t.Fatalf("delivered %d unexpected notifications", len(parent.messages))
	}
}
