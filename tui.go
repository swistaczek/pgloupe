package main

import (
	"fmt"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

const (
	headerLines       = 1
	footerLines       = 1
	chromeLines       = headerLines + footerLines
	defaultTruncateW  = 80
	smallScreenHeight = 5
)

// styles holds the lipgloss styles for one TUI run. Kept on the model
// (not as package globals) so that --no-color is per-instance and tests
// don't leak rendering state between runs. lipgloss/v2 dropped
// AdaptiveColor in favor of a per-render LightDark helper that requires
// listening to tea.BackgroundColorMsg — too much ceremony for v0.1.
// Light-terminal users can pass --no-color to fall back to bold/italic.
type styles struct {
	err, inflight, ok, chrome, paused, drop lipgloss.Style
}

func defaultStyles() styles {
	return styles{
		// red+bold for ERR — bold remains as a non-color signal for
		// red-green color blindness (deuteranopia/protanopia).
		err: lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Bold(true),
		// yellow+italic for inflight — italic is the non-color signal so
		// the row is distinguishable from OK without depending on hue.
		inflight: lipgloss.NewStyle().Foreground(lipgloss.Color("11")).Italic(true),
		ok:       lipgloss.NewStyle().Foreground(lipgloss.Color("245")),
		chrome:   lipgloss.NewStyle().Foreground(lipgloss.Color("63")).Bold(true),
		paused:   lipgloss.NewStyle().Foreground(lipgloss.Color("208")).Bold(true),
		drop:     lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Italic(true),
	}
}

func plainStyles() styles {
	return styles{
		err:      lipgloss.NewStyle().Bold(true),
		inflight: lipgloss.NewStyle().Italic(true),
		ok:       lipgloss.NewStyle(),
		chrome:   lipgloss.NewStyle().Bold(true),
		paused:   lipgloss.NewStyle().Bold(true),
		drop:     lipgloss.NewStyle().Italic(true),
	}
}

type keyMap struct {
	Quit, Pause, Clear, Up, Down, PageUp, PageDown, Home, Jump, Help key.Binding
}

func defaultKeys() keyMap {
	return keyMap{
		Quit:     key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "quit")),
		Pause:    key.NewBinding(key.WithKeys("p"), key.WithHelp("p", "pause")),
		Clear:    key.NewBinding(key.WithKeys("c"), key.WithHelp("c", "clear")),
		Up:       key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/k", "up")),
		Down:     key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "down")),
		PageUp:   key.NewBinding(key.WithKeys("pgup", "ctrl+b"), key.WithHelp("PgUp", "page up")),
		PageDown: key.NewBinding(key.WithKeys("pgdown", "ctrl+f"), key.WithHelp("PgDn", "page down")),
		Home:     key.NewBinding(key.WithKeys("home", "G"), key.WithHelp("Home/G", "oldest")),
		Jump:     key.NewBinding(key.WithKeys("g"), key.WithHelp("g", "newest")),
		Help:     key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "help")),
	}
}

func (k keyMap) ShortHelp() []key.Binding {
	return []key.Binding{k.Quit, k.Pause, k.Up, k.Down, k.Jump, k.Help}
}

func (k keyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{k.Quit, k.Pause, k.Clear},
		{k.Up, k.Down, k.PageUp, k.PageDown},
		{k.Jump, k.Home, k.Help},
	}
}

type eventMsg Event

type model struct {
	events       *ringBuffer
	keys         keyMap
	help         help.Model
	styles       styles
	dropped      *atomic.Uint64 // shared with proxy; nil-safe
	windowW      int
	windowH      int
	paused       bool
	scrollOffset int
	showHelp     bool
	noColor      bool
	truncateW    int // 0 = no truncation (full width)
}

func newModel(maxEvents int) model {
	h := help.New()
	return model{
		events:    newRingBuffer(maxEvents),
		keys:      defaultKeys(),
		help:      h,
		styles:    defaultStyles(),
		truncateW: defaultTruncateW,
	}
}

func (m model) Init() tea.Cmd { return nil }

func (m model) visibleRows() int {
	v := m.windowH - chromeLines
	if v < 1 {
		return 1
	}
	return v
}

func (m model) maxScrollOffset() int {
	o := m.events.len() - m.visibleRows()
	if o < 0 {
		return 0
	}
	return o
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.windowW, m.windowH = msg.Width, msg.Height
		m.help.SetWidth(msg.Width)
	case eventMsg:
		m.events.push(Event(msg))
		if !m.paused {
			m.scrollOffset = 0
		}
	case tea.MouseWheelMsg:
		// Mouse wheel scroll. Bubble Tea v2 reports wheel direction via
		// Button. Up scrolls toward older events (away from newest);
		// down scrolls back toward newest.
		switch msg.Button {
		case tea.MouseWheelUp:
			m.scrollUp(3)
		case tea.MouseWheelDown:
			m.scrollDown(3)
		}
	case tea.KeyPressMsg:
		switch {
		case key.Matches(msg, m.keys.Quit):
			return m, tea.Quit
		case key.Matches(msg, m.keys.Pause):
			m.paused = !m.paused
		case key.Matches(msg, m.keys.Clear):
			m.events.clear()
			m.scrollOffset = 0
			m.paused = false
		case key.Matches(msg, m.keys.Up):
			m.scrollUp(1)
		case key.Matches(msg, m.keys.Down):
			m.scrollDown(1)
		case key.Matches(msg, m.keys.PageUp):
			m.scrollUp(m.visibleRows())
		case key.Matches(msg, m.keys.PageDown):
			m.scrollDown(m.visibleRows())
		case key.Matches(msg, m.keys.Home):
			m.scrollOffset, m.paused = m.maxScrollOffset(), true
		case key.Matches(msg, m.keys.Jump):
			m.scrollOffset, m.paused = 0, false
		case key.Matches(msg, m.keys.Help):
			m.showHelp = !m.showHelp
			m.help.ShowAll = m.showHelp
		}
	}
	return m, nil
}

