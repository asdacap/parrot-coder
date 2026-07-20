package monitor

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/amirulashraf/parrot-coder/internal/process"
	"github.com/amirulashraf/parrot-coder/internal/session"
	"github.com/amirulashraf/parrot-coder/internal/subagent"
	"github.com/amirulashraf/parrot-coder/internal/workspace"
)

type discardOutputStore struct{}

type discardManagedOutput struct{}

func (discardManagedOutput) Write(p []byte) (int, error) { return len(p), nil }
func (discardManagedOutput) ID() string                  { return "" }
func (discardManagedOutput) Finalize(context.Context) (process.StoredOutput, error) {
	return process.StoredOutput{}, nil
}
func (discardManagedOutput) Discard() {}

func (discardOutputStore) Create(context.Context) (process.ManagedOutput, error) {
	return discardManagedOutput{}, nil
}
func (discardOutputStore) Read(string, int64, int64) ([]byte, error) { return nil, nil }

type recordingAdmitter struct{ admitted chan session.AdmitParams }

func (a *recordingAdmitter) Admit(_ context.Context, _ string, params session.AdmitParams) (session.Admission, error) {
	a.admitted <- params
	return session.Admission{}, nil
}

type recordingWaker struct{ woken chan string }

func (w *recordingWaker) Wake(sessionID string) { w.woken <- sessionID }

func TestServiceNotifiesOnExitAndTimeoutWithoutConsumingOrStoppingProcess(t *testing.T) {
	workspaceRoot, err := workspace.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runner, err := process.NewRunner(process.Config{Workspace: workspaceRoot, TerminationGrace: 50 * time.Millisecond, OutputStore: discardOutputStore{}})
	if err != nil {
		t.Fatal(err)
	}
	defer runner.Close()

	admitter := &recordingAdmitter{admitted: make(chan session.AdmitParams, 2)}
	waker := &recordingWaker{woken: make(chan string, 2)}
	service := NewService(runner, subagent.NewManager(nil, subagent.Config{}), admitter)
	service.SetWaker(waker)
	defer service.Close(context.Background())

	exited, err := runner.RunPersistent(context.Background(), process.PersistentRequest{
		Shell: "/bin/sh", Command: "sleep .35; exit 4", SessionID: "session", Yield: process.MinYieldTime, Unrestricted: true,
	})
	if err != nil || exited.ProcessID == nil {
		t.Fatalf("exited process start = %#v, %v", exited, err)
	}
	timed, err := runner.RunPersistent(context.Background(), process.PersistentRequest{
		Shell: "/bin/sh", Command: "sleep 5", SessionID: "session", Yield: process.MinYieldTime, Unrestricted: true,
	})
	if err != nil || timed.ProcessID == nil {
		t.Fatalf("timed process start = %#v, %v", timed, err)
	}
	if err := service.Start("other", *exited.ProcessID, 0); err == nil || !strings.Contains(err.Error(), "unknown process") {
		t.Fatalf("cross-session start error = %v", err)
	}
	if err := service.Start("session", *exited.ProcessID, 0); err != nil {
		t.Fatal(err)
	}
	if err := service.Start("session", *timed.ProcessID, 20*time.Millisecond); err != nil {
		t.Fatal(err)
	}

	contents := make([]string, 0, 2)
	for range 2 {
		select {
		case params := <-admitter.admitted:
			if params.MessageID == "" || params.Delivery != session.DeliverySteer {
				t.Fatalf("admission = %#v", params)
			}
			contents = append(contents, params.Content)
		case <-time.After(2 * time.Second):
			t.Fatal("monitor notification timed out")
		}
		select {
		case sessionID := <-waker.woken:
			if sessionID != "session" {
				t.Fatalf("woken session = %q", sessionID)
			}
		case <-time.After(time.Second):
			t.Fatal("session was not woken")
		}
	}
	joined := strings.Join(contents, "\n")
	if !strings.Contains(joined, "exited with code 4") || !strings.Contains(joined, "timed out after 20ms") || !strings.Contains(joined, "was not stopped") {
		t.Fatalf("notifications = %q", contents)
	}

	observer, err := runner.ObservePersistent("session", *timed.ProcessID)
	if err != nil {
		t.Fatalf("timed out process was removed: %v", err)
	}
	waitCtx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := observer.Wait(waitCtx); err == nil {
		t.Fatal("timed out monitor stopped process")
	}
	drained, err := runner.WritePersistent(context.Background(), process.PersistentWriteRequest{
		SessionID: "session", ProcessID: *timed.ProcessID, Chars: "\x03", Yield: time.Second,
	})
	if err != nil || drained.ProcessID != nil {
		t.Fatalf("timed process cleanup = %#v, %v", drained, err)
	}
	exitOutput, err := runner.WritePersistent(context.Background(), process.PersistentWriteRequest{
		SessionID: "session", ProcessID: *exited.ProcessID, Chars: "\x03", Yield: process.MinYieldTime,
	})
	if err != nil || exitOutput.ExitCode == nil || *exitOutput.ExitCode != 4 {
		t.Fatalf("exit process output was consumed = %#v, %v", exitOutput, err)
	}
}

func TestServiceRequiresWakerAndStopsNotificationsOnClose(t *testing.T) {
	workspaceRoot, err := workspace.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runner, err := process.NewRunner(process.Config{Workspace: workspaceRoot, TerminationGrace: 50 * time.Millisecond, OutputStore: discardOutputStore{}})
	if err != nil {
		t.Fatal(err)
	}
	defer runner.Close()
	admitter := &recordingAdmitter{admitted: make(chan session.AdmitParams, 1)}
	service := NewService(runner, subagent.NewManager(nil, subagent.Config{}), admitter)
	running, err := runner.RunPersistent(context.Background(), process.PersistentRequest{
		Shell: "/bin/sh", Command: "sleep 5", SessionID: "session", Yield: process.MinYieldTime, Unrestricted: true,
	})
	if err != nil || running.ProcessID == nil {
		t.Fatalf("process start = %#v, %v", running, err)
	}
	if err := service.Start("session", *running.ProcessID, 0); err == nil || !strings.Contains(err.Error(), "waker") {
		t.Fatalf("start without waker error = %v", err)
	}
	service.SetWaker(&recordingWaker{woken: make(chan string, 1)})
	if err := service.Start("session", *running.ProcessID, 0); err != nil {
		t.Fatal(err)
	}
	if err := service.Start("session", *running.ProcessID, 0); err == nil || !strings.Contains(err.Error(), "already being monitored") {
		t.Fatalf("duplicate monitor error = %v", err)
	}
	if err := service.SuspendSession(context.Background(), "session"); err != nil {
		t.Fatal(err)
	}
	if err := service.SuspendSession(context.Background(), "session"); err != nil {
		t.Fatal(err)
	}
	if err := service.Start("session", *running.ProcessID, 0); err == nil || !strings.Contains(err.Error(), "paused") {
		t.Fatalf("start while nested suspension is active = %v", err)
	}
	service.ResumeSession("session")
	if err := service.Start("session", *running.ProcessID, 0); err == nil || !strings.Contains(err.Error(), "paused") {
		t.Fatalf("start after partial resume = %v", err)
	}
	service.ResumeSession("session")
	if err := service.Start("session", *running.ProcessID, 0); err != nil {
		t.Fatalf("start after complete resume: %v", err)
	}
	if err := service.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := service.Start("session", *running.ProcessID, 0); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("start after close error = %v", err)
	}
	select {
	case notification := <-admitter.admitted:
		t.Fatalf("close admitted notification: %#v", notification)
	default:
	}
}
