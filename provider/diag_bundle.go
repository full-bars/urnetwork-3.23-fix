package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"runtime/pprof"
	"sort"
	"strings"
	"sync"
	"time"
)

// A diagnostic bundle is the evidence for "why is this provider using so much
// memory or CPU", taken from the RUNNING process. The profiles are written to
// disk by this process with runtime/pprof, so there is no listener, no port and
// no token: the request arrives on the owner-only control socket and the files
// land in <state dir>/diag/<UTC timestamp>/. Nothing here restarts anything,
// which matters because a restart resets a 4.6 GB process to 30 MB and destroys
// the state being examined.

const (
	diagDirName = "diag"
	diagLock    = ".lock"
	// diagStampLayout names a bundle. It sorts the same way as time does, which
	// is what retention relies on.
	diagStampLayout = "20060102T150405Z"
	diagPartialExt  = ".partial"

	diagKeepBundles = 5
	diagCPUFor      = 30 * time.Second
	// diagCPUStopGrace bounds StopCPUProfile. It normally returns at once; if it
	// does not, the command must still end.
	diagCPUStopGrace = 10 * time.Second
	// diagTotalDeadline bounds the whole request, so the control socket is never
	// held past it. CPU window plus stop grace plus room for the forced GC and
	// the other two profiles.
	diagTotalDeadline = 80 * time.Second
	// diagStaleLockAge is how old a lock must be before it is treated as left
	// behind by a crashed capture. A healthy capture is over in under
	// diagTotalDeadline, so five minutes is far past any live one.
	diagStaleLockAge = 5 * time.Minute

	// Profiles are much smaller than the heap, so use a fixed space floor.
	diagMinFreeBytes = 512 << 20
)

var (
	errDiagDeadline = errors.New("diagnostic capture passed its overall deadline")
	errDiagHotSwap  = errors.New("a hot swap started; the diagnostic capture was aborted and its partial files removed")

	diagStampRe = regexp.MustCompile(`^\d{8}T\d{6}Z$`)
)

// diagStats is what the summary reports about the process.
type diagStats struct {
	RSSBytes     uint64
	HeapInuse    uint64
	HeapSys      uint64
	NumGC        uint32
	LastGC       time.Time
	Goroutines   int
	Uptime       time.Duration
	MemLimit     int64 // math.MaxInt64 = no limit
	GOGC         int64 // negative = off
	Clients      int64
	ProxyCount   int
	ProxiesKnown bool
	Version      string
}

// diagResult is the success reply, JSON-encoded into the control response.
type diagResult struct {
	Dir           string   `json:"dir"`
	Files         []string `json:"files"`
	Notes         []string `json:"notes,omitempty"`
	Pruned        int      `json:"pruned,omitempty"`
	RSSBytes      uint64   `json:"rss_bytes"`
	HeapInuse     uint64   `json:"heap_inuse_bytes"` // after the forced GC
	HeapSys       uint64   `json:"heap_sys_bytes"`
	Goroutines    int      `json:"goroutines"`
	UptimeSeconds float64  `json:"uptime_seconds"`
	Clients       int64    `json:"clients"`
}

// diagEnv is everything runDiag touches outside its own directory, so a test
// can supply a fake clock, a small disk, and a failing profile writer.
type diagEnv struct {
	stateDir     string
	now          func() time.Time
	freeBytes    func(dir string) (uint64, error)
	stats        func() diagStats
	runGC        func()
	cpuFor       time.Duration
	cpuStopGrace time.Duration
	staleLockAge time.Duration
	keep         int

	writeHeap       func(w io.Writer) error
	writeGoroutines func(w io.Writer) error
	startCPU        func(w io.Writer) error
	stopCPU         func()
}

