//go:build linux

package connect

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// The startup disk benchmark exists for one reason: on a slow disk (SD card,
// saturated spinning disk) the provider's logs are better kept in RAM, and the
// auto profile uses this result to decide that. Everything here is built so the
// measurement can never cost more than the answer is worth:
//
//   - it is bounded in bytes and in wall-clock time, and startup never waits
//     longer than the budget plus a short grace, even if a write hangs in the
//     kernel (a hung write is itself the answer: slow);
//   - it only writes inside the provider's state dir, never the working dir;
//   - it refuses to touch a nearly full disk;
//   - the test file is per-process and stale ones (left by a killed process)
//     are swept on the next run;
//   - the answer is cached per device, so a restart loop cannot repeat it;
//   - it does not run inside a container unless forced (URNETWORK_DISK_BENCH=1),
//     because there the disk is shared with unknown neighbours and a fleet of
//     containers started together (compose scale-out) would contend and all
//     measure "slow", which under the auto profile switches every one of them to
//     RAM logs;
//   - it does not run when the host's disk is already busy (io pressure), which
//     is how containers sense each other without any coordination, and a busy
//     disk is never reported as a slow one.
const (
	// The sample is bounded by TIME, not by this figure: the loop stops when the
	// budget is spent. The ceiling only stops a very fast device (an NVMe writes
	// 256 MiB in under 100 ms) from grinding for the whole budget, and it is what a
	// slow device never reaches because its rate trips the early exit first.
	diskBenchMaxBytes     = 256 << 20
	diskBenchChunk        = 1 << 20         // one synchronous write
	diskBenchBudget       = 3 * time.Second // wall-clock cap on the write loop
	diskBenchGrace        = 500 * time.Millisecond
	diskBenchEarlyAfter   = 1 * time.Second // after this long, a clearly slow rate ends the test
	diskBenchMinFreeMiB   = 256             // below this the disk is not benchmarked at all
	diskBenchFreeDiv      = 64              // never write more than free/64
	diskBenchSlowMBps     = 50.0            // below this is "slow" (same threshold as before)
	diskBenchFastTTL      = 24 * time.Hour  // how long a fast result is reused
	diskBenchSlowTTL      = 1 * time.Hour   // a slow or hung result is rechecked sooner
	diskBenchStaleAfter   = 2 * time.Minute // debris older than this is removed
	diskBenchPrefix       = ".io-test"
	diskBenchCacheFile    = "disk-bench.json"
	diskBenchMinTailBytes = 2 << 20 // the tail check needs at least this much data
	diskBenchBusyPSI      = 10.0    // skip when the host's recent io pressure ("some avg10", %) is at least this
)

type diskBenchFile interface {
	Write(p []byte) (int, error)
	Close() error
}

// diskBenchOpen opens the test file; replaced in tests.
var diskBenchOpen = func(path string) (diskBenchFile, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|os.O_SYNC, 0600)
}

type diskBenchResult struct {
	MBps     float64 `json:"mbps"`
	Bytes    int64   `json:"bytes"`
	Millis   int64   `json:"millis,omitempty"` // sample length: a rate means nothing without it
	Slow     bool    `json:"slow"`
	TimedOut bool    `json:"timed_out,omitempty"`
	Err      string  `json:"err,omitempty"`
}

// diskBenchCacheSample identifies the sampling configuration a cached result
// was measured with. The published rate is a function of how much was written:
// a 16 MiB burst and a 256 MiB budgeted sample are not the same measurement, so
// a cache written by one sampler must never satisfy a request for the other.
// Bump this whenever the sampling shape changes.
const diskBenchCacheSample = "budget-3s-256mib-v1"

type diskBenchCache struct {
	At     time.Time       `json:"at"`
	Device uint64          `json:"device"`
	Sample string          `json:"sample"`
	Result diskBenchResult `json:"result"`
}

// CheckDiskSpace returns the free space under dir in MiB and whether it is low
// (under 1 GiB). A directory that does not exist yet is measured through its
// nearest existing parent, however many missing levels that takes. A path no
// level can measure is reported as (0, false): unknown is not low.
func CheckDiskSpace(dir string) (int64, bool) {
	freeMiB, low, _ := checkDiskSpaceMeasured(dir)
	return freeMiB, low
}

