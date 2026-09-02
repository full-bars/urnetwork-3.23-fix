package main

import (
	"os"
	"path/filepath"
	"slices"

	"fmt"
	"github.com/docopt/docopt-go"
	"github.com/urnetwork/connect"
	"strconv"
	"strings"
)

// proxy_trim.go implements the operator `proxy trim <N>` hard cap: hold the
// running proxy pool at (or below) N, shedding the worst-graded (A-F) proxies
// above it. The target is persisted so it survives restarts and reloads and
// stays in effect until raised or cleared.

// proxyTrimPath returns the operator trim-target file path.
func proxyTrimPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".urnetwork", "proxy_trim"), nil
}

// readTrimTarget returns the operator hard cap on running proxies (0 = no cap).
// A missing, empty, "off", or unparseable file means no cap.
func readTrimTarget() (int, error) {
	path, err := proxyTrimPath()
	if err != nil {
		return 0, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	s := strings.ToLower(strings.TrimSpace(string(b)))
	if s == "" || s == "off" || s == "0" {
		return 0, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0, nil // treat unparseable as no cap, never a false cap
	}
	return n, nil
}

// writeTrimTarget sets the operator cap. n <= 0 clears it.
func writeTrimTarget(n int) error {
	path, err := proxyTrimPath()
	if err != nil {
		return err
	}
	if n <= 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(strconv.Itoa(n)), 0o600)
}

// healthRank orders the shed priority by last-known health (lower = shed first),
// mirroring the URL pool controller's ranking.
func healthRank(health string) int {
	switch health {
	case "dead":
		return 0
	case "inactive":
		return 1
	case "long_offline":
		return 2
	case "offline":
		return 3
	case "recently_offline":
		return 4
	default: // "up" and unknown shed last
		return 5
	}
}

// trimRank captures the sort key for a running proxy: shed the A-F-worst first.
type trimRank struct {
	addr    string
	health  int
	grade   float64 // -1 = never graded (shed before any graded proxy)
	traffic uint64
}

// buildTrimGradeResolver returns a per-address A-F grade resolver backed by the
// existing proxyGradeFor unifier, which reads the grade from the correct store
// for each proxy (paid/file ProxyEntry wins, else the URL cache ProxyURLEntry)
// nil urlState degrades to the paid store only.
func buildTrimGradeResolver(state *ProxyState, urlState *ProxyURLState) func(addr string) (float64, bool) {
	return func(addr string) (float64, bool) {
		g, ok := proxyGradeFor(addr, state, urlState)
		if !ok {
			return 0, false
		}
		return g.Score, true
	}
}

// effectiveTrimScore maps the probe grade score to an effective reputation for
// trim eviction. Confirmed F-tier proxies (score < 0.6) are proven broken/failing
// and shed before unverified/probationary (ungraded) proxies. Ungraded proxies
// are assigned an effective score of 0.595 (sitting between F < 0.6 and D >= 0.6),
// so proven failures shed first, while proven healthy D/C/B/A are retained.
func effectiveTrimScore(grade float64) float64 {
	if grade < 0 {
		return 0.595 // ungraded / probationary
	}
	return grade
}

// selectWorstRunningProxies ranks the given running addresses worst-first using
// health, active billable traffic protection, and reachability grade score, and
// returns the worst `n` to shed.
//
// Eviction Hierarchy:
//  1. Unhealthy/Dead First: Dead, inactive, and offline proxies always shed first.
//  2. Earning Protection: Proxies with active billable traffic (traffic > 0) are
//     protected against idle proxies (traffic == 0). An active earner is never
//     shed while idle proxies remain.
//  3. Idle Proxies (traffic == 0): F-tier (proven failing, score < 0.6) sheds first
//     -> Ungraded / Probationary (0.595) -> D-tier (0.6) -> C -> B -> A.
//  4. Active Earners (traffic > 0): Smaller traffic earners shed before larger
//     earners (preserving high revenue streams), with grade as secondary tiebreak.
func selectWorstRunningProxies(state map[string]ProxyEntry, gradeFor func(addr string) (float64, bool), traffic map[string]uint64, running []string, n int) []string {
	var cands []trimRank
	for _, addr := range running {
		e := state[addr]
		rank := trimRank{addr: addr, health: healthRank(e.Health), grade: -1, traffic: traffic[addr]}
		if gradeFor != nil {
			if score, graded := gradeFor(addr); graded {
				rank.grade = score
			}
		} else if e.Graded {
			rank.grade = e.Score
		}
		cands = append(cands, rank)
	}
	slices.SortFunc(cands, func(a, b trimRank) int {
		if a.health != b.health {
			return a.health - b.health
		}

		hasTrafficA := a.traffic > 0
		hasTrafficB := b.traffic > 0
		if hasTrafficA != hasTrafficB {
			if !hasTrafficA {
				return -1 // A is idle, B is earning -> shed A first
			}
			return 1 // A is earning, B is idle -> shed B first
		}

		if hasTrafficA && hasTrafficB {
			// Both are active earners: preserve larger traffic streams.
			if a.traffic != b.traffic {
				if a.traffic < b.traffic {
					return -1
				}
				return 1
			}
		}

		// Among same traffic level (e.g. both idle, or equal traffic):
		// Higher reputation score wins (F < Ungraded < D < C < B < A).
		effA := effectiveTrimScore(a.grade)
		effB := effectiveTrimScore(b.grade)
		if effA != effB {
			if effA < effB {
				return -1
			}
			return 1
		}

		return strings.Compare(a.addr, b.addr)
	})
	if n > len(cands) {
		n = len(cands)
	}
	out := make([]string, n)
	for i := 0; i < n; i++ {
		out[i] = cands[i].addr
	}
	return out
}

