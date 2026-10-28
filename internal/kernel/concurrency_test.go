package kernel

import (
	"sync"
	"testing"
)

// TestSeq_ConcurrentAppendsHaveNoGapsAndNoDuplicates pins dense allocation
// under contention: eight goroutines, four hundred records, Seq 1..400 each
// exactly once.
func TestSeq_ConcurrentAppendsHaveNoGapsAndNoDuplicates(t *testing.T) {
	k := newKernel("sess-1", nil)

	const goroutines = 8
	const perGoroutine = 50
	const total = goroutines * perGoroutine

	claims := make(chan uint64, total)
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for worker := 0; worker < goroutines; worker++ {
		go func() {
			defer wg.Done()
			for index := 0; index < perGoroutine; index++ {
				claims <- k.Frame(frameBody("f"), RecordAt{}).Seq
			}
		}()
	}
	wg.Wait()
	close(claims)

	times := make(map[uint64]int, total)
	for seq := range claims {
		times[seq]++
	}
	if len(times) != total {
		t.Fatalf("%d distinct Seq values claimed, want %d", len(times), total)
	}
	for seq := 1; seq <= total; seq++ {
		if got := times[uint64(seq)]; got != 1 {
			t.Fatalf("Seq %d was handed out %d times", seq, got)
		}
	}

	records := k.Records(Cursor{})
	if len(records) != total {
		t.Fatalf("the log holds %d records, want %d", len(records), total)
	}
	for index, record := range records {
		if record.Seq != uint64(index+1) {
			t.Fatalf("record %d has Seq %d: the log is not in Seq order", index, record.Seq)
		}
	}
}

// TestObserveFrom_ConcurrentAppendSeesEveryRecordExactlyOnce pins that the
// replay→live seam loses nothing while an adapter keeps appending.
func TestObserveFrom_ConcurrentAppendSeesEveryRecordExactlyOnce(t *testing.T) {
	k := newKernel("sess-1", nil)

	const total = 200

	var mu sync.Mutex
	deliveries := make(map[uint64]int, total)

	var appending sync.WaitGroup
	appending.Add(1)
	start := make(chan struct{})
	go func() {
		defer appending.Done()
		<-start
		for index := 0; index < total; index++ {
			k.Frame(frameBody("f"), RecordAt{})
		}
	}()

	close(start)
	cancel, err := k.ObserveFrom(Cursor{}, func(record Record) {
		mu.Lock()
		deliveries[record.Seq]++
		mu.Unlock()
	})
	if err != nil {
		t.Fatalf("ObserveFrom: %v", err)
	}
	appending.Wait()
	cancel()

	mu.Lock()
	defer mu.Unlock()
	if len(deliveries) != total {
		t.Fatalf("%d distinct Seq values delivered, want %d", len(deliveries), total)
	}
	for seq := 1; seq <= total; seq++ {
		if got := deliveries[uint64(seq)]; got != 1 {
			t.Fatalf("Seq %d was delivered %d times, want exactly once", seq, got)
		}
	}
}

// TestObserver_ConcurrentObserveAndCancelLeaveNoLeak pins that observe/cancel
// racing appends terminates and leaves the registry empty.
func TestObserver_ConcurrentObserveAndCancelLeaveNoLeak(t *testing.T) {
	k := newKernel("sess-1", nil)

	var appending sync.WaitGroup
	appending.Add(1)
	go func() {
		defer appending.Done()
		for index := 0; index < 200; index++ {
			k.Frame(frameBody("f"), RecordAt{})
		}
	}()

	var observing sync.WaitGroup
	observing.Add(8)
	for worker := 0; worker < 8; worker++ {
		go func() {
			defer observing.Done()
			for round := 0; round < 50; round++ {
				cancel := k.Observe(func(Record) {})
				cancel()
				cancel() // idempotent under contention too
			}
		}()
	}

	observing.Wait()
	appending.Wait()

	k.mu.Lock()
	remaining := len(k.observers)
	k.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("%d observers leaked after every cancel", remaining)
	}

	// The stream is still usable afterwards.
	if records := k.Records(Cursor{}); len(records) != 200 {
		t.Fatalf("the log holds %d records, want 200", len(records))
	}
}