// checkDiskSpaceMeasured is CheckDiskSpace plus whether any level could be
// measured, so a caller can tell an unmeasurable path from a full disk.
func checkDiskSpaceMeasured(dir string) (freeMiB int64, low bool, measured bool) {
	probe := dir
	for {
		var stat syscall.Statfs_t
		if err := diskSpaceStatfs(probe, &stat); err == nil {
			freeMiB = int64(stat.Bavail) * int64(stat.Bsize) / 1024 / 1024
			if freeMiB < 1024 {
				fmt.Printf("🚨 [audit] CRITICAL: Low disk space (%d MiB available). Provider may fail.\n", freeMiB)
				return freeMiB, true, true
			}
			return freeMiB, false, true
		}
		next := filepath.Dir(probe)
		if next == probe {
			// walked to the root without a measurable level: we could not
			// measure, which is not the same as low. Say nothing, so a wrong
			// path cannot raise a false alert.
			return 0, false, false
		}
		probe = next
	}
}

// RunDiskAudit measures synchronous write speed under dir, safely. It returns
// (slowDisk, lowSpace). It never blocks startup for longer than the budget.
func RunDiskAudit(dir string) (slowDisk bool, lowSpace bool) {
	sweepDiskBenchDebris(dir)

	if why := diskBenchSkipReason(); why != "" {
		_, lowSpace = CheckDiskSpace(dir)
		fmt.Printf("[audit] Disk benchmark skipped: %s\n", why)
		return false, lowSpace
	}

	freeMiB, lowSpace, measured := checkDiskSpaceMeasured(dir)
	if !measured {
		// unknown is not zero: do not report a full disk for a path we could not measure
		fmt.Printf("[audit] Disk benchmark skipped: could not measure free space under %s\n", dir)
		return false, false
	}
	if freeMiB < diskBenchMinFreeMiB {
		fmt.Printf("[audit] Disk benchmark skipped: only %d MiB free under %s\n", freeMiB, dir)
		return false, lowSpace
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return false, lowSpace
	}

	dev, devOK := diskBenchDevice(dir)
	if r, ok := readDiskBenchCache(dir, dev, devOK, time.Now()); ok {
		fmt.Printf("[audit] Disk write speed: %.1f MB/s (cached result, no test run)\n", r.MBps)
		return r.Slow, lowSpace
	}

	maxBytes := diskBenchCap(freeMiB)
	r := runDiskBench(dir, maxBytes, diskBenchBudget)
	switch {
	case r.TimedOut:
		fmt.Printf("[audit] Disk write did not finish within %s: treating the disk as slow.\n", diskBenchBudget+diskBenchGrace)
	case r.Err != "":
		fmt.Printf("[audit] Disk I/O test failed: %s\n", r.Err)
	default:
		fmt.Printf("[audit] Disk write speed: %.1f MB/s (%d MB written in %d ms)\n", r.MBps, r.Bytes>>20, r.Millis)
	}
	if r.Slow {
		fmt.Printf("[audit] Slow disk I/O detected.\n")
		fmt.Printf("[audit] Hint: Suggest enabling RAM logging to eliminate I/O bottlenecks: 'urnet-tools ramlogs on'.\n")
	}
	writeDiskBenchCache(dir, dev, devOK, r, time.Now())
	return r.Slow, lowSpace
}

// runDiskBench writes at most maxBytes in 1 MiB synchronous writes, stopping at
// the budget, and returns the rate. If the write loop does not return within
// budget+grace (a write stuck in the kernel), it gives up and reports a timed
// out, slow result; the abandoned goroutine removes its file when it unblocks.
func runDiskBench(dir string, maxBytes int64, budget time.Duration) diskBenchResult {
	path := filepath.Join(dir, fmt.Sprintf("%s-%d", diskBenchPrefix, os.Getpid()))
	done := make(chan diskBenchResult, 1)
	var once sync.Once
	cleanup := func() { once.Do(func() { os.Remove(path) }) }
	go func() {
		defer cleanup()
		done <- benchWrite(path, maxBytes, budget)
	}()
	timer := time.NewTimer(budget + diskBenchGrace)
	defer timer.Stop()
	select {
	case r := <-done:
		return r
	case <-timer.C:
		// unlink now so the space comes back as soon as the stuck write releases it
		os.Remove(path)
		return diskBenchResult{Slow: true, TimedOut: true, Millis: (budget + diskBenchGrace).Milliseconds()}
	}
}

