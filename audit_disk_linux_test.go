//go:build linux

package connect

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// fakeBenchFile lets a test script how a write behaves.
type fakeBenchFile struct {
	write  func(p []byte) (int, error)
	closed atomic.Bool
}

func (f *fakeBenchFile) Write(p []byte) (int, error) { return f.write(p) }
func (f *fakeBenchFile) Close() error                { f.closed.Store(true); return nil }

func withBenchOpen(t *testing.T, open func(path string) (diskBenchFile, error)) {
	t.Helper()
	orig := diskBenchOpen
	diskBenchOpen = open
	t.Cleanup(func() { diskBenchOpen = orig })
}

func TestRunDiskBenchFastDiskIsNotSlowAndLeavesNoFile(t *testing.T) {
	dir := t.TempDir()
	withBenchOpen(t, func(path string) (diskBenchFile, error) {
		return &fakeBenchFile{write: func(p []byte) (int, error) { return len(p), nil }}, nil
	})
	r := runDiskBench(dir, 8<<20, time.Second)
	if r.Slow || r.TimedOut || r.Err != "" {
		t.Fatalf("fast disk judged slow: %+v", r)
	}
	if r.Bytes != 8<<20 {
		t.Fatalf("wrote %d bytes, want exactly the 8 MiB cap", r.Bytes)
	}
}

func TestRunDiskBenchNeverWritesMoreThanTheCap(t *testing.T) {
	dir := t.TempDir()
	var total atomic.Int64
	withBenchOpen(t, func(path string) (diskBenchFile, error) {
		return &fakeBenchFile{write: func(p []byte) (int, error) { total.Add(int64(len(p))); return len(p), nil }}, nil
	})
	runDiskBench(dir, 3<<20, time.Second)
	if got := total.Load(); got != 3<<20 {
		t.Fatalf("wrote %d bytes, cap is %d", got, 3<<20)
	}
}

func TestRunDiskBenchStopsAtTheBudgetOnASlowDisk(t *testing.T) {
	dir := t.TempDir()
	withBenchOpen(t, func(path string) (diskBenchFile, error) {
		return &fakeBenchFile{write: func(p []byte) (int, error) { time.Sleep(200 * time.Millisecond); return len(p), nil }}, nil
	})
	start := time.Now()
	r := runDiskBench(dir, 16<<20, 600*time.Millisecond)
	if took := time.Since(start); took > 600*time.Millisecond+diskBenchGrace+300*time.Millisecond {
		t.Fatalf("took %s, budget was 600ms", took)
	}
	if !r.Slow || r.TimedOut {
		t.Fatalf("5 MB/s disk should be slow but not a timeout: %+v", r)
	}
	if r.Bytes >= 16<<20 {
		t.Fatalf("slow disk was written to the cap (%d bytes) instead of stopping early", r.Bytes)
	}
}