func (m *model) scrollUp(n int) {
	max := m.maxScrollOffset()
	if max == 0 {
		return
	}
	m.scrollOffset += n
	if m.scrollOffset > max {
		m.scrollOffset = max
	}
	m.paused = true
}

func (m *model) scrollDown(n int) {
	if m.scrollOffset == 0 {
		return
	}
	m.scrollOffset -= n
	if m.scrollOffset <= 0 {
		m.scrollOffset = 0
		m.paused = false // back at top → resume autoscroll
	}
}

func (m model) View() tea.View {
	v := tea.NewView(m.viewContent())
	v.AltScreen = true
	// Enable mouse-wheel scroll. Cell motion is the lighter mode (fires
	// only on press / release / wheel, not on every cell move) and is
	// what users expect from `less`/`htop`. We don't react to clicks —
	// only MouseWheelMsg in Update.
	v.MouseMode = tea.MouseModeCellMotion
	return v
}

func (m model) viewContent() string {
	if m.windowH > 0 && m.windowH < smallScreenHeight {
		return m.styles.chrome.Render("pgloupe — terminal too small")
	}

	var b strings.Builder
	b.WriteString(m.headerLine())
	b.WriteString("\n")

	visible := m.visibleRows()
	count := 0
	m.events.forEach(m.scrollOffset, visible, func(e Event) bool {
		b.WriteString(m.renderRow(e))
		b.WriteString("\n")
		count++
		return true
	})
	for i := count; i < visible; i++ {
		b.WriteString("\n")
	}

	b.WriteString(m.footerLine())
	return b.String()
}

func (m model) headerLine() string {
	parts := []string{m.styles.chrome.Render("pgloupe")}
	if m.paused {
		parts = append(parts, m.styles.paused.Render("[PAUSED]"))
	}
	if m.dropped != nil {
		if d := m.dropped.Load(); d > 0 {
			parts = append(parts, m.styles.drop.Render(fmt.Sprintf("%d dropped", d)))
		}
	}
	parts = append(parts, m.styles.chrome.Render(fmt.Sprintf("%d events", m.events.len())))
	return strings.Join(parts, "  ")
}

func (m model) footerLine() string {
	if m.showHelp {
		return m.help.View(m.keys)
	}
	return m.styles.chrome.Render(m.help.ShortHelpView(m.keys.ShortHelp()))
}

func (m model) renderRow(e Event) string {
	ts := e.Started.Format("15:04:05.000")
	sql := normalizeSQL(e.SQL)
	if sql == "" {
		sql = "—"
	}
	switch e.Status() {
	case StatusErr:
		return m.styles.err.Render(fmt.Sprintf("%s  ERR     %-12s %s",
			ts, "—", truncate(sql, m.truncateW)+"  →  "+e.Err))
	case StatusInflight:
		return m.styles.inflight.Render(fmt.Sprintf("%s  …       %-12s %s",
			ts, "—", truncate(sql, m.truncateW)))
	default:
		dur := e.Duration().Round(time.Microsecond * 100).String()
		return m.styles.ok.Render(fmt.Sprintf("%s  %-7s %-12s %s",
			ts, dur, e.Tag, truncate(sql, m.truncateW)))
	}
}

// normalizeSQL collapses runs of whitespace (including newlines) into single
// spaces so multi-line SQL rendered in the row doesn't blow up the layout.
func normalizeSQL(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// truncate clips a string to at most n display runes, appending an ellipsis.
// Operates on runes (not bytes) so multi-byte UTF-8 isn't sliced mid-codepoint.
// n <= 0 means "no truncation" (full width); n == 1 returns "…" alone.
func truncate(s string, n int) string {
	if n <= 0 {
		return s
	}
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	if n == 1 {
		return "…"
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}

// RunTUI starts the Bubble Tea program and forwards events from the channel
// into model updates. Blocks until the user quits or events is closed.
func RunTUI(events <-chan Event, maxEvents int, dropped *atomic.Uint64, opts ...ProgramOption) error {
	m := newModel(maxEvents)
	m.dropped = dropped
	for _, opt := range opts {
		opt(&m)
	}
	p := tea.NewProgram(m)
	go func() {
		for ev := range events {
			p.Send(eventMsg(ev))
		}
	}()
	_, err := p.Run()
	return err
}

// ProgramOption tunes the TUI before it starts.
type ProgramOption func(*model)

// WithNoColor swaps the model's render styles to bold/italic-only
// equivalents. Per-instance — does not mutate package globals — so a
// process can host a colored TUI and an uncolored TUI without state
// crossover, and tests don't leak rendering state across cases.
func WithNoColor() ProgramOption {
	return func(m *model) {
		m.noColor = true
		m.styles = plainStyles()
	}
}

// WithTruncateWidth sets the SQL truncation column. w <= 0 means
// "no truncation" — render the full normalized SQL. The flag layer
// (--truncate-sql 0) relies on this to mean "show everything".
func WithTruncateWidth(w int) ProgramOption {
	return func(m *model) {
		if w < 0 {
			w = 0
		}
		m.truncateW = w
	}
}
