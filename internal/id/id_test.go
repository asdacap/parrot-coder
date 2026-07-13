package id

import (
	"strings"
	"testing"
	"time"
)

func TestNewAndValidate(t *testing.T) {
	value, err := New("msg")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(value, "msg_") {
		t.Fatalf("New() = %q", value)
	}
	if err := Validate(value); err != nil {
		t.Fatalf("Validate(%q): %v", value, err)
	}
	if err := ValidatePrefix(value, "msg"); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePrefix(value, "ses"); err == nil {
		t.Fatal("ValidatePrefix accepted wrong prefix")
	}
}

func TestNewIsUnique(t *testing.T) {
	seen := make(map[string]bool)
	for range 1000 {
		value, err := New("evt")
		if err != nil {
			t.Fatal(err)
		}
		if seen[value] {
			t.Fatalf("duplicate ID %q", value)
		}
		seen[value] = true
	}
}

func TestNewSortsByTimestamp(t *testing.T) {
	first, err := New("msg")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	second, err := New("msg")
	if err != nil {
		t.Fatal(err)
	}
	if first >= second {
		t.Fatalf("IDs not time ordered: %q >= %q", first, second)
	}
}

func TestValidateRejectsMalformedIDs(t *testing.T) {
	tests := []string{
		"",
		"msg",
		"MSG_01ARZ3NDEKTSV4RRFFQ69G5FAV",
		"msg_01ARZ3NDEKTSV4RRFFQ69G5FA",
		"msg_81ARZ3NDEKTSV4RRFFQ69G5FAV",
		"msg_01ARZ3NDEKTSV4RRFFQ69G5FAI",
	}
	for _, value := range tests {
		if err := Validate(value); err == nil {
			t.Errorf("Validate(%q) succeeded", value)
		}
	}
}

func TestNewRejectsInvalidPrefix(t *testing.T) {
	for _, prefix := range []string{"", "Message", "two_words", "white space"} {
		if _, err := New(prefix); err == nil {
			t.Errorf("New(%q) succeeded", prefix)
		}
	}
}
