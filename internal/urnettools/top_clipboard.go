package urnettools

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/urnetwork/connect/internal/tui"
	"github.com/urnetwork/connect/internal/tui/tcellui"
)

// top_clipboard.go puts the screen on the clipboard for the `c` key. Three
// methods are tried in order: a local clipboard tool, an OSC 52 escape
// sequence written to the terminal, and the --copy file. Each method is behind
// a seam so a test never starts a process or touches a real clipboard.

const (
	// topClipTimeout bounds one clipboard tool.
	topClipTimeout = 2 * time.Second
	// topClipMaxBytes caps the text sent to the terminal clipboard. Many
	// terminals refuse a payload over about 100 KB, and base64 is a third
	// larger than the text, so the text stops well under that.
	topClipMaxBytes = 64 << 10
)

// topClipEnv is the machine facts that decide which methods can work. It is a
// value rather than the live environment so the choice is testable.
type topClipEnv struct {
	GOOS    string
	SSH     bool
	Wayland bool
	X11     bool
	WSL     bool
	Tmux    bool
	Screen  bool
}

// topClipRunner runs one clipboard tool: argv, the text on stdin, and nothing
// else. A non-nil error means the tool did not take the text.
type topClipRunner func(name string, args []string, stdin string) error

// topClipTerminal writes one escape sequence to the terminal.
type topClipTerminal func(seq string) error

// topClipMethod is one way to put text on the clipboard.
type topClipMethod struct {
	name string
	// terminal marks a method that only writes an escape sequence, so the
	// terminal cannot confirm that the clipboard took the text.
	terminal bool
	copy     func(text string) error
}

// topClipResult is what one copy did.
type topClipResult struct {
	method    string
	terminal  bool
	lines     int
	truncated bool
	// err says why no clipboard method took the text.
	err error
	// path is where the text was saved instead, when it was.
	path string
	// saveErr says why saving failed, when it did.
	saveErr error
}

// topClipboard holds the seams a copy uses.
type topClipboard struct {
	env  topClipEnv
	run  topClipRunner
	term topClipTerminal
	save func(text string, now time.Time) (string, error)
	now  func() time.Time
}

// methods returns the ways this session can reach the clipboard, in the order
// they are tried: a local clipboard tool first, then the terminal.
func (c *topClipboard) methods() []topClipMethod {
	var out []topClipMethod
	// A remote session has no local display the tool could reach, so the
	// terminal is the only way out.
	if !c.env.SSH {
		switch {
		case c.env.GOOS == "darwin":
			out = append(out, c.exec("pbcopy", nil))
		case c.env.WSL:
			// WSL ships clip.exe, and it works without a display server. It
			// reads the Windows clipboard format, so the text goes in as
			// UTF-16 with a byte order mark; box characters would otherwise
			// arrive mangled.
			out = append(out, c.execEncoded("clip.exe", nil, utf16LE))
		}
		if c.env.Wayland {
			out = append(out, c.exec("wl-copy", nil))
		}
		if c.env.X11 {
			out = append(out,
				c.exec("xclip", []string{"-selection", "clipboard"}),
				c.exec("xsel", []string{"--clipboard", "--input"}),
			)
		}
	}
	if c.term != nil {
		out = append(out, c.osc52())
	}
	return out
}

// exec is one clipboard tool. The text goes on its stdin and its exit status is
// the only thing top sees.
func (c *topClipboard) exec(name string, args []string) topClipMethod {
	return c.execEncoded(name, args, nil)
}

// execEncoded is exec with a way to turn the text into the bytes the tool
// reads; nil sends the text as it is, which is what the Unix tools want.
func (c *topClipboard) execEncoded(name string, args []string, encode func(string) string) topClipMethod {
	return topClipMethod{name: name, copy: func(text string) error {
		if c.run == nil {
			return fmt.Errorf("%s is not available", name)
		}
		if encode != nil {
			text = encode(text)
		}
		return c.run(name, args, text)
	}}
}

// utf16LE encodes text as the Windows clipboard tools read it: a byte order
// mark, then two bytes per UTF-16 code unit.
func utf16LE(text string) string {
	units := utf16.Encode([]rune(text))
	out := make([]byte, 0, 2+2*len(units))
	out = append(out, 0xff, 0xfe)
	for _, u := range units {
		out = append(out, byte(u), byte(u>>8))
	}
	return string(out)
}

// osc52 writes the text to the terminal's clipboard. Nothing comes back, so the
// result is never reported as a confirmed copy.
func (c *topClipboard) osc52() topClipMethod {
	name := "OSC 52"
	switch {
	case c.env.Tmux:
		name = "OSC 52 in tmux"
	case c.env.Screen:
		name = "OSC 52 in screen"
	}
	return topClipMethod{name: name, terminal: true, copy: func(text string) error {
		if c.term == nil {
			return fmt.Errorf("no terminal to write to")
		}
		return c.term(osc52Sequence(text, c.env.Tmux, c.env.Screen))
	}}
}

// osc52Sequence builds the escape sequence that asks the terminal to set the
// clipboard. Every ESC inside it is doubled when tmux passes it on, and a GNU
// screen session needs the whole sequence inside its own DCS.
func osc52Sequence(text string, tmux, screen bool) string {
	seq := "\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte(text)) + "\a"
	switch {
	case tmux:
		return "\x1bPtmux;" + strings.ReplaceAll(seq, "\x1b", "\x1b\x1b") + "\x1b\\"
	case screen:
		return "\x1bP" + seq + "\x1b\\"
	}
	return seq
}

