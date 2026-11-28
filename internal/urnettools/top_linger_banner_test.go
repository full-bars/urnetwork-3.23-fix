package urnettools

import (
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/gdamore/tcell/v2"
	"github.com/urnetwork/connect/internal/tui"
	"github.com/urnetwork/connect/internal/tui/tcellui"
)

// lingerModel is a model whose provider runs as a systemd user unit for
// alice, with lingering off unless the test says otherwise.
func lingerModel(t *testing.T, enabled bool) (*topModel, *fakeClock) {
	t.Helper()
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	m.providers[0].User, m.providers[0].Unit = "alice", "urnetwork.service"
	m.linger = []topLingerReading{{User: "alice", Enabled: enabled, CheckedAt: topBase}}
	m.lingerFixable = true
	return m, clock
}

// lingerSeams swaps the exec seams for the duration of one test.
func lingerSeams(t *testing.T, run func(name string, args []string, interactive bool) error, root bool, sudo bool) {
	t.Helper()
	oldRun, oldRoot, oldSudo := topLingerRunFn, topLingerRootFn, topLingerSudoFn
	t.Cleanup(func() { topLingerRunFn, topLingerRootFn, topLingerSudoFn = oldRun, oldRoot, oldSudo })
	topLingerRunFn = run
	topLingerRootFn = func() bool { return root }
	topLingerSudoFn = func(string) (string, error) {
		if sudo {
			return "/usr/bin/sudo", nil
		}
		return "", errors.New("executable file not found in $PATH")
	}
}

// lingerChecks makes the recheck answer from a map, and counts the reads.
func lingerChecks(t *testing.T, answers map[string]bool) *int {
	t.Helper()
	old := statusLingerCheckFn
	t.Cleanup(func() { statusLingerCheckFn = old })
	reads := new(int)
	statusLingerCheckFn = func(user string) (bool, error) {
		*reads++
		if v, ok := answers[user]; ok {
			return v, nil
		}
		return false, errors.New("no answer")
	}
	return reads
}

// fakeLingerTerminal records the suspend and resume calls in order.
type fakeLingerTerminal struct {
	calls      []string
	suspendErr error
	resumeErr  error
}

func (f *fakeLingerTerminal) Suspend() error {
	f.calls = append(f.calls, "suspend")
	return f.suspendErr
}

func (f *fakeLingerTerminal) Resume() error {
	f.calls = append(f.calls, "resume")
	return f.resumeErr
}

// run is the command recorder: it appends a readable line per command.
type lingerCalls struct{ lines []string }

func (c *lingerCalls) run(name string, args []string, interactive bool) error {
	mode := "plain"
	if interactive {
		mode = "terminal"
	}
	c.lines = append(c.lines, name+" "+strings.Join(args, " ")+" ["+mode+"]")
	return nil
}

func TestLingerBannerTextTiers(t *testing.T) {
	for _, tc := range []struct {
		name  string
		width int
		want  string
	}{
		{"full", 120, "LINGERING IS OFF for alice: the provider STOPS when you log out.  [ click here or press L to fix ]"},
		{"medium", 95, "LINGERING IS OFF for alice: the provider STOPS at logout.  [ click here or press L to fix ]"},
		{"short", 80, "LINGERING IS OFF for alice.  [ click here or press L to fix ]"},
		{"narrow", 30, "LINGERING OFF alice  [ L ]"},
		{"tiny", 24, "LINGERING OFF (1)  [ L ]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := lingerBannerText([]string{"alice"}, tc.width, false, true); got != tc.want {
				t.Fatalf("width %d: got %q want %q", tc.width, got, tc.want)
			}
		})
	}
	// ASCII keeps a marker, and every tier still names the fix.
	got := lingerBannerText([]string{"alice"}, 120, true, true)
	if !strings.HasPrefix(got, "!! LINGERING IS OFF") || !strings.Contains(got, "press L") {
		t.Fatalf("ascii banner = %q", got)
	}
	// Without a terminal to run the fix from, the text names the command.
	got = lingerBannerText([]string{"alice", "bob"}, 200, false, false)
	if !strings.Contains(got, "loginctl enable-linger alice bob") || strings.Contains(got, "press L") {
		t.Fatalf("not-fixable banner = %q", got)
	}
	if lingerBannerText(nil, 80, false, true) != "" {
		t.Fatal("no users must draw no banner")
	}
}

