package connect

import (
	"math"
	"runtime/debug"
	"testing"
)

// With many proxy servers, ApplyAutoTuning is invoked once per proxy. The
// per-proxy settings mutation is correct, but the "[tune] auto-profile" summary
// is global and must be emitted only once per process so it does not spam the
// log on startup (e.g. ~3000 proxies -> ~3000 identical lines).
func TestApplyAutoTuningLogsOncePerProcess(t *testing.T) {
	t.Setenv("URNETWORK_PROFILE", "auto")
	t.Setenv("GOGC", "")
	t.Setenv("GOMEMLIMIT", "")

	autoTuneLogged.Store(false)
	// ApplyAutoTuning writes PROCESS-WIDE runtime knobs (debug.SetGCPercent and
	// debug.SetMemoryLimit) through the tier it picks from this host's RAM.
	// Without restoring them this test leaves GOGC=200 and a finite GOMEMLIMIT
	// behind for every later test in the binary: on a big runner the tier is
	// Tier4Extreme, on a small one 50/75/100. Runner-dependent, which is why it
	// went unnoticed.
	restoreRuntimeTuning(t)

	orig := autoTuneLogf
	logCount := 0
	autoTuneLogf = func(format string, args ...any) { logCount++ }
	t.Cleanup(func() { autoTuneLogf = orig })

	const proxies = 5
	for i := 0; i < proxies; i++ {
		ApplyAutoTuning(DefaultClientSettings(), DefaultLocalUserNatSettings())
	}

	if logCount != 1 {
		t.Fatalf("expected auto-profile to log exactly once across %d calls, got %d", proxies, logCount)
	}
}

// The once-guard must gate only the log, not the settings application. Assert
// against a specific tier rather than the host's real one: selectTier returns
// Tier4Extreme on a large runner, which matched no case in the old switch, so
// the assertion never ran and the test passed vacuously. applyTier1/2 take an
// explicit ramLimit, which is the same seam the other tier tests use.
func TestApplyAutoTuningOnceGuardDoesNotGateTierSettings(t *testing.T) {
	t.Setenv("URNETWORK_PROFILE", "auto")
	t.Setenv("GOGC", "")
	t.Setenv("GOMEMLIMIT", "")

	for _, c := range []struct {
		name string
		want int64
		run  func(cs *ClientSettings, ns *LocalUserNatSettings)
	}{
		{"tier1 low", kib(128), func(cs *ClientSettings, ns *LocalUserNatSettings) {
			applyTier1(cs, ns, 1<<30)
		}},
		{"tier2 balanced", kib(256), func(cs *ClientSettings, ns *LocalUserNatSettings) {
			applyTier2(cs, ns, 1<<30)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			restoreRuntimeTuning(t)
			cs := DefaultClientSettings()
			ns := DefaultLocalUserNatSettings()
			c.run(cs, ns)
			if got := cs.ContractManagerSettings.InitialContractTransferByteCount; got != c.want {
				t.Fatalf("%s: contract floor = %d, want %d", c.name, got, c.want)
			}
		})
	}
}

func TestApplyAutoTuningSkippedWhenProfileNotAuto(t *testing.T) {
	t.Setenv("URNETWORK_PROFILE", "")

	autoTuneLogged.Store(false)
	logCount := 0
	orig := autoTuneLogf
	autoTuneLogf = func(format string, args ...any) { logCount++ }
	defer func() { autoTuneLogf = orig }()

	cs := DefaultClientSettings()
	ns := DefaultLocalUserNatSettings()
	if ApplyAutoTuning(cs, ns) {
		t.Fatalf("ApplyAutoTuning should return false when profile is not 'auto'")
	}
	if logCount != 0 {
		t.Fatalf("expected no auto-profile log when profile is not 'auto', got %d", logCount)
	}
}

func TestSelectTierThresholds(t *testing.T) {
	cases := []struct {
		name string
		ram  int64
		want string
	}{
		{"just under 1.2GiB is low", 1200*1024*1024 - 1, Tier1Low},
		{"exactly 1.2GiB is balanced", 1200 * 1024 * 1024, Tier2Balanced},
		{"just under 3GiB is balanced", 3000*1024*1024 - 1, Tier2Balanced},
		{"exactly 3GiB is performance", 3000 * 1024 * 1024, Tier3Performance},
	}
	for _, c := range cases {
		if got := selectTier(c.ram); got != c.want {
			t.Errorf("%s: selectTier(%d) = %q, want %q", c.name, c.ram, got, c.want)
		}
	}
}