func TestRunDiskBenchHungWriteDoesNotBlockAndIsSlow(t *testing.T) {
	dir := t.TempDir()
	release := make(chan struct{})
	opened := make(chan struct{}, 1)
	withBenchOpen(t, func(path string) (diskBenchFile, error) {
		os.WriteFile(path, []byte("x"), 0600) // the file exists while the write hangs
		opened <- struct{}{}
		return &fakeBenchFile{write: func(p []byte) (int, error) { <-release; return len(p), nil }}, nil
	})
	start := time.Now()
	r := runDiskBench(dir, 16<<20, 300*time.Millisecond)
	took := time.Since(start)
	close(release) // let the abandoned goroutine finish
	<-opened
	if took > 300*time.Millisecond+diskBenchGrace+300*time.Millisecond {
		t.Fatalf("a hung write blocked the caller for %s", took)
	}
	if !r.Slow || !r.TimedOut {
		t.Fatalf("hung write must report slow+timed out: %+v", r)
	}
	// the stuck goroutine removes the file once it returns
	deadline := time.Now().Add(2 * time.Second)
	for {
		left, _ := filepath.Glob(filepath.Join(dir, diskBenchPrefix+"*"))
		if len(left) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("test file left behind: %v", left)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRunDiskBenchWriteErrorIsSlowAndCleansUp(t *testing.T) {
	dir := t.TempDir()
	withBenchOpen(t, func(path string) (diskBenchFile, error) {
		os.WriteFile(path, []byte("x"), 0600)
		return &fakeBenchFile{write: func(p []byte) (int, error) { return 0, errors.New("no space left on device") }}, nil
	})
	r := runDiskBench(dir, 4<<20, time.Second)
	if !r.Slow || r.Err == "" {
		t.Fatalf("write error must report slow with the error: %+v", r)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, diskBenchPrefix+"*")); len(left) != 0 {
		t.Fatalf("test file left behind after an error: %v", left)
	}
}

func TestRunDiskBenchOpenFailureIsNotEvidenceOfASlowDisk(t *testing.T) {
	withBenchOpen(t, func(path string) (diskBenchFile, error) { return nil, errors.New("read-only file system") })
	r := runDiskBench(t.TempDir(), 4<<20, time.Second)
	if r.Slow {
		t.Fatalf("a disk we could not even open was labelled slow: %+v", r)
	}
}

func TestRealRunDiskBenchOnTempDirIsBoundedAndClean(t *testing.T) {
	dir := t.TempDir()
	start := time.Now()
	r := runDiskBench(dir, 2<<20, 2*time.Second)
	if time.Since(start) > 4*time.Second {
		t.Fatalf("took too long: %s", time.Since(start))
	}
	if r.Bytes > 2<<20 {
		t.Fatalf("wrote past the cap: %d", r.Bytes)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, diskBenchPrefix+"*")); len(left) != 0 {
		t.Fatalf("test file left behind: %v", left)
	}
}

func TestSweepRemovesOnlyStaleBenchFiles(t *testing.T) {
	dir := t.TempDir()
	old := time.Now().Add(-10 * time.Minute)
	mk := func(name string, mtime time.Time) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, make([]byte, 1024), 0600); err != nil {
			t.Fatal(err)
		}
		os.Chtimes(p, mtime, mtime)
		return p
	}
	staleA := mk(".io-test", old)
	staleB := mk(".io-test-4242", old)
	fresh := mk(".io-test-9999", time.Now())
	other := mk("jwt", old)
	cache := mk(diskBenchCacheFile, old)
	sweepDiskBenchDebris(dir)
	for _, p := range []string{staleA, staleB} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("stale debris %s was not removed", p)
		}
	}
	for _, p := range []string{fresh, other, cache} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s must not be removed: %v", filepath.Base(p), err)
		}
	}
}

func TestSweepAlsoClearsLegacyFileInTheWorkingDir(t *testing.T) {
	cwd := t.TempDir()
	state := t.TempDir()
	t.Chdir(cwd)
	legacy := filepath.Join(cwd, ".io-test")
	os.WriteFile(legacy, make([]byte, 2048), 0600)
	old := time.Now().Add(-time.Hour)
	os.Chtimes(legacy, old, old)
	notBench := filepath.Join(cwd, ".io-test-keep") // only the exact legacy name is swept in the cwd
	os.WriteFile(notBench, []byte("x"), 0600)
	os.Chtimes(notBench, old, old)
	sweepDiskBenchDebris(state)
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatal("legacy .io-test in the working dir was not removed")
	}
	if _, err := os.Stat(notBench); err != nil {
		t.Fatal("a different file in the working dir was removed")
	}
}

func TestSweepNeverRemovesDirectories(t *testing.T) {
	dir := t.TempDir()
	d := filepath.Join(dir, ".io-test-dir")
	os.Mkdir(d, 0700)
	old := time.Now().Add(-time.Hour)
	os.Chtimes(d, old, old)
	sweepDiskBenchDebris(dir)
	if _, err := os.Stat(d); err != nil {
		t.Fatal("a directory named like the test file was removed")
	}
}