func benchWrite(path string, maxBytes int64, budget time.Duration) diskBenchResult {
	f, err := diskBenchOpen(path)
	if err != nil {
		return diskBenchResult{Err: err.Error()} // cannot test: not evidence of a slow disk
	}
	defer f.Close()
	chunk := make([]byte, diskBenchChunk)
	for i := range chunk {
		chunk[i] = byte(i % 255)
	}
	var written int64
	start := time.Now()
	// A device write cache can flatter a short run: remember where the last
	// quarter began so a cache cliff shows up even if the average looks fine.
	tailAt := maxBytes - maxBytes/4
	var tailStartBytes int64 = -1
	var tailStartTime time.Time
	for written < maxBytes {
		if tailStartBytes < 0 && written >= tailAt {
			tailStartBytes, tailStartTime = written, time.Now()
		}
		n, werr := f.Write(chunk)
		written += int64(n)
		if werr != nil {
			return diskBenchResult{Bytes: written, Slow: true, Err: werr.Error(), Millis: time.Since(start).Milliseconds()}
		}
		elapsed := time.Since(start)
		if elapsed >= budget {
			break
		}
		if elapsed >= diskBenchEarlyAfter && rateMBps(written, elapsed) < diskBenchSlowMBps {
			break // clearly slow: no need to keep writing
		}
	}
	elapsed := time.Since(start)
	mbps := rateMBps(written, elapsed)
	slow := written == 0 || mbps < diskBenchSlowMBps
	if !slow && tailStartBytes >= 0 && written-tailStartBytes >= diskBenchMinTailBytes {
		if tail := rateMBps(written-tailStartBytes, time.Since(tailStartTime)); tail < diskBenchSlowMBps {
			slow, mbps = true, tail // sustained rate fell off after the cache filled
		}
	}
	return diskBenchResult{MBps: mbps, Bytes: written, Slow: slow, Millis: elapsed.Milliseconds()}
}

func rateMBps(bytes int64, elapsed time.Duration) float64 {
	if elapsed <= 0 {
		return 0
	}
	return float64(bytes) / (1 << 20) / elapsed.Seconds()
}

// sweepDiskBenchDebris removes test files a killed process left behind: any
// "<prefix>*" regular file in dir, and the legacy "<prefix>" in the working
// dir, that is older than the stale threshold.
func sweepDiskBenchDebris(dir string) {
	now := time.Now()
	sweep := func(d string, exact bool) {
		entries, err := os.ReadDir(d)
		if err != nil {
			return
		}
		for _, e := range entries {
			name := e.Name()
			if !strings.HasPrefix(name, diskBenchPrefix) || (exact && name != diskBenchPrefix) {
				continue
			}
			info, err := e.Info()
			if err != nil || !info.Mode().IsRegular() || now.Sub(info.ModTime()) < diskBenchStaleAfter {
				continue
			}
			if os.Remove(filepath.Join(d, name)) == nil {
				fmt.Printf("[audit] Removed stale disk test file %s (%d MiB)\n", filepath.Join(d, name), info.Size()>>20)
			}
		}
	}
	sweep(dir, false)
	if wd, err := os.Getwd(); err == nil && wd != dir {
		sweep(wd, true)
	}
}

func diskBenchDevice(dir string) (uint64, bool) {
	var st syscall.Stat_t
	if err := syscall.Stat(dir, &st); err != nil {
		return 0, false
	}
	return uint64(st.Dev), true
}

func readDiskBenchCache(dir string, dev uint64, devOK bool, now time.Time) (diskBenchResult, bool) {
	b, err := os.ReadFile(filepath.Join(dir, diskBenchCacheFile))
	if err != nil {
		return diskBenchResult{}, false
	}
	var c diskBenchCache
	if json.Unmarshal(b, &c) != nil || (devOK && c.Device != dev) {
		return diskBenchResult{}, false
	}
	// A cache from an older sampler says nothing about the current one: without
	// this, an upgraded host keeps reporting the old burst rate until the entry
	// ages out, and the new sample is never run.
	if c.Sample != diskBenchCacheSample {
		return diskBenchResult{}, false
	}
	ttl := diskBenchFastTTL
	if c.Result.Slow {
		ttl = diskBenchSlowTTL
	}
	if age := now.Sub(c.At); age < 0 || age > ttl {
		return diskBenchResult{}, false
	}
	return c.Result, true
}

