package connect

import (
	"testing"
)

// With many proxy servers, ApplyAutoTuning is invoked once per proxy. The
// per-proxy settings mutation is correct, but the "[tune] auto-profile" summary
// is global and must be emitted only once per process so it does not spam the
// log on startup (e.g. ~3000 proxies -> ~3000 identical lines).
func TestApplyAutoTuningLogsOncePerProcess(t *testing.T) {
	t.Setenv("URNETWORK_PROFILE", "auto")

	autoTuneLogged.Store(false)

	logCount := 0
	orig := autoTuneLogf
	autoTuneLogf = func(format string, args ...any) { logCount++ }
	defer func() { autoTuneLogf = orig }()

	const proxies = 5
	var lastCs *ClientSettings
	for i := 0; i < proxies; i++ {
		cs := DefaultClientSettings()
		ns := DefaultLocalUserNatSettings()
		ApplyAutoTuning(cs, ns)
		lastCs = cs
	}

	if logCount != 1 {
		t.Fatalf("expected auto-profile to log exactly once across %d calls, got %d", proxies, logCount)
	}

	// The once-guard must gate only the log, not the settings application.
	// Verify the final call still applied tier settings (for the low/balanced
	// tiers where there is an observable change).
	switch tier := selectTier(DetectEffectiveRAMLimitBytes()); tier {
	case Tier1Low:
		if got := lastCs.ContractManagerSettings.InitialContractTransferByteCount; got != kib(128) {
			t.Fatalf("tier1: settings not applied on later call; contract floor = %d, want %d", got, kib(128))
		}
	case Tier2Balanced:
		if got := lastCs.ContractManagerSettings.InitialContractTransferByteCount; got != kib(256) {
			t.Fatalf("tier2: settings not applied on later call; contract floor = %d, want %d", got, kib(256))
		}
	case Tier3Performance:
		t.Logf("host is performance tier; tier settings are a no-op, log-once still verified")
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
