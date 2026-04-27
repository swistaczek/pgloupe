package main

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

func TestUpdateAppendsEventAtTop(t *testing.T) {
	m := newModel(3)
	updated, _ := m.Update(eventMsg{SQL: "first"})
	updated, _ = updated.(model).Update(eventMsg{SQL: "second"})
	got := updated.(model).events.snapshot()
	if len(got) != 2 || got[0].SQL != "second" || got[1].SQL != "first" {
		t.Fatalf("got=[%s, %s], want [second, first]", got[0].SQL, got[1].SQL)
	}
}

func TestQuitKeyReturnsTeaQuit(t *testing.T) {
	m := newModel(3)
	_, cmd := m.Update(tea.KeyPressMsg{Code: 'q'})
	if cmd == nil {
		t.Fatal("expected tea.Quit cmd")
	}
}

func TestPauseKeyTogglesFlag(t *testing.T) {
	m := newModel(3)
	updated, _ := m.Update(tea.KeyPressMsg{Code: 'p'})
	if !updated.(model).paused {
		t.Fatal("paused should be true after first 'p'")
	}
	updated, _ = updated.(model).Update(tea.KeyPressMsg{Code: 'p'})
	if updated.(model).paused {
		t.Fatal("paused should be false after second 'p'")
	}
}

func TestViewIncludesPausedBadgeWhenPaused(t *testing.T) {
	m := newModel(3)
	m.windowH = 10
	m.windowW = 80
	m.paused = true
	out := m.View().Content
	if !strings.Contains(out, "PAUSED") {
		t.Fatalf("View missing PAUSED badge:\n%s", out)
	}
}

func TestViewRendersInflightSQL(t *testing.T) {
	m := newModel(3)
	m.windowH = 10
	m.windowW = 80
	m.events.push(Event{SQL: "SELECT pg_sleep(5)", Started: time.Now()})
	out := m.View().Content
	if !strings.Contains(out, "SELECT pg_sleep(5)") {
		t.Fatalf("View missing inflight SQL:\n%s", out)
	}
}

func TestViewRendersErrorRow(t *testing.T) {
	m := newModel(3)
	m.windowH = 10
	m.windowW = 80
	m.events.push(Event{
		SQL:      "SELECT * FROM nope",
		Started:  time.Now(),
		Finished: time.Now().Add(time.Millisecond),
		Err:      "ERROR: 42P01: relation does not exist",
	})
	out := m.View().Content
	if !strings.Contains(out, "42P01") {
		t.Fatalf("View missing error code:\n%s", out)
	}
}

func TestRingBufferRespectsCap(t *testing.T) {
	m := newModel(2)
	m.events.push(Event{SQL: "a"})
	m.events.push(Event{SQL: "b"})
	m.events.push(Event{SQL: "c"})
	if m.events.len() != 2 {
		t.Fatalf("len=%d, want 2", m.events.len())
	}
}