func TestBenchCacheRoundTripAndExpiry(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	fast := diskBenchResult{MBps: 120, Bytes: 16 << 20}
	writeDiskBenchCache(dir, 7, true, fast, now)
	if r, ok := readDiskBenchCache(dir, 7, true, now.Add(time.Hour)); !ok || r.MBps != 120 {
		t.Fatalf("fresh fast result not reused: %+v ok=%v", r, ok)
	}
	if _, ok := readDiskBenchCache(dir, 7, true, now.Add(25*time.Hour)); ok {
		t.Fatal("fast result reused past its TTL")
	}
	if _, ok := readDiskBenchCache(dir, 8, true, now.Add(time.Minute)); ok {
		t.Fatal("result reused for a different device")
	}
	slow := diskBenchResult{MBps: 4, Bytes: 4 << 20, Slow: true}
	writeDiskBenchCache(dir, 7, true, slow, now)
	if _, ok := readDiskBenchCache(dir, 7, true, now.Add(30*time.Minute)); !ok {
		t.Fatal("fresh slow result not reused")
	}
	if _, ok := readDiskBenchCache(dir, 7, true, now.Add(2*time.Hour)); ok {
		t.Fatal("slow result must be rechecked after an hour")
	}
	if _, ok := readDiskBenchCache(dir, 7, true, now.Add(-time.Minute)); ok {
		t.Fatal("a cache dated in the future (clock moved) was trusted")
	}
}

func TestBenchCacheIgnoresGarbageAndSkipsNonAnswers(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, diskBenchCacheFile), []byte("{not json"), 0600)
	if _, ok := readDiskBenchCache(dir, 1, true, time.Now()); ok {
		t.Fatal("garbage cache accepted")
	}
	os.Remove(filepath.Join(dir, diskBenchCacheFile))
	writeDiskBenchCache(dir, 1, true, diskBenchResult{Err: "read-only file system"}, time.Now())
	if _, err := os.Stat(filepath.Join(dir, diskBenchCacheFile)); !os.IsNotExist(err) {
		t.Fatal("a failed-to-run test was cached as if it were an answer")
	}
}

func TestRunDiskAuditUsesCacheWithoutWriting(t *testing.T) {
	withBenchEnv(t, false, 0, true)
	dir := t.TempDir()
	dev, ok := diskBenchDevice(dir)
	writeDiskBenchCache(dir, dev, ok, diskBenchResult{MBps: 3, Bytes: 4 << 20, Slow: true}, time.Now())
	var opens atomic.Int32
	withBenchOpen(t, func(path string) (diskBenchFile, error) { opens.Add(1); return nil, errors.New("must not open") })
	slow, _ := RunDiskAudit(dir)
	if !slow {
		t.Fatal("cached slow result not honoured")
	}
	if opens.Load() != 0 {
		t.Fatal("benchmark wrote to disk despite a fresh cached result")
	}
}

func TestRunDiskAuditNeverWritesOutsideTheStateDir(t *testing.T) {
	withBenchEnv(t, false, 0, true)
	cwd := t.TempDir()
	state := filepath.Join(t.TempDir(), "state")
	t.Chdir(cwd)
	var paths []string
	var mu sync.Mutex
	withBenchOpen(t, func(path string) (diskBenchFile, error) {
		mu.Lock()
		paths = append(paths, path)
		mu.Unlock()
		return &fakeBenchFile{write: func(p []byte) (int, error) { return len(p), nil }}, nil
	})
	RunDiskAudit(state)
	if len(paths) != 1 || !strings.HasPrefix(paths[0], state+string(os.PathSeparator)) {
		t.Fatalf("benchmark file paths %v are not inside the state dir %s", paths, state)
	}
	if left, _ := os.ReadDir(cwd); len(left) != 0 {
		t.Fatalf("working dir was touched: %v", left)
	}
}

