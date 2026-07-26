package task

import (
	"context"
	"testing"
)

type testTask struct{ snapshot Snapshot }

func (t testTask) Snapshot() Snapshot { return t.snapshot }

func (testTask) Wait(context.Context) (Completion, error) { return Completion{}, nil }

func (t testTask) Interrupt(context.Context) (Snapshot, error) { return t.snapshot, nil }

func TestManagerListsBlockedRunningAndPendingTasksAsActive(t *testing.T) {
	t.Parallel()
	manager := NewManager()
	for _, status := range []string{"blocked", "running", "pending", "succeeded"} {
		err := manager.Register(testTask{snapshot: Snapshot{SessionID: "session-" + status, Kind: KindAgent, Status: status}}, func(string) bool { return true })
		if err != nil {
			t.Fatal(err)
		}
	}
	active := manager.ListActive("session")
	if len(active) != 3 || active[0].Status != "blocked" || active[1].Status != "pending" || active[2].Status != "running" {
		t.Fatalf("active tasks = %#v", active)
	}
}

func TestManagerRegisterRequiresDomainIdentity(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name     string
		snapshot Snapshot
		wantErr  bool
	}{
		{name: "missing session", snapshot: Snapshot{Kind: KindAgent}, wantErr: true},
		{name: "agent with process", snapshot: Snapshot{SessionID: "session", ProcessID: "process", Kind: KindAgent}, wantErr: true},
		{name: "shell without process", snapshot: Snapshot{SessionID: "session", Kind: KindShell}, wantErr: true},
		{name: "unsupported kind", snapshot: Snapshot{SessionID: "session"}, wantErr: true},
		{name: "agent", snapshot: Snapshot{SessionID: "session", Kind: KindAgent}},
		{name: "shell", snapshot: Snapshot{SessionID: "session", ProcessID: "process", Kind: KindShell}},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager := NewManager()
			err := manager.Register(testTask{snapshot: test.snapshot}, func(string) bool { return true })
			if (err != nil) != test.wantErr {
				t.Fatalf("Register() error = %v, want error %t", err, test.wantErr)
			}
		})
	}
}
