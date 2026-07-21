package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNormalizeProviderPreferences(t *testing.T) {
	for _, tc := range []struct {
		name        string
		input       json.RawMessage
		wantErr     string
		wantEmitted bool
	}{
		{name: "nil", input: nil, wantEmitted: false},
		{name: "empty", input: json.RawMessage{}, wantEmitted: false},
		{
			name:        "object",
			input:       json.RawMessage(`{"order":["anthropic"],"allow_fallbacks":false}`),
			wantEmitted: true,
		},
		{name: "object with whitespace", input: json.RawMessage(`  {"sort":"price"}  `), wantEmitted: true},
		{name: "array rejected", input: json.RawMessage(`["anthropic"]`), wantErr: "must be a JSON object"},
		{name: "string rejected", input: json.RawMessage(`"anthropic"`), wantErr: "must be a JSON object"},
		{name: "number rejected", input: json.RawMessage(`42`), wantErr: "must be a JSON object"},
		{name: "invalid json rejected", input: json.RawMessage(`{oops}`), wantErr: "not valid JSON"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeProviderPreferences(tc.input)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantEmitted && len(got) == 0 {
				t.Fatalf("expected non-nil preferences, got nil")
			}
			if !tc.wantEmitted && len(got) != 0 {
				t.Fatalf("expected nil preferences, got %s", got)
			}
		})
	}
}
