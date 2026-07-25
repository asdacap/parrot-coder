package monitor

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	v1 "github.com/amirulashraf/parrot-coder/internal/api/v1"
	"github.com/amirulashraf/parrot-coder/internal/process"
	managedtask "github.com/amirulashraf/parrot-coder/internal/task"
	"github.com/amirulashraf/parrot-coder/internal/workspace"
)

type retainedOutputStore struct{ data []byte }
type retainedManagedOutput struct {
	bytes.Buffer
	store *retainedOutputStore
}

func (s *retainedOutputStore) Create(context.Context) (process.ManagedOutput, error) {
	return &retainedManagedOutput{store: s}, nil
}
func (s *retainedOutputStore) Read(_ string, offset, limit int64) ([]byte, error) {
	return append([]byte(nil), s.data[offset:min(offset+limit, int64(len(s.data)))]...), nil
}
func (o *retainedManagedOutput) ID() string { return "retained" }
func (o *retainedManagedOutput) Finalize(context.Context) (process.StoredOutput, error) {
	o.store.data = append([]byte(nil), o.Bytes()...)
	return process.StoredOutput{ID: o.ID(), Size: int64(len(o.store.data))}, nil
}
func (*retainedManagedOutput) Discard() {}

type sentMessage struct {
	sessionID string
	content   string
}

type recordingAgentSessions struct{ sent chan sentMessage }

func (s *recordingAgentSessions) Get(sessionID string) AgentSession {
	return recordingAgentSession{id: sessionID, sent: s.sent}
}

type recordingAgentSession struct {
	id   string
	sent chan sentMessage
}

func (s recordingAgentSession) Send(_ context.Context, content string) (string, error) {
	s.sent <- sentMessage{sessionID: s.id, content: content}
	return "msg_recorded", nil
}

type recordingLifecycle struct{ events chan v1.Event }

func (p *recordingLifecycle) PublishEvent(event v1.Event) { p.events <- event }

type testAgentTask struct {
	snapshot   managedtask.Snapshot
	completion chan managedtask.Completion
}

func (t *testAgentTask) Snapshot() managedtask.Snapshot { return t.snapshot }
func (t *testAgentTask) Wait(ctx context.Context) (managedtask.Completion, error) {
	select {
	case completion := <-t.completion:
		return completion, nil
	case <-ctx.Done():
		return managedtask.Completion{}, ctx.Err()
	}
}
func (t *testAgentTask) Interrupt(context.Context) (managedtask.Snapshot, error) {
	return t.snapshot, nil
}

func registerAgentTask(t *testing.T, tasks *managedtask.Manager, id, name string) *testAgentTask {
	t.Helper()
	item := &testAgentTask{snapshot: managedtask.Snapshot{ID: id, Name: name, SessionID: id, Kind: managedtask.KindAgent, Status: "running"}, completion: make(chan managedtask.Completion, 1)}
	if err := tasks.Register(item, func(sessionID string) bool { return sessionID == "session" }); err != nil {
		t.Fatal(err)
	}
	return item
}