func TestRunSystemAuditWithoutStateDirNeverRunsTheBenchmark(t *testing.T) {
	withBenchOpen(t, func(path string) (diskBenchFile, error) { t.Fatal("benchmark ran with no state dir"); return nil, nil })
	if slow, _ := RunSystemAudit("", false); slow {
		t.Fatal("slow reported with no benchmark")
	}
}

func TestRunSystemAuditSkipDiskNeverWrites(t *testing.T) {
	dir := t.TempDir()
	withBenchOpen(t, func(path string) (diskBenchFile, error) { t.Fatal("benchmark ran although skipped"); return nil, nil })
	if slow, _ := RunSystemAudit(dir, true); slow {
		t.Fatal("skipped benchmark reported slow")
	}
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Fatalf("skipped audit wrote files: %v", left)
	}
}

func withBenchEnv(t *testing.T, container bool, psi float64, psiOK bool) {
	t.Helper()
	t.Setenv("URNETWORK_DISK_BENCH", "")
	oc, op := diskBenchInContainer, diskBenchIOPressure
	diskBenchInContainer = func() bool { return container }
	diskBenchIOPressure = func() (float64, bool) { return psi, psiOK }
	t.Cleanup(func() { diskBenchInContainer, diskBenchIOPressure = oc, op })
}

func TestDiskBenchSkipsInContainersUnlessForced(t *testing.T) {
	withBenchEnv(t, true, 0, true)
	if diskBenchSkipReason() == "" {
		t.Fatal("benchmark would run in a container by default")
	}
	t.Setenv("URNETWORK_DISK_BENCH", "1")
	if r := diskBenchSkipReason(); r != "" {
		t.Fatalf("forced benchmark still skipped: %s", r)
	}
}

func TestDiskBenchCanBeDisabledEverywhere(t *testing.T) {
	withBenchEnv(t, false, 0, true)
	t.Setenv("URNETWORK_DISK_BENCH", "0")
	if diskBenchSkipReason() == "" {
		t.Fatal("URNETWORK_DISK_BENCH=0 did not disable the benchmark")
	}
}

func TestDiskBenchSkipsWhenTheHostDiskIsBusyEvenIfForced(t *testing.T) {
	withBenchEnv(t, false, 35.0, true)
	if diskBenchSkipReason() == "" {
		t.Fatal("benchmark would run on a busy disk")
	}
	t.Setenv("URNETWORK_DISK_BENCH", "1")
	if diskBenchSkipReason() == "" {
		t.Fatal("forcing the benchmark must not bypass the busy-disk guard")
	}
}

func TestDiskBenchRunsOnQuietBareMetalAndWhenNoPressureInfo(t *testing.T) {
	withBenchEnv(t, false, 0.5, true)
	if r := diskBenchSkipReason(); r != "" {
		t.Fatalf("quiet bare metal skipped: %s", r)
	}
	withBenchEnv(t, false, 0, false)
	if r := diskBenchSkipReason(); r != "" {
		t.Fatalf("no pressure info (old kernel) must not block the benchmark: %s", r)
	}
}

func TestSkippedBenchmarkNeverWritesAndIsNeverSlow(t *testing.T) {
	withBenchEnv(t, true, 0, true)
	dir := t.TempDir()
	withBenchOpen(t, func(path string) (diskBenchFile, error) { t.Fatal("benchmark wrote in a container"); return nil, nil })
	if slow, _ := RunDiskAudit(dir); slow {
		t.Fatal("a skipped benchmark was reported as a slow disk")
	}
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Fatalf("skipped benchmark left files: %v", left)
	}
}