func TestLingerBannerDrawsAndTakesARow(t *testing.T) {
	m, _ := lingerModel(t, false)
	m.apply(0, topFlowing(t), nil)
	b := tui.New(120, 40)
	renderFresh(m, b)
	text := b.String()
	if !strings.Contains(text, "LINGERING IS OFF for alice") {
		t.Fatalf("no banner in the frame:\n%s", strings.SplitN(text, "\n", 3)[0])
	}
	if m.contentRect.Y != 3 {
		t.Fatalf("content starts at y=%d, want 3 with the banner row", m.contentRect.Y)
	}
	// The banner hands its row back when lingering is on.
	m.linger = []topLingerReading{{User: "alice", Enabled: true, CheckedAt: topBase}}
	renderFresh(m, b)
	if strings.Contains(b.String(), "LINGERING") {
		t.Fatal("banner still drawn with lingering on")
	}
	if m.contentRect.Y != 2 {
		t.Fatalf("content starts at y=%d, want 2 without the banner", m.contentRect.Y)
	}
	// No readings at all is the not-applicable case: nothing is shown and the
	// layout is the one top always had.
	m.linger = nil
	renderFresh(m, b)
	if strings.Contains(b.String(), "LINGERING") || m.contentRect.Y != 2 {
		t.Fatal("a provider with no linger reading must draw no banner")
	}
}

func TestLingerBannerClickAndKeyOpenTheDialog(t *testing.T) {
	m, _ := lingerModel(t, false)
	m.apply(0, topFlowing(t), nil)
	b := tui.New(120, 40)
	renderFresh(m, b)
	// The banner is the row under the header.
	if name := hitRegion(m.regions, 5, 1); name != lingerBannerHit {
		t.Fatalf("region at the banner = %q", name)
	}
	if eff := m.handle(mouseEv(tcellui.MousePress, tcellui.MouseLeft, 5, 1)); eff != topNone {
		t.Fatalf("click effect = %v", eff)
	}
	if !m.lingerFix.dialog || m.lingerFix.sel != 0 {
		t.Fatalf("click did not open the dialog with No selected: %+v", m.lingerFix)
	}
	m.lingerFix.dialog = false
	m.handle(tcellui.Event{Kind: tcellui.EventRune, Rune: 'L'})
	if !m.lingerFix.dialog {
		t.Fatal("L did not open the dialog")
	}
	m.lingerFix.dialog = false
	// With lingering on there is nothing to fix and the key does nothing.
	m.linger = []topLingerReading{{User: "alice", Enabled: true}}
	m.handle(tcellui.Event{Kind: tcellui.EventRune, Rune: 'L'})
	if m.lingerFix.dialog {
		t.Fatal("L opened the dialog with lingering on")
	}
}

func TestLingerDialogDeclinesRunNothing(t *testing.T) {
	m, _ := lingerModel(t, false)
	calls := &lingerCalls{}
	lingerSeams(t, calls.run, false, true)
	for _, tc := range []struct {
		name string
		ev   tcellui.Event
	}{
		{"esc", tcellui.Event{Kind: tcellui.EventKey, Key: tcellui.KeyEsc}},
		{"n", tcellui.Event{Kind: tcellui.EventRune, Rune: 'n'}},
		{"enter on no", tcellui.Event{Kind: tcellui.EventKey, Key: tcellui.KeyEnter}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m.openLingerDialog()
			if eff := m.handle(tc.ev); eff != topNone {
				t.Fatalf("effect = %v, want none", eff)
			}
			if m.lingerFix.dialog {
				t.Fatal("the dialog stayed open")
			}
		})
	}
	if len(calls.lines) != 0 {
		t.Fatalf("declining ran %v", calls.lines)
	}
}

