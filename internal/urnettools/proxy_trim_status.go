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
// configured pool size, and the live counts, so a reduced-capacity box is
// visible without guessing from the proxy count.
func printTrimStatus(p Provider) error {
	cap, raw, err := readTrimFile(p.StateDir)
	if err != nil {
		return fmt.Errorf("read trim cap: %w", err)
	}

	snap, _, snapErr := fetchSnapshotFn(p)
	hasSnap := snapErr == nil

	if cap < 0 {
		fmt.Printf("Trim cap: none (%q in %s is not a proxy count)\n", raw, filepath.Join(p.StateDir, "proxy_trim"))
	} else if cap == 0 {
		fmt.Printf("Trim cap: none, this provider runs its full pool\n")
	} else {
		fmt.Printf("Trim cap: %d running proxies (operator)\n", cap)
	}

	if !hasSnap {
		if cap > 0 {
			fmt.Printf("The provider is not answering, so the configured pool size and live counts are unavailable\n")
		}
		return nil
	}

	px := snap.Proxies
	configured := px.Configured
	effective := px.TrimCap
	source := trimSourceLabel(px.TrimCapSource)

	switch {
	case effective > 0 && configured > effective:
		fmt.Printf("Configured pool: %d, holding %d back (%s)\n", configured, configured-effective, source)
	case effective > 0:
		fmt.Printf("Configured pool: %d, at or below the %s of %d\n", configured, source, effective)
	case configured > 0:
		fmt.Printf("Configured pool: %d, no cap applied\n", configured)
	}

	total := px.Up + px.Degraded + px.Connecting + px.Dead
	if total > 0 {
		fmt.Printf("Running now: %d up, %d connecting, %d dropped, %d dead (%d total)\n",
			px.Up, px.Connecting, px.Degraded, px.Dead, total)
	}
	return nil
}
