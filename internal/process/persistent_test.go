package process

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type notificationSession struct{ messages chan string }

func (s *notificationSession) Send(_ context.Context, content string) (string, error) {
	s.messages <- content
	return "", nil
}

type notificationSessions struct{ session AgentSession }

func (s notificationSessions) Get(string) AgentSession { return s.session }

func TestYieldedProcessCompletionSendsRequestedOutputToAgent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "output.txt")
	if err := os.WriteFile(path, []byte("build output\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	zero := 0
	tests := []struct {
		name            string
		maxOutputTokens *int
		wantOutput      bool
	}{
		{name: "default budget", wantOutput: true},
		{name: "zero budget", maxOutputTokens: &zero},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			session := &notificationSession{messages: make(chan string, 1)}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			runner := &Runner{
				notifyCtx: ctx, notifications: make(map[string]map[string]*activeNotification),
				notifyPaused: make(map[string]int), config: Config{AgentSessions: notificationSessions{session: session}, MaxOutputBytes: 64 << 10},
			}
			exitCode := 0
			finished := make(chan struct{})
			close(finished)
			item := &persistentProcess{
				id: "proc_test", sessionID: "ses_test", finished: finished, exitCode: &exitCode,
				storedOutput: &StoredOutput{Path: path, Size: int64(len("build output\n"))}, maxOutputTokens: test.maxOutputTokens,
			}
			runner.notifyPersistentCompletion(item)

			select {
			case got := <-session.messages:
				if !strings.Contains(got, "task proc_test exited with code 0") || strings.Contains(got, "Output:\nbuild output\n") != test.wantOutput {
					t.Fatalf("notification = %q, want output %t", got, test.wantOutput)
				}
			case <-time.After(time.Second):
				t.Fatal("timed out waiting for completion notification")
			}
			runner.notifyWG.Wait()
		})
	}
}
