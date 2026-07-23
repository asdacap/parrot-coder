package task

import (
	"context"
	"testing"
)

type testTask struct{ snapshot Snapshot }

func (t testTask) Snapshot() Snapshot { return t.snapshot }

func (testTask) Wait(context.Context) (Completion, error) { return Completion{}, nil }

func (t testTask) Interrupt(context.Context) (Snapshot, error) { return t.snapshot, nil }

func TestManagerRegisterRequiresTaskAndSessionIDs(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name     string
		snapshot Snapshot
		wantErr  bool
	}{
		{name: "missing task ID", snapshot: Snapshot{SessionID: "session"}, wantErr: true},
		{name: "missing session ID", snapshot: Snapshot{ID: "task"}, wantErr: true},
		{name: "complete owner", snapshot: Snapshot{ID: "task", SessionID: "session"}},
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
