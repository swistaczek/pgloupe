package main

import (
	"sync"
	"testing"
	"time"
)

func TestEventStatusOK(t *testing.T) {
	e := Event{Started: time.Now(), Finished: time.Now().Add(time.Millisecond), Tag: "SELECT 5"}
	if e.Status() != StatusOK {
		t.Fatalf("expected StatusOK, got %v", e.Status())
	}
}

func TestEventStatusErr(t *testing.T) {
	e := Event{Started: time.Now(), Err: "42P01: relation does not exist"}
	if e.Status() != StatusErr {
		t.Fatalf("expected StatusErr, got %v", e.Status())
	}
}

func TestEventStatusInflight(t *testing.T) {
	e := Event{Started: time.Now()}
	if e.Status() != StatusInflight {
		t.Fatalf("expected StatusInflight, got %v", e.Status())
	}
}

func TestEventDurationZeroWhenInflight(t *testing.T) {
	e := Event{Started: time.Now()}
	if e.Duration() != 0 {
		t.Fatalf("inflight event should have zero duration, got %v", e.Duration())
	}
}

func TestRingBufferPushAndLen(t *testing.T) {
	rb := newRingBuffer(3)
	rb.push(Event{SQL: "a"})
	rb.push(Event{SQL: "b"})
	if got := rb.len(); got != 2 {
		t.Fatalf("len=%d, want 2", got)
	}
}

func TestRingBufferEvictsOldest(t *testing.T) {
	rb := newRingBuffer(2)
	rb.push(Event{SQL: "a"})
	rb.push(Event{SQL: "b"})
	rb.push(Event{SQL: "c"})
	if rb.len() != 2 {
		t.Fatalf("len=%d, want 2", rb.len())
	}
	got := rb.snapshot()
	if got[0].SQL != "c" || got[1].SQL != "b" {
		t.Fatalf("snapshot=[%s, %s], want [c, b]", got[0].SQL, got[1].SQL)
	}
}

func TestRingBufferSnapshotIsACopy(t *testing.T) {
	rb := newRingBuffer(2)
	rb.push(Event{SQL: "a"})
	snap := rb.snapshot()
	snap[0].SQL = "mutated"
	if rb.snapshot()[0].SQL != "a" {
		t.Fatal("snapshot mutation leaked into ring buffer")
	}
}

func TestRingBufferForEach(t *testing.T) {
	rb := newRingBuffer(3)
	rb.push(Event{SQL: "a"})
	rb.push(Event{SQL: "b"})
	rb.push(Event{SQL: "c"})
	var got []string
	rb.forEach(0, 10, func(e Event) bool {
		got = append(got, e.SQL)
		return true
	})
	want := []string{"c", "b", "a"}
	for i, s := range got {
		if s != want[i] {
			t.Fatalf("forEach[%d]=%q, want %q", i, s, want[i])
		}
	}
}

func TestRingBufferForEachOffset(t *testing.T) {
	rb := newRingBuffer(5)
	for _, s := range []string{"a", "b", "c", "d", "e"} {
		rb.push(Event{SQL: s})
	}
	var got []string
	rb.forEach(2, 2, func(e Event) bool {
		got = append(got, e.SQL)
		return true
	})
	want := []string{"c", "b"} // newest=e, then d, c (offset 2), b (offset 3)
	for i, s := range got {
		if s != want[i] {
			t.Fatalf("forEach[%d]=%q, want %q", i, s, want[i])
		}
	}
}

func TestRingBufferDroppedCounter(t *testing.T) {
	rb := newRingBuffer(3)
	rb.noteDropped()
	rb.noteDropped()
	if got := rb.droppedCount(); got != 2 {
		t.Fatalf("droppedCount=%d, want 2", got)
	}
}

// TestRingBufferConcurrentPushSnapshot — race-detector probe. With -race,
// this fails immediately if push/snapshot have an unprotected data race.
func TestRingBufferConcurrentPushSnapshot(t *testing.T) {
	rb := newRingBuffer(100)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 10000; i++ {
			rb.push(Event{Rows: int64(i)})
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			_ = rb.snapshot()
		}
	}()
	wg.Wait()
}
