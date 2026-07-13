package terminal

import (
	"strings"
	"testing"
	"unicode"
)

func FuzzSanitize(f *testing.F) {
	for _, seed := range []string{"plain text", "safe\n\ttabs", "\x1b[2J\x9b31m", "invalid\xffutf8"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		output := Sanitize(input)
		if strings.IndexFunc(output, func(r rune) bool {
			return unicode.IsControl(r) && r != '\n' && r != '\t'
		}) >= 0 {
			t.Fatalf("sanitized output contains a terminal control: %q", output)
		}
	})
}
