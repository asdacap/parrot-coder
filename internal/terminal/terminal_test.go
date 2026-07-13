package terminal

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestSanitizeRemovesTerminalControls(t *testing.T) {
	input := "safe\x1b[2J\x1b]0;title\a\rtext\n\ttail\u009b31m"
	got := Sanitize(input)
	if strings.ContainsAny(got, "\x1b\a\r\u009b") {
		t.Fatalf("Sanitize() left terminal controls in %q", got)
	}
	if got != "safe[2J]0;titletext\n\ttail31m" {
		t.Fatalf("Sanitize() = %q", got)
	}
	var output bytes.Buffer
	w := Writer{W: &output}
	if n, err := w.Write([]byte(input)); err != nil || n != len(input) {
		t.Fatalf("Write() = %d, %v", n, err)
	}
	if output.String() != got {
		t.Fatalf("Writer output = %q", output.String())
	}
}

func TestCharacterDeviceIsNotAssumedToBeTTY(t *testing.T) {
	file, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if IsTTY(file) {
		t.Fatal("null device reported as a terminal")
	}
}
