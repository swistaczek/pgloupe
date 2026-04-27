package main

import (
	"sync"
	"sync/atomic"
	"time"
)

type Status int

const (
	StatusInflight Status = iota
	StatusOK
	StatusErr
)

type Event struct {
	ConnID   uint64
	Started  time.Time
	Finished time.Time
	SQL      string
	Tag      string
	Rows     int64
	Err      string
	TxStatus byte
}

func (e Event) Status() Status {
	switch {
	case e.Err != "":
		return StatusErr
	case e.Finished.IsZero():
		return StatusInflight
	default:
		return StatusOK
	}
}

func (e Event) Duration() time.Duration {
	if e.Finished.IsZero() {
		return 0
	}
	return e.Finished.Sub(e.Started)
}

// ringBuffer is a fixed-capacity circular log of events, newest-first.
// Backed by a head index so push is O(1) instead of O(n) prepend.
// Goroutine-safe via an internal RWMutex; the proxy goroutine calls push
// while the TUI goroutine calls snapshot/len/forEach.
type ringBuffer struct {
	mu      sync.RWMutex
	cap     int
	data    []Event // size up to cap; circular
	head    int     // index of the NEWEST entry
	count   int     // number of valid entries (≤ cap)
	dropped atomic.Uint64
}

func newRingBuffer(cap int) *ringBuffer {
	return &ringBuffer{cap: cap, data: make([]Event, cap)}
}

func (r *ringBuffer) push(e Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.head = (r.head - 1 + r.cap) % r.cap
	if r.count > 0 && r.count == r.cap {
		// We just overwrote the entry at the new head position.
	}
	r.data[r.head] = e
	if r.count < r.cap {
		r.count++
	}
}

func (r *ringBuffer) len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.count
}

// snapshot returns a freshly-allocated newest-first copy of the buffer.
// Held by the TUI's render path; safe under concurrent push.
func (r *ringBuffer) snapshot() []Event {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Event, r.count)
	for i := 0; i < r.count; i++ {
		out[i] = r.data[(r.head+i)%r.cap]
	}
	return out
}

// forEach iterates from offset (0 = newest) up to limit entries, calling fn.
// Avoids allocating a snapshot for the render path. Stops if fn returns false.
func (r *ringBuffer) forEach(offset, limit int, fn func(Event) bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	end := offset + limit
	if end > r.count {
		end = r.count
	}
	for i := offset; i < end; i++ {
		if !fn(r.data[(r.head+i)%r.cap]) {
			return
		}
	}
}

// noteDropped increments the counter of events lost because the events
// channel was full. Cheap, lock-free.
func (r *ringBuffer) noteDropped() { r.dropped.Add(1) }

// droppedCount reads the dropped-event counter atomically.
func (r *ringBuffer) droppedCount() uint64 { return r.dropped.Load() }