func TestParseIOPressure(t *testing.T) {
	v, ok := parseIOPressure("some avg10=12.34 avg60=1.00 avg300=0.14 total=545\nfull avg10=9.00 avg60=0.00 avg300=0.14 total=543\n")
	if !ok || v != 12.34 {
		t.Fatalf("got %v %v, want 12.34 true (the 'some' line, not 'full')", v, ok)
	}
	if _, ok := parseIOPressure("garbage"); ok {
		t.Fatal("garbage parsed as pressure")
	}
}

// A disk that is fast while its cache fills and slow afterwards must not pass
// just because the average over the whole run is above the line.
func TestRunDiskBenchCatchesACacheCliffInTheLastQuarter(t *testing.T) {
	dir := t.TempDir()
	var n atomic.Int32
	withBenchOpen(t, func(path string) (diskBenchFile, error) {
		return &fakeBenchFile{write: func(p []byte) (int, error) {
			if n.Add(1) > 24 {
				time.Sleep(30 * time.Millisecond) // ~33 MB/s once the cache is full
			}
			return len(p), nil
		}}, nil
	})
	r := runDiskBench(dir, 32<<20, 3*time.Second)
	if !r.Slow {
		t.Fatalf("cache cliff passed as a fast disk: %+v", r)
	}
	if r.MBps >= diskBenchSlowMBps {
		t.Fatalf("reported rate %.1f should be the slow tail rate", r.MBps)
	}
}

func TestRunDiskBenchSteadyFastDiskStillPassesTheTailCheck(t *testing.T) {
	dir := t.TempDir()
	withBenchOpen(t, func(path string) (diskBenchFile, error) {
		return &fakeBenchFile{write: func(p []byte) (int, error) { return len(p), nil }}, nil
	})
	if r := runDiskBench(dir, 32<<20, 3*time.Second); r.Slow {
		t.Fatalf("steady fast disk failed the tail check: %+v", r)
	}
}

func TestDiskBenchCapIsWholeChunksAndNeverAboveOneSixtyFourthOfFree(t *testing.T) {
	cases := []struct{ freeMiB, wantBytes int64 }{
		{300, 4 << 20},      // 4.69 MiB rounds down, not up to 5
		{256, 4 << 20},      // the smallest disk that is benchmarked
		{1000, 15 << 20},    // 15.6 MiB
		{1024, 16 << 20},    // free/64 = 16 MiB, still under the ceiling
		{16384, 256 << 20},  // free/64 `exactly` reaches the ceiling
		{500000, 256 << 20}, // large disks stay at the ceiling, not above it
	}
	for _, c := range cases {
		got := diskBenchCap(c.freeMiB)
		if got != c.wantBytes {
			t.Errorf("free %d MiB: cap %d, want %d", c.freeMiB, got, c.wantBytes)
		}
		if got%diskBenchChunk != 0 || got > c.freeMiB*1024*1024/diskBenchFreeDiv {
			t.Errorf("free %d MiB: cap %d is not whole chunks within free/64", c.freeMiB, got)
		}
	}
}

// captureStdout redirects os.Stdout to a pipe for the duration of fn and
// returns what was printed.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	fn()
	w.Close()
	os.Stdout = old
	b, err := io.ReadAll(r)
	r.Close()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(b)
}

// ghostPath returns a path of n levels that do not exist below base.
func ghostPath(base string, n int) string {
	for i := 0; i < n; i++ {
		base = filepath.Join(base, fmt.Sprintf("ghost-%d", i))
	}
	return base
}

