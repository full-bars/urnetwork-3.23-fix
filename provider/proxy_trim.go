package main

import (
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"sync/atomic"

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
		// Treat unparseable as no cap, never a false cap, but say so once: a
		// cap the operator believes is set has silently stopped applying.
		// The ramlog line is immediate, but the events.log copy is an
		// open+write+fsync and this runs while reload() holds r.mu, so it is
		// queued for after the lock is released (same reason the trim and
		// ledger writes there are deferred). Callers with no deferred queue
		// simply drain it themselves.
		if w := trimGarbageWarning(s); w != "" {
			tlog("%s\n", w)
			deferCritWrite(w)
		}
		return 0, nil
	}
	return n, nil
}

// deferredCrit holds critical-log lines produced while a caller holds a lock
// it must not block on. Drained by drainDeferredCrit after the lock is gone.
var deferredCrit struct {
	sync.Mutex
	lines    []string
	draining bool
	cond     *sync.Cond
}

// deferCritWrite queues a line for the post-unlock critical-log write.
func deferCritWrite(line string) {
	deferredCrit.Lock()
	defer deferredCrit.Unlock()
	deferredCrit.lines = append(deferredCrit.lines, line)
}

// drainDeferredCritFn hands the queued lines to fn, in the order the batches
// were queued. Batches are serialised, so lines reach events.log in the order
// the reloads that produced them ran, but the file I/O happens with no lock
// held.
//
// The serialisation is needed because r.mu is released by reload()'s OWN defer,
// which runs AFTER this drain defer: a second reload can otherwise start and
// finish its drain while the first is still writing, and the trim ledger can
// record a later cap change before an earlier one.
func drainDeferredCritFn(fn func([]string)) {
	deferredCrit.Lock()
	if deferredCrit.cond == nil {
		deferredCrit.cond = sync.NewCond(&deferredCrit.Mutex)
	}
	for deferredCrit.draining {
		deferredCrit.cond.Wait()
	}
	deferredCrit.draining = true
	batch := deferredCrit.lines
	deferredCrit.lines = nil
	deferredCrit.Unlock() // the writes themselves hold no lock

	func() {
		defer func() {
			deferredCrit.Lock()
			deferredCrit.draining = false
			deferredCrit.cond.Broadcast()
			deferredCrit.Unlock()
		}()
		fn(batch)
	}()
}

// drainDeferredCrit returns and clears the queued lines without writing them.
// Used where the caller wants the lines itself (tests, and the startup path
// which writes them directly). It does NOT participate in batch ordering, so
// only one such caller may run at a time; the startup path runs once per
// process, before reloads exist.
func drainDeferredCrit() []string {
	deferredCrit.Lock()
	defer deferredCrit.Unlock()
	out := deferredCrit.lines
	deferredCrit.lines = nil
	return out
}

// trimGarbageSeen is every unparseable proxy_trim value already warned about,
// so a bad file is reported once instead of on every reload. A SET, keyed by the
// FULL value: keying on a truncated one collides (two different long values
// sharing a first 40 bytes would look identical), and keeping only the last
// value re-warns on an A, B, A sequence. Growth is bounded by the operator
// editing the file, not by the reload loop.
var trimGarbageSeen struct {
	sync.Mutex
	seen map[string]bool
}

// trimGarbageReset clears the warned set. Tests only.
func trimGarbageReset() {
	trimGarbageSeen.Lock()
	defer trimGarbageSeen.Unlock()
	trimGarbageSeen.seen = nil
}

// trimGarbageWarning returns the warning line for unparseable proxy_trim
// content the first time that exact value is seen, and "" for any repeat.
// Truncation happens only when formatting the message, never in the key.
func trimGarbageWarning(content string) string {
	trimGarbageSeen.Lock()
	if trimGarbageSeen.seen == nil {
		trimGarbageSeen.seen = map[string]bool{}
	}
	if trimGarbageSeen.seen[content] {
		trimGarbageSeen.Unlock()
		return ""
	}
	trimGarbageSeen.seen[content] = true
	trimGarbageSeen.Unlock()

	shown := content
	if len(shown) > 40 {
		shown = shown[:40] + "..."
	}
	return fmt.Sprintf("[proxy][trim] warn: proxy_trim holds %q, which is not a proxy count, so no operator cap applies; set one with `urnet-tools proxy trim <count>`", shown)
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
	// Write to a unique temp file and rename over the target. A plain WriteFile
	// truncates first, so a reader (or a crash) between truncate and write sees
	// an empty file, which reads as "no cap", and the cap silently lapses.
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.WriteString(strconv.Itoa(n)); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return nil
}