func productionDiagEnv(stateDir string) diagEnv {
	return diagEnv{
		stateDir:     stateDir,
		now:          time.Now,
		freeBytes:    diskFreeBytes,
		stats:        collectDiagStats,
		runGC:        runtime.GC,
		cpuFor:       diagCPUFor,
		cpuStopGrace: diagCPUStopGrace,
		staleLockAge: diagStaleLockAge,
		keep:         diagKeepBundles,
		writeHeap: func(w io.Writer) error {
			return pprof.Lookup("heap").WriteTo(w, 0)
		},
		writeGoroutines: func(w io.Writer) error {
			return pprof.Lookup("goroutine").WriteTo(w, 1)
		},
		startCPU: pprof.StartCPUProfile,
		stopCPU:  pprof.StopCPUProfile,
	}
}

func collectDiagStats() diagStats {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	st := diagStats{
		HeapInuse:  ms.HeapInuse,
		HeapSys:    ms.HeapSys,
		NumGC:      ms.NumGC,
		Goroutines: runtime.NumGoroutine(),
		// A negative input reads the limit without changing it.
		MemLimit: debug.SetMemoryLimit(-1),
		Version:  Version,
	}
	if ms.LastGC != 0 {
		st.LastGC = time.Unix(0, int64(ms.LastGC))
	}
	// SetGCPercent has no read-only form (it sets and returns the old value),
	// so read GOGC from runtime/metrics instead of racing a set-and-restore.
	sample := []metrics.Sample{{Name: "/gc/gogc:percent"}}
	metrics.Read(sample)
	if sample[0].Value.Kind() == metrics.KindUint64 {
		st.GOGC = int64(sample[0].Value.Uint64())
	} else {
		st.GOGC = 100
	}
	res := collectResources()
	st.RSSBytes = res.RSSBytes
	if snap := nodeSnapshots.Get(); snap != nil {
		st.Uptime = time.Duration(snap.UptimeSeconds * float64(time.Second))
		st.Clients = snap.Clients
		st.ProxyCount = snap.Proxies.Up + snap.Proxies.Degraded + snap.Proxies.Connecting + snap.Proxies.Dead
		st.ProxiesKnown = true
	}
	if st.Uptime <= 0 {
		st.Uptime = time.Since(providerStartTime)
	}
	return st
}

// ---- one capture at a time, and the hot swap abort ----

var (
	diagActiveMu sync.Mutex
	diagActive   *diagRun
)

type diagRun struct {
	cancel context.CancelCauseFunc
	done   chan struct{}
}

// abortDiagForHotSwap is called at the hot swap commit point. It cancels a
// running capture and waits briefly for it to remove its partial directory, so
// the handoff does not exec away from a half-written bundle.
func abortDiagForHotSwap() {
	diagActiveMu.Lock()
	run := diagActive
	diagActiveMu.Unlock()
	if run == nil {
		return
	}
	run.cancel(errDiagHotSwap)
	select {
	case <-run.done:
	case <-time.After(3 * time.Second):
		// A forced GC or a profile write cannot be interrupted. The orphan sweep
		// at the start of the next capture removes whatever this leaves.
	}
}

// handleDiagRequest runs one capture for the control socket. The context ends
// at diagTotalDeadline, so a request never holds its connection longer.
func handleDiagRequest(stateDir string) (diagResult, error) {
	return runDiagTracked(productionDiagEnv(stateDir))
}

// runDiagTracked is handleDiagRequest with the environment supplied, so a test
// can drive the same registration the hot swap abort relies on.
func runDiagTracked(env diagEnv) (diagResult, error) {
	ctx, cancel := context.WithTimeoutCause(context.Background(), diagTotalDeadline, errDiagDeadline)
	defer cancel()
	cctx, ccancel := context.WithCancelCause(ctx)
	run := &diagRun{cancel: ccancel, done: make(chan struct{})}
	diagActiveMu.Lock()
	ownsSlot := diagActive == nil
	if ownsSlot {
		diagActive = run
	}
	diagActiveMu.Unlock()
	defer func() {
		if ownsSlot {
			diagActiveMu.Lock()
			if diagActive == run {
				diagActive = nil
			}
			diagActiveMu.Unlock()
		}
		ccancel(nil)
		close(run.done)
	}()
	return runDiag(cctx, env)
}

