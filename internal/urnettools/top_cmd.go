package urnettools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/urnetwork/connect/internal/tui"
	"github.com/urnetwork/connect/internal/tui/tcellui"
)

// Seams so the command is testable without a terminal or live providers.
var (
	topStdoutIsTerminal = func() bool { return term.IsTerminal(int(os.Stdout.Fd())) }
	topOpenScreen       = tcellui.Open
	topSourceFn         = func() topSource { return socketTopSource{} }
)

// cmdTop implements `urnet-tools top [target] [--interval D]`, the live
// full-screen view. Target selection is status's: the same flags, the same
// auto-narrowing to the one provider an unprivileged caller can reach, and the
// same cross-user re-exec under sudo.
func cmdTop(args []string) error {
	// --demo is hidden: it draws synthetic snapshots so the screen can be seen
	// on a box with no provider. It is peeled off before the real parsers.
	demo, args := stripDemoFlag(args)
	interval, copyOnQuit, plain, theme, targetArgs, err := parseTopFlags(args)
	if err != nil {
		return err
	}
	// Checked first: with no terminal there is nothing to draw on, and the
	// question of which provider to show has no bearing on that.
	if !topStdoutIsTerminal() {
		return errors.New("top needs an interactive terminal (stdout is not one); use `urnet-tools status` for one-shot or scripted output, add --json for machine-readable output")
	}
	providers, cur := []Provider{demoProvider()}, 0
	if !demo {
		t, extra, err := parseTargetFlags(targetArgs)
		if err != nil {
			return err
		}
		if len(extra) > 0 {
			return errUnexpectedPositional("top", extra)
		}
		providers = discoverStatusFn()
		p, _, err := selectTargetOrSoleAccessible(providers, t, false)
		if err != nil {
			// Several providers and no target: status summarizes them, top starts
			// on the first and lets Tab move between them.
			if hasExplicitTarget(t) || len(providers) < 2 {
				return err
			}
			p = providers[0]
		}
		if elevated, err := maybeElevateForCrossUser("top", p, args, false, false); elevated {
			return err
		}
		for i, q := range providers {
			if matchKey(q) == matchKey(p) {
				cur = i
				break
			}
		}
	}

	scr, err := topOpenScreen()
	if err != nil {
		return fmt.Errorf("cannot open the terminal for top: %w (use `urnet-tools status` instead)", err)
	}
	// SIGINT and SIGTERM end the loop through the context, so the terminal is
	// restored by runTop's own exit path rather than by the signal killing the
	// process mid-frame. In raw mode Ctrl-C arrives as a key, not a signal, and
	// is handled by the model.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	th := tui.ThemeFromEnv(os.Getenv)
	m := newTopModel(providers, cur, interval, th, time.Now)
	// A terminal that cannot report the mouse starts in Keyboard Mode, where
	// the terminal's own text selection works. --no-mouse asks for the same.
	m.mouseSupported = topTerminalReportsMouse(os.Getenv)
	if hasNoMouseFlag(args) || !m.mouseSupported {
		m.mouse = false
	}
	// The environment's own choice (NO_COLOR, a dumb terminal) outranks a saved
	// theme; see applySettings.
	m.themeLocked = th.ASCII
	m.settingsPath = topSettingsPath()
	m.applySettings(loadTopSettings(m.settingsPath))

	// A --theme flag overrides the saved preference. A saved theme takes effect
	// at once, so the choice applies live without a restart.
	if theme != "" {
		if neonTheme, ok := tui.ThemeByName(theme); ok {
			m.theme = neonTheme
			m.savedTheme = theme
			if err := saveTopSettings(m.settingsPath, m.settings()); err != nil {
				return err
			}
		} else {
			return fmt.Errorf("unknown theme %q (try one of: %s)", theme, strings.Join(tui.ThemeNames(), ", "))
		}
	}

	m.copyOnQuit, m.copyPlain = copyOnQuit, plain
	m.enableLogo(plain, copyOnQuit)
	m.clip = newTopClipboard(scr)
	src := topSourceFn()
	if demo {
		src = newDemoTopSource(time.Now)
		// Fill the demo heatmap at typical terminal sizes so the visualization
		// shows how a large proxy pool uses the available panel space.
		cells := make([]proxyCell, 4400)
		for i := range cells {
			cells[i] = proxyCell{index: i, state: uint8(i % 5), bucket: uint8(i % 8), clients: uint8(i % 4)}
		}
		m.applyProxyCells(m.gen, cells, nil)
	}
	if err := runTop(ctx, scr, m, src, topTick); err != nil {
		return err
	}
	// The copy is printed only after the terminal is back, so the screen text
	// lands on the real stdout, not the alternate screen.
	return reportTopCopy(m, os.Stdout, os.Stderr)
}

// reportTopCopy prints the copied screen and the result of saving it. A copy
// that could not be saved is an error, so a script that asked for --copy sees
// the failure in the exit status. The screen text is still printed first.
func reportTopCopy(m *topModel, stdout, stderr io.Writer) error {
	if m.copyText != "" {
		fmt.Fprintln(stdout, m.copyText)
	}
	if m.copyErr != nil {
		fmt.Fprintln(stderr, "could not write the copy:", m.copyErr)
		return fmt.Errorf("could not write the copy: %w", m.copyErr)
	}
	if m.copyPath != "" {
		fmt.Fprintf(stderr, "copy written to %s\n", m.copyPath)
	}
	return nil
}

func hasNoMouseFlag(args []string) bool {
	for _, a := range args {
		if a == "--no-mouse" || a == "--no-mouse=true" || a == "--no-mouse=1" {
			return true
		}
	}
	return false
}

// parseTopFlags pulls --interval, --no-mouse, --copy, --plain and --theme out
// of args, leaving the target flags for the strict target parser. It accepts a
// Go duration (500ms, 2s) for --interval.
func parseTopFlags(args []string) (interval time.Duration, copyOnQuit, plain bool, theme string, rest []string, err error) {
	interval = topDefaultInterval
	for i := 0; i < len(args); i++ {
		a := args[i]
		var val string
		switch {
		case a == "--no-mouse" || a == "--no-mouse=true" || a == "--no-mouse=false" || a == "--no-mouse=1" || a == "--no-mouse=0":
			continue
		case a == "--interval":
			if i+1 >= len(args) {
				return 0, false, false, "", nil, errors.New("--interval needs a value, for example --interval 500ms")
			}
			i++
			val = args[i]
		case strings.HasPrefix(a, "--interval="):
			val = strings.TrimPrefix(a, "--interval=")
		case a == "--copy":
			copyOnQuit = true
			continue
		case a == "--plain":
			plain = true
			continue
		case a == "--theme":
			if i+1 >= len(args) {
				return 0, false, false, "", nil, errors.New("--theme needs a value, for example --theme neon")
			}
			i++
			theme = args[i]
			continue
		case strings.HasPrefix(a, "--theme="):
			theme = strings.TrimPrefix(a, "--theme=")
			continue
		default:
			rest = append(rest, a)
			continue
		}
		d, perr := time.ParseDuration(val)
		if perr != nil {
			secs, ferr := strconv.ParseFloat(val, 64)
			if ferr != nil {
				return 0, false, false, "", nil, fmt.Errorf("--interval %q is not a duration (try 500ms or 2s)", val)
			}
			d = time.Duration(secs * float64(time.Second))
		}
		if d < topMinInterval {
			return 0, false, false, "", nil, fmt.Errorf("--interval must be at least %s", topMinInterval)
		}
		interval = d
	}
	return interval, copyOnQuit, plain, theme, rest, nil
}
