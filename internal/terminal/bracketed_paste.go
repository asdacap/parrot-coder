package terminal

import "io"

const (
	bracketedPasteEnable  = "\x1b[?2004h"
	bracketedPasteDisable = "\x1b[?2004l"
)

// SetBracketedPaste enables or disables terminal bracketed paste mode.
// When enabled, terminals wrap pasted text in CSI 200~/CSI 201~ markers so
// embedded newlines can be handled as paste content instead of submit keys.
func SetBracketedPaste(w io.Writer, enabled bool) error {
	if w == nil {
		return nil
	}
	seq := bracketedPasteDisable
	if enabled {
		seq = bracketedPasteEnable
	}
	_, err := io.WriteString(w, seq)
	return err
}