// ---- the capture ----

func runDiag(ctx context.Context, env diagEnv) (res diagResult, err error) {
	root := filepath.Join(env.stateDir, diagDirName)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return res, diagDirError(root, err)
	}

	release, replacedStale, err := acquireDiagLock(root, env)
	if err != nil {
		return res, err
	}
	defer release()
	if replacedStale != "" {
		res.Notes = append(res.Notes, replacedStale)
	}

	// Holding the lock means no other capture is writing, so any .partial here
	// is the leftover of a crashed or interrupted one.
	removeDiagOrphans(root)
	// Pruning is here, before the capture, and never during it. It also runs
	// before the free-space check, because pruning is what frees space.
	res.Pruned = pruneDiagBundles(root, max(env.keep-1, 0))

	before := env.stats()
	need := uint64(diagMinFreeBytes)
	avail, ferr := env.freeBytes(root)
	if ferr != nil {
		return res, fmt.Errorf("diag: cannot measure free space on the filesystem holding %s (%v); refusing to start a capture it cannot check. Check that the state directory exists and is readable by the provider", root, ferr)
	}
	if avail < need {
		return res, fmt.Errorf("diag: not enough free space on the filesystem holding %s: need %s (512 MiB, enough for the bundle), have %s, short by %s. Free some space (or delete old bundles in %s) and run it again",
			root, diagBytes(need), diagBytes(avail), diagBytes(need-avail), root)
	}

	stamp := env.now().UTC().Format(diagStampLayout)
	final := filepath.Join(root, stamp)
	partial := final + diagPartialExt
	if _, serr := os.Lstat(final); serr == nil {
		return res, fmt.Errorf("diag: a bundle named %s already exists; wait a second and run it again", stamp)
	}
	if err := os.Mkdir(partial, 0o700); err != nil {
		return res, diagDirError(root, err)
	}
	committed := false
	defer func() {
		// A half-bundle must never be mistaken for evidence.
		if !committed {
			os.RemoveAll(partial)
		}
	}()

	cause := func() error {
		if ctx.Err() == nil {
			return nil
		}
		return context.Cause(ctx)
	}

	// Goroutines first: it is the cheapest, and the stacks should describe the
	// process as it was when the operator asked.
	if err := writeDiagFile(partial, "goroutine.txt", env.writeGoroutines); err != nil {
		return res, err
	}
	if err := cause(); err != nil {
		return res, err
	}

	// A heap profile reports the state as of the last completed GC, so force one:
	// otherwise the profile can be a full GC cycle stale. "In use" below is
	// therefore post-GC, which is also the number worth reading.
	env.runGC()
	after := env.stats()
	if err := writeDiagFile(partial, "heap.pb.gz", env.writeHeap); err != nil {
		return res, err
	}
	if err := cause(); err != nil {
		return res, err
	}

	cpuNote, cpuCaptured, err := captureDiagCPU(ctx, env, filepath.Join(partial, "cpu.pb.gz"))
	if err != nil {
		return res, err
	}
	if cerr := cause(); cerr != nil && !errors.Is(cerr, errDiagDeadline) {
		return res, cerr
	}
	files := []string{"summary.txt", "heap.pb.gz", "goroutine.txt"}
	if cpuCaptured {
		files = append(files, "cpu.pb.gz")
	}
	if cpuNote != "" {
		res.Notes = append(res.Notes, cpuNote)
	}

	summary := renderDiagSummary(stamp, before, after, cpuCaptured, res.Notes, env)
	if err := writeDiagFile(partial, "summary.txt", func(w io.Writer) error {
		_, werr := io.WriteString(w, summary)
		return werr
	}); err != nil {
		return res, err
	}

	if err := os.Rename(partial, final); err != nil {
		return res, fmt.Errorf("diag: could not finalize the bundle in %s: %v", root, err)
	}
	committed = true

	res.Dir = final
	res.Files = files
	res.RSSBytes = after.RSSBytes
	res.HeapInuse = after.HeapInuse
	res.HeapSys = after.HeapSys
	res.Goroutines = before.Goroutines
	res.UptimeSeconds = before.Uptime.Seconds()
	res.Clients = before.Clients
	return res, nil
}