func TestServiceNotifiesForChildAgentsAndRejectsShellTasks(t *testing.T) {
	tasks := managedtask.NewManager()
	completed := registerAgentTask(t, tasks, "agent-completed", "completed-agent")
	registerAgentTask(t, tasks, "agent-timed", "timed-agent")
	shell := &testAgentTask{snapshot: managedtask.Snapshot{ID: "shell", SessionID: "session", Kind: managedtask.KindShell, Status: "running"}, completion: make(chan managedtask.Completion)}
	if err := tasks.Register(shell, func(string) bool { return true }); err != nil {
		t.Fatal(err)
	}

	agents := &recordingAgentSessions{sent: make(chan sentMessage, 2)}
	lifecycle := &recordingLifecycle{events: make(chan v1.Event, 4)}
	service := NewService(tasks, lifecycle)
	service.SetAgentSessions(agents)
	defer service.Close(context.Background())

	if err := service.Start(Request{SessionID: "other", TaskID: "agent-completed"}); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("cross-session start error = %v", err)
	}
	if err := service.Start(Request{SessionID: "session", TaskID: "shell"}); !errors.Is(err, managedtask.ErrWrongKind) {
		t.Fatalf("shell start error = %v", err)
	}
	if err := service.Start(Request{SessionID: "session", CallerTask: "task_main", ToolCallID: "call_complete", TaskID: "agent-completed"}); err != nil {
		t.Fatal(err)
	}
	if err := service.Start(Request{SessionID: "session", CallerTask: "task_main", ToolCallID: "call_timeout", TaskID: "agent-timed", Timeout: 20 * time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	completed.completion <- managedtask.Completion{Task: managedtask.Snapshot{ID: "agent-completed", Kind: managedtask.KindAgent, Status: "succeeded"}, Output: "agent output"}

	contents := make([]string, 0, 2)
	for range 2 {
		select {
		case sent := <-agents.sent:
			if sent.sessionID != "session" {
				t.Fatalf("sent session = %q", sent.sessionID)
			}
			contents = append(contents, sent.content)
		case <-time.After(time.Second):
			t.Fatal("monitor notification timed out")
		}
	}
	started, finished := map[string]bool{}, map[string]string{}
	for range 4 {
		select {
		case event := <-lifecycle.events:
			payload, decodeErr := v1.DecodeEventData(event)
			if decodeErr != nil {
				t.Fatal(decodeErr)
			}
			item := payload.(*v1.MonitorEvent)
			if event.SessionID != "session" || event.TaskID != "task_main" {
				t.Fatalf("monitor event attribution = %#v", event)
			}
			if event.Type == v1.EventMonitorStarted {
				started[item.ToolCallID] = true
			} else {
				finished[item.ToolCallID] = item.Status
			}
		case <-time.After(time.Second):
			t.Fatal("monitor lifecycle event timed out")
		}
	}
	if !started["call_complete"] || !started["call_timeout"] || finished["call_complete"] != "completed" || finished["call_timeout"] != "timed_out" {
		t.Fatalf("monitor lifecycle = started %#v, finished %#v", started, finished)
	}
	joined := strings.Join(contents, "\n")
	if !strings.Contains(joined, "agent task agent-completed finished with status succeeded") || !strings.Contains(joined, "agent output") || !strings.Contains(joined, "timed out after 20ms") || !strings.Contains(joined, "was not stopped") {
		t.Fatalf("notifications = %q", contents)
	}
}

func TestTaskManagerWaitYieldsThenReturnsShellCompletionWithoutConsumingOutput(t *testing.T) {
	workspaceRoot, err := workspace.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := &retainedOutputStore{}
	tasks := managedtask.NewManager()
	runner, err := process.NewRunner(process.Config{Workspace: workspaceRoot, TerminationGrace: 50 * time.Millisecond, OutputStore: store, Tasks: tasks})
	if err != nil {
		t.Fatal(err)
	}
	defer runner.Close()
	running, err := runner.RunPersistent(context.Background(), process.PersistentRequest{
		Shell: "/bin/sh", Command: "sleep .3; printf retained; sleep .05; exit 4", SessionID: "session", Yield: process.MinYieldTime, Unrestricted: true,
	})
	if err != nil || running.ProcessID == nil {
		t.Fatalf("process start = %#v, %v", running, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	result, err := tasks.Wait(ctx, "session", *running.ProcessID)
	if !errors.Is(err, context.DeadlineExceeded) || result.Status != "running" {
		t.Fatalf("yield = %#v, %v", result, err)
	}
	result, err = tasks.Wait(context.Background(), "session", *running.ProcessID)
	if err != nil || result.Status != "failed" || result.ExitCode == nil || *result.ExitCode != 4 {
		t.Fatalf("completion = %#v, %v", result, err)
	}
	drained, err := runner.WritePersistent(context.Background(), process.PersistentWriteRequest{
		SessionID: "session", ProcessID: *running.ProcessID, Yield: process.MinYieldTime,
	})
	if err != nil || drained.Output != "retained" {
		t.Fatalf("drained output = %#v, %v", drained, err)
	}
	if _, err := tasks.Wait(context.Background(), "other", *running.ProcessID); err == nil {
		t.Fatal("cross-session wait succeeded")
	}
}

func TestServiceRequiresAgentSessionsAndStopsNotificationsOnClose(t *testing.T) {
	tasks := managedtask.NewManager()
	registerAgentTask(t, tasks, "agent", "agent")
	agents := &recordingAgentSessions{sent: make(chan sentMessage, 1)}
	service := NewService(tasks, nil)
	if err := service.Start(Request{SessionID: "session", TaskID: "agent"}); err == nil || !strings.Contains(err.Error(), "agent sessions") {
		t.Fatalf("start without agent sessions error = %v", err)
	}
	service.SetAgentSessions(agents)
	if err := service.Start(Request{SessionID: "session", TaskID: "agent"}); err != nil {
		t.Fatal(err)
	}
	if err := service.Start(Request{SessionID: "session", TaskID: "agent"}); err == nil || !strings.Contains(err.Error(), "already being monitored") {
		t.Fatalf("duplicate monitor error = %v", err)
	}
	if err := service.SuspendSession(context.Background(), "session"); err != nil {
		t.Fatal(err)
	}
	if err := service.SuspendSession(context.Background(), "session"); err != nil {
		t.Fatal(err)
	}
	if err := service.Start(Request{SessionID: "session", TaskID: "agent"}); err == nil || !strings.Contains(err.Error(), "paused") {
		t.Fatalf("start while nested suspension is active = %v", err)
	}
	service.ResumeSession("session")
	if err := service.Start(Request{SessionID: "session", TaskID: "agent"}); err == nil || !strings.Contains(err.Error(), "paused") {
		t.Fatalf("start after partial resume = %v", err)
	}
	service.ResumeSession("session")
	if err := service.Start(Request{SessionID: "session", TaskID: "agent"}); err != nil {
		t.Fatalf("start after complete resume: %v", err)
	}
	if err := service.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := service.Start(Request{SessionID: "session", TaskID: "agent"}); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("start after close error = %v", err)
	}
	select {
	case notification := <-agents.sent:
		t.Fatalf("close sent notification: %#v", notification)
	default:
	}
}
