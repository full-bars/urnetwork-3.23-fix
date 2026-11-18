package urnettools

import (
	"fmt"
	"os"
	"os/signal"
	"time"

	"golang.org/x/term"
)

// runStatusLiveFn is a seam for tests to stub live status refresh.
var runStatusLiveFn = runStatusLive

// runStatusLive refreshes the one-screen summary in place until interrupted,
// without clearing the whole screen so output stays readable.
func runStatusLive(p Provider, interval time.Duration, plain bool) error {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)
	defer signal.Stop(stop)

	for {
		d := collectStatusData(p)
		text, err := formatStatusSummary(d, plain)
		if err != nil {
			return err
		}
		redrawLive(text, plain)
		select {
		case <-stop:
			return nil
		case <-tick.C:
		}
	}
}

// redrawLive writes the summary for one live frame. On an interactive terminal
// it uses cursor home plus erase-below so redraws do not flicker; in plain
// mode or when stdout is not a terminal, it repeatedly prints so logs and
// non-interactive shells still see updates.
func redrawLive(text string, plain bool) {
	fd := int(os.Stdout.Fd())
	if plain || !term.IsTerminal(fd) {
		fmt.Print(text)
		return
	}
	// cursor home, erase below — no full clear, no flicker
	fmt.Print("\x1b[H\x1b[J")
	fmt.Print(text)
}
