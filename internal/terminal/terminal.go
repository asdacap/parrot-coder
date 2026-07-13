// Package terminal implements Parrot's append-only terminal contract.
package terminal

import (
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"unicode"
)

// IsTTY reports whether value is an actual terminal. Character devices such as
// /dev/null are not terminals even though their file mode has ModeCharDevice.
func IsTTY(value any) bool {
	file, ok := value.(*os.File)
	if !ok || file == nil {
		return false
	}
	_, err := getTerminalState(file.Fd())
	return err == nil
}

// OpenInput opens the controlling terminal. Piped stdin is never reused for
// permission or question replies.
func OpenInput() (*os.File, error) { return os.OpenFile("/dev/tty", os.O_RDWR, 0) }

// Sanitize removes terminal controls from untrusted text while preserving
// newline and tab. ESC and all C0/C1 controls cannot reach the terminal.
func Sanitize(value string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, value)
}

type Writer struct{ W io.Writer }

func (w Writer) Write(data []byte) (int, error) {
	clean := []byte(Sanitize(string(data)))
	_, err := w.W.Write(clean)
	if err != nil {
		return 0, err
	}
	return len(data), nil
}

// OpenBrowser starts the platform browser opener without invoking a shell.
func OpenBrowser(url string) error {
	name := "xdg-open"
	if runtime.GOOS == "darwin" {
		name = "open"
	}
	return exec.Command(name, url).Start()
}
