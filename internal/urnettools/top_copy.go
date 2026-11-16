package urnettools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/urnetwork/connect/internal/tui"
)

// top_copy.go turns the screen into plain text an operator can paste into a
// chat or a ticket: no styles, trailing spaces trimmed, box characters kept,
// or replaced by + - | when --plain is asked for. The copy is written to the
// tool's state dir on exit, and printed to stdout when --copy asks for it.

// boxToASCII swaps the box drawing set for the ASCII one, so a screen pasted
// into a chat that eats unicode still lines up.
var boxToASCII = strings.NewReplacer(
	"─", "-",
	"│", "|",
	"┌", "+", "┐", "+", "└", "+", "┘", "+",
	"├", "+", "┤", "+", "┬", "+", "┴", "+", "┼", "+",
	"╭", "+", "╮", "+", "╰", "+", "╯", "+",
)

// copyText renders b as the plain text form of the screen. Buffer.String
// already drops styles, skips continuation cells and trims trailing spaces, so
// all that is left is the box-character decision.
func copyText(b *tui.Buffer, plain bool) string {
	s := b.String()
	if !plain {
		return s
	}
	return boxToASCII.Replace(s)
}

// topStateDir is the tool's state dir: $XDG_STATE_HOME/urnet-tools, or
// ~/.local/state/urnet-tools when that is unset.
func topStateDir() string {
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "urnet-tools")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".local", "state", "urnet-tools")
}

// topCopyPath names the copy file for one run: top-<time>.txt.
func topCopyPath(now time.Time) string {
	return filepath.Join(topStateDir(), "top-"+now.Format("20060102-150405")+".txt")
}

// writeTopCopy writes text to the state dir. The dir is created 0700 and the
// file 0600: a copy can carry addresses and versions, so it stays private to
// the user. It returns the path it wrote.
func writeTopCopy(text string, now time.Time) (string, error) {
	dir := topStateDir()
	if dir == "" {
		return "", fmt.Errorf("no state dir to write the copy to")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := topCopyPath(now)
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// copyOnExit renders the last frame of the screen, writes it to the state
// dir and remembers the text so the caller can print it once the terminal is
// back. The terminal is still in raw mode when this runs, so nothing is
// written to stdout here.
func (m *topModel) copyOnExit(b *tui.Buffer) {
	text := copyText(b, m.copyPlain)
	path, err := writeTopCopy(text, m.now())
	if err != nil {
		m.copyPath, m.copyErr = "", err
		return
	}
	m.copyPath, m.copyText, m.copyErr = path, text, nil
}
