package urnettools

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect/internal/tui"
)

// fakeClip records every clipboard tool the copy tries, in order, and fails the
// ones the test names. No process is started.
type fakeClip struct {
	fail  map[string]error
	tried []string
	stdin []string
}

func (f *fakeClip) run(name string, args []string, stdin string) error {
	f.tried = append(f.tried, name+" "+strings.Join(args, " "))
	f.stdin = append(f.stdin, stdin)
	if err := f.fail[name]; err != nil {
		return err
	}
	return nil
}

// fakeTerminal records the escape sequences written to the terminal.
type fakeTerminal struct {
	seqs []string
	err  error
}

func (f *fakeTerminal) write(seq string) error {
	f.seqs = append(f.seqs, seq)
	return f.err
}

// fakeSave records the text handed to the --copy file.
type fakeSave struct {
	path  string
	err   error
	texts []string
}

func (f *fakeSave) save(text string, now time.Time) (string, error) {
	f.texts = append(f.texts, text)
	return f.path, f.err
}

func clipFor(env topClipEnv, run *fakeClip, term *fakeTerminal, save *fakeSave) *topClipboard {
	c := &topClipboard{env: env, now: func() time.Time { return topBase }}
	if run != nil {
		c.run = run.run
	}
	if term != nil {
		c.term = term.write
	}
	if save != nil {
		c.save = save.save
	}
	return c
}

func methodNames(methods []topClipMethod) []string {
	out := make([]string, len(methods))
	for i, m := range methods {
		out[i] = m.name
	}
	return out
}

