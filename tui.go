package main

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

var (
	errStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	inflightSty = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))
	okStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	chromeSty   = lipgloss.NewStyle().Foreground(lipgloss.Color("63")).Bold(true)
)

type keyMap struct{ Quit, Pause, Up, Down, Jump key.Binding }

var keys = keyMap{
	Quit:  key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "quit")),
	Pause: key.NewBinding(key.WithKeys("p"), key.WithHelp("p", "pause")),
	Up:    key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/k", "scroll up")),
	Down:  key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "scroll down")),
	Jump:  key.NewBinding(key.WithKeys("g"), key.WithHelp("g", "jump to newest")),
}

type eventMsg Event

type model struct {
	events       *ringBuffer
	windowW      int
	windowH      int
	paused       bool
	scrollOffset int
}

func newModel(maxEvents int) model {
	return model{events: newRingBuffer(maxEvents)}
}

func (m model) Init() tea.Cmd { return nil }

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.windowW, m.windowH = msg.Width, msg.Height
	case eventMsg:
		m.events.push(Event(msg))
		if !m.paused {
			m.scrollOffset = 0
		}
	case tea.KeyPressMsg:
		switch {
		case key.Matches(msg, keys.Quit):
			return m, tea.Quit
		case key.Matches(msg, keys.Pause):
			m.paused = !m.paused
		case key.Matches(msg, keys.Up):
			if m.scrollOffset < m.events.len()-1 {
				m.scrollOffset++
				m.paused = true
			}
		case key.Matches(msg, keys.Down):
			if m.scrollOffset > 0 {
				m.scrollOffset--
			}
		case key.Matches(msg, keys.Jump):
			m.scrollOffset, m.paused = 0, false
		}
	}
	return m, nil
}

func (m model) View() tea.View {
	var b strings.Builder
	header := "pgloupe"
	if m.paused {
		header += " [PAUSED]"
	}
	b.WriteString(chromeSty.Render(header))
	b.WriteString("\n")

	visible := m.windowH - 2
	if visible < 1 {
		visible = 1
	}
	snap := m.events.snapshot()
	end := m.scrollOffset + visible
	if end > len(snap) {
		end = len(snap)
	}
	for _, e := range snap[m.scrollOffset:end] {
		b.WriteString(renderRow(e))
		b.WriteString("\n")
	}
	b.WriteString(chromeSty.Render("q quit · p pause · ↑↓ scroll · g newest"))
	v := tea.NewView(b.String())
	v.AltScreen = true
	return v
}

func renderRow(e Event) string {
	ts := e.Started.Format("15:04:05.000")
	switch e.Status() {
	case StatusErr:
		return errStyle.Render(fmt.Sprintf("%s  ERR     %-50s  %s", ts, truncate(e.SQL, 50), e.Err))
	case StatusInflight:
		return inflightSty.Render(fmt.Sprintf("%s  …       %s", ts, truncate(e.SQL, 80)))
	default:
		return okStyle.Render(fmt.Sprintf("%s  %-7s %-10s %s",
			ts, e.Duration().Round(100_000), e.Tag, truncate(e.SQL, 80)))
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// RunTUI starts the Bubble Tea program and forwards events from the channel
// into model updates. Blocks until the user quits.
func RunTUI(events <-chan Event, maxEvents int) error {
	p := tea.NewProgram(newModel(maxEvents))
	go func() {
		for ev := range events {
			p.Send(eventMsg(ev))
		}
	}()
	_, err := p.Run()
	return err
}