func TestLingerDialogYesAsksTheLoopToRun(t *testing.T) {
	m, _ := lingerModel(t, false)
	m.openLingerDialog()
	// Tab moves to Yes, then Enter confirms.
	m.handle(tcellui.Event{Kind: tcellui.EventKey, Key: tcellui.KeyTab})
	if m.lingerFix.sel != 1 {
		t.Fatalf("selection after Tab = %d", m.lingerFix.sel)
	}
	if eff := m.handle(tcellui.Event{Kind: tcellui.EventKey, Key: tcellui.KeyEnter}); eff != topLingerEnable {
		t.Fatalf("Enter on Yes = %v, want topLingerEnable", eff)
	}
	if m.lingerFix.dialog {
		t.Fatal("the dialog stayed open after Yes")
	}
	// y is Yes directly.
	m.openLingerDialog()
	if eff := m.handle(tcellui.Event{Kind: tcellui.EventRune, Rune: 'y'}); eff != topLingerEnable {
		t.Fatalf("y = %v, want topLingerEnable", eff)
	}
}

func TestLingerUserLevelSucceedsWithoutSudo(t *testing.T) {
	m, _ := lingerModel(t, false)
	calls := &lingerCalls{}
	lingerSeams(t, calls.run, false, true)
	lingerChecks(t, map[string]bool{"alice": true})
	term := &fakeLingerTerminal{}
	m.enableLingering(term)
	if len(calls.lines) != 1 || calls.lines[0] != "loginctl enable-linger alice [plain]" {
		t.Fatalf("commands = %v", calls.lines)
	}
	if len(term.calls) != 0 {
		t.Fatalf("the terminal was touched: %v", term.calls)
	}
	if m.lingerBannerActive() {
		t.Fatal("the banner survived a successful fix")
	}
	if !strings.Contains(m.notice, "Lingering enabled for alice") {
		t.Fatalf("notice = %q", m.notice)
	}
}

func TestLingerSudoPathSuspendsAroundTheCommand(t *testing.T) {
	m, _ := lingerModel(t, false)
	calls := &lingerCalls{}
	lingerSeams(t, func(name string, args []string, interactive bool) error {
		if name == "loginctl" {
			return errors.New("Failed to enable linger: Access denied")
		}
		return calls.run(name, args, interactive)
	}, false, true)
	lingerChecks(t, map[string]bool{"alice": true})
	term := &fakeLingerTerminal{}
	m.enableLingering(term)
	if got := strings.Join(term.calls, ","); got != "suspend,resume" {
		t.Fatalf("terminal calls = %s", got)
	}
	if len(calls.lines) != 1 || calls.lines[0] != "sudo loginctl enable-linger alice [terminal]" {
		t.Fatalf("sudo command = %v", calls.lines)
	}
	// Sudo runs after the suspend: the prompt needs the shell's terminal.
	if len(term.calls) == 0 || term.calls[0] != "suspend" {
		t.Fatal("the command ran before the suspend")
	}
	if m.lingerBannerActive() {
		t.Fatal("the banner survived a successful sudo run")
	}
}

func TestLingerSudoFailureRestoresTheTerminalAndKeepsTheBanner(t *testing.T) {
	m, _ := lingerModel(t, false)
	lingerSeams(t, func(name string, args []string, interactive bool) error {
		if name == "loginctl" {
			return errors.New("denied")
		}
		return errors.New("sudo: no password was provided\nusage: sudo")
	}, false, true)
	lingerChecks(t, map[string]bool{"alice": false})
	term := &fakeLingerTerminal{}
	m.enableLingering(term)
	if got := strings.Join(term.calls, ","); got != "suspend,resume" {
		t.Fatalf("terminal calls = %s", got)
	}
	if !m.lingerBannerActive() {
		t.Fatal("the banner went away without a recheck saying on")
	}
	if !strings.Contains(m.notice, "sudo: no password was provided") || !strings.Contains(m.notice, "sudo loginctl enable-linger alice") {
		t.Fatalf("notice = %q", m.notice)
	}
}

