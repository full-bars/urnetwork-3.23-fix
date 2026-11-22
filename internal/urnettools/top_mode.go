package urnettools

import "time"

// The two terminal modes top runs in. Mouse Mode has the terminal report
// clicks and hover to top; Keyboard Mode turns that reporting off so the
// terminal's own text selection works. K switches between them.
const (
	// topModeHintMouse is the footer hint shown in Mouse Mode: the mode K
	// would switch to.
	topModeHintMouse = "K keyboard mode"
	// topModeHintKeys is the same hint in Keyboard Mode.
	topModeHintKeys = "K mouse mode"
	// topModeLabelMouse and topModeLabelKeys name the mode that is on, drawn
	// at the right end of the footer row.
	topModeLabelMouse = "mouse"
	topModeLabelKeys  = "keyboard"

	topNoticeMouse    = "Mouse Mode: top uses the mouse. K for Keyboard Mode"
	topNoticeKeyboard = "Keyboard Mode: select text with your terminal. K for Mouse Mode"
	topNoticeNoMouse  = "This terminal cannot report the mouse. Keyboard Mode stays on."
)

// topNoticeFor is how long a one-line message holds the footer row.
const topNoticeFor = 4 * time.Second

// topTerminalReportsMouse reports whether the terminal named by the
// environment can report the mouse. A dumb or unset TERM cannot, so top starts
// in Keyboard Mode there rather than waiting for clicks that never come.
func topTerminalReportsMouse(getenv func(string) string) bool {
	term := getenv("TERM")
	return term != "" && term != "dumb"
}

// toggleMouseMode switches between Mouse Mode and Keyboard Mode and says which
// one is on. A terminal that cannot report the mouse stays in Keyboard Mode and
// says so, rather than accepting the key and doing nothing.
func (m *topModel) toggleMouseMode() {
	if !m.mouse && !m.mouseSupported {
		m.setNotice(topNoticeNoMouse)
		return
	}
	m.mouse = !m.mouse
	if m.mouse {
		m.setNotice(topNoticeMouse)
		return
	}
	m.setNotice(topNoticeKeyboard)
}

// setNotice shows text on the footer row for topNoticeFor. The renderer draws
// it in place of the hints, so the row is never half a message and half a hint.
func (m *topModel) setNotice(text string) {
	m.notice, m.noticeUntil = text, m.now().Add(topNoticeFor)
}

// noticeActive reports whether a message still holds the footer row.
func (m *topModel) noticeActive() bool {
	return m.notice != "" && m.now().Before(m.noticeUntil)
}
