package main

import (
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
		t.Fatalf("snapshot=%v, want [c, b]", []string{got[0].SQL, got[1].SQL})
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
