package main

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

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

func TestScrollCapClampsToMaxOffset(t *testing.T) {
	m := newModel(100)
	m.windowH = 10 // visibleRows = 8
	for i := 0; i < 50; i++ {
		m.events.push(Event{SQL: "x"})
	}
	// max offset = 50 - 8 = 42
	updated := tea.Model(m)
	for i := 0; i < 200; i++ {
		updated, _ = updated.(model).Update(tea.KeyPressMsg{Code: tea.KeyUp})
	}
	got := updated.(model).scrollOffset
	if got != 42 {
		t.Fatalf("scrollOffset=%d, want 42 (clamped)", got)
	}
}

func TestScrollDownToZeroAutoUnpauses(t *testing.T) {
	m := newModel(100)
	m.windowH = 10
	for i := 0; i < 20; i++ {
		m.events.push(Event{SQL: "x"})
	}
	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyUp}) // → paused, offset 1
	updated, _ = updated.(model).Update(tea.KeyPressMsg{Code: tea.KeyDown})
	state := updated.(model)
	if state.paused {
		t.Fatalf("paused=true after scrolling back to 0; want auto-unpause")
	}
	if state.scrollOffset != 0 {
		t.Fatalf("scrollOffset=%d, want 0", state.scrollOffset)
	}
}

func TestJumpKeyResets(t *testing.T) {
	m := newModel(100)
	m.windowH = 10
	for i := 0; i < 20; i++ {
		m.events.push(Event{SQL: "x"})
	}
	m.scrollOffset = 5
	m.paused = true
	updated, _ := m.Update(tea.KeyPressMsg{Code: 'g'})
	state := updated.(model)
	if state.scrollOffset != 0 || state.paused {
		t.Fatalf("after g: offset=%d paused=%v, want 0/false", state.scrollOffset, state.paused)
	}
}

func TestWindowSizeMsg(t *testing.T) {
	m := newModel(3)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	state := updated.(model)
	if state.windowW != 80 || state.windowH != 24 {
		t.Fatalf("got w=%d h=%d, want 80/24", state.windowW, state.windowH)
	}
}

func TestViewWindowTooSmall(t *testing.T) {
	m := newModel(3)
	m.windowH = 2
	out := m.View().Content
	if !strings.Contains(out, "too small") {
		t.Fatalf("expected too-small message:\n%s", out)
	}
}

func TestViewEmptyBufferDoesNotPanic(t *testing.T) {
	m := newModel(3)
	m.windowH = 10
	m.windowW = 80
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panic: %v", r)
		}
	}()
	_ = m.View().Content
}

func TestTruncateUTF8Safe(t *testing.T) {
	// "użytkownicy" — 'ż', 'ó' are 2-byte UTF-8 codepoints. Slicing on byte
	// boundaries would corrupt them.
	got := truncate("SELECT * FROM użytkownicy_aktywni WHERE foo='bar'", 20)
	if !utf8.ValidString(got) {
		t.Fatalf("truncate returned invalid UTF-8: %q", got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("expected ellipsis, got %q", got)
	}
}

func TestNormalizeSQLCollapsesWhitespace(t *testing.T) {
	got := normalizeSQL("SELECT *\n  FROM users\nWHERE id = 1")
	want := "SELECT * FROM users WHERE id = 1"
	if got != want {
		t.Fatalf("normalizeSQL=%q, want %q", got, want)
	}
}

func TestRenderRowEmptySQLShowsPlaceholder(t *testing.T) {
	m := newModel(3)
	m.windowH = 10
	m.windowW = 80
	m.truncateW = 80
	m.events.push(Event{Started: time.Now(), Finished: time.Now().Add(time.Millisecond), Tag: "BEGIN"})
	out := m.View().Content
	if !strings.Contains(out, "—") {
		t.Fatalf("expected — placeholder for empty SQL:\n%s", out)
	}
}

func TestPageUpScrollsByVisibleRows(t *testing.T) {
	m := newModel(100)
	m.windowH = 10 // visible = 8
	for i := 0; i < 50; i++ {
		m.events.push(Event{SQL: "x"})
	}
	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyPgUp})
	if updated.(model).scrollOffset != 8 {
		t.Fatalf("PgUp scrollOffset=%d, want 8", updated.(model).scrollOffset)
	}
}

func TestDroppedCountInHeader(t *testing.T) {
	m := newModel(3)
	m.windowH = 10
	m.windowW = 80
	d := &atomic.Uint64{}
	d.Store(7)
	m.dropped = d
	out := m.View().Content
	if !strings.Contains(out, "7 dropped") {
		t.Fatalf("expected '7 dropped' in header:\n%s", out)
	}
}