// trimPreviewText renders what `proxy trim <count>` would do to the given
// running set. It is pure so it can be tested and so the running provider (the
// only process that knows its running set) can produce it for the CLI.
func trimPreviewText(state *ProxyState, urlState *ProxyURLState, running []string, traffic map[string]uint64, count int) string {
	if len(running) <= count {
		return fmt.Sprintf("preview: running=%d <= %d, nothing to shed\n", len(running), count)
	}
	gradeFor := buildTrimGradeResolver(state, urlState)
	shed := selectWorstRunningProxies(state.Proxies, gradeFor, traffic, running, len(running)-count)
	var b strings.Builder
	fmt.Fprintf(&b, "preview: %d running; would shed %d worst-graded to reach %d:\n", len(running), len(shed), count)
	for _, key := range shed {
		fmt.Fprintf(&b, "  %s\n", proxyKeyDisplay(key))
	}
	return b.String()
}

// livePreviewText builds the preview from THIS process's running set. It must
// run inside the provider (via the control socket): a CLI process has an empty
// health registry and would always report zero running proxies.
func livePreviewText(count int) (string, error) {
	state, err := readProxyState()
	if err != nil {
		return "", err
	}
	urlState, _ := readProxyURLState()
	// The direct transport registers itself in the health registry, but a live
	// trim never counts or sheds it (reload skips directProxyKey and the cap
	// applies to non-direct proxies only), so the preview must not either.
	var running []string
	for _, k := range runningProxyAddresses() {
		if k != directProxyKey {
			running = append(running, k)
		}
	}
	return trimPreviewText(state, urlState, running, runningProxyTraffic(), count), nil
}

// previewViaControlSocket asks the running provider what a trim would shed.
func previewViaControlSocket(count int) (string, error) {
	resp, err := dialControlSocket(controlRequest{Cmd: "trim_preview", Value: strconv.Itoa(count)})
	if err != nil {
		return "", err
	}
	if !resp.OK {
		return "", fmt.Errorf("%s", resp.Error)
	}
	return resp.Value, nil
}

// startupTrimSelection splits the desired list into the proxies to launch and
// the proxies to hold back so a fresh provider never launches more than the
// operator's trim cap. Nothing is running yet, so the "worst" are chosen from
// persisted health, decayed earnings (a proven earner is protected) and grade,
// with the same ranking a live trim uses. Launch order is preserved; held
// proxies stay desired and are admitted by the reload budget when the cap
// rises. A cap <= 0 or >= len(all) launches everything.
func startupTrimSelection(all []*connect.ProxySettings, cap int, state map[string]ProxyEntry, gradeFor func(string) (float64, bool), earnings func(string) float64) (launch, held []*connect.ProxySettings) {
	if cap <= 0 || len(all) <= cap {
		return all, nil
	}
	keys := make([]string, 0, len(all))
	traffic := make(map[string]uint64, len(all))
	for _, p := range all {
		k := p.Key()
		keys = append(keys, k)
		if e := earnings(k); e > 0 {
			traffic[k] = uint64(e)
		}
	}
	heldSet := make(map[string]bool, len(all)-cap)
	for _, k := range selectWorstRunningProxies(state, gradeFor, traffic, keys, len(all)-cap) {
		heldSet[k] = true
	}
	launch = make([]*connect.ProxySettings, 0, cap)
	held = make([]*connect.ProxySettings, 0, len(all)-cap)
	for _, p := range all {
		if heldSet[p.Key()] {
			held = append(held, p)
		} else {
			launch = append(launch, p)
		}
	}
	return launch, held
}

// trimCapSeen is the last effective trim cap the reload loop acknowledged
// (0 = none). It lets the reload log a receipt exactly once per change.
// trimCapSourceSeen is the source (trimCapOperator/trimCapOOM) that cap came
// from, so a later change away from it (in particular a clear) can attribute
// the ledger entry correctly instead of guessing.
var (
	trimCapSeen       atomic.Int64
	trimCapSourceSeen atomic.Value // string
)

// primeTrimCapSeen seeds trimCapSeen/trimCapSourceSeen with the cap this
// process's startup already read and acted on (startupTrimSelection), without
// logging anything. Without this, the first reload after a capped startup
// reads the same cap fresh (prev==0) and treats it as new: a duplicate
// "[proxy][trim] received" line and a duplicate ledger "applied" entry, whose
// From is a partial mid-ramp running count even though startup already logged
// and applied the cap.
func primeTrimCapSeen(trimCap int, source string) {
	trimCapSeen.Store(int64(trimCap))
	trimCapSourceSeen.Store(source)
}