func TestTopClipboardMethodsPerEnvironment(t *testing.T) {
	x11 := topClipEnv{GOOS: "linux", X11: true}
	wayland := topClipEnv{GOOS: "linux", Wayland: true}
	for _, tc := range []struct {
		name string
		env  topClipEnv
		term bool
		want []string
	}{
		{"wayland", wayland, true, []string{"wl-copy", "OSC 52"}},
		{"x11", x11, true, []string{"xclip", "xsel", "OSC 52"}},
		{"both", topClipEnv{GOOS: "linux", Wayland: true, X11: true}, true, []string{"wl-copy", "xclip", "xsel", "OSC 52"}},
		{"wsl", topClipEnv{GOOS: "linux", WSL: true, Wayland: true, X11: true}, true, []string{"clip.exe", "wl-copy", "xclip", "xsel", "OSC 52"}},
		{"macos", topClipEnv{GOOS: "darwin"}, true, []string{"pbcopy", "OSC 52"}},
		{"ssh", topClipEnv{GOOS: "linux", SSH: true, Wayland: true, X11: true}, true, []string{"OSC 52"}},
		{"ssh tmux", topClipEnv{GOOS: "linux", SSH: true, Tmux: true}, true, []string{"OSC 52 in tmux"}},
		{"ssh screen", topClipEnv{GOOS: "linux", SSH: true, Screen: true}, true, []string{"OSC 52 in screen"}},
		{"local tmux", topClipEnv{GOOS: "linux", Wayland: true, Tmux: true}, true, []string{"wl-copy", "OSC 52 in tmux"}},
		{"no terminal", x11, false, []string{"xclip", "xsel"}},
		{"nothing", topClipEnv{GOOS: "linux"}, false, nil},
	} {
		var term *fakeTerminal
		if tc.term {
			term = &fakeTerminal{}
		}
		c := clipFor(tc.env, &fakeClip{}, term, nil)
		if got := methodNames(c.methods()); strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Errorf("%s: methods = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestTopClipboardFallsThroughToTheNextTool(t *testing.T) {
	run := &fakeClip{fail: map[string]error{"wl-copy": errors.New("exit status 1")}}
	term := &fakeTerminal{}
	c := clipFor(topClipEnv{GOOS: "linux", Wayland: true, X11: true}, run, term, nil)
	text := "q quit\n? help\n"
	res := c.copy(text)
	if len(run.tried) == 0 || !strings.HasPrefix(run.tried[0], "wl-copy") {
		t.Fatalf("tried = %v, want wl-copy first", run.tried)
	}
	if !strings.HasPrefix(run.tried[1], "xclip -selection clipboard") {
		t.Fatalf("second tool = %q, want xclip with the clipboard selection", run.tried[1])
	}
	if res.method != "xclip" {
		t.Fatalf("method = %q, want xclip", res.method)
	}
	if len(term.seqs) != 0 {
		t.Fatalf("a working tool should stop the fallback: %v", term.seqs)
	}
	if res.lines != 3 {
		t.Fatalf("lines = %d, want 3", res.lines)
	}
	if got := topClipMessage(res); got != "copied 3 lines with xclip" {
		t.Fatalf("message = %q", got)
	}
}

func TestTopClipboardOsc52PayloadIsTheScreen(t *testing.T) {
	term := &fakeTerminal{}
	c := clipFor(topClipEnv{GOOS: "linux", SSH: true}, &fakeClip{}, term, nil)
	text := "one\ntwo"
	res := c.copy(text)
	if res.method != "OSC 52" || !res.terminal {
		t.Fatalf("result = %+v, want a terminal method named OSC 52", res)
	}
	if len(term.seqs) != 1 {
		t.Fatalf("sequences = %v, want one", term.seqs)
	}
	prefix, suffix := "\x1b]52;c;", "\a"
	seq := term.seqs[0]
	if !strings.HasPrefix(seq, prefix) || !strings.HasSuffix(seq, suffix) {
		t.Fatalf("sequence framing is wrong: %q", seq)
	}
	payload := strings.TrimSuffix(strings.TrimPrefix(seq, prefix), suffix)
	got, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		t.Fatalf("payload is not base64: %v", err)
	}
	if string(got) != text {
		t.Fatalf("payload decoded to %q, want %q", got, text)
	}
	// A terminal cannot confirm the clipboard took it, so the message must not
	// say the copy happened.
	if msg := topClipMessage(res); msg != "sent 2 lines to the terminal clipboard (OSC 52)" {
		t.Fatalf("message = %q", msg)
	}
}

func TestTopClipboardTmuxAndScreenWrapping(t *testing.T) {
	text := "x"
	inner := "\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte(text)) + "\a"
	for _, tc := range []struct {
		env  topClipEnv
		want string
	}{
		{topClipEnv{SSH: true, Tmux: true}, "\x1bPtmux;" + strings.ReplaceAll(inner, "\x1b", "\x1b\x1b") + "\x1b\\"},
		{topClipEnv{SSH: true, Screen: true}, "\x1bP" + inner + "\x1b\\"},
	} {
		term := &fakeTerminal{}
		c := clipFor(tc.env, &fakeClip{}, term, nil)
		c.copy(text)
		if len(term.seqs) != 1 || term.seqs[0] != tc.want {
			t.Fatalf("env %+v: sequence = %q, want %q", tc.env, term.seqs, tc.want)
		}
	}
	if topClipMessage(topClipResult{method: "OSC 52 in tmux", terminal: true, lines: 1}) != "sent 1 lines to the terminal clipboard (OSC 52 in tmux)" {
		t.Fatalf("tmux message names the wrong method")
	}
}

func TestTopClipboardCapsThePayload(t *testing.T) {
	term := &fakeTerminal{}
	c := clipFor(topClipEnv{SSH: true}, &fakeClip{}, term, nil)
	// Many lines, more than the cap.
	var sb strings.Builder
	for sb.Len() < 3*topClipMaxBytes {
		sb.WriteString("a line of the screen that is not very long at all\n")
	}
	text := sb.String()
	res := c.copy(text)
	if !res.truncated {
		t.Fatal("a text over the cap should be marked truncated")
	}
	seq := term.seqs[0]
	payload := strings.TrimSuffix(strings.TrimPrefix(seq, "\x1b]52;c;"), "\a")
	got, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) > topClipMaxBytes {
		t.Fatalf("payload is %d bytes, over the %d cap", len(got), topClipMaxBytes)
	}
	if text[:len(got)] != string(got) {
		t.Fatal("the payload should be the start of the text")
	}
	// The cut lands between lines: the last line sent is whole.
	if !strings.HasSuffix(string(got), "at all") {
		t.Fatalf("the cut landed mid-line: %q", string(got)[len(got)-40:])
	}
	if !strings.HasPrefix(text[len(got):], "\n") {
		t.Fatal("the cut should land on a line boundary")
	}
	if msg := topClipMessage(res); !strings.Contains(msg, "cut to 64 KiB") {
		t.Fatalf("message = %q, want it to say the text was cut", msg)
	}
}

func TestTopClipboardSavesTheFileWhenNothingWorks(t *testing.T) {
	run := &fakeClip{fail: map[string]error{"wl-copy": errors.New("exit status 1")}}
	save := &fakeSave{path: "/home/user/.local/state/urnet-tools/top-20260101-000000-1.txt"}
	c := clipFor(topClipEnv{GOOS: "linux", Wayland: true}, run, nil, save)
	res := c.copy("a\nb\n")
	if res.method != "" {
		t.Fatalf("method = %q, want no method", res.method)
	}
	if res.err == nil || !strings.Contains(res.err.Error(), "wl-copy") {
		t.Fatalf("err = %v, want it to name the tool that failed", res.err)
	}
	if len(save.texts) != 1 || save.texts[0] != "a\nb\n" {
		t.Fatalf("saved %q, want the screen text", save.texts)
	}
	msg := topClipMessage(res)
	if !strings.Contains(msg, save.path) || !strings.Contains(msg, "wl-copy") || !strings.Contains(msg, "saved 3 lines") {
		t.Fatalf("message = %q, want the reason, the line count and the path", msg)
	}
}