// writeDiagFile creates name inside dir, fills it, syncs and closes it, and
// refuses an empty result: an empty profile is a failed capture, not evidence.
//
// SECURITY: a heap profile and the goroutine stacks can contain session tokens
// and proxy credentials that were in memory. The files are therefore created
// 0600 inside a 0700 directory, never wider than the state directory itself, and
// summary.txt tells the operator to treat the bundle as a secret.
func writeDiagFile(dir, name string, fill func(io.Writer) error) error {
	path := filepath.Join(dir, name)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return diagDirError(dir, err)
	}
	if err := fill(f); err != nil {
		f.Close()
		return fmt.Errorf("diag: writing %s failed: %v", name, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("diag: syncing %s failed: %v", name, err)
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return fmt.Errorf("diag: %s: %v", name, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("diag: closing %s failed: %v", name, err)
	}
	if st.Size() == 0 {
		return fmt.Errorf("diag: %s came out empty", name)
	}
	return nil
}

// captureDiagCPU records a fixed CPU window. The provider keeps serving: the
// profiler only samples. It returns a note, empty on a clean capture, when the
// profile was skipped or cut short, and whether a cpu.pb.gz exists; a non-nil
// error means abort the bundle.
func captureDiagCPU(ctx context.Context, env diagEnv, path string) (note string, captured bool, err error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // 0600: see writeDiagFile
	if err != nil {
		return "", false, diagDirError(filepath.Dir(path), err)
	}
	if err := env.startCPU(f); err != nil {
		f.Close()
		os.Remove(path)
		// Only one CPU profile can run in a process. Another one in progress is
		// not a reason to throw away the heap and goroutine evidence.
		return fmt.Sprintf("cpu.pb.gz was not captured: %v", err), false, nil
	}

	timer := time.NewTimer(env.cpuFor)
	select {
	case <-timer.C:
	case <-ctx.Done():
		timer.Stop()
		if !errors.Is(context.Cause(ctx), errDiagDeadline) {
			// Hot swap or another abort: stop the profiler and discard everything.
			go func() { env.stopCPU(); f.Close() }()
			return "", false, context.Cause(ctx)
		}
		note = fmt.Sprintf("the overall %s deadline ran out during the CPU window, so cpu.pb.gz covers less than the planned %s and is incomplete", diagTotalDeadline, env.cpuFor)
	}

	// The goroutine owns the file from here: if StopCPUProfile never returns,
	// the command still does, and the file is closed whenever it eventually does.
	stopped := make(chan struct{})
	go func() {
		env.stopCPU()
		f.Sync()
		f.Close()
		close(stopped)
	}()
	grace := time.NewTimer(env.cpuStopGrace)
	defer grace.Stop()
	select {
	case <-stopped:
	case <-grace.C:
		// The goroutine owns the open descriptor until StopCPUProfile returns.
		// Do not commit the unfinished profile as diagnostic evidence.
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", false, fmt.Errorf("diag: could not discard unfinished CPU profile: %w", err)
		}
		return fmt.Sprintf("StopCPUProfile did not return within %s; cpu.pb.gz was dropped because the profile could not be finalized", env.cpuStopGrace), false, nil
	}
	return note, true, nil
}

// ---- lock ----

type diagLockInfo struct {
	started time.Time
	known   bool
}

