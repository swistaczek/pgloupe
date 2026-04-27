// Package main is pgloupe — a TUI for live Postgres wire-protocol
// inspection. This file holds the shared Event type and the ringbuffer
// backing the TUI's scrollback. Producers (the proxy) push; consumers
// (the TUI render loop) snapshot or iterate under an RWMutex.
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
	mu       sync.RWMutex
	capacity int
	data     []Event // size up to capacity; circular
	head     int     // index of the NEWEST entry
	count    int     // number of valid entries (≤ capacity)
	dropped  atomic.Uint64
}

func newRingBuffer(capacity int) *ringBuffer {
	return &ringBuffer{capacity: capacity, data: make([]Event, capacity)}
}

// push prepends a new Event. When the buffer is full, the write at the
// new head position silently overwrites the oldest entry (the slot is
// reused, no allocation).
func (r *ringBuffer) push(e Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.head = (r.head - 1 + r.capacity) % r.capacity
	r.data[r.head] = e
	if r.count < r.capacity {
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
		out[i] = r.data[(r.head+i)%r.capacity]
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
		if !fn(r.data[(r.head+i)%r.capacity]) {
			return
		}
	}
}

// clear discards all buffered events. Triggered by the TUI's `c` key.
// Does NOT reset the dropped-event counter — that's a measurement of
// proxy backpressure since startup, not "what's currently visible".
func (r *ringBuffer) clear() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.head = 0
	r.count = 0
	// Zero the slice so the GC can reclaim Event.SQL strings (which
	// can be large). Re-using the underlying array.
	for i := range r.data {
		r.data[i] = Event{}
	}
}

// noteDropped increments the counter of events lost because the events
// channel was full. Cheap, lock-free.
func (r *ringBuffer) noteDropped() { r.dropped.Add(1) }

// droppedCount reads the dropped-event counter atomically.
func (r *ringBuffer) droppedCount() uint64 { return r.dropped.Load() }
