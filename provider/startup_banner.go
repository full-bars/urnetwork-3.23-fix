package main

import (
	"fmt"
	"os"
	"runtime"
	"sync"
	"time"
)

// startupBanner collects phase results during boot and prints a clean
// summary when the provider is ready. Under systemd, phases emit via
// tlog (for the journal) and the final banner is suppressed. In
// interactive mode, phases render to stderr and the banner is printed.
type startupBanner struct {
	mu        sync.Mutex
	phases    []phaseResult
	startTime time.Time
}

type phaseResult struct {
	name    string
	status  string // "✓", "⚠", "✗"
	detail  string
	elapsed time.Duration
}

var banner = &startupBanner{startTime: time.Now()}

// phase returns a finish function for the named phase.
func (b *startupBanner) phase(name string) func(detail string, status ...string) {
	start := time.Now()
	return func(detail string, status ...string) {
		elapsed := time.Since(start)
		st := "✓"
		if len(status) > 0 {
			st = status[0]
		}
		b.mu.Lock()
		b.phases = append(b.phases, phaseResult{
			name:    name,
			status:  st,
			detail:  detail,
			elapsed: elapsed,
		})
		b.mu.Unlock()
	}
}

// printReady renders the final startup banner.
func (b *startupBanner) printReady() {
	b.mu.Lock()
	defer b.mu.Unlock()

	profile := os.Getenv("URNETWORK_PROFILE")
	if profile == "" {
		profile = "auto"
	}

	ramlogs := os.Getenv("URNETWORK_RAMLOGS") == "1"
	logDest := "stderr"
	if ramlogs {
		logDest = "/dev/shm"
	}

	totalElapsed := time.Since(b.startTime)
	isWindows := runtime.GOOS == "windows"

	// Print phase summary
	fmt.Println()
	for _, p := range b.phases {
		if isWindows {
			fmt.Printf("  %s %-24s %s", p.status, p.name, p.detail)
		} else {
			fmt.Fprintf(os.Stderr, "  %s %-24s %s", p.status, p.name, p.detail)
		}
		if p.elapsed > 100*time.Millisecond {
			fmt.Printf("  (%s)", p.elapsed.Round(time.Millisecond))
		}
		fmt.Println()
	}
	fmt.Println()

	// Ready line
	version := RequireVersion()
	if version == "" {
		version = "unknown"
	}
	host, _ := os.Hostname()
	if host == "" {
		host = "unknown"
	}

	if isWindows {
		fmt.Printf("== Ready ==\n")
		fmt.Printf("  Provider %s on %s\n", version, host)
		fmt.Printf("  Profile: %s | Logs -> %s\n", profile, logDest)
		fmt.Printf("  Boot time: %s\n", totalElapsed.Round(time.Millisecond))
	} else {
		fmt.Fprintln(os.Stderr, "── Ready ─────────────────────────────────")
		fmt.Fprintf(os.Stderr, "  Provider %s on %s\n", version, host)
		fmt.Fprintf(os.Stderr, "  Profile: %s | Logs -> %s\n", profile, logDest)
		fmt.Fprintf(os.Stderr, "  Boot time: %s\n", totalElapsed.Round(time.Millisecond))
		fmt.Fprintln(os.Stderr)
	}
}

// bannerPhase returns a finish function. Under systemd, it logs to
// tlog (for the journal) and does NOT suppress the event. The banner
// is only the interactive overlay in terminal mode.
func bannerPhase(name string) func(string, ...string) {
	condensed := startupBannerCondensed()
	bannerFinish := banner.phase(name)

	return func(detail string, status ...string) {
		st := "ok"
		if len(status) > 0 {
			st = status[0]
		}

		if condensed {
			// Under systemd: always emit tlog so the journal captures
			// the event. The visual banner is suppressed.
			tlog("[startup] %s: %s [%s]\n", name, detail, st)
			return
		}

		// Interactive mode: finish the banner (for the summary box).
		// The banner.finish() handles the visual.
		bannerFinish(detail, status...)
	}
}

// startupBannerCondensed returns true when running under systemd
// (journal handles the structured output) or when explicitly disabled.
func startupBannerCondensed() bool {
	if os.Getenv("INVOCATION_ID") != "" {
		return true // systemd
	}
	return os.Getenv("URNETWORK_STARTUP_BANNER") == "0"
}

// printReadyUnlessCondensed prints the final banner unless under systemd.
func printReadyUnlessCondensed() {
	if !startupBannerCondensed() {
		banner.printReady()
	}
}
