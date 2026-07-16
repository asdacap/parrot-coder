package terminal

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestPickerFilterNavigateAndSelect(t *testing.T) {
	options := []Candidate{
		{Value: "alpha", Description: "first"},
		{Value: "alpine", Description: "mountain"},
		{Value: "beta", Description: "second"},
	}
	picker := NewPickerIO(bytes.NewBufferString("al\x1b[B\r"), nil, options)
	selected, err := picker.Pick(context.Background())
	if err != nil || selected.Value != "alpine" {
		t.Fatalf("Pick() = %#v, %v", selected, err)
	}
}

func TestPickerCtrlKKillsToEndOfFilter(t *testing.T) {
	options := []Candidate{{Value: "alpha"}, {Value: "alpine"}}
	picker := NewPickerIO(bytes.NewBufferString("alXX\x1b[D\x1b[D\x0b\r"), nil, options)
	selected, err := picker.Pick(context.Background())
	if err != nil || selected.Value != "alpha" {
		t.Fatalf("Pick() = %#v, %v; want alpha", selected, err)
	}
}

func TestPickerCancel(t *testing.T) {
	picker := NewPickerIO(bytes.NewBufferString("\x1b"), nil, []Candidate{{Value: "one"}})
	_, err := picker.Pick(context.Background())
	if !errors.Is(err, ErrCanceled) {
		t.Fatalf("Pick() error = %v", err)
	}
}

func TestPickerShowsOverflowAndCollapsesSelectedInput(t *testing.T) {
	options := make([]Candidate, 15)
	for i := range options {
		options[i] = Candidate{Value: fmt.Sprintf("option-%02d", i+1)}
	}
	var output bytes.Buffer
	renderer := NewLiveRenderer(&output, RendererConfig{TTY: true, Columns: 80, MaxInputRows: 12})
	picker := NewPickerIO(bytes.NewBufferString("\r"), &output, options, WithPickerRenderer(renderer))
	selected, err := picker.Pick(context.Background())
	if err != nil || selected.Value != "option-01" {
		t.Fatalf("Pick() = %#v, %v", selected, err)
	}
	text := output.String()
	if !strings.Contains(text, "Showing 1-10 of 15 options; 5 hidden") {
		t.Fatalf("picker did not report hidden options: %q", text)
	}
	if !strings.Contains(text, "Filter: option-01") {
		t.Fatalf("picker did not render selected input before collapse: %q", text)
	}
	if len(renderer.rows) != 1 || renderer.rows[0] != "Filter: option-01" {
		t.Fatalf("picker final input state = %#v", renderer.rows)
	}
}
