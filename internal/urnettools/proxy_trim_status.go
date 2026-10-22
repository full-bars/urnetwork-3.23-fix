package urnettools

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// trimSourceLabel turns the provider's cap source into operator-facing words.
func trimSourceLabel(source string) string {
	switch source {
	case "operator":
		return "operator cap"
	case "oomcap":
		return "OOM cap"
	case "thrashcap":
		return "thrash cap"
	case "":
		return ""
	default:
		return source
	}
}

// readTrimFile returns the operator cap stored beside the provider state
// (0 = none, -1 = unparseable) and the raw value for reporting.
func readTrimFile(stateDir string) (int, string, error) {
	path := filepath.Join(stateDir, "proxy_trim")
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, "", nil
		}
		return 0, "", err
	}
	raw := strings.TrimSpace(string(b))
	lower := strings.ToLower(raw)
	if lower == "" || lower == "off" || lower == "0" {
		return 0, raw, nil
	}
	n, convErr := strconv.Atoi(lower)
	if convErr != nil || n < 0 {
		return -1, raw, nil
	}
	return n, raw, nil
}

// printTrimStatus reports the trim cap this provider is running under, the
// configured pool size and the live counts, so a reduced-capacity box is
// visible without inferring it from the proxy count.
func printTrimStatus(p Provider) error {
	fileCap, raw, err := readTrimFile(p.StateDir)
	if err != nil {
		return fmt.Errorf("read trim cap: %w", err)
	}

	snap, _, snapErr := fetchSnapshotFn(p)
	if snapErr != nil {
		printFileTrimCap(fileCap, raw, p.StateDir)
		if fileCap > 0 {
			fmt.Printf("The provider is not answering, so the configured pool size and live counts are unavailable\n")
		}
		return nil
	}

	px := snap.Proxies
	// A provider that reports the fields is authoritative: an automatic cap
	// can be lower than the operator file, and only the provider knows which
	// one is in force.
	if px.Configured > 0 || px.TrimCap > 0 {
		if px.TrimCap > 0 {
			fmt.Printf("Trim cap: %d running proxies (%s)\n", px.TrimCap, orDash(trimSourceLabel(px.TrimCapSource)))
		} else {
			fmt.Printf("Trim cap: none, this provider runs its full pool\n")
		}
		switch {
		case px.TrimCap > 0 && px.Configured > px.TrimCap:
			fmt.Printf("Configured pool: %d, holding %d back\n", px.Configured, px.Configured-px.TrimCap)
		case px.Configured > 0:
			fmt.Printf("Configured pool: %d, no cap applied\n", px.Configured)
		}
	} else {
		// Provider predates trim reporting: fall back to the operator file.
		printFileTrimCap(fileCap, raw, p.StateDir)
		if fileCap > 0 {
			fmt.Printf("This provider build does not report the configured pool size\n")
		}
	}

	if total := px.Up + px.Degraded + px.Connecting + px.Dead; total > 0 {
		fmt.Printf("Running now: %d up, %d connecting, %d dropped, %d dead (%d total)\n",
			px.Up, px.Connecting, px.Degraded, px.Dead, total)
	}
	return nil
}

// printFileTrimCap reports the operator file's cap, used when the provider is
// stopped or too old to report the effective one.
func printFileTrimCap(cap int, raw, stateDir string) {
	switch {
	case cap < 0:
		fmt.Printf("Trim cap: none (%q in %s is not a proxy count)\n", raw, filepath.Join(stateDir, "proxy_trim"))
	case cap == 0:
		fmt.Printf("Trim cap: none, this provider runs its full pool\n")
	default:
		fmt.Printf("Trim cap: %d running proxies (operator)\n", cap)
	}
}