// A state dir several missing levels deep must still be measured through its
// nearest existing parent, not reported as a full disk. The statfs is injected so
// the expected value cannot drift with the real disk, and the probes show which
// level answered.
func TestCheckDiskSpaceWalksUpPastMissingLevels(t *testing.T) {
	base := t.TempDir()
	var probed []string
	orig := diskSpaceStatfs
	diskSpaceStatfs = func(path string, st *syscall.Statfs_t) error {
		probed = append(probed, path)
		if path != base {
			return syscall.ENOENT
		}
		st.Bsize = 4096
		st.Bavail = 5000 * 1024 * 1024 / 4096 // 5000 MiB
		return nil
	}
	t.Cleanup(func() { diskSpaceStatfs = orig })

	var got int64
	var low bool
	out := captureStdout(t, func() { got, low = CheckDiskSpace(ghostPath(base, 12)) })
	if got != 5000 || low {
		t.Fatalf("deep missing path: got (%d, %v), want (5000, false)", got, low)
	}
	if strings.Contains(out, "CRITICAL") {
		t.Fatalf("a measurable dir printed a CRITICAL line: %q", out)
	}
	if len(probed) != 13 || probed[len(probed)-1] != base {
		t.Fatalf("probed %d paths ending at %q, want 12 missing levels then %q", len(probed), probed[len(probed)-1], base)
	}
}

// A path that cannot be measured at any level is unknown, not low: (0, false)
// and no output, so a wrong path can never raise a false alert.
func TestCheckDiskSpaceUnmeasurablePathIsQuiet(t *testing.T) {
	orig := diskSpaceStatfs
	diskSpaceStatfs = func(path string, st *syscall.Statfs_t) error { return syscall.ENOENT }
	t.Cleanup(func() { diskSpaceStatfs = orig })
	var got int64
	var low bool
	out := captureStdout(t, func() { got, low = CheckDiskSpace(ghostPath(t.TempDir(), 12)) })
	if got != 0 || low {
		t.Fatalf("unmeasurable path: got (%d, %v), want (0, false)", got, low)
	}
	if out != "" {
		t.Fatalf("unmeasurable path printed %q, want silence", out)
	}
}

// An unmeasurable path must not read as a full disk in the benchmark's own skip
// message either: unknown is not zero.
func TestRunDiskAuditUnmeasurablePathIsNotReportedAsAFullDisk(t *testing.T) {
	withBenchEnv(t, false, 0, true)
	orig := diskSpaceStatfs
	diskSpaceStatfs = func(string, *syscall.Statfs_t) error { return syscall.ENOENT }
	t.Cleanup(func() { diskSpaceStatfs = orig })
	dir := filepath.Join(t.TempDir(), "state", "deep")

	var slow, low bool
	out := captureStdout(t, func() { slow, low = RunDiskAudit(dir) })
	if slow || low {
		t.Fatalf("unmeasurable path: got (slow %v, low %v), want (false, false)", slow, low)
	}
	if !strings.Contains(out, "could not measure free space") || strings.Contains(out, "only 0 MiB free") {
		t.Fatalf("skip message = %q, want the could-not-measure wording and never 0 MiB free", out)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("the benchmark created the state dir although it could not measure it")
	}
}

// A cache written by a sampler with a different shape must not answer for the
// current one: the published rate depends on how much was written, so an old
// burst measurement would keep a host on the previous verdict until the entry
// aged out, without the new sample ever running.
func TestDiskBenchCacheFromAnOlderSamplerIsRejected(t *testing.T) {
	dir := t.TempDir()
	dev, devOK := diskBenchDevice(dir)
	now := time.Now()
	path := filepath.Join(dir, diskBenchCacheFile)

	// What the 16 MiB sampler would have written: fresh, same device, no marker.
	old := diskBenchCache{At: now, Device: dev, Result: diskBenchResult{MBps: 900, Bytes: 16 << 20, Millis: 18}}
	b, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := readDiskBenchCache(dir, dev, devOK, now); ok {
		t.Fatal("a cache from an older sampler must be rejected, not reused")
	}

	// The current sampler's own entry is still honoured, or the cache is useless.
	old.Sample = diskBenchCacheSample
	if b, err = json.Marshal(old); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := readDiskBenchCache(dir, dev, devOK, now); !ok {
		t.Fatal("a cache from the current sampler must be reused")
	}
}