func readDiagLock(path string) diagLockInfo {
	b, err := os.ReadFile(path)
	if err != nil {
		return diagLockInfo{}
	}
	var pid int
	var stamp string
	if _, err := fmt.Sscanf(string(b), "pid=%d started=%s", &pid, &stamp); err == nil {
		if t, err := time.Parse(time.RFC3339, stamp); err == nil {
			return diagLockInfo{started: t, known: true}
		}
	}
	// Unreadable content: the file's own time is the best evidence of age.
	if st, err := os.Stat(path); err == nil {
		return diagLockInfo{started: st.ModTime(), known: true}
	}
	return diagLockInfo{}
}

func diagBusyError(root string, env diagEnv) error {
	info := readDiagLock(filepath.Join(root, diagLock))
	when := "at an unknown time"
	if info.known {
		when = info.started.UTC().Format(time.RFC3339)
	}
	return fmt.Errorf("diag: a capture is already running (started %s). Wait for it to finish; a lock older than %s is treated as left behind by a crash and replaced automatically", when, env.staleLockAge)
}

// acquireDiagLock takes <root>/.lock with O_EXCL. A lock older than
// env.staleLockAge is a crashed capture's leftover and is replaced; the second
// return value then says so, for summary.txt.
func acquireDiagLock(root string, env diagEnv) (release func(), staleNote string, err error) {
	path := filepath.Join(root, diagLock)
	token := fmt.Sprintf("pid=%d started=%s\n", os.Getpid(), env.now().UTC().Format(time.RFC3339))
	for attempt := 0; attempt < 2; attempt++ {
		f, cerr := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if cerr == nil {
			_, werr := io.WriteString(f, token)
			if cerr = errors.Join(werr, f.Close()); cerr != nil {
				os.Remove(path)
				return nil, "", diagDirError(root, cerr)
			}
			return func() {
				// Remove only our own lock, not one a later capture replaced.
				if b, rerr := os.ReadFile(path); rerr == nil && string(b) == token {
					os.Remove(path)
				}
			}, staleNote, nil
		}
		if !errors.Is(cerr, os.ErrExist) {
			return nil, "", diagDirError(root, cerr)
		}
		info := readDiagLock(path)
		if !info.known || env.now().Sub(info.started) <= env.staleLockAge {
			return nil, "", diagBusyError(root, env)
		}
		staleNote = fmt.Sprintf("a lock from %s was older than %s, so it was treated as left behind by a crashed capture and replaced",
			info.started.UTC().Format(time.RFC3339), env.staleLockAge)
		if rerr := os.Remove(path); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
			return nil, "", diagDirError(root, rerr)
		}
	}
	return nil, "", diagBusyError(root, env)
}

// ---- retention ----

func listDiagBundles(root string) []string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		// Only directories shaped like a bundle stamp are ours to prune.
		if e.IsDir() && diagStampRe.MatchString(e.Name()) {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names
}

// pruneDiagBundles removes all but the newest keep complete bundles and returns
// how many it removed.
func pruneDiagBundles(root string, keep int) int {
	names := listDiagBundles(root)
	removed := 0
	for i := 0; i < len(names)-keep; i++ {
		if os.RemoveAll(filepath.Join(root, names[i])) == nil {
			removed++
		}
	}
	return removed
}

func removeDiagOrphans(root string) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() && strings.HasSuffix(name, diagPartialExt) && diagStampRe.MatchString(strings.TrimSuffix(name, diagPartialExt)) {
			os.RemoveAll(filepath.Join(root, name))
		}
	}
}

// ---- messages and summary ----

func diagDirError(dir string, err error) error {
	if errors.Is(err, os.ErrPermission) {
		return fmt.Errorf("diag: the provider cannot write to %s (%v). Fix the ownership of the state directory so the user the provider runs as owns it, then run it again", dir, err)
	}
	return fmt.Errorf("diag: could not write under %s: %v. If the disk is full, free some space and run it again", dir, err)
}