func TestTopClipboardSaysWhenEvenTheFileFailed(t *testing.T) {
	save := &fakeSave{err: errors.New("permission denied")}
	c := clipFor(topClipEnv{GOOS: "linux"}, nil, nil, save)
	res := c.copy("a")
	msg := topClipMessage(res)
	if !strings.Contains(msg, "could not copy") || !strings.Contains(msg, "permission denied") {
		t.Fatalf("message = %q", msg)
	}
}

// A session with no method at all runs nothing and writes nothing.
func TestTopClipboardRunsNothingWithoutAMethod(t *testing.T) {
	run := &fakeClip{}
	term := &fakeTerminal{}
	save := &fakeSave{path: "/tmp/x"}
	c := clipFor(topClipEnv{GOOS: "linux"}, run, term, save)
	c.term = nil
	res := c.copy("a")
	if len(run.tried) != 0 || len(term.seqs) != 0 {
		t.Fatalf("ran %v and wrote %v with no method available", run.tried, term.seqs)
	}
	if len(save.texts) != 1 {
		t.Fatalf("the fallback file should have been written, got %v", save.texts)
	}
	if res.err == nil || !strings.Contains(res.err.Error(), "no clipboard method") {
		t.Fatalf("err = %v", res.err)
	}
}

// ---- the c key ----

func TestTopCopyKeyCopiesTheFrame(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	m.apply(0, topFlowing(t), nil)
	run := &fakeClip{}
	m.clip = clipFor(topClipEnv{GOOS: "linux", Wayland: true}, run, nil, nil)
	b := tui.New(100, 30)
	m.render(b)
	if eff := m.handle(runeKey('c')); eff != topCopy {
		t.Fatalf("c gave effect %v, want topCopy", eff)
	}
	m.copyFrame(b)
	want := copyText(b, false)
	if len(run.stdin) != 1 || run.stdin[0] != want {
		t.Fatalf("the clipboard got %d texts, and not the rendered frame", len(run.stdin))
	}
	if !strings.Contains(m.notice, "copied") {
		t.Fatalf("notice = %q, want the result", m.notice)
	}
	// The copy itself leaves the frame as it was: only the message row differs.
	after := tui.New(100, 30)
	m.render(after)
	before := strings.Split(want, "\n")
	now := strings.Split(after.String(), "\n")
	for i := 0; i < len(before)-1 && i < len(now)-1; i++ {
		if before[i] != now[i] {
			t.Fatalf("row %d changed by the copy:\n%q\n%q", i, before[i], now[i])
		}
	}
}

func TestTopCopyKeyFollowsPlain(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	m.apply(0, topFlowing(t), nil)
	m.copyPlain = true
	run := &fakeClip{}
	m.clip = clipFor(topClipEnv{GOOS: "linux", Wayland: true}, run, nil, nil)
	b := tui.New(100, 30)
	m.render(b)
	m.copyFrame(b)
	if len(run.stdin) != 1 {
		t.Fatalf("copied %d times", len(run.stdin))
	}
	if !strings.Contains(run.stdin[0], "+") || strings.Contains(run.stdin[0], "│") {
		t.Fatalf("--plain should turn the box characters into ASCII:\n%s", run.stdin[0])
	}
}

func TestTopCopyKeyWorksOnADirectionPage(t *testing.T) {
	m, b, screen := directionPageModel(t, 120, 40, "billable")
	run := &fakeClip{}
	m.clip = clipFor(topClipEnv{GOOS: "linux", Wayland: true}, run, nil, nil)
	if eff := m.handle(runeKey('c')); eff != topCopy {
		t.Fatalf("c gave effect %v on the billable page", eff)
	}
	m.copyFrame(b)
	if len(run.stdin) != 1 {
		t.Fatal("nothing was copied")
	}
	if !strings.Contains(run.stdin[0], "Billable") || !strings.Contains(screen, "Billable") {
		t.Fatalf("the copy should be the billable page:\n%s", run.stdin[0])
	}
}

func TestTopCopyKeyWithoutAClipboardSaysSo(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	if eff := m.handle(runeKey('c')); eff != topNone {
		t.Fatalf("c gave effect %v with no clipboard", eff)
	}
	if !strings.Contains(strings.ToLower(m.notice), "clipboard") {
		t.Fatalf("notice = %q, want it to say there is no clipboard", m.notice)
	}
	for _, line := range m.helpText() {
		if strings.HasPrefix(line, "c ") {
			t.Fatalf("the help still offers the copy key: %q", line)
		}
	}
}

func TestTopCopyKeyStaysOutOfTheMenu(t *testing.T) {
	m, _ := menuModel(t)
	m.clip = clipFor(topClipEnv{GOOS: "linux", Wayland: true}, &fakeClip{}, nil, nil)
	pressKey(m, 'm')
	if eff := m.handle(runeKey('c')); eff == topCopy {
		t.Fatal("c requested a copy from under the menu")
	}
}

// The clipboard writer takes the terminal's own lock and resyncs the screen, so
// the frame renderer never loses the cursor to an escape sequence it did not
// write. See the tcellui adapter tests for the real screen.
