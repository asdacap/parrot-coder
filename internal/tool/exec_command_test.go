package tool

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/amirulashraf/parrot-coder/internal/process"
)

func TestFormatPersistentResult(t *testing.T) {
	processID := "proc_internal"
	exitCode := 7
	tests := []struct {
		name   string
		result process.PersistentResult
		want   string
	}{
		{
			name: "yielded",
			result: process.PersistentResult{
				ChunkID: "abcdef", Name: "project-tests", WallTime: 2 * time.Second,
				Output: "hidden output", ProcessID: &processID, OriginalTokenCount: 3,
			},
			want: "Waiting for process project-tests (proc_internal) yielded.",
		},
		{
			name: "completed",
			result: process.PersistentResult{
				ChunkID: "123456", WallTime: 1500 * time.Millisecond, Output: "command output",
				ExitCode: &exitCode, OriginalTokenCount: 4,
			},
			want: "Chunk ID: 123456\nWall time: 1.5000 seconds\nProcess exited with code 7\nOriginal token count: 4\nOutput:\ncommand output",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := formatPersistentResult(test.result); got != test.want {
				t.Fatalf("formatPersistentResult() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestExecCommandOutputTailIsBounded(t *testing.T) {
	lines := make([]string, 12)
	for i := range lines {
		lines[i] = strings.Repeat(string(rune('a'+i)), 2000)
	}
	got := execCommandOutputTail("10%\r20%\r100%\r\n" + strings.Join(lines, "\r\n") + "\r\n")
	if len(got) > 16<<10 || strings.Contains(got, "10%") || strings.Contains(got, "20%") || strings.Contains(got, strings.Repeat("a", 20)) || strings.Contains(got, strings.Repeat("b", 20)) || !strings.HasSuffix(got, strings.Repeat("l", 2000)) {
		t.Fatalf("bounded output tail has %d bytes and unexpected content", len(got))
	}
}

func TestYieldTimeDescriptionsMentionAutomaticResultDelivery(t *testing.T) {
	tests := []struct {
		name       string
		schema     json.RawMessage
		wantTiming string
	}{
		{"exec_command", NewExecCommandTool(nil).JSONSchema(), "Defaults to 10000 ms; effective range is 250-30000 ms."},
		{"write_stdin", NewWriteStdinTool(nil).JSONSchema(), "Non-empty writes default to 250 ms and cap at 30000 ms; empty polls wait 5000-300000 ms by default."},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var schema struct {
				Properties map[string]struct {
					Description string `json:"description"`
				} `json:"properties"`
			}
			if err := json.Unmarshal(test.schema, &schema); err != nil {
				t.Fatal(err)
			}
			description := schema.Properties["yield_time_ms"].Description
			if !strings.Contains(description, test.wantTiming) || !strings.Contains(description, "result will automatically be sent once done after the yield") {
				t.Fatalf("yield_time_ms description = %q", description)
			}
		})
	}
}
