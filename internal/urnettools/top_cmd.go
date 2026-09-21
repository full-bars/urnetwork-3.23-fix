package urnettools

import (
	"context"
	"errors"
	"fmt"
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
	interval, targetArgs, err := parseTopFlags(args)
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
		t, _, err := parseTargetFlags(targetArgs)
		if err != nil {
			return err
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
	m := newTopModel(providers, cur, interval, tui.ThemeFromEnv(os.Getenv), time.Now)
	src := topSourceFn()
	if demo {
		src = newDemoTopSource(time.Now)
	}
	return runTop(ctx, scr, m, src, topTick)
}

// parseTopFlags pulls --interval out of args, leaving the target flags for the
// strict target parser. It accepts a Go duration (500ms, 2s) or plain seconds.
func parseTopFlags(args []string) (interval time.Duration, rest []string, err error) {
	interval = topDefaultInterval
	for i := 0; i < len(args); i++ {
		a := args[i]
		var val string
		switch {
		case a == "--interval":
			if i+1 >= len(args) {
				return 0, nil, errors.New("--interval needs a value, for example --interval 500ms")
			}
			i++
			val = args[i]
		case strings.HasPrefix(a, "--interval="):
			val = strings.TrimPrefix(a, "--interval=")
		default:
			rest = append(rest, a)
			continue
		}
		d, perr := time.ParseDuration(val)
		if perr != nil {
			secs, ferr := strconv.ParseFloat(val, 64)
			if ferr != nil {
				return 0, nil, fmt.Errorf("--interval %q is not a duration (try 500ms or 2s)", val)
			}
			d = time.Duration(secs * float64(time.Second))
		}
		if d < topMinInterval {
			return 0, nil, fmt.Errorf("--interval must be at least %s", topMinInterval)
		}
		interval = d
	}
	return interval, rest, nil
}
