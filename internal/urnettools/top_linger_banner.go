package urnettools

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/urnetwork/connect/internal/tui"
	"github.com/urnetwork/connect/internal/tui/tcellui"
)

// topLingerFix is the state of the fix dialog and of a run in progress. The
// banner itself is computed from m.linger every frame, so it cannot be
// dismissed for the session: it goes away when a recheck says lingering is on.
type topLingerFix struct {
	dialog bool
	// sel is the selected button: 0 is No, which is the default, and 1 is Yes.
	sel int
	// running is set while a command runs, so one Yes cannot start two.
	running bool
}

// lingerBannerHint is the click affordance on the banner. The banner is where
// the fix is offered, so the hint survives every width the text degrades to.
const lingerBannerHint = "  [ click here or press L to fix ]"

// lingerBannerHit is the click region of the whole banner row.
const lingerBannerHit = "linger:banner"

// The dialog's buttons, as click regions.
const (
	lingerYesHit = "linger:yes"
	lingerNoHit  = "linger:no"
)

// lingerDialogWidth is the widest the confirmation box gets.
const lingerDialogWidth = 68

// topLingerValidUser accepts a plain Linux account name. The fix only runs for
// users that came from the provider discovery, and this is the second check
// before anything reaches exec.
var topLingerValidUser = func() func(string) bool {
	re := regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]{0,31}$`)
	return re.MatchString
}()

// Seams, so the tests never run loginctl or sudo and never need a real
// terminal.
var (
	// topLingerRunFn runs one command. interactive means the command owns the
	// terminal: its output goes to the real stdout and stderr, so a sudo
	// password prompt works, and nothing is captured.
	topLingerRunFn = func(name string, args []string, interactive bool) error {
		if interactive {
			cmd := exec.Command(name, args...)
			cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
			return cmd.Run()
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
		if err != nil {
			if text := strings.TrimSpace(string(out)); text != "" {
				return fmt.Errorf("%v: %s", err, strings.SplitN(text, "\n", 2)[0])
			}
		}
		return err
	}
	// topLingerRootFn reports whether top already runs as root, in which case
	// loginctl needs no sudo.
	topLingerRootFn = func() bool { return os.Geteuid() == 0 }
	// topLingerSudoFn finds sudo, so a missing sudo is reported with the
	// command to run in a root shell instead of a failed exec.
	topLingerSudoFn = exec.LookPath
)

// lingerOffUsers returns the sorted, unique users whose lingering is known to
// be off.
func (m *topModel) lingerOffUsers() []string {
	seen := make(map[string]bool, len(m.linger))
	var users []string
	for _, r := range m.linger {
		if r.Enabled || r.User == "" || seen[r.User] {
			continue
		}
		seen[r.User] = true
		users = append(users, r.User)
	}
	sortStrings(users)
	return users
}

// lingerBannerActive reports whether the banner takes a row this frame.
func (m *topModel) lingerBannerActive() bool {
	return len(m.lingerOffUsers()) > 0
}

// sortStrings is insertion sort for the short lists here.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// lingerNames joins the users, shortening a long list so the sentence stays
// readable at a normal width.
func lingerNames(users []string) string {
	const most = 4
	if len(users) > most {
		return fmt.Sprintf("%s and %d more", strings.Join(users[:most], ", "), len(users)-most)
	}
	if len(users) == 2 {
		return users[0] + " and " + users[1]
	}
	return strings.Join(users, ", ")
}

// lingerBannerText is the banner line. It drops words from the sentence as the
// width shrinks and keeps the fix hint, and at the narrowest widths it keeps
// only the count and the key. fixable is false when there is no terminal to
// run the command from; the text then names the command instead of the key.
func lingerBannerText(users []string, width int, ascii, fixable bool) string {
	if len(users) == 0 || width <= 0 {
		return ""
	}
	marker := ""
	if ascii {
		marker = "!! "
	}
	names := lingerNames(users)
	if !fixable {
		cmd := "loginctl enable-linger " + strings.Join(users, " ")
		return tui.Truncate(marker+"LINGERING IS OFF for "+names+": the provider STOPS when you log out.  Run: "+cmd, width, ascii)
	}
	tiers := []string{
		marker + "LINGERING IS OFF for " + names + ": the provider STOPS when you log out." + lingerBannerHint,
		marker + "LINGERING IS OFF for " + names + ": the provider STOPS at logout." + lingerBannerHint,
		marker + "LINGERING IS OFF for " + names + "." + lingerBannerHint,
		marker + "LINGERING OFF " + names + "  [ L ]",
	}
	for _, text := range tiers {
		if tui.StringWidth(text) <= width {
			return text
		}
	}
	// The names no longer fit with even the shortest hint, so keep the count.
	short := marker + fmt.Sprintf("LINGERING OFF (%d)  [ L ]", len(users))
	if tui.StringWidth(short) <= width {
		return short
	}
	return tui.Truncate(short, width, ascii)
}

// drawLingerBanner paints one row and records the click region for it.
func (m *topModel) drawLingerBanner(b *tui.Buffer, at tui.Rect) {
	text := lingerBannerText(m.lingerOffUsers(), b.Width(), m.theme.ASCII, m.lingerFixable)
	if text == "" || b.Height() == 0 {
		return
	}
	style := m.theme.Bad
	if !m.theme.ASCII {
		style = style.With(tui.AttrBold)
	}
	b.Fill(b.Rect(), ' ', style)
	b.Put(tui.Truncate(text, b.Width(), m.theme.ASCII), 0, 0, style)
	m.record(lingerBannerHit, at)
}

// lingerCommands is the command list the dialog shows, in the order they run.
// A root session needs the first one only.
func (m *topModel) lingerCommands() []string {
	users := m.lingerOffUsers()
	if len(users) == 0 {
		return nil
	}
	cmd := "loginctl enable-linger " + strings.Join(users, " ")
	if topLingerRootFn() {
		return []string{cmd}
	}
	return []string{cmd, "sudo " + cmd}
}

// openLingerDialog opens the confirmation dialog with No selected.
func (m *topModel) openLingerDialog() bool {
	if m.lingerFix.running || !m.lingerBannerActive() || !m.lingerFixable {
		return false
	}
	m.lingerFix.dialog, m.lingerFix.sel = true, 0
	return true
}

// drawLingerDialog draws the confirmation box over everything else.
func (m *topModel) drawLingerDialog(b *tui.Buffer) {
	users := m.lingerOffUsers()
	if len(users) == 0 {
		m.lingerFix.dialog = false
		return
	}
	th := m.theme
	lines := []string{"Enable lingering for " + lingerNames(users) + "?"}
	if len(users) > 1 {
		lines = []string{"Enable lingering for these accounts?"}
	}
	lines = append(lines, "The provider keeps running after you log out.")
	for i, c := range m.lingerCommands() {
		label := "Command: "
		if i > 0 {
			label = "Then if refused: "
		}
		lines = append(lines, label+c)
	}
	w := min(lingerDialogWidth, b.Width())
	h := min(len(lines)+5, b.Height())
	rect := tui.Rect{X: max((b.Width()-w)/2, 0), Y: max((b.Height()-h)/2, 0), W: w, H: h}
	if rect.W < 2 || rect.H < 2 {
		return
	}
	panel := b.Sub(rect)
	panel.Fill(panel.Rect(), ' ', tui.Style{})
	in := tui.DrawBox(panel, "Enable lingering", th.Frame, th.Border, th.Accent, th.ASCII)
	y := 0
	for _, line := range lines {
		if y >= in.Height() {
			break
		}
		in.Put(tui.Truncate(line, in.Width(), th.ASCII), 0, y, th.Warn)
		y++
	}
	// The buttons sit below the text, and the key line under them. The selected
	// one is accent, the other dim, so the default No is visible.
	row := y + 1
	if row >= in.Height() {
		return
	}
	yes, no := "[ Yes ]", "[ No ]"
	ys, ns := th.Dim, th.Accent
	if m.lingerFix.sel == 1 {
		ys, ns = th.Accent, th.Dim
	}
	width := tui.StringWidth(yes) + tui.StringWidth(no) + 2
	left := max((in.Width()-width)/2, 0)
	x := in.Put(yes, left, row, ys)
	x = in.Put("  ", x, row, tui.Style{})
	in.Put(no, x, row, ns)
	// Remember the buttons in absolute cells for the mouse.
	base := rect.X + 1 + left
	m.record(lingerYesHit, tui.Rect{X: base, Y: rect.Y + 1 + row, W: tui.StringWidth(yes), H: 1})
	m.record(lingerNoHit, tui.Rect{X: base + tui.StringWidth(yes) + 2, Y: rect.Y + 1 + row, W: tui.StringWidth(no), H: 1})
	if row+1 < in.Height() {
		keys := "Tab or arrows: switch   Enter: run   Esc: cancel"
		in.Put(tui.Truncate(keys, in.Width(), th.ASCII), 0, row+1, th.Dim)
	}
}

// lingerDialogEvent applies one event while the dialog is open. Everything the
// dialog does not use is swallowed, so a key meant for the dialog cannot reach
// the dashboard behind it.
func (m *topModel) lingerDialogEvent(ev tcellui.Event) topEffect {
	switch ev.Kind {
	case tcellui.EventQuit:
		m.quit = true
		return topQuit
	case tcellui.EventKey:
		switch ev.Key {
		case tcellui.KeyCtrlC:
			m.quit = true
			return topQuit
		case tcellui.KeyEsc:
			m.lingerFix.dialog = false
		case tcellui.KeyTab, tcellui.KeyLeft, tcellui.KeyRight, tcellui.KeyBacktab:
			m.lingerFix.sel = 1 - m.lingerFix.sel
		case tcellui.KeyEnter:
			return m.lingerConfirm()
		}
	case tcellui.EventRune:
		switch ev.Rune {
		case 'y', 'Y':
			m.lingerFix.sel = 1
			return m.lingerConfirm()
		case 'n', 'N':
			m.lingerFix.dialog = false
		}
	case tcellui.EventMouse:
		if ev.Action != tcellui.MousePress || ev.Button != tcellui.MouseLeft {
			return topNone
		}
		switch hitRegion(m.regions, ev.X, ev.Y) {
		case lingerYesHit:
			m.lingerFix.sel = 1
			return m.lingerConfirm()
		case lingerNoHit:
			m.lingerFix.dialog = false
		}
	}
	return topNone
}

// lingerConfirm is Yes: close the dialog and ask the loop to run the fix.
func (m *topModel) lingerConfirm() topEffect {
	if m.lingerFix.sel != 1 || m.lingerFix.running {
		m.lingerFix.dialog = false
		return topNone
	}
	m.lingerFix.dialog = false
	return topLingerEnable
}

// topLingerTerminal is the part of the screen the fix needs: the terminal goes
// back to the shell for the length of a password prompt and is taken over
// again after.
type topLingerTerminal interface {
	Suspend() error
	Resume() error
}

// enableLingering runs the fix for every user still off. The caller is the run
// loop, so the terminal this blocks is the only one top owns. It never claims
// success without a recheck: what the banner shows afterwards comes from
// loginctl, not from the command's exit status.
func (m *topModel) enableLingering(term topLingerTerminal) {
	users := m.lingerOffUsers()
	if len(users) == 0 || m.lingerFix.running {
		return
	}
	m.lingerFix.running = true
	defer func() { m.lingerFix.running = false }()
	for _, user := range users {
		if !topLingerValidUser(user) {
			m.setNotice("Lingering not enabled: " + user + " is not a plain account name.")
			return
		}
	}
	args := append([]string{"enable-linger"}, users...)
	plain := "loginctl " + strings.Join(args, " ")
	if err := topLingerRunFn("loginctl", args, false); err == nil {
		m.finishLingering(users, plain, "")
		return
	}
	if topLingerRootFn() {
		m.finishLingering(users, plain, "loginctl refused")
		return
	}
	if _, err := topLingerSudoFn("sudo"); err != nil {
		m.setNotice("Lingering not enabled: sudo is not installed. Run this in a root shell: sudo " + plain)
		return
	}
	// The password prompt has to be readable, so the terminal goes back to the
	// shell for the length of the command and is taken over again after, on
	// every path including a panic in the command.
	if term != nil {
		if err := term.Suspend(); err == nil {
			defer term.Resume()
		}
	}
	sudo := append([]string{"loginctl"}, args...)
	if err := topLingerRunFn("sudo", sudo, true); err != nil {
		m.finishLingering(users, "sudo "+plain, topLingerFirstLine(err))
		return
	}
	m.finishLingering(users, "sudo "+plain, "")
}

// finishLingering rechecks the users and reports what loginctl actually says.
func (m *topModel) finishLingering(users []string, command, failure string) {
	stillOff := m.recheckLingering(users)
	if len(stillOff) == 0 {
		m.setNotice("Lingering enabled for " + lingerNames(users) + ".")
		return
	}
	msg := "Lingering is still off for " + lingerNames(stillOff) + "."
	if failure != "" {
		msg = "Lingering not enabled: " + failure + "."
	}
	m.setNotice(msg + " Run by hand: " + command)
}

// recheckLingering reads the users again and applies the readings. It returns
// the users that are still off. A reading that fails leaves the last one in
// place, so a lost loginctl never hides the banner.
func (m *topModel) recheckLingering(users []string) (off []string) {
	now := m.now()
	for _, user := range users {
		enabled, err := statusLingerCheckFn(user)
		if err != nil {
			off = append(off, user)
			continue
		}
		m.setLingerReading(topLingerReading{User: user, Enabled: enabled, CheckedAt: now})
		if !enabled {
			off = append(off, user)
		}
	}
	// A fetch requested before the fix carries the old answer; bumping the
	// revision drops it when it lands.
	m.lingerRev++
	return off
}

// setLingerReading replaces one user's reading, or adds it when the user has
// none yet.
func (m *topModel) setLingerReading(r topLingerReading) {
	for i := range m.linger {
		if m.linger[i].User == r.User {
			m.linger[i] = r
			return
		}
	}
	m.linger = append(m.linger, r)
}

// topLingerFirstLine is the first line of a command's error, which is what the
// message has room for.
func topLingerFirstLine(err error) string {
	if err == nil {
		return ""
	}
	text := strings.TrimSpace(err.Error())
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		text = text[:i]
	}
	return text
}
