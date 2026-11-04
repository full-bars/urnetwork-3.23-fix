package main

import (
	"context"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// diagTestClock is a hand-moved clock; nothing in these tests sleeps to make
// time pass.
type diagTestClock struct{ t time.Time }

func (c *diagTestClock) now() time.Time { return c.t }

func newDiagTestEnv(t *testing.T) (diagEnv, *diagTestClock) {
	t.Helper()
	clock := &diagTestClock{t: time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)}
	env := diagEnv{
		stateDir:     t.TempDir(),
		now:          clock.now,
		freeBytes:    func(string) (uint64, error) { return 1 << 40, nil },
		runGC:        func() {},
		cpuFor:       20 * time.Millisecond,
		cpuStopGrace: 2 * time.Second,
		staleLockAge: 5 * time.Minute,
		keep:         5,
		stats: func() diagStats {
			return diagStats{
				RSSBytes: 4 << 30, HeapInuse: 3 << 30, HeapSys: 4 << 30,
				NumGC: 1234, LastGC: clock.t.Add(-12 * time.Second),
				Goroutines: 91234, Uptime: 11 * time.Hour,
				MemLimit: math.MaxInt64, GOGC: 100,
				Clients: 12, ProxyCount: 40, ProxiesKnown: true, Version: "test",
			}
		},
		writeHeap:       func(w io.Writer) error { _, err := w.Write([]byte("heap")); return err },
		writeGoroutines: func(w io.Writer) error { _, err := w.Write([]byte("goroutine 1 [running]")); return err },
		startCPU:        func(w io.Writer) error { _, err := w.Write([]byte("cpu")); return err },
		stopCPU:         func() {},
	}
	env.stats = func() func() diagStats {
		inner := env.stats
		return func() diagStats {
			s := inner()
			s.HeapInuse = 40 << 20
			return s
		}
	}()
	return env, clock
}

func diagRoot(env diagEnv) string { return filepath.Join(env.stateDir, diagDirName) }