// ApplyAutoTuning runs once per proxy server. The per-proxy settings (buffers,
// contract floor) are right to reapply, but GOGC is PROCESS-wide: resetting it
// on every launch silently reverts whatever the GC governor (or an operator
// `set gogc`) chose since the previous launch. Apply the tier's GOGC once.
func TestApplyAutoTuningSetsGCPercentOncePerProcess(t *testing.T) {
	t.Setenv("URNETWORK_PROFILE", "auto")
	t.Setenv("GOGC", "")
	autoGCPercentApplied.Store(false)
	restoreRuntimeTuning(t)
	orig := debug.SetGCPercent(100)
	t.Cleanup(func() { debug.SetGCPercent(orig); autoGCPercentApplied.Store(false) })

	cs, ns := DefaultClientSettings(), DefaultLocalUserNatSettings()
	applyTier1(cs, ns, 1<<30)
	if got := readGCPercent(); got != 50 {
		t.Fatalf("first apply: GOGC=%d, want the tier value 50", got)
	}

	// A governor (or operator) tightens GC after the first launch.
	debug.SetGCPercent(25)
	applyTier1(cs, ns, 1<<30)
	if got := readGCPercent(); got != 25 {
		t.Fatalf("second launch reset GOGC to %d, want the governor's 25 left alone", got)
	}
}

// A persisted operator/control gogc must win over the tier default even on the
// first launch.
func TestApplyAutoTuningSkipsGCPercentWhenOperatorPinned(t *testing.T) {
	t.Setenv("URNETWORK_PROFILE", "auto")
	t.Setenv("GOGC", "")
	autoGCPercentApplied.Store(false)
	restoreRuntimeTuning(t)
	orig := debug.SetGCPercent(80)
	t.Cleanup(func() { debug.SetGCPercent(orig); autoGCPercentApplied.Store(false); AutoTuneOperatorPinned = nil })

	AutoTuneOperatorPinned = func(key string) bool { return key == "gogc" }
	applyTier1(DefaultClientSettings(), DefaultLocalUserNatSettings(), 1<<30)
	if got := readGCPercent(); got != 80 {
		t.Fatalf("operator-pinned gogc was overridden to %d", got)
	}
}

// readGCPercent reads the current GOGC without leaving it changed
// (SetGCPercent returns the previous value).
func readGCPercent() int {
	cur := debug.SetGCPercent(100)
	debug.SetGCPercent(cur)
	return cur
}

// applyTier1 and friends set PROCESS-wide runtime knobs (GOGC and, when none is
// set, a soft memory limit of a fraction of RAM). A test that calls them must put
// both back, or every later test in the binary runs under a different GC
// configuration.
func restoreRuntimeTuning(t *testing.T) {
	t.Helper()
	gogc := readGCPercent()
	limit := debug.SetMemoryLimit(-1) // a negative input only reads the limit
	t.Cleanup(func() {
		debug.SetGCPercent(gogc)
		debug.SetMemoryLimit(limit)
	})
}

func TestAutoTuningTestsRestoreTheProcessWideRuntimeKnobs(t *testing.T) {
	t.Setenv("URNETWORK_PROFILE", "auto")
	t.Setenv("GOGC", "")
	t.Setenv("GOMEMLIMIT", "")
	beforeLimit, beforeGC := debug.SetMemoryLimit(-1), readGCPercent()
	autoGCPercentApplied.Store(false)

	t.Run("a test that applies a tier", func(t *testing.T) {
		restoreRuntimeTuning(t)
		debug.SetMemoryLimit(math.MaxInt64) // "no finite limit", so applyTier1 sets one
		applyTier1(DefaultClientSettings(), DefaultLocalUserNatSettings(), 1<<30)
		if debug.SetMemoryLimit(-1) == math.MaxInt64 {
			t.Fatal("the fixture did not exercise the memory limit path")
		}
	})

	if got := debug.SetMemoryLimit(-1); got != beforeLimit {
		t.Fatalf("the soft memory limit leaked out of the test: %d, was %d", got, beforeLimit)
	}
	if got := readGCPercent(); got != beforeGC {
		t.Fatalf("GOGC leaked out of the test: %d, was %d", got, beforeGC)
	}
	autoGCPercentApplied.Store(false)
}
