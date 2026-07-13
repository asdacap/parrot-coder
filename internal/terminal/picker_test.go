package terminal

import (
	"bytes"
	"context"
	"errors"
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

func TestPickerCancel(t *testing.T) {
	picker := NewPickerIO(bytes.NewBufferString("\x1b"), nil, []Candidate{{Value: "one"}})
	_, err := picker.Pick(context.Background())
	if !errors.Is(err, ErrCanceled) {
		t.Fatalf("Pick() error = %v", err)
	}
}
