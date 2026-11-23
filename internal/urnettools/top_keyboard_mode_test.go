package urnettools

import (
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect/internal/tui"
	"github.com/urnetwork/connect/internal/tui/tcellui"
)

// footerLine renders the model and returns the bottom row of the screen.
func footerLine(t *testing.T, m *topModel, w, h int) string {
	t.Helper()
	b := tui.New(w, h)
	m.render(b)
	lines := strings.Split(b.String(), "\n")
	return lines[len(lines)-1]
}

func runeKey(r rune) tcellui.Event { return tcellui.Event{Kind: tcellui.EventRune, Rune: r} }

func TestTopKeyboardModeKeyTogglesBothWays(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	if !m.mouse {
		t.Fatal("top should start with mouse reporting on")
	}
	if eff := m.handle(runeKey('K')); eff != topNone {
		t.Fatalf("K gave effect %v, want topNone", eff)
	}
	if m.mouse {
		t.Fatal("K should have switched to keyboard mode")
	}
	if !strings.Contains(m.notice, "Keyboard Mode") {
		t.Fatalf("notice = %q, want it to name Keyboard Mode", m.notice)
	}
	m.handle(runeKey('K'))
	if !m.mouse {
		t.Fatal("the second K should have switched back to mouse mode")
	}
	if !strings.Contains(m.notice, "Mouse Mode") {
		t.Fatalf("notice = %q, want it to name Mouse Mode", m.notice)
	}
}

// The old M key stays as an alias so nobody loses the toggle, but it is not
// advertised anywhere the operator reads.
func TestTopMouseKeyStaysAsAnAlias(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	m.handle(runeKey('M'))
	if m.mouse {
		t.Fatal("M should still switch to keyboard mode")
	}
	m.handle(runeKey('M'))
	if !m.mouse {
		t.Fatal("M should still switch back to mouse mode")
	}
	for _, line := range helpLines {
		if strings.HasPrefix(line, "M ") {
			t.Fatalf("the help overlay still advertises the alias: %q", line)
		}
	}
	if line := footerLine(t, m, 120, 40); strings.Contains(line, "M mouse") || strings.Contains(line, "M keyboard") {
		t.Fatalf("the footer still advertises the alias: %q", line)
	}
}

func TestTopFooterShowsTheModeHintAndMarker(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	m.apply(0, topFlowing(t), nil)

	line := footerLine(t, m, 120, 40)
	if !strings.Contains(line, topModeHintMouse) {
		t.Fatalf("mouse mode footer %q should offer %q", line, topModeHintMouse)
	}
	if !strings.HasSuffix(strings.TrimRight(line, " "), topModeLabelMouse) {
		t.Fatalf("mouse mode footer %q should end with the %q marker", line, topModeLabelMouse)
	}

	m.handle(runeKey('K'))
	clock.t = topBase.Add(10 * time.Second) // let the message bar go first
	line = footerLine(t, m, 120, 40)
	if !strings.Contains(line, topModeHintKeys) {
		t.Fatalf("keyboard mode footer %q should offer %q", line, topModeHintKeys)
	}
	if !strings.HasSuffix(strings.TrimRight(line, " "), topModeLabelKeys) {
		t.Fatalf("keyboard mode footer %q should end with the %q marker", line, topModeLabelKeys)
	}
}