func TestLingerPanicInTheRunnerStillRestoresTheTerminal(t *testing.T) {
	m, _ := lingerModel(t, false)
	lingerSeams(t, func(name string, args []string, interactive bool) error {
		if name == "sudo" {
			panic("the runner crashed")
		}
		return errors.New("denied")
	}, false, true)
	lingerChecks(t, map[string]bool{})
	term := &fakeLingerTerminal{}
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("the panic did not propagate")
			}
		}()
		m.enableLingering(term)
	}()
	if got := strings.Join(term.calls, ","); got != "suspend,resume" {
		t.Fatalf("terminal calls after a panic = %s", got)
	}
}

func TestLingerRootRunsLoginctlWithoutSudo(t *testing.T) {
	m, _ := lingerModel(t, false)
	calls := &lingerCalls{}
	lingerSeams(t, calls.run, true, true)
	lingerChecks(t, map[string]bool{"alice": true})
	m.enableLingering(&fakeLingerTerminal{})
	if len(calls.lines) != 1 || !strings.HasPrefix(calls.lines[0], "loginctl enable-linger alice") {
		t.Fatalf("commands = %v", calls.lines)
	}
	if m.lingerBannerActive() {
		t.Fatal("the banner survived a root run")
	}
}

func TestLingerMissingSudoShowsTheCommand(t *testing.T) {
	m, _ := lingerModel(t, false)
	lingerSeams(t, func(string, []string, bool) error { return errors.New("denied") }, false, false)
	lingerChecks(t, map[string]bool{})
	m.enableLingering(&fakeLingerTerminal{})
	if !strings.Contains(m.notice, "sudo is not installed") || !strings.Contains(m.notice, "sudo loginctl enable-linger alice") {
		t.Fatalf("notice = %q", m.notice)
	}
	if !m.lingerBannerActive() {
		t.Fatal("the banner went away")
	}
}

func TestLingerSeveralUsersOneCommand(t *testing.T) {
	m, _ := lingerModel(t, false)
	m.linger = []topLingerReading{{User: "bob", Enabled: false}, {User: "alice", Enabled: false}}
	calls := &lingerCalls{}
	lingerSeams(t, calls.run, true, true)
	lingerChecks(t, map[string]bool{"alice": true, "bob": true})
	m.enableLingering(&fakeLingerTerminal{})
	if len(calls.lines) != 1 || calls.lines[0] != "loginctl enable-linger alice bob [plain]" {
		t.Fatalf("commands = %v", calls.lines)
	}
	if !strings.Contains(m.notice, "alice and bob") {
		t.Fatalf("notice = %q", m.notice)
	}
}

func TestLingerNonTerminalOffersNoAction(t *testing.T) {
	m, _ := lingerModel(t, false)
	m.lingerFixable = false
	b := tui.New(120, 10)
	renderFresh(m, b)
	if strings.Contains(b.String(), "press L") {
		t.Fatal("the banner offered the action without a terminal")
	}
	if !strings.Contains(b.String(), "loginctl enable-linger alice") {
		t.Fatal("the banner did not name the command")
	}
	if m.openLingerDialog() || m.lingerFix.dialog {
		t.Fatal("the dialog opened without a terminal")
	}
}

func TestLingerBannerCompact(t *testing.T) {
	m, _ := lingerModel(t, false)
	m.apply(0, topFlowing(t), nil)
	b := tui.New(60, 12) // below topMin, so the compact view
	renderFresh(m, b)
	first := strings.SplitN(b.String(), "\n", 2)[0]
	if !strings.Contains(first, "LINGERING") {
		t.Fatalf("the compact view has no banner: %q", first)
	}
}

