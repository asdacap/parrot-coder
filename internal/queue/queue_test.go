package queue

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
)

func TestQueueLifecycleDirectionsAndJSONL(t *testing.T) {
	state := t.TempDir()
	sessionID := "ses_test"
	if err := os.MkdirAll(filepath.Join(state, "session", sessionID), 0o700); err != nil {
		t.Fatal(err)
	}
	queues := New(state)
	created, err := queues.Create(sessionID, "build-work-now", "release tasks")
	if err != nil {
		t.Fatal(err)
	}
	wantPath := filepath.Join(state, "session", sessionID, "queues", "build-work-now.jsonl")
	if created != (Info{Path: wantPath, Name: "build-work-now", Description: "release tasks"}) {
		t.Fatalf("Create() = %#v", created)
	}
	data, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatal(err)
	}
	if want := "{\"name\":\"build-work-now\",\"description\":\"release tasks\"}\n"; string(data) != want {
		t.Fatalf("empty JSONL = %q, want %q", data, want)
	}

	pushes := []struct {
		item string
		dir  Direction
	}{
		{"one", ""},
		{"three", Back},
		{"zero", Front},
		{"two", Front},
	}
	for _, push := range pushes {
		if _, err := queues.Push(sessionID, "build-work-now", push.item, push.dir); err != nil {
			t.Fatalf("Push(%q, %q): %v", push.item, push.dir, err)
		}
	}
	// Order is now two, zero, one, three. Exercise both explicit and default
	// take directions while checking that size tracks persisted contents.
	takes := []struct {
		dir  Direction
		want string
		size int
	}{
		{Back, "three", 3},
		{"", "two", 2},
		{Front, "zero", 1},
		{Back, "one", 0},
	}
	for _, take := range takes {
		got, info, err := queues.Take(sessionID, "build-work-now", take.dir)
		if err != nil || got != take.want || info.Size != take.size {
			t.Fatalf("Take(%q) = %q, %#v, %v; want %q size %d", take.dir, got, info, err, take.want, take.size)
		}
	}
	_, emptyInfo, err := queues.Take(sessionID, "build-work-now", "")
	if !errors.Is(err, ErrEmpty) || emptyInfo.Size != 0 {
		t.Fatalf("empty Take() = %#v, %v", emptyInfo, err)
	}
	if _, err := os.Stat(wantPath); err != nil {
		t.Fatalf("empty queue was not retained: %v", err)
	}
}

func TestValidationExplicitCreationAndSessionScope(t *testing.T) {
	state := t.TempDir()
	if err := os.MkdirAll(filepath.Join(state, "session", "ses_a"), 0o700); err != nil {
		t.Fatal(err)
	}
	queues := New(state)

	invalidNames := []string{"", "one", "one-two", "one-two-three-four", "One-two-three", "one_two-three", "one--three", "é-one-two"}
	for _, name := range invalidNames {
		if _, err := queues.Create("ses_a", name, ""); !errors.Is(err, ErrInvalidName) {
			t.Errorf("Create(%q) error = %v, want ErrInvalidName", name, err)
		}
	}
	for _, sessionID := range []string{"", ".", "..", "../ses_a", "a/b", `a\b`} {
		if _, err := queues.Create(sessionID, "one-two-three", ""); err == nil {
			t.Errorf("Create session %q unexpectedly succeeded", sessionID)
		}
	}
	if _, err := queues.Create("missing", "one-two-three", ""); err == nil {
		t.Fatal("Create unexpectedly created a missing session")
	}
	if _, err := queues.Push("ses_a", "one-two-three", "item", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Push without Create error = %v, want ErrNotFound", err)
	}
	if _, err := queues.Create("ses_a", "one-two-three", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := queues.Create("ses_a", "one-two-three", "changed"); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("duplicate Create error = %v", err)
	}
	if _, err := queues.Push("ses_a", "one-two-three", "item", Direction("side")); !errors.Is(err, ErrInvalidDirection) {
		t.Fatalf("invalid Push direction error = %v", err)
	}
	if _, _, err := queues.Take("ses_a", "one-two-three", Direction("side")); !errors.Is(err, ErrInvalidDirection) {
		t.Fatalf("invalid Take direction error = %v", err)
	}
}

func TestListSortedEmptyAndMalformed(t *testing.T) {
	state := t.TempDir()
	queues := New(state)
	got, err := queues.List("ses_no_directory")
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("List absent directory = %#v, %v; want non-nil empty", got, err)
	}
	if err := os.MkdirAll(filepath.Join(state, "session", "ses_a"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct{ name, description string }{
		{"zebra-work-now", "last"},
		{"alpha-work-now", "first"},
	} {
		if _, err := queues.Create("ses_a", item.name, item.description); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := queues.Push("ses_a", "zebra-work-now", "a", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := queues.Push("ses_a", "zebra-work-now", "b", ""); err != nil {
		t.Fatal(err)
	}
	got, err = queues.List("ses_a")
	if err != nil {
		t.Fatal(err)
	}
	want := []Info{
		{Path: filepath.Join(state, "session", "ses_a", "queues", "alpha-work-now.jsonl"), Name: "alpha-work-now", Description: "first", Size: 0},
		{Path: filepath.Join(state, "session", "ses_a", "queues", "zebra-work-now.jsonl"), Name: "zebra-work-now", Description: "last", Size: 2},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("List() = %#v, want %#v", got, want)
	}

	malformed := filepath.Join(state, "session", "ses_a", "queues", "bad-queue-name.jsonl")
	if err := os.WriteFile(malformed, []byte("not json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := queues.List("ses_a"); err == nil {
		t.Fatal("List accepted malformed queue")
	}
}

func TestConcurrentPushAndTake(t *testing.T) {
	state := t.TempDir()
	if err := os.MkdirAll(filepath.Join(state, "session", "ses_a"), 0o700); err != nil {
		t.Fatal(err)
	}
	queues := New(state)
	otherStore := New(state)
	if _, err := queues.Create("ses_a", "many-items-here", ""); err != nil {
		t.Fatal(err)
	}
	const count = 100
	var wg sync.WaitGroup
	errs := make(chan error, count)
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			store := queues
			if index%2 == 0 {
				store = otherStore
			}
			_, err := store.Push("ses_a", "many-items-here", "item", "")
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	listed, err := queues.Get("ses_a", "many-items-here")
	if err != nil || listed.Size != count {
		t.Fatalf("after concurrent Push: %#v, %v", listed, err)
	}

	errs = make(chan error, count)
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			store := queues
			if index%2 == 0 {
				store = otherStore
			}
			_, _, err := store.Take("ses_a", "many-items-here", "")
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	listed, err = queues.Get("ses_a", "many-items-here")
	if err != nil || listed.Size != 0 {
		t.Fatalf("after concurrent Take: %#v, %v", listed, err)
	}
}
