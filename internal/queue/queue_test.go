package queue

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sync"
	"testing"
	"time"
)

func TestNewValidatesAndProvisionsAbsoluteDirectory(t *testing.T) {
	if _, err := New(""); err == nil {
		t.Fatal("New accepted an empty directory")
	}
	if _, err := New("relative/queues"); err == nil {
		t.Fatal("New accepted a relative directory")
	}

	directory := filepath.Join(t.TempDir(), "nested", "queues", "..", "queues")
	queues, err := New(directory)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Clean(directory); queues.Directory() != want {
		t.Fatalf("Directory() = %q, want %q", queues.Directory(), want)
	}
	if stat, err := os.Stat(queues.Directory()); err != nil || !stat.IsDir() || stat.Mode().Perm() != 0o700 {
		t.Fatalf("provisioned directory stat = %#v, %v", stat, err)
	}

	file := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(file); err == nil {
		t.Fatal("New accepted a file")
	}
}

func TestQueueLifecycleDirectionsAndJSONL(t *testing.T) {
	queues := newStore(t, filepath.Join(t.TempDir(), "queues"))
	created, err := queues.Create("build-work-now", "release tasks")
	if err != nil {
		t.Fatal(err)
	}
	wantPath := filepath.Join(queues.Directory(), "build-work-now.jsonl")
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

	for _, push := range []struct {
		item string
		dir  Direction
	}{{"one", ""}, {"three", Back}, {"zero", Front}, {"two", Front}} {
		if _, err := queues.Push("build-work-now", push.item, push.dir); err != nil {
			t.Fatalf("Push(%q, %q): %v", push.item, push.dir, err)
		}
	}
	for _, take := range []struct {
		dir  Direction
		want string
		size int
	}{{Back, "three", 3}, {"", "two", 2}, {Front, "zero", 1}, {Back, "one", 0}} {
		got, info, err := queues.Take("build-work-now", take.dir)
		if err != nil || got != take.want || info.Size != take.size {
			t.Fatalf("Take(%q) = %q, %#v, %v; want %q size %d", take.dir, got, info, err, take.want, take.size)
		}
	}
	_, emptyInfo, err := queues.Take("build-work-now", "")
	if !errors.Is(err, ErrEmpty) || emptyInfo.Size != 0 {
		t.Fatalf("empty Take() = %#v, %v", emptyInfo, err)
	}
	if _, err := os.Stat(wantPath); err != nil {
		t.Fatalf("empty queue was not retained: %v", err)
	}
}

func TestValidationAndExplicitCreation(t *testing.T) {
	queues := newStore(t, filepath.Join(t.TempDir(), "queues"))
	for _, name := range []string{"", "One-two-three", "one_two-three", "one--three", "-one", "one-", "é-one-two"} {
		if _, err := queues.Create(name, ""); !errors.Is(err, ErrInvalidName) {
			t.Errorf("Create(%q) error = %v, want ErrInvalidName", name, err)
		}
	}
	for _, name := range []string{"one", "one-two", "one-two-three-four"} {
		if _, err := queues.Create(name, ""); err != nil {
			t.Errorf("Create(%q) error = %v", name, err)
		}
	}
	if _, err := queues.Push("one-two-three", "item", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Push without Create error = %v, want ErrNotFound", err)
	}
	if _, err := queues.Create("one-two-three", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := queues.Create("one-two-three", "changed"); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("duplicate Create error = %v", err)
	}
	if _, err := queues.Push("one-two-three", "item", Direction("side")); !errors.Is(err, ErrInvalidDirection) {
		t.Fatalf("invalid Push direction error = %v", err)
	}
	if _, _, err := queues.Take("one-two-three", Direction("side")); !errors.Is(err, ErrInvalidDirection) {
		t.Fatalf("invalid Take direction error = %v", err)
	}
}