// runningProxyTraffic builds a per-address traffic map (keyed on the address)
// for the shed tiebreak. Best-effort: only used as a last-resort tiebreak among
// addresses with identical health and grade.
func runningProxyTraffic() map[string]uint64 {
	_, _, _, bandwidth, _ := connect.ProxyHealthSnapshot()
	traffic := map[string]uint64{}
	for key, bw := range bandwidth {
		if bw == nil {
			continue
		}
		// Bandwidth is keyed by the display string "proxy[<n>] (<addr>)"; the
		// tiebreak must key by the address so it matches ProxyEntry keys.
		_, hp := parseProxyString(key)
		traffic[hp] += bw.TotalRx.Load() + bw.TotalTx.Load()
	}
	return traffic
}

// runningProxyAddresses returns the currently RUNNING addresses from the health
// surface (bandwidth + connecting), so --preview reports the running pool rather
// than the larger desired set in proxy.state).
func runningProxyAddresses() []string {
	_, _, _, bandwidth, connecting := connect.ProxyHealthSnapshot()
	seen := map[string]bool{}
	var out []string
	add := func(a string) {
		if a == "" || seen[a] {
			return
		}
		seen[a] = true
		out = append(out, a)
	}
	for key := range bandwidth {
		_, hp := parseProxyString(key)
		add(hp)
	}
	for _, c := range connecting {
		add(c)
	}
	return out
}

// triggerProxyReload pokes the running provider's reload watcher so it applies
// a state change (trim target) immediately.
func triggerProxyReload() {
	if reloadPath, err := proxyReloadPath(); err == nil {
		if err := writeReloadTrigger(reloadPath); err != nil {
			tlog("[proxy][trim] warn: reload trigger write failed: %v\n", err)
		}
	}
}

// proxyTrim implements `provider proxy trim <count> [--preview]`: it sets (or
// clears) the operator hard cap on running proxies and triggers a reload so the
// running provider sheds the A-F-worst above the cap. --preview lists what would
// be shed without writing anything.
func proxyTrim(opts docopt.Opts) {
	state, err := readProxyState()
	if err != nil {
		shmLogFatal(70, "could not read provider state: %v", err)
	}

	count := 0
	if s, _ := opts.String("<count>"); s != "" {
		if strings.EqualFold(s, "off") {
			count = 0
		} else {
			if n, err := strconv.Atoi(s); err == nil && n >= 0 {
				count = n
			} else {
				fmt.Printf("invalid proxy count %q (use a number or 'off')\n", s)
				return
			}
		}
	}
	preview, _ := opts.Bool("--preview")

	if count <= 0 {
		if preview {
			fmt.Println("preview: would clear the proxy trim cap")
			return
		}
		// Clearing works even while the provider is stopped (a bad cap must be
		// removable), so the running check must NOT gate this path.
		if err := writeTrimTarget(0); err != nil {
			shmLogFatal(71, "could not clear proxy trim cap: %v", err)
		}
		fmt.Println("proxy trim: cap cleared")
		if state != nil && !state.StartedAt.IsZero() {
			triggerProxyReload()
		}
		return
	}

	if state.StartedAt.IsZero() {
		shmLogFatal(70, "provider does not appear to be running")
	}

	running := runningProxyAddresses()
	traffic := runningProxyTraffic()
	urlState, _ := readProxyURLState()
	gradeFor := buildTrimGradeResolver(state, urlState)

	if preview {
		if len(running) > count {
			shed := selectWorstRunningProxies(state.Proxies, gradeFor, traffic, running, len(running)-count)
			fmt.Printf("preview: %d running; would shed %d worst-graded to reach %d:\n", len(running), len(shed), count)
			for _, addr := range shed {
				fmt.Printf("  %s\n", addr)
			}
		} else {
			fmt.Printf("preview: running=%d <= %d, nothing to shed\n", len(running), count)
		}
		return
	}

	if err := writeTrimTarget(count); err != nil {
		shmLogFatal(72, "could not write proxy trim cap: %v", err)
	}
	fmt.Printf("proxy trim: cap set to %d running proxies; reloading to shed the worst-graded above it\n", count)
	triggerProxyReload()
}
