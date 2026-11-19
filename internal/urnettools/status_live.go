package urnettools

import (
	"fmt"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

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

	var lastDiscovery time.Time
	prevLines := 0
	for {
		// The provider may restart or stop while the screen is open, so its
		// lifecycle state (running, pid, version) is looked up again, not
		// reused from the first frame.
		if time.Since(lastDiscovery) >= statusLiveRediscoverEvery {
			p = refreshStatusProvider(p)
			lastDiscovery = time.Now()
		}
		d := collectStatusData(p)
		text, err := formatStatusSummary(d, plain)
		if err != nil {
			return err
		}
		prevLines = redrawLive(text, plain, prevLines)
		select {
		case <-stop:
			return nil
		case <-tick.C:
		}
	}
}

// statusLiveRediscoverEvery limits how often the live loop looks the provider
// up again, so a short --interval does not run discovery on every frame.
const statusLiveRediscoverEvery = 2 * time.Second

// refreshStatusProvider returns the provider as discovery sees it now, matched
// by its stable key. When discovery no longer lists it, the earlier value is
// kept and the snapshot rows report that it does not answer.
func refreshStatusProvider(p Provider) Provider {
	key := matchKey(p)
	for _, q := range discoverStatusFn() {
		if matchKey(q) == key {
			return q
		}
	}
	return p
}

var ansiEscape = regexp.MustCompile("\x1b\\[[0-9;?]*[A-Za-z]")

// statusFrameLines counts the terminal rows a frame occupies, with lines that
// wrap at the terminal width counted as more than one row.
func statusFrameLines(text string, width int) int {
	text = strings.TrimSuffix(text, "\n")
	if text == "" {
		return 0
	}
	rows := 0
	for _, line := range strings.Split(text, "\n") {
		n := utf8.RuneCountInString(ansiEscape.ReplaceAllString(line, ""))
		if width > 0 && n > width {
			rows += (n + width - 1) / width
		} else {
			rows++
		}
	}
	return rows
}

// redrawLive writes the summary for one live frame and returns how many rows it
// used. On an interactive terminal it moves the cursor up over the previous
// frame and erases from there, so only that frame is replaced and the rest of
// the screen stays. When the frame no longer fits the screen it clears the
// whole screen instead. In plain mode or when stdout is not a terminal, it
// prints each frame in turn so logs and non-interactive shells still see
// updates.
func redrawLive(text string, plain bool, prevLines int) int {
	fd := int(os.Stdout.Fd())
	if plain || !term.IsTerminal(fd) {
		fmt.Print(text)
		return 0
	}
	width, height, err := term.GetSize(fd)
	if err != nil {
		width, height = 0, 0
	}
	fmt.Print(liveRedrawPrefix(prevLines, height))
	fmt.Print(text)
	return statusFrameLines(text, width)
}

// liveRedrawPrefix is the escape sequence that clears the previous frame: none
// for the first frame, cursor home and erase when the frame filled the screen,
// and otherwise cursor up over exactly that frame and erase below it.
func liveRedrawPrefix(prevLines, height int) string {
	switch {
	case prevLines == 0:
		return ""
	case height > 0 && prevLines >= height:
		return "\x1b[H\x1b[J"
	default:
		return fmt.Sprintf("\x1b[%dA\r\x1b[J", prevLines)
	}
}