func writeDiskBenchCache(dir string, dev uint64, devOK bool, r diskBenchResult, now time.Time) {
	if r.Err != "" && !r.Slow {
		return // could not run the test at all: do not cache a non-answer
	}
	if !devOK {
		dev = 0
	}
	b, err := json.Marshal(diskBenchCache{At: now, Device: dev, Sample: diskBenchCacheSample, Result: r})
	if err != nil {
		return
	}
	tmp := filepath.Join(dir, fmt.Sprintf(".%s.%d.tmp", diskBenchCacheFile, os.Getpid()))
	if os.WriteFile(tmp, b, 0600) != nil {
		return
	}
	if os.Rename(tmp, filepath.Join(dir, diskBenchCacheFile)) != nil {
		os.Remove(tmp)
	}
}

// DiskBenchStatus is the last benchmark result recorded under dir, for
// /metrics. ok is false when no result has been recorded; age is how old it is.
func DiskBenchStatus(dir string) (mbps float64, slow bool, age time.Duration, ok bool) {
	b, err := os.ReadFile(filepath.Join(dir, diskBenchCacheFile))
	if err != nil {
		return 0, false, 0, false
	}
	var c diskBenchCache
	if json.Unmarshal(b, &c) != nil || c.At.IsZero() {
		return 0, false, 0, false
	}
	return c.Result.MBps, c.Result.Slow, max(0, time.Since(c.At)), true
}

// Hooks for tests.
var (
	diskBenchInContainer = runningInContainer
	diskBenchIOPressure  = readIOPressure
	// diskSpaceStatfs is CheckDiskSpace's statfs; tests replace it to
	// reproduce a path no level can measure.
	diskSpaceStatfs = syscall.Statfs
)

// diskBenchSkipReason returns why the benchmark must not run now, or "".
// URNETWORK_DISK_BENCH: "0" never runs it, "1" runs it even in a container
// (the busy-disk check still applies), unset runs it on bare metal/VMs only.
func diskBenchSkipReason() string {
	mode := os.Getenv("URNETWORK_DISK_BENCH")
	if mode == "0" {
		return "disabled (URNETWORK_DISK_BENCH=0)"
	}
	if mode != "1" && diskBenchInContainer() {
		return "running in a container, where the disk is shared with unknown neighbours (set URNETWORK_DISK_BENCH=1 to force)"
	}
	if some, ok := diskBenchIOPressure(); ok && some >= diskBenchBusyPSI {
		return fmt.Sprintf("host disk is busy (io pressure %.1f%%), a measurement now would not describe this disk", some)
	}
	return ""
}

// runningInContainer reports whether this process looks containerised
// (Docker, Podman, Kubernetes, LXC).
func runningInContainer() bool {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	if _, err := os.Stat("/run/.containerenv"); err == nil {
		return true
	}
	if os.Getenv("container") != "" || os.Getenv("KUBERNETES_SERVICE_HOST") != "" {
		return true
	}
	b, err := os.ReadFile("/proc/1/cgroup")
	if err != nil {
		return false
	}
	c := string(b)
	return strings.Contains(c, "docker") || strings.Contains(c, "kubepods") || strings.Contains(c, "containerd") || strings.Contains(c, "crio") || strings.Contains(c, "/lxc/")
}

// readIOPressure returns the "some avg10" value of /proc/pressure/io, the share
// of the last 10 s in which some task was stalled on disk. It is host-wide, so
// it reflects work other containers are doing. ok is false when the kernel has
// no pressure info.
func readIOPressure() (float64, bool) {
	b, err := os.ReadFile("/proc/pressure/io")
	if err != nil {
		return 0, false
	}
	return parseIOPressure(string(b))
}

func parseIOPressure(s string) (float64, bool) {
	for _, line := range strings.Split(s, "\n") {
		if !strings.HasPrefix(line, "some ") {
			continue
		}
		for _, f := range strings.Fields(line) {
			if v, ok := strings.CutPrefix(f, "avg10="); ok {
				x, err := strconv.ParseFloat(v, 64)
				return x, err == nil
			}
		}
	}
	return 0, false
}

// DiskBenchSkipReason reports why the disk benchmark would not run right now
// ("" when it would). Exported for the provider's tests and diagnostics.
func DiskBenchSkipReason() string { return diskBenchSkipReason() }

// diskBenchCap is the most the benchmark may write on a disk with freeMiB free:
// the byte cap or free/64, whichever is smaller, rounded down to whole chunks
// because every write is a full chunk. Callers have already ruled out disks
// under diskBenchMinFreeMiB, so the result is at least four chunks.
func diskBenchCap(freeMiB int64) int64 {
	n := min(int64(diskBenchMaxBytes), freeMiB*1024*1024/diskBenchFreeDiv)
	return n - n%diskBenchChunk
}
