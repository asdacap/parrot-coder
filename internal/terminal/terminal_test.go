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

func TestColorDisabledForNonTTYAndDumbTerminal(t *testing.T) {
	previousNoColor, hadNoColor := os.LookupEnv("NO_COLOR")
	if err := os.Unsetenv("NO_COLOR"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if hadNoColor {
			_ = os.Setenv("NO_COLOR", previousNoColor)
		} else {
			_ = os.Unsetenv("NO_COLOR")
		}
	})
	t.Setenv("TERM", "xterm-256color")
	var output bytes.Buffer
	if ColorEnabled(&output, false) {
		t.Fatal("color enabled for non-TTY output")
	}
	if !colorEnabled(true, false) {
		t.Fatal("color disabled for an ordinary TTY")
	}
	t.Setenv("TERM", "dumb")
	if colorEnabled(true, false) {
		t.Fatal("color enabled for TERM=dumb")
	}
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("NO_COLOR", "")
	if colorEnabled(true, false) {
		t.Fatal("color enabled with NO_COLOR")
	}
	if colorEnabled(true, true) {
		t.Fatal("color enabled when explicitly disabled")
	}
}