// capClipText cuts text to the payload cap on a line boundary, so a copy never
// sends half a line.
func capClipText(text string) (string, bool) {
	if len(text) <= topClipMaxBytes {
		return text, false
	}
	cut := text[:topClipMaxBytes]
	if i := strings.LastIndexByte(cut, '\n'); i > 0 {
		cut = cut[:i]
	}
	return cut, true
}

// clipLineCount counts the lines a copy sends, the way the text is read: every
// newline starts another line.
func clipLineCount(text string) int {
	if text == "" {
		return 0
	}
	return strings.Count(text, "\n") + 1
}

// copy sends text to the clipboard, and to the --copy file when nothing takes
// it. The first method that accepts the text wins; a method that fails is
// reported only when every method has failed.
func (c *topClipboard) copy(text string) topClipResult {
	text, truncated := capClipText(text)
	res := topClipResult{lines: clipLineCount(text), truncated: truncated}
	var reasons []string
	for _, m := range c.methods() {
		err := m.copy(text)
		if err == nil {
			res.method, res.terminal = m.name, m.terminal
			return res
		}
		reasons = append(reasons, m.name+": "+err.Error())
	}
	if len(reasons) == 0 {
		res.err = errors.New("no clipboard method is available")
	} else {
		res.err = errors.New(strings.Join(reasons, "; "))
	}
	if c.save == nil {
		return res
	}
	now := time.Now
	if c.now != nil {
		now = c.now
	}
	path, err := c.save(text, now())
	if err != nil {
		res.saveErr = err
		return res
	}
	res.path = path
	return res
}

// newTopClipboard builds the clipboard for this run: the environment facts, the
// process runner, the terminal as the OSC 52 writer, and the --copy file as the
// last resort.
func newTopClipboard(scr tcellui.Screen) *topClipboard {
	c := &topClipboard{
		env: topClipEnv{
			GOOS:    runtime.GOOS,
			SSH:     os.Getenv("SSH_CONNECTION") != "" || os.Getenv("SSH_TTY") != "",
			Wayland: os.Getenv("WAYLAND_DISPLAY") != "",
			X11:     os.Getenv("DISPLAY") != "",
			WSL:     os.Getenv("WSL_DISTRO_NAME") != "" || procVersionSaysWSL(),
			Tmux:    os.Getenv("TMUX") != "",
			Screen:  os.Getenv("STY") != "",
		},
		run:  runTopClipCommand,
		save: writeTopCopy,
		now:  time.Now,
	}
	if scr != nil {
		c.term = func(seq string) error { return scr.WriteTerminal([]byte(seq)) }
	}
	return c
}

// procVersionSaysWSL recognizes the WSL kernel the same way the rest of the
// tool does, for the case where WSL_DISTRO_NAME is not in the environment.
func procVersionSaysWSL() bool {
	b, err := os.ReadFile("/proc/version")
	if err != nil {
		return false
	}
	return strings.Contains(strings.ToLower(string(b)), "microsoft")
}

// runTopClipCommand runs one clipboard tool with argv and the text on stdin. No
// shell is involved. The tool's own output is discarded so it cannot land on the
// screen, and the wait is bounded: a clipboard tool that hangs must not hold the
// dashboard.
func runTopClipCommand(name string, args []string, stdin string) error {
	path, err := exec.LookPath(name)
	if err != nil {
		return err
	}
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer devNull.Close()
	// A file for stdin rather than a pipe the runtime copies: wl-copy forks a
	// child that serves the selection, and a copied writer could keep the
	// parent's Wait waiting on it.
	pr, pw, err := os.Pipe()
	if err != nil {
		return err
	}
	defer pr.Close()
	go func() {
		_, _ = pw.WriteString(stdin)
		_ = pw.Close()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), topClipTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = pr, devNull, devNull
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("gave up after %s", topClipTimeout)
		}
		return err
	}
	return nil
}

// topClipMessage turns one copy result into the line the footer shows. A
// terminal gives no acknowledgement, so its message says the text was sent, not
// that the clipboard took it.
func topClipMessage(r topClipResult) string {
	if r.method != "" {
		msg := fmt.Sprintf("copied %d lines with %s", r.lines, r.method)
		if r.terminal {
			msg = fmt.Sprintf("sent %d lines to the terminal clipboard (%s)", r.lines, r.method)
		}
		if r.truncated {
			msg += fmt.Sprintf(", cut to %d KiB", topClipMaxBytes>>10)
		}
		return msg
	}
	if r.saveErr != nil {
		return fmt.Sprintf("could not copy the screen: %v; could not write the copy: %v", r.err, r.saveErr)
	}
	if r.path != "" {
		return fmt.Sprintf("no clipboard here (%v); saved %d lines to %s", r.err, r.lines, r.path)
	}
	return fmt.Sprintf("could not copy the screen: %v", r.err)
}

// copyFrame sends the frame the loop last drew to the clipboard. It runs on the
// loop goroutine, so it reads the buffer the loop owns and never writes to the
// terminal itself: the result is reported as the next frame's message line.
func (m *topModel) copyFrame(b *tui.Buffer) {
	if m.clip == nil || b.Width() == 0 || b.Height() == 0 {
		return
	}
	m.setNotice(topClipMessage(m.clip.copy(copyText(b, m.copyPlain))))
}
