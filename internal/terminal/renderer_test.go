package terminal

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
)

func TestLiveRendererExactBytesAndClear(t *testing.T) {
	var output bytes.Buffer
	renderer := NewLiveRenderer(&output, RendererConfig{TTY: true, Columns: 80})
	if err := renderer.Update([]string{"one"}); err != nil {
		t.Fatal(err)
	}
	if err := renderer.Clear(); err != nil {
		t.Fatal(err)
	}
	want := "\x1b[?25l\x1b[2Kone\x1b[?25h" +
		"\x1b[?25l\r\x1b[2K\r\x1b[?25h"
	if output.String() != want {
		t.Fatalf("renderer bytes = %q; want %q", output.String(), want)
	}
}

func TestLiveRendererAllowedEscapesAndSanitization(t *testing.T) {
	var output bytes.Buffer
	renderer := NewLiveRenderer(&output, RendererConfig{TTY: true, Columns: 4, MaxRows: 3})
	if err := renderer.Update([]string{"abcdef", "bad\x1b[2J"}); err != nil {
		t.Fatal(err)
	}
	if err := renderer.Update([]string{"next"}); err != nil {
		t.Fatal(err)
	}
	if err := renderer.Commit("safe\x1b]0;title\a"); err != nil {
		t.Fatal(err)
	}

	data := output.String()
	escape := regexp.MustCompile("\\x1b(?:\\[\\?25[lh]|\\[2K|\\[[0-9]+[AB])")
	withoutAllowed := escape.ReplaceAllString(data, "")
	if strings.ContainsRune(withoutAllowed, '\x1b') {
		t.Fatalf("renderer emitted a disallowed escape: %q", data)
	}
	if strings.Contains(data, "\x1b[2J") || strings.Contains(data, "\x1b]") {
		t.Fatalf("untrusted terminal control survived: %q", data)
	}
	if !strings.HasSuffix(data, "safe]0;title\n") {
		t.Fatalf("Commit output = %q", data)
	}
}

func TestLiveRendererWidthResizeAndRowBound(t *testing.T) {
	var output bytes.Buffer
	renderer := NewLiveRenderer(&output, RendererConfig{TTY: true, Columns: 4, MaxRows: 2})
	if err := renderer.Update([]string{"123456789"}); err != nil {
		t.Fatal(err)
	}
	if len(renderer.rows) != 2 || renderer.rows[0] != "5678" || renderer.rows[1] != "9" {
		t.Fatalf("wrapped rows = %#v", renderer.rows)
	}
	renderer.SetColumns(2)
	if err := renderer.Update([]string{"界a"}); err != nil {
		t.Fatal(err)
	}
	if len(renderer.rows) != 2 || renderer.rows[0] != "界" || renderer.rows[1] != "a" {
		t.Fatalf("resized rows = %#v", renderer.rows)
	}
}

func TestLiveRendererPlainFallbackDeduplicates(t *testing.T) {
	var output bytes.Buffer
	renderer := NewLiveRenderer(&output, RendererConfig{TTY: false, Columns: 80})
	if err := renderer.Update([]string{"working", "working", "unsafe\x1b[2J"}); err != nil {
		t.Fatal(err)
	}
	if err := renderer.Update([]string{"working", "done"}); err != nil {
		t.Fatal(err)
	}
	if got, want := output.String(), "working\nunsafe[2J\ndone\n"; got != want {
		t.Fatalf("plain output = %q; want %q", got, want)
	}
	if strings.ContainsRune(output.String(), '\x1b') {
		t.Fatal("plain renderer emitted an escape")
	}
}