func TestLingerBannerAsciiRendering(t *testing.T) {
	m, _ := lingerModel(t, false)
	m.theme = mustTheme(t, "mono")
	m.apply(0, topFlowing(t), nil)
	b := tui.New(120, 40)
	renderFresh(m, b)
	if !strings.Contains(b.String(), "!! LINGERING IS OFF") {
		t.Fatal("the ascii banner lost its marker")
	}
}

func TestLingerDialogRendersTheCommand(t *testing.T) {
	lingerSeams(t, nil, false, true)
	m, _ := lingerModel(t, false)
	m.lingerFixable = true
	m.openLingerDialog()
	b := tui.New(100, 40)
	renderFresh(m, b)
	text := b.String()
	for _, want := range []string{"Enable lingering", "Command: loginctl enable-linger alice", "Then if refused: sudo loginctl enable-linger alice", "[ Yes ]", "[ No ]"} {
		if !strings.Contains(text, want) {
			t.Fatalf("the dialog is missing %q", want)
		}
	}
	// Both buttons are clickable, and the click on Yes asks the loop to run.
	var yes tui.Rect
	for _, r := range m.regions {
		if r.Name == lingerYesHit {
			yes = r.Rect
		}
	}
	if yes.W == 0 {
		t.Fatal("no Yes click region")
	}
	if eff := m.handle(mouseEv(tcellui.MousePress, tcellui.MouseLeft, yes.X, yes.Y)); eff != topLingerEnable {
		t.Fatalf("the Yes click = %v", eff)
	}
}

func TestLingerRecheckUsesTheReadingNotTheExitStatus(t *testing.T) {
	m, _ := lingerModel(t, false)
	calls := &lingerCalls{}
	lingerSeams(t, calls.run, false, true)
	reads := lingerChecks(t, map[string]bool{"alice": false})
	m.enableLingering(&fakeLingerTerminal{})
	if *reads == 0 {
		t.Fatal("no recheck ran")
	}
	if !m.lingerBannerActive() {
		t.Fatal("the banner went away although the recheck says off")
	}
	if !strings.Contains(m.notice, "still off") {
		t.Fatalf("notice = %q", m.notice)
	}
}

func TestLingerInvalidUserNameRunsNothing(t *testing.T) {
	m, _ := lingerModel(t, false)
	m.linger = []topLingerReading{{User: "alice;reboot"}}
	calls := &lingerCalls{}
	lingerSeams(t, calls.run, false, true)
	m.enableLingering(&fakeLingerTerminal{})
	if len(calls.lines) != 0 {
		t.Fatalf("commands = %v", calls.lines)
	}
	if !strings.Contains(m.notice, "not a plain account name") {
		t.Fatalf("notice = %q", m.notice)
	}
}

func TestLingerCtrlCBelongsToTheChildCommand(t *testing.T) {
	// In raw mode Ctrl-C is a key; this is the signal path for the stretch
	// where a child owns the terminal.
	topSignalIgnore.Store(false)
	if !topSignalEndsTop(os.Interrupt) {
		t.Fatal("SIGINT ended top late")
	}
	topSignalIgnore.Store(true)
	if topSignalEndsTop(os.Interrupt) {
		t.Fatal("SIGINT ended top while a child owned the terminal")
	}
	if !topSignalEndsTop(syscall.SIGTERM) {
		t.Fatal("SIGTERM did not end top")
	}
	topSignalIgnore.Store(false)

	// The flag is cleared after the run, success or failure.
	m, _ := lingerModel(t, false)
	lingerSeams(t, func(name string, args []string, interactive bool) error {
		if name == "sudo" {
			return errors.New("cancelled")
		}
		return errors.New("denied")
	}, false, true)
	lingerChecks(t, map[string]bool{})
	m.enableLingering(&fakeLingerTerminal{})
	if topSignalIgnore.Load() {
		t.Fatal("the signal guard stayed on after the command")
	}
}