// leftovers lists everything in the diag dir that is not the (transient) lock.
func diagEntries(t *testing.T, env diagEnv) []string {
	t.Helper()
	entries, err := os.ReadDir(diagRoot(env))
	if err != nil {
		t.Fatalf("read diag dir: %v", err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestDiagBundleContents(t *testing.T) {
	env, _ := newDiagTestEnv(t)
	res, err := runDiag(context.Background(), env)
	if err != nil {
		t.Fatalf("runDiag: %v", err)
	}
	if want := filepath.Join(diagRoot(env), "20261103T120000Z"); res.Dir != want {
		t.Fatalf("Dir = %q, want %q", res.Dir, want)
	}
	for _, name := range []string{"summary.txt", "heap.pb.gz", "goroutine.txt", "cpu.pb.gz"} {
		st, err := os.Stat(filepath.Join(res.Dir, name))
		if err != nil {
			t.Fatalf("%s missing: %v", name, err)
		}
		if st.Size() == 0 {
			t.Fatalf("%s is empty", name)
		}
		if runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %v, want 0600", name, st.Mode().Perm())
		}
	}
	summary, _ := os.ReadFile(filepath.Join(res.Dir, "summary.txt"))
	text := string(summary)
	for _, want := range []string{
		"summary.txt", "heap.pb.gz", "goroutine.txt", "cpu.pb.gz", // each file is named
		"Process memory (RSS)", "Go heap in use", "Go heap reserved", "Garbage collections",
		"91234", "Goroutines", "Process uptime", "11h0m0s",
		"GOMEMLIMIT", "no limit set", "GOGC", "Active connections", "Proxy slots",
		"post-GC", "session tokens",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("summary.txt lacks %q:\n%s", want, text)
		}
	}
	// The lock is released and nothing is left over.
	if got := diagEntries(t, env); len(got) != 1 || got[0] != "20261103T120000Z" {
		t.Fatalf("diag dir = %v, want only the bundle", got)
	}
}

func TestDiagRefusesWithoutFreeSpace(t *testing.T) {
	env, _ := newDiagTestEnv(t)
	env.freeBytes = func(string) (uint64, error) { return 100 << 20, nil }
	_, err := runDiag(context.Background(), env)
	if err == nil {
		t.Fatal("runDiag succeeded with 100 MiB free; want a refusal")
	}
	// The fixed 512 MiB floor leaves a 412 MiB shortfall.
	for _, want := range []string{"not enough free space", "need 512.0 MiB", "have 100.0 MiB", "short by 412.0 MiB"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	if got := diagEntries(t, env); len(got) != 0 {
		t.Fatalf("a refused capture left %v behind", got)
	}
}

func TestDiagFloorIsFixed(t *testing.T) {
	env, _ := newDiagTestEnv(t)
	env.stats = func() diagStats { return diagStats{HeapInuse: 4600 << 20} }
	env.freeBytes = func(string) (uint64, error) { return 30 << 30, nil }
	if _, err := runDiag(context.Background(), env); err != nil {
		t.Fatalf("large heap with 30 GiB free was refused: %v", err)
	}
}

func TestDiagFailedCaptureLeavesNothing(t *testing.T) {
	env, _ := newDiagTestEnv(t)
	sawPartial := false
	env.writeHeap = func(w io.Writer) error {
		// By now goroutine.txt is already on disk inside the .partial directory.
		parts, _ := filepath.Glob(filepath.Join(diagRoot(env), "*"+diagPartialExt, "goroutine.txt"))
		sawPartial = len(parts) == 1
		return errors.New("injected heap failure")
	}
	if _, err := runDiag(context.Background(), env); err == nil || !strings.Contains(err.Error(), "injected heap failure") {
		t.Fatalf("err = %v, want the injected failure", err)
	}
	if !sawPartial {
		t.Fatal("test premise broken: no .partial with goroutine.txt existed when the heap writer ran")
	}
	if got := diagEntries(t, env); len(got) != 0 {
		t.Fatalf("a failed capture left %v behind; want neither a bundle nor a .partial", got)
	}
}

func TestDiagRetentionKeepsFiveNewest(t *testing.T) {
	env, _ := newDiagTestEnv(t)
	root := diagRoot(env)
	old := []string{"20261101T000000Z", "20261101T010000Z", "20261101T020000Z", "20261101T030000Z", "20261101T040000Z", "20261101T050000Z"}
	for _, n := range old {
		if err := os.MkdirAll(filepath.Join(root, n), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// Something that is not a bundle must never be pruned.
	if err := os.MkdirAll(filepath.Join(root, "operator-notes"), 0o700); err != nil {
		t.Fatal(err)
	}
	res, err := runDiag(context.Background(), env)
	if err != nil {
		t.Fatalf("runDiag: %v", err)
	}
	if res.Pruned != 1 {
		t.Fatalf("Pruned = %d, want 1", res.Pruned)
	}
	if _, err := os.Stat(filepath.Join(root, old[0])); !os.IsNotExist(err) {
		t.Fatalf("oldest bundle survived: %v", err)
	}
	for _, n := range old[1:] {
		if _, err := os.Stat(filepath.Join(root, n)); err != nil {
			t.Fatalf("bundle %s should survive: %v", n, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "operator-notes")); err != nil {
		t.Fatalf("a non-bundle directory was pruned: %v", err)
	}
	if _, err := os.Stat(res.Dir); err != nil {
		t.Fatalf("the new bundle is missing: %v", err)
	}
}

func TestDiagPruneIsNotRunMidCapture(t *testing.T) {
	// Retention runs before the capture starts, never during it: count the
	// bundles from inside the heap writer.
	env, _ := newDiagTestEnv(t)
	root := diagRoot(env)
	for _, n := range []string{"20261101T000000Z", "20261101T010000Z", "20261101T020000Z", "20261101T030000Z", "20261101T040000Z", "20261101T050000Z", "20261101T060000Z"} {
		os.MkdirAll(filepath.Join(root, n), 0o700)
	}
	var seen int
	env.writeHeap = func(w io.Writer) error {
		seen = len(listDiagBundles(root))
		_, err := w.Write([]byte("heap"))
		return err
	}
	if _, err := runDiag(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	if seen != 5 {
		t.Fatalf("bundles during the capture = %d, want 5 (pruned before it started)", seen)
	}
}

func TestDiagSecondCaptureFailsFast(t *testing.T) {
	env, _ := newDiagTestEnv(t)
	inHeap := make(chan struct{})
	release := make(chan struct{})
	env.writeHeap = func(w io.Writer) error {
		close(inHeap)
		<-release
		_, err := w.Write([]byte("heap"))
		return err
	}
	done := make(chan error, 1)
	go func() { _, err := runDiag(context.Background(), env); done <- err }()
	<-inHeap

	second, _ := newDiagTestEnv(t)
	second.stateDir = env.stateDir
	_, err := runDiag(context.Background(), second)
	if err == nil || !strings.Contains(err.Error(), "a capture is already running (started ") {
		t.Fatalf("second capture err = %v, want the already-running message", err)
	}
	if !strings.Contains(err.Error(), "older than 5m0s") {
		t.Fatalf("error should state the stale-lock cutoff: %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("first capture: %v", err)
	}
	// With the first finished, a new one runs.
	second.now = func() time.Time { return time.Date(2026, 11, 3, 12, 5, 0, 0, time.UTC) }
	if _, err := runDiag(context.Background(), second); err != nil {
		t.Fatalf("capture after the first ended: %v", err)
	}
}

func TestDiagLockFromAnotherProcessBlocks(t *testing.T) {
	env, clock := newDiagTestEnv(t)
	root := diagRoot(env)
	os.MkdirAll(root, 0o700)
	started := clock.t.Add(-30 * time.Second).Format(time.RFC3339)
	os.WriteFile(filepath.Join(root, diagLock), []byte("pid=999999 started="+started+"\n"), 0o600)
	_, err := runDiag(context.Background(), env)
	if err == nil || !strings.Contains(err.Error(), "a capture is already running (started "+started+")") {
		t.Fatalf("err = %v, want blocked by a 30s old lock", err)
	}
	if got := diagEntries(t, env); len(got) != 1 || got[0] != diagLock {
		t.Fatalf("a blocked capture changed the dir: %v", got)
	}
}

func TestDiagStaleLockDoesNotBlock(t *testing.T) {
	env, clock := newDiagTestEnv(t)
	root := diagRoot(env)
	os.MkdirAll(root, 0o700)
	started := clock.t.Add(-6 * time.Minute).Format(time.RFC3339)
	os.WriteFile(filepath.Join(root, diagLock), []byte("pid=999999 started="+started+"\n"), 0o600)
	res, err := runDiag(context.Background(), env)
	if err != nil {
		t.Fatalf("a 6 minute old lock blocked the capture: %v", err)
	}
	if len(res.Notes) == 0 || !strings.Contains(res.Notes[0], "left behind by a crashed capture") {
		t.Fatalf("notes = %v, want the stale-lock replacement reported", res.Notes)
	}
	summary, _ := os.ReadFile(filepath.Join(res.Dir, "summary.txt"))
	if !strings.Contains(string(summary), "left behind by a crashed capture") {
		t.Fatalf("summary.txt does not mention the replaced lock:\n%s", summary)
	}
	if _, err := os.Stat(filepath.Join(root, diagLock)); !os.IsNotExist(err) {
		t.Fatalf("lock not released: %v", err)
	}
}

func TestDiagUnparseableLockFallsBackToFileAge(t *testing.T) {
	env, clock := newDiagTestEnv(t)
	root := diagRoot(env)
	os.MkdirAll(root, 0o700)
	lock := filepath.Join(root, diagLock)
	os.WriteFile(lock, []byte("garbage"), 0o600)
	os.Chtimes(lock, clock.t.Add(-time.Hour), clock.t.Add(-time.Hour))
	if _, err := runDiag(context.Background(), env); err != nil {
		t.Fatalf("an hour-old unreadable lock blocked the capture: %v", err)
	}
}

func TestDiagRemovesOrphanPartial(t *testing.T) {
	env, _ := newDiagTestEnv(t)
	orphan := filepath.Join(diagRoot(env), "20261102T000000Z"+diagPartialExt)
	os.MkdirAll(orphan, 0o700)
	os.WriteFile(filepath.Join(orphan, "heap.pb.gz"), []byte("x"), 0o600)
	if _, err := runDiag(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("orphan .partial from a crashed capture survived: %v", err)
	}
}

func TestDiagStuckStopCPUProfileIsBounded(t *testing.T) {
	env, _ := newDiagTestEnv(t)
	env.cpuStopGrace = 50 * time.Millisecond
	stuck := make(chan struct{})
	t.Cleanup(func() { close(stuck) })
	env.stopCPU = func() { <-stuck }

	done := make(chan struct{})
	var res diagResult
	var err error
	go func() { res, err = runDiag(context.Background(), env); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runDiag hung on a StopCPUProfile that never returns")
	}
	if err != nil {
		t.Fatalf("runDiag: %v", err)
	}
	summary, _ := os.ReadFile(filepath.Join(res.Dir, "summary.txt"))
	if !strings.Contains(string(summary), "StopCPUProfile did not return within 50ms") {
		t.Fatalf("summary.txt does not say the CPU capture overran:\n%s", summary)
	}
	if _, err := os.Stat(filepath.Join(res.Dir, "cpu.pb.gz")); err != nil {
		t.Fatalf("the overrunning CPU profile was not kept: %v", err)
	}
}

func TestDiagCPUProfileAlreadyRunningKeepsTheRest(t *testing.T) {
	env, _ := newDiagTestEnv(t)
	env.startCPU = func(io.Writer) error { return errors.New("cpu profiling already in use") }
	res, err := runDiag(context.Background(), env)
	if err != nil {
		t.Fatalf("runDiag: %v", err)
	}
	if _, err := os.Stat(filepath.Join(res.Dir, "cpu.pb.gz")); !os.IsNotExist(err) {
		t.Fatalf("cpu.pb.gz should not exist: %v", err)
	}
	summary, _ := os.ReadFile(filepath.Join(res.Dir, "summary.txt"))
	if !strings.Contains(string(summary), "NOT CAPTURED") || !strings.Contains(string(summary), "already in use") {
		t.Fatalf("summary.txt does not explain the missing CPU profile:\n%s", summary)
	}
}

func TestDiagHotSwapDuringCaptureLeavesNothing(t *testing.T) {
	env, _ := newDiagTestEnv(t)
	env.cpuFor = time.Minute // would take a minute if nothing aborted it
	started := make(chan struct{})
	env.startCPU = func(w io.Writer) error { close(started); _, err := w.Write([]byte("cpu")); return err }

	done := make(chan error, 1)
	go func() { _, err := runDiagTracked(env); done <- err }()
	<-started
	quiesceControlSocket() // the real hot swap commit point
	select {
	case err := <-done:
		if !errors.Is(err, errDiagHotSwap) {
			t.Fatalf("err = %v, want errDiagHotSwap", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("capture did not stop when the hot swap began")
	}
	if got := diagEntries(t, env); len(got) != 0 {
		t.Fatalf("an aborted capture left %v behind", got)
	}
	// abortDiagForHotSwap must wait for cleanup: nothing is listed as active.
	diagActiveMu.Lock()
	active := diagActive
	diagActiveMu.Unlock()
	if active != nil {
		t.Fatal("capture still registered after it ended")
	}
}

func TestDiagUnwritableStateDir(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("permission bits do not stop this user")
	}
	env, _ := newDiagTestEnv(t)
	if err := os.Chmod(env.stateDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(env.stateDir, 0o700) })
	_, err := runDiag(context.Background(), env)
	if err == nil || !strings.Contains(err.Error(), "cannot write to") || !strings.Contains(err.Error(), "ownership") {
		t.Fatalf("err = %v, want a not-writable message that says what to do", err)
	}
}

// A real capture against this test process: the profiles are produced by
// runtime/pprof itself and are valid, non-empty files.
func TestDiagRealProfilesAreWritten(t *testing.T) {
	env := productionDiagEnv(t.TempDir())
	env.cpuFor = 50 * time.Millisecond
	res, err := runDiag(context.Background(), env)
	if err != nil {
		t.Fatalf("runDiag with real profilers: %v", err)
	}
	for _, name := range []string{"heap.pb.gz", "goroutine.txt", "cpu.pb.gz", "summary.txt"} {
		st, err := os.Stat(filepath.Join(res.Dir, name))
		if err != nil || st.Size() == 0 {
			t.Fatalf("%s: %v (size %v)", name, err, st)
		}
	}
	b, _ := os.ReadFile(filepath.Join(res.Dir, "heap.pb.gz"))
	if len(b) < 2 || b[0] != 0x1f || b[1] != 0x8b {
		t.Fatal("heap.pb.gz is not gzip data")
	}
}

// diag holds its reply for the whole capture. The connection's 5 second write
// deadline is set before the handler runs, so without a fresh one the reply to a
// healthy client would fail.
func TestDiagSocketSlowReply(t *testing.T) {
	withTempHome(t)
	resetGlobalControlStateForTest()
	prev := diagRunner
	diagRunner = func(string) (diagResult, error) {
		time.Sleep(6 * time.Second)
		return diagResult{Dir: "/tmp/x/diag/20261103T120000Z", Files: []string{"summary.txt"}}, nil
	}
	t.Cleanup(func() { diagRunner = prev })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cleanup, err := startControlSocket(ctx, globalControlState)
	if err != nil {
		t.Fatalf("startControlSocket: %v", err)
	}
	defer cleanup()

	resp, err := dialControlSocket(controlRequest{Cmd: "diag"})
	if err != nil {
		t.Fatalf("dial diag: %v", err)
	}
	if !resp.OK || !strings.Contains(resp.Value, "20261103T120000Z") {
		t.Fatalf("diag reply = %+v", resp)
	}
}

func TestDiagEmptyProfileIsAFailedCapture(t *testing.T) {
	env, _ := newDiagTestEnv(t)
	env.writeHeap = func(io.Writer) error { return nil } // writes nothing
	_, err := runDiag(context.Background(), env)
	if err == nil || !strings.Contains(err.Error(), "heap.pb.gz came out empty") {
		t.Fatalf("err = %v, want the empty heap profile refused", err)
	}
	if got := diagEntries(t, env); len(got) != 0 {
		t.Fatalf("left %v behind", got)
	}
}