// noteTrimCap records the cap and source the reload just read and reports the
// previous cap and source and whether the cap differs, so a new or cleared cap
// is acknowledged in the log once instead of on every periodic reload.
func noteTrimCap(cur int, source string) (prev int, prevSource string, changed bool) {
	prev = int(trimCapSeen.Swap(int64(cur)))
	if v, ok := trimCapSourceSeen.Load().(string); ok {
		prevSource = v
	}
	trimCapSourceSeen.Store(source)
	return prev, prevSource, prev != cur
}

func resetTrimCapSeen() {
	trimCapSeen.Store(0)
	trimCapSourceSeen.Store("")
}

// trimmedConfiguredCount is the number of proxies this provider will actually
// run: the desired count, capped by the operator trim target. The status line
// uses it as its denominator, so a healthy box trimmed to N reads N/N, not
// N/desired (which reads "critical" and teaches operators to ignore it).
func trimmedConfiguredCount(desired int) int {
	if cap, err := effectiveTrimCap(); err == nil && cap > 0 && desired > cap {
		return cap
	}
	return desired
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

// buildTrimGradeResolver returns a per-identity-key A-F grade resolver backed by the
// existing proxyGradeFor unifier, which reads the grade from the correct store
// for each proxy (paid/file ProxyEntry wins, else the URL cache ProxyURLEntry)
// nil urlState degrades to the paid store only.
func buildTrimGradeResolver(state *ProxyState, urlState *ProxyURLState) func(key string) (float64, bool) {
	return func(key string) (float64, bool) {
		g, ok := proxyGradeFor(key, state, urlState)
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

// runningProxyTraffic builds a per-identity traffic map (keyed by
// ProxySettings.Key(): address, or address+user for a credentialed proxy) for
// the shed tiebreak. The key MUST match the running list and proxy.state keys
// the rankings look it up with; keying by the bare address made a credentialed
// proxy's traffic invisible, so an active earner ranked as idle and could be
// shed first. Best-effort: only used as a last-resort tiebreak among proxies
// with identical health and grade.
func runningProxyTraffic() map[string]uint64 {
	traffic := map[string]uint64{}
	for key, bw := range connect.ProxyBandwidthSnapshotByKey() {
		if bw == nil {
			continue
		}
		traffic[key] += bw.TotalRx.Load() + bw.TotalTx.Load()
	}
	return traffic
}

// runningProxyAddresses returns the currently RUNNING proxies as identity keys
// (ProxySettings.Key(): address, or address+user for a credentialed proxy) from
// the health surface (bandwidth + connecting), so --preview reports the running
// pool rather than the larger desired set in proxy.state. The keys must match
// the ones proxy.state and runningProxyTraffic use, or the preview ranks a
// credentialed proxy as unknown and idle. The health snapshot lists proxies as
// "proxy[N] (addr)" display strings, so each one is resolved to its identity via
// the registry index, falling back to the parsed address when it is not registered.
func runningProxyAddresses() []string {
	_, _, _, bandwidth, connecting := connect.ProxyHealthSnapshot()
	seen := map[string]bool{}
	var out []string
	add := func(display string) {
		key := ""
		if idx := parseProxyIndex(display); idx >= 0 {
			key = connect.ProxyKeyByIndex(idx)
		}
		if key == "" {
			_, key = parseProxyString(display)
		}
		if key == "" || seen[key] {
			return
		}
		seen[key] = true
		out = append(out, key)
	}
	for display := range bandwidth {
		add(display)
	}
	for _, c := range connecting {
		add(c)
	}
	sort.Strings(out)
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

	if preview {
		text, err := previewViaControlSocket(count)
		if err != nil {
			// Never fall back to a local computation: this process has no running
			// proxies, so it would report "nothing to shed" and mislead.
			fmt.Printf("preview unavailable: %v (the running provider computes the preview; it must be running and on a build with trim_preview)\n", err)
			return
		}
		fmt.Print(text)
		return
	}

	if err := writeTrimTarget(count); err != nil {
		shmLogFatal(72, "could not write proxy trim cap: %v", err)
	}
	fmt.Printf("proxy trim: cap set to %d running proxies; reloading to shed the worst-graded above it\n", count)
	triggerProxyReload()
}