func diagBytes(b uint64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.2f GiB", float64(b)/float64(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(b)/float64(1<<20))
	default:
		return fmt.Sprintf("%d KiB", b>>10)
	}
}

func diagLimitText(limit int64) string {
	if limit == math.MaxInt64 || limit <= 0 {
		return "no limit set"
	}
	return diagBytes(uint64(limit))
}

func diagGOGCText(g int64) string {
	if g < 0 {
		return "off (the collector runs only to honor the memory limit)"
	}
	return fmt.Sprintf("%d", g)
}

func renderDiagSummary(stamp string, before, after diagStats, cpuCaptured bool, notes []string, env diagEnv) string {
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format, args...) }
	w("PROVIDER DIAGNOSTIC BUNDLE %s (UTC)\n", stamp)
	if before.Version != "" {
		w("Provider version: %s\n", before.Version)
	}
	w("\nHEADLINE NUMBERS (read when the capture started, unless marked)\n")
	if before.RSSBytes > 0 {
		w("  Process memory (RSS):     %s   what the operating system charges the process; this is the figure `top` shows\n", diagBytes(before.RSSBytes))
	} else {
		w("  Process memory (RSS):     not available on this platform\n")
	}
	w("  Go heap in use:           %s before the forced garbage collection, %s after it (the profile is the after figure)\n", diagBytes(before.HeapInuse), diagBytes(after.HeapInuse))
	w("  Go heap reserved (sys):   %s   memory the Go runtime holds from the OS, in use or not\n", diagBytes(before.HeapSys))
	if before.LastGC.IsZero() {
		w("  Garbage collections:      %d so far, none finished yet\n", before.NumGC)
	} else {
		w("  Garbage collections:      %d so far, the last at %s (%s before the capture started)\n",
			before.NumGC, before.LastGC.UTC().Format(time.RFC3339), env.now().Sub(before.LastGC).Round(time.Second))
	}
	w("  Goroutines:               %d\n", before.Goroutines)
	w("  Process uptime:           %s\n", before.Uptime.Round(time.Second))
	w("  Memory limit (GOMEMLIMIT): %s\n", diagLimitText(before.MemLimit))
	w("  GC target (GOGC):         %s\n", diagGOGCText(before.GOGC))
	w("  Active connections:       %d clients\n", before.Clients)
	if before.ProxiesKnown {
		w("  Proxy slots:              %d\n", before.ProxyCount)
	}

	w("\nHOW TO READ IT\n")
	w("  If heap in use AFTER the collection is close to the RSS, the memory is live data the program is holding;\n")
	w("  open heap.pb.gz to see which code allocated it. If it is far below the RSS, the rest is garbage not yet\n")
	w("  returned to the OS, or memory that is not the Go heap (stacks, buffers, the runtime itself).\n")
	w("  A goroutine count in the tens of thousands that grows between bundles points at goroutines that never exit:\n")
	w("  open goroutine.txt and look at the largest groups.\n")

	w("\nFILES\n")
	w("  summary.txt     this file\n")
	w("  heap.pb.gz      Go heap profile, who allocated the memory still in use. Open with: go tool pprof -sample_index=inuse_space heap.pb.gz\n")
	w("  goroutine.txt   every goroutine's stack, grouped with a count per distinct stack\n")
	if cpuCaptured {
		w("  cpu.pb.gz       %s CPU profile taken while the provider kept serving. Open with: go tool pprof cpu.pb.gz\n", env.cpuFor)
	} else {
		w("  cpu.pb.gz       NOT CAPTURED, see NOTES\n")
	}

	w("\nNOTES\n")
	w("  Taking the heap profile forced one garbage collection, so the \"in use\" figure for the profile is post-GC.\n")
	w("  These files can contain session tokens and proxy credentials that were in memory. Treat the bundle as a\n")
	w("  secret: do not attach it to a public issue or paste it anywhere.\n")
	for _, n := range notes {
		w("  %s\n", n)
	}
	return b.String()
}
