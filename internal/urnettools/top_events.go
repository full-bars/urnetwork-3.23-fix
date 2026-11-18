package urnettools

import (
	"fmt"
	"strings"
	"time"

	"github.com/urnetwork/connect/internal/tui"
)

// topEventView is one line of the Events panel: an event, plus how many
// consecutive repeats it stands for. Consecutive repeats collapse into one row
// with a count, so a storm of the same line reads as one row with a count
// instead of filling the panel with copies.
type topEventView struct {
	At    time.Time
	Text  string
	Level topLevel
	Count int
}

// eventTime renders when an event happened: relative ("12m ago", "3h ago") in
// the normal view, absolute in the zoomed view, where the exact minute is the
// thing an operator is looking for.
func eventTime(at, now time.Time, zoom bool) string {
	if zoom {
		if !sameDay(at, now) {
			return at.Format("01-02 15:04")
		}
		return at.Format("15:04:05")
	}
	age := now.Sub(at)
	switch {
	case age < time.Minute:
		return "just now"
	case age < time.Hour:
		return fmt.Sprintf("%dm ago", int(age.Minutes()))
	case age < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(age.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(age.Hours()/24))
	}
}

// eventIcon is the one character that says the severity before the color is
// read: a dot for information, an exclamation for a warning, a cross for a
// failure. The theme carries the symbol for each role; the ASCII fallback
// stands in for the glyphs a dumb terminal cannot draw, so the meaning survives
// the fallback unchanged.
func eventIcon(level topLevel, th tui.Theme) rune {
	if th.ASCII {
		switch level {
		case topInfo:
			return 'i'
		case topGood:
			return '+'
		case topWarn:
			return '!'
		case topBad:
			return 'x'
		}
		return '?'
	}
	switch level {
	case topInfo:
		return iconOr(th.IconOK, '•')
	case topGood:
		return '✓'
	case topWarn:
		return iconOr(th.IconWarn, '!')
	case topBad:
		return iconOr(th.IconBad, '✗')
	}
	return '?'
}

// iconOr falls back when the theme has no symbol for the role.
func iconOr(r, fallback rune) rune {
	if r == 0 {
		return fallback
	}
	return r
}

// eventFilter is what the Events view keeps: a minimum severity and a text the
// line must contain. Both empty mean "everything".
type eventFilter struct {
	MinLevel topLevel
	Text     string
}

// keep reports whether an event passes the filter. It is a pure function on its
// inputs so the filtering can be tested without a model or a terminal.
func (f eventFilter) keep(e topEvent) bool {
	if e.Level < f.MinLevel {
		return false
	}
	return f.Text == "" || strings.Contains(strings.ToLower(e.Text), strings.ToLower(f.Text))
}

// filterEvents keeps the events that pass f, in order.
func filterEvents(events []topEvent, f eventFilter) []topEvent {
	out := make([]topEvent, 0, len(events))
	for _, e := range events {
		if f.keep(e) {
			out = append(out, e)
		}
	}
	return out
}

// collapseEvents collapses consecutive events with the same text and level into
// one row with a count. The input is newest first (what sortedEvents returns);
// the output keeps that order.
func collapseEvents(events []topEvent) []topEventView {
	if len(events) == 0 {
		return nil
	}
	out := make([]topEventView, 0, len(events))
	for _, e := range events {
		if n := len(out); n > 0 && out[n-1].Text == e.Text && out[n-1].Level == e.Level {
			out[n-1].Count++
			continue
		}
		out = append(out, topEventView{At: e.At, Text: e.Text, Level: e.Level, Count: 1})
	}
	return out
}

// eventRow renders one collapsed event as the text the Events panel draws:
// "12m ago  !  text  x12".
func eventRow(v topEventView, now time.Time, zoom bool, th tui.Theme) string {
	line := eventTime(v.At, now, zoom) + "  " + string(eventIcon(v.Level, th)) + "  " + v.Text
	if v.Count > 1 {
		line += "  x" + fmt.Sprint(v.Count)
	}
	return line
}

// scrollEvents moves the zoomed events view by delta rows. It never scrolls
// past the top of the list, and never more than the list holds.
func (m *topModel) scrollEvents(delta int) {
	n := len(collapseEvents(filterEvents(m.sortedEvents(), m.eventFilter)))
	m.eventScroll = min(max(m.eventScroll+delta, 0), max(n-max(m.eventHeight, 1), 0))
}