// The footer never wraps and never cuts a hint in half: at a narrow width the
// hints that do not fit whole are left out.
func TestTopFooterDegradesAtEveryWidth(t *testing.T) {
	clock := &fakeClock{t: topBase}
	for _, mode := range []bool{true, false} {
		m := newTestModel(tui.DefaultTheme(), topProviders(2), clock)
		m.mouse = mode
		m.apply(0, topFlowing(t), nil)
		want := topModeHintMouse
		if !mode {
			want = topModeHintKeys
		}
		for _, w := range []int{120, 100, 80, 60} {
			b := tui.New(w, 1)
			m.drawFooter(b)
			line := b.String()
			if strings.Contains(line, "\n") {
				t.Fatalf("%d cols: the footer wrapped: %q", w, line)
			}
			if tui.StringWidth(line) > w {
				t.Fatalf("%d cols: the footer is %d cells wide: %q", w, tui.StringWidth(line), line)
			}
			if w >= 80 && !strings.Contains(line, want) {
				t.Fatalf("%d cols: the footer %q should carry %q whole", w, line, want)
			}
			// A label cut at the edge would leave a prefix of the hint that
			// looks like a hint but is not one.
			if i := strings.Index(line, "K "); i >= 0 {
				tail := strings.TrimRight(line[i:], " ")
				if tail != want && !strings.HasPrefix(tail, want+" ") {
					t.Fatalf("%d cols: the mode hint was cut: %q", w, line)
				}
			}
		}
	}
}

// The menu owns the keyboard, so K must not reach the screen behind it.
func TestTopKeyboardAndCopyKeysStayOutOfTheMenu(t *testing.T) {
	m, _ := menuModel(t)
	pressKey(m, 'm')
	pressKey(m, 'K')
	if !m.mouse {
		t.Fatal("K changed the mode from under the menu")
	}
}

// Every other plain key works while the overview is up (it swallows only the
// keys that belong to it), and K follows them.
func TestTopKeyboardModeFollowsTheOverview(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(2), clock)
	m.overview = true
	m.handle(runeKey('K'))
	if m.mouse {
		t.Fatal("K should work in the overview like the other plain keys")
	}
	// The keys the overview owns stay swallowed.
	m.handle(runeKey('?'))
	if m.help {
		t.Fatal("? should stay swallowed in the overview")
	}
}

func TestTopNoMouseStartsInKeyboardMode(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	m.mouse = false
	m.apply(0, topFlowing(t), nil)
	line := footerLine(t, m, 120, 40)
	if !strings.Contains(line, topModeHintKeys) {
		t.Fatalf("keyboard mode footer %q should offer %q", line, topModeHintKeys)
	}
	if m.handle(runeKey('K')); !m.mouse {
		t.Fatal("K should leave keyboard mode")
	}
}

func TestTopTerminalReportsMouse(t *testing.T) {
	env := func(kv map[string]string) func(string) string {
		return func(k string) string { return kv[k] }
	}
	for _, tc := range []struct {
		term string
		want bool
	}{
		{"xterm-256color", true},
		{"screen-256color", true},
		{"dumb", false},
		{"", false},
	} {
		if got := topTerminalReportsMouse(env(map[string]string{"TERM": tc.term})); got != tc.want {
			t.Errorf("TERM=%q: reports mouse = %v, want %v", tc.term, got, tc.want)
		}
	}
}

// A terminal that cannot report the mouse says so instead of silently doing
// nothing when K asks for mouse mode.
func TestTopKeyboardModeSaysWhenTheTerminalCannotReportTheMouse(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	m.mouse, m.mouseSupported = false, false
	m.handle(runeKey('K'))
	if m.mouse {
		t.Fatal("a terminal without mouse reporting should stay in keyboard mode")
	}
	if !strings.Contains(strings.ToLower(m.notice), "mouse") {
		t.Fatalf("notice = %q, want it to explain why nothing changed", m.notice)
	}
}

// The mode message is a status line for a few seconds and then gives the row
// back to the hints, so the footer is not permanently rewritten.
func TestTopModeNoticeExpires(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	m.apply(0, topFlowing(t), nil)
	m.handle(runeKey('K'))
	if line := footerLine(t, m, 120, 40); !strings.Contains(line, "Keyboard Mode") {
		t.Fatalf("the toggle should replace the footer for a moment: %q", line)
	}
	clock.t = topBase.Add(10 * time.Second)
	if line := footerLine(t, m, 120, 40); !strings.Contains(line, topModeHintKeys) {
		t.Fatalf("the hints should be back after the message: %q", line)
	}
}
