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

func (s *notificationSession) Send(_ context.Context, content string) (string, string, error) {
	s.messages <- content
	return "", "", nil
}

type notificationSessions struct{ session AgentSession }

func (s notificationSessions) Get(string) AgentSession { return s.session }

func TestYieldedProcessCompletionSendsRequestedOutputToAgent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "output.txt")
	if err := os.WriteFile(path, []byte("build output\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	zero, one := 0, 1
	tests := []struct {
		name            string
		maxOutputTokens *int
		wantOutput      string
		wantTruncated   bool
	}{
		{name: "default budget", wantOutput: "Output:\nbuild output\n"},
		{name: "limited budget", maxOutputTokens: &one, wantOutput: "Output (tail only; full output is too large):\nput\n", wantTruncated: true},
		{name: "zero budget", maxOutputTokens: &zero, wantTruncated: true},
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
				id: "proc_test", name: "project-tests", sessionID: "ses_test", finished: finished, exitCode: &exitCode,
				storedOutput: &StoredOutput{Path: path, Size: int64(len("build output\n"))}, maxOutputTokens: test.maxOutputTokens,
			}
			runner.notifyPersistentCompletion(item)

			select {
			case got := <-session.messages:
				if !strings.Contains(got, "process project-tests exited with code 0") || strings.Contains(got, "proc_test") || strings.Contains(got, "task project-tests") || (test.wantOutput != "" && !strings.Contains(got, test.wantOutput)) {
					t.Fatalf("notification = %q, want output %q", got, test.wantOutput)
				}
				pathMessage := "Full output is available at " + path + ". Read that file separately for the complete output."
				if strings.Contains(got, pathMessage) != test.wantTruncated {
					t.Fatalf("notification = %q, want truncated %t", got, test.wantTruncated)
				}
			case <-time.After(time.Second):
				t.Fatal("timed out waiting for completion notification")
			}
			runner.notifyWG.Wait()
		})
	}
}
