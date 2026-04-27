package main

import "time"

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

type ringBuffer struct {
	cap  int
	data []Event
}

func newRingBuffer(cap int) *ringBuffer { return &ringBuffer{cap: cap} }

func (r *ringBuffer) push(e Event) {
	r.data = append([]Event{e}, r.data...)
	if len(r.data) > r.cap {
		r.data = r.data[:r.cap]
	}
}

func (r *ringBuffer) len() int { return len(r.data) }

func (r *ringBuffer) snapshot() []Event {
	out := make([]Event, len(r.data))
	copy(out, r.data)
	return out
}