func TestListSortedEmptyAndMalformed(t *testing.T) {
	queues := newStore(t, filepath.Join(t.TempDir(), "queues"))
	got, err := queues.List()
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("List empty directory = %#v, %v; want non-nil empty", got, err)
	}
	for _, item := range []struct{ name, description string }{{"zebra-work-now", "last"}, {"alpha-work-now", "first"}} {
		if _, err := queues.Create(item.name, item.description); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := queues.Push("zebra-work-now", "a", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := queues.Push("zebra-work-now", "b", ""); err != nil {
		t.Fatal(err)
	}
	got, err = queues.List()
	if err != nil {
		t.Fatal(err)
	}
	want := []Info{
		{Path: filepath.Join(queues.Directory(), "alpha-work-now.jsonl"), Name: "alpha-work-now", Description: "first"},
		{Path: filepath.Join(queues.Directory(), "zebra-work-now.jsonl"), Name: "zebra-work-now", Description: "last", Size: 2},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("List() = %#v, want %#v", got, want)
	}

	malformed := filepath.Join(queues.Directory(), "bad-queue-name.jsonl")
	if err := os.WriteFile(malformed, []byte("not json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := queues.List(); err == nil {
		t.Fatal("List accepted malformed queue")
	}
}

func TestPersistenceAcrossReconstructedStoresAndDirectoryIsolation(t *testing.T) {
	root := t.TempDir()
	firstDirectory := filepath.Join(root, "first")
	first := newStore(t, firstDirectory)
	if _, err := first.Create("persist-work-here", "kept"); err != nil {
		t.Fatal(err)
	}
	for _, item := range []string{"one", "two"} {
		if _, err := first.Push("persist-work-here", item, ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := first.Monitor("persist-work-here", true); err != nil {
		t.Fatal(err)
	}

	reconstructed := newStore(t, firstDirectory)
	info, err := reconstructed.Get("persist-work-here")
	if err != nil || info.Description != "kept" || info.Size != 2 || !info.Monitored {
		t.Fatalf("reconstructed Get() = %#v, %v", info, err)
	}
	for _, want := range []string{"one", "two"} {
		got, _, err := reconstructed.Take("persist-work-here", "")
		if err != nil || got != want {
			t.Fatalf("reconstructed Take() = %q, %v; want %q", got, err, want)
		}
	}

	second := newStore(t, filepath.Join(root, "second"))
	if got, err := second.List(); err != nil || len(got) != 0 {
		t.Fatalf("isolated List() = %#v, %v", got, err)
	}
	if _, err := second.Create("persist-work-here", "independent"); err != nil {
		t.Fatal(err)
	}
	firstInfo, err := reconstructed.Get("persist-work-here")
	if err != nil || firstInfo.Description != "kept" || firstInfo.Size != 0 {
		t.Fatalf("first directory changed through second: %#v, %v", firstInfo, err)
	}
	secondInfo, err := second.Get("persist-work-here")
	if err != nil || secondInfo.Description != "independent" || secondInfo.Size != 0 {
		t.Fatalf("second directory Get() = %#v, %v", secondInfo, err)
	}
}

func TestMonitorPersistenceSelectionAndFIFO(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "queues")
	queues := newStore(t, directory)
	for _, name := range []string{"zebra-work-now", "alpha-work-now", "empty-work-now"} {
		if _, err := queues.Create(name, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := queues.Monitor(name, true); err != nil {
			t.Fatal(err)
		}
	}
	for _, item := range []struct{ name, value string }{{"zebra-work-now", "zebra"}, {"alpha-work-now", "first"}, {"alpha-work-now", "second"}} {
		if _, err := queues.Push(item.name, item.value, ""); err != nil {
			t.Fatal(err)
		}
	}
	queues = newStore(t, directory)
	for _, want := range []Notification{{Name: "alpha-work-now", Item: "first"}, {Name: "alpha-work-now", Item: "second"}, {Name: "zebra-work-now", Item: "zebra"}} {
		var got Notification
		delivered, err := queues.DeliverMonitored(func(notification Notification) (bool, error) {
			got = notification
			return true, nil
		})
		if err != nil || !delivered || got.ID == "" || got.Name != want.Name || got.Item != want.Item {
			t.Fatalf("DeliverMonitored() = %#v, %v, %v; want %#v", got, delivered, err, want)
		}
	}
	if delivered, err := queues.DeliverMonitored(func(Notification) (bool, error) { return true, nil }); err != nil || delivered {
		t.Fatalf("empty DeliverMonitored() = %v, %v", delivered, err)
	}
	if delivered, err := queues.DeliverMonitored(nil); err == nil || delivered {
		t.Fatalf("nil DeliverMonitored() = %v, %v", delivered, err)
	}
	if _, err := queues.Push("alpha-work-now", "retained", ""); err != nil {
		t.Fatal(err)
	}
	var deliveryID string
	delivered, err := queues.DeliverMonitored(func(notification Notification) (bool, error) {
		deliveryID = notification.ID
		return false, nil
	})
	if err != nil || delivered || deliveryID == "" {
		t.Fatalf("rejected DeliverMonitored() = %v, %v, ID %q", delivered, err, deliveryID)
	}
	callbackErr := errors.New("delivery failed")
	if delivered, err = queues.DeliverMonitored(func(notification Notification) (bool, error) {
		if notification.ID != deliveryID {
			t.Fatalf("delivery ID changed from %q to %q", deliveryID, notification.ID)
		}
		return false, callbackErr
	}); delivered || !errors.Is(err, callbackErr) {
		t.Fatalf("failed DeliverMonitored() = %v, %v", delivered, err)
	}
	item, _, err := queues.Take("alpha-work-now", "")
	if err != nil || item != "retained" {
		t.Fatalf("retained item = %q, %v", item, err)
	}
	info, err := queues.Get("alpha-work-now")
	if err != nil || !info.Monitored || info.Size != 0 {
		t.Fatalf("monitor retained = %#v, %v", info, err)
	}
	info, err = queues.Monitor("alpha-work-now", false)
	if err != nil || info.Monitored {
		t.Fatalf("disable monitor = %#v, %v", info, err)
	}
	if _, err := queues.Monitor("missing-work-now", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("monitor missing error = %v", err)
	}
}

func TestConcurrentMultiProducerMultiConsumerExactlyOnce(t *testing.T) {
	const (
		producerCount  = 6
		consumerCount  = 6
		itemsPerWorker = 40
	)
	const itemCount = producerCount * itemsPerWorker

	directory := filepath.Join(t.TempDir(), "queues")
	stores := make([]*Store, producerCount)
	for i := range stores {
		stores[i] = newStore(t, directory)
	}
	if _, err := stores[0].Create("many-items-here", ""); err != nil {
		t.Fatal(err)
	}

	producerDone := make(chan struct{})
	consumed := make(chan string, itemCount)
	errs := make(chan error, producerCount+consumerCount)
	var producers sync.WaitGroup
	for producer := 0; producer < producerCount; producer++ {
		producers.Add(1)
		go func(producer int) {
			defer producers.Done()
			for item := 0; item < itemsPerWorker; item++ {
				value := fmt.Sprintf("%d-%d", producer, item)
				if _, err := stores[producer].Push("many-items-here", value, ""); err != nil {
					errs <- err
					return
				}
			}
		}(producer)
	}
	go func() {
		producers.Wait()
		close(producerDone)
	}()

	var consumers sync.WaitGroup
	for consumer := 0; consumer < consumerCount; consumer++ {
		consumers.Add(1)
		go func(consumer int) {
			defer consumers.Done()
			for {
				item, _, err := stores[consumer].Take("many-items-here", "")
				if err == nil {
					consumed <- item
					continue
				}
				if !errors.Is(err, ErrEmpty) {
					errs <- err
					return
				}
				select {
				case <-producerDone:
					return
				default:
					runtime.Gosched()
				}
			}
		}(consumer)
	}
	completed := make(chan struct{})
	go func() {
		consumers.Wait()
		close(completed)
	}()
	select {
	case <-completed:
	case <-time.After(15 * time.Second):
		t.Fatal("concurrent producers and consumers timed out")
	}
	close(consumed)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	seen := make(map[string]int, itemCount)
	for item := range consumed {
		seen[item]++
	}
	if len(seen) != itemCount {
		t.Fatalf("consumed %d unique items, want %d (total deliveries %d)", len(seen), itemCount, totalCounts(seen))
	}
	for producer := 0; producer < producerCount; producer++ {
		for item := 0; item < itemsPerWorker; item++ {
			value := fmt.Sprintf("%d-%d", producer, item)
			if seen[value] != 1 {
				t.Errorf("item %q delivered %d times, want exactly once", value, seen[value])
			}
		}
	}
	info, err := stores[0].Get("many-items-here")
	if err != nil || info.Size != 0 {
		t.Fatalf("after concurrent operations Get() = %#v, %v", info, err)
	}
}

func newStore(t *testing.T, directory string) *Store {
	t.Helper()
	store, err := New(directory)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func totalCounts(counts map[string]int) int {
	total := 0
	for _, count := range counts {
		total += count
	}
	return total
}