func TestLingerFailedResumeIsReported(t *testing.T) {
	m, _ := lingerModel(t, false)
	lingerSeams(t, func(name string, args []string, interactive bool) error {
		if name == "loginctl" {
			return errors.New("denied")
		}
		return nil
	}, false, true)
	lingerChecks(t, map[string]bool{"alice": true})
	term := &fakeLingerTerminal{resumeErr: errors.New("tcell: could not take the terminal back")}
	err := m.enableLingering(term)
	if err == nil || !strings.Contains(err.Error(), "tcell: could not take the terminal back") {
		t.Fatalf("the failed resume was dropped: %v", err)
	}
	if strings.ContainsAny(err.Error(), "\n") {
		t.Fatalf("the error is not one line: %q", err)
	}
	// The terminal never came back, so nothing is claimed about the fix and the
	// reading stays as it was.
	if strings.Contains(m.notice, "Lingering enabled") {
		t.Fatalf("notice = %q", m.notice)
	}
	if !m.lingerBannerActive() {
		t.Fatal("the banner went away without a recheck that says on")
	}
}

func TestLingerFailedResumeEndsTop(t *testing.T) {
	m, _ := lingerModel(t, false)
	lingerSeams(t, func(name string, args []string, interactive bool) error {
		if name == "loginctl" {
			return errors.New("denied")
		}
		return nil
	}, false, true)
	lingerChecks(t, map[string]bool{"alice": true})
	src, _ := fakeSource(t)
	rig := startLoop(t, m, src, 100, 30)
	rig.scr.failResume.Store(true)
	// L opens the dialog, y confirms, and the sudo path then cannot take the
	// terminal back.
	rig.sim.InjectKey(tcell.KeyRune, 'L', 0)
	rig.sim.InjectKey(tcell.KeyRune, 'y', 0)
	err := rig.wait(t)
	if err == nil {
		t.Fatal("top kept running without a terminal")
	}
	if !strings.Contains(err.Error(), "the terminal did not come back") || !strings.Contains(err.Error(), "resume failed") {
		t.Fatalf("error = %q", err)
	}
}

func TestLingerBannerIsPaintedBoldInTheBadStyle(t *testing.T) {
	m, _ := lingerModel(t, false)
	m.apply(0, topFlowing(t), nil)
	b := tui.New(120, 40)
	renderFresh(m, b)
	cell := b.Cell(0, 1)
	if cell.Style.Attrs&tui.AttrBold == 0 {
		t.Fatalf("the banner is not bold: %+v", cell.Style)
	}
	if cell.Style.FG != m.theme.Bad.FG {
		t.Fatalf("the banner fg = %+v, want the theme's bad color %+v", cell.Style.FG, m.theme.Bad.FG)
	}
	// The mono theme carries no color, so only the text says it.
	m.theme = mustTheme(t, "mono")
	renderFresh(m, b)
	if cell := b.Cell(0, 1); cell.Style.FG != (tui.Color{}) || cell.Style.BG != (tui.Color{}) {
		t.Fatalf("the mono banner carries color: %+v", cell.Style)
	}
}

func TestLingerDialogEndsDividerDrag(t *testing.T) {
	m, _ := lingerModel(t, false)
	m.drag = &topDrag{region: "throughput"}
	m.lastPress = topPress{region: "throughput"}
	m.handle(tcellui.Event{Kind: tcellui.EventRune, Rune: 'L'})
	if !m.lingerFix.dialog {
		t.Fatal("L did not open the dialog")
	}
	m.handle(tcellui.Event{Kind: tcellui.EventKey, Key: tcellui.KeyEsc})
	if m.drag != nil || m.lastPress != (topPress{}) {
		t.Fatalf("stale gesture after dialog: drag=%v press=%v", m.drag, m.lastPress)
	}
}
