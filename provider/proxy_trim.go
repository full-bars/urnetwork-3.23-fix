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

// buildTrimGradeResolver returns a per-address A-F grade resolver honoring the
// same desired-set ownership rule the grade summary uses (paid/file wins over a
// URL provenance tag when the address is in the desired set; otherwise the URL
// cache grade applies). This keeps the shed ranking on the real grade for both
// file and URL-sourced proxies (review finding CRITICAL).
func buildTrimGradeResolver(state *ProxyState, urlState *ProxyURLState, desiredSet map[string]*connect.ProxySettings) func(addr string) (float64, bool) {
	if urlState == nil {
		urlState = &ProxyURLState{Cache: map[string]ProxyURLEntry{}}
	}
	return func(addr string) (float64, bool) {
		if desiredSet != nil {
			if _, ok := desiredSet[addr]; ok {
				e := state.Proxies[addr]
				return e.Score, e.Graded
			}
		}
		if ue, ok := urlState.Cache[addr]; ok {
			return ue.Score, ue.Graded
		}
		e := state.Proxies[addr]
		return e.Score, e.Graded
	}
}

// selectWorstRunningProxies ranks the given running addresses worst-first using
// the A-F website-reachability grade (ProxyEntry.Score / Graded), health, and
// per-address traffic, and returns the worst `n` to shed. Ungraded proxies shed
// before any graded one (they have never proven reachability); among graded,
// lower reachability score (worse grade) sheds first; traffic is the tiebreak so
// an earning proxy is shed last.
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
		// grade: -1 (ungraded) sheds before any graded score.
		ag, bg := a.grade, b.grade
		if ag != bg {
			if ag < bg {
				return -1
			}
			return 1
		}
		if a.traffic != b.traffic {
			if a.traffic < b.traffic {
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
		traffic[key] = bw.TotalRx.Load() + bw.TotalTx.Load()
	}
	return traffic
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
	if err != nil || state.StartedAt.IsZero() {
		shmLogFatal(60, "provider does not appear to be running")
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
		if err := writeTrimTarget(0); err != nil {
			shmLogFatal(62, "could not clear proxy trim cap: %v", err)
		}
		fmt.Println("proxy trim: cap cleared")
		triggerProxyReload()
		return
	}

	running := make([]string, 0, len(state.Proxies))
	for addr := range state.Proxies {
		running = append(running, addr)
	}
	traffic := runningProxyTraffic()

	if preview {
		if len(running) > count {
			urlState, _ := readProxyURLState()
			gradeFor := buildTrimGradeResolver(state, urlState, nil)
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
		shmLogFatal(63, "could not write proxy trim cap: %v", err)
	}
	fmt.Printf("proxy trim: cap set to %d running proxies; reloading to shed the worst-graded above it\n", count)
	triggerProxyReload()
}
