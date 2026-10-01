package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testNow is a fixed instant so nothing in these tests depends on the wall
// clock.
var testNow = func() time.Time { return time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC) }

// The baseline recorder is the operator's free upgrade baseline, so it is on
// by default and `set baseline off` must actually stop it live. These tests pin
// the key end to end: canonical form, validation, the live effect, the default
// that clearing restores, and that the file survives being switched off.

// The canonical form and the hiddenByDesign classification are CLI-side
// (internal/urnettools) and are asserted there; what matters here is that the
// value the socket accepts is on or off and nothing else.
func TestBaselineControlKeyValidatesItsValue(t *testing.T) {
	for _, v := range []string{"on", "off", "ON", "Off"} {
		if err := validateControlValue("baseline", v); err != nil {
			t.Errorf("value %q rejected: %v", v, err)
		}
	}
	// Anything else must be refused before it reaches the socket. A typo that
	// silently became "on" would be a recorder the operator cannot switch off.
	for _, v := range []string{"", "yes", "1", "true", "shadow", "enable"} {
		if err := validateControlValue("baseline", v); err == nil {
			t.Errorf("value %q was accepted; only on and off are valid", v)
		}
	}
}

func TestBaselineControlKeyIsLiveAndDefaultsOn(t *testing.T) {
	if !liveEffectKeys["baseline"] {
		t.Error("baseline is not a live-effect key, so `set baseline off` would report " +
			"success and keep sampling until the next restart")
	}
	// Clearing must turn it back ON, not leave it off: the whole point is a
	// baseline without anyone remembering to configure it.
	if got := liveDefaults["baseline"]; got != "on" {
		t.Errorf("liveDefaults[baseline] = %q, want on (clearing re-enables the recorder)", got)
	}
}

func TestBaselineControlKeyOffStopsSamplingWithoutDeletingTheFile(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/baseline.jsonl"
	// A record exists before the key is touched.
	if err := baselineAppend(path, baselineStartSample("v1", "v1", testNow()), baselineMaxBytes); err != nil {
		t.Fatal(err)
	}
	prev := baselineEnabled.Load()
	t.Cleanup(func() { baselineEnabled.Store(prev) })

	if err := applyLiveSideEffect("baseline", "off"); err != nil {
		t.Fatalf("applyLiveSideEffect(off): %v", err)
	}
	if baselineIsEnabled() {
		t.Error("baseline is still enabled after `set baseline off`")
	}
	// Off means stop recording, NOT erase: the file is the only copy of the
	// pre-upgrade behaviour and losing it would defeat the purpose.
	if _, err := baselineTail(path, 10); err != nil {
		t.Errorf("the existing file became unreadable after switching off: %v", err)
	}

	if err := applyLiveSideEffect("baseline", "on"); err != nil {
		t.Fatalf("applyLiveSideEffect(on): %v", err)
	}
	if !baselineIsEnabled() {
		t.Error("baseline did not come back on")
	}
	rows, err := baselineTail(path, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Errorf("the pre-existing record is gone: %d rows, want 1", len(rows))
	}
}

// A persisted "off" has to be replayed at startup, or `set baseline off` would
// silently revert on the next restart and the operator would have no idea why
// the file started growing again.
func TestBaselinePersistedOffIsReplayedAtStartup(t *testing.T) {
	state := newControlState()
	if err := state.set("baseline", "off"); err != nil {
		t.Fatalf("set baseline off: %v", err)
	}
	prev := baselineEnabled.Load()
	t.Cleanup(func() { baselineEnabled.Store(prev) })
	baselineEnabled.Store(true) // start from the default-on position

	applyPersistedRuntimeTuning(state)

	if baselineIsEnabled() {
		t.Error("a persisted baseline=off was not applied at startup; the recorder " +
			"would resume writing after every restart")
	}
}

func TestBaselineDefaultAppliesWhenNoPersistedValueExists(t *testing.T) {
	// The mirror of the test above: no explicit value must leave it ON, which is
	// the whole reason the feature needs no setup.
	state := newControlState()
	prev := baselineEnabled.Load()
	t.Cleanup(func() { baselineEnabled.Store(prev) })
	baselineEnabled.Store(false)

	applyPersistedRuntimeTuning(state)

	if !baselineIsEnabled() {
		t.Error("with no persisted value the recorder must default to on")
	}
}

// The provider-side half of the classification tripwire: every control key this
// package knows must be a live-effect key or explicitly exempt, or
// TestEveryControlKeyClassified goes red.
func TestBaselineKeyIsALiveEffectKey(t *testing.T) {
	if !liveEffectKeys["baseline"] {
		t.Error("baseline is not in liveEffectKeys, so it is neither applied live nor " +
			"listed as exempt, and the classification test will fail")
	}
}

// Clearing the key must land on the documented default rather than on whatever
// it happened to be, which is the bug the smart_dialer and metrics entries in
// liveDefaults were added for.
func TestBaselineClearRestoresTheDefault(t *testing.T) {
	if got := liveDefaults["baseline"]; !strings.EqualFold(got, "on") {
		t.Fatalf("clearing baseline would restore %q, but the recorder's default is on", got)
	}
}

// A provider that boots with the key persisted off must still have a sampler,
// or turning the key on later would log "applied" while nothing ever records.
func TestBaselineSamplerRunsWhenTheKeyStartsOffSoOnTakesEffectLive(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	baselineEnabled.Store(false)
	t.Cleanup(func() { baselineEnabled.Store(true) })

	ctx, cancel := context.WithCancel(context.Background())
	done := baselineStart(ctx, "v-test", "")
	if done == nil {
		t.Fatal("baselineStart launched no sampler while the key was off, so `set baseline on` " +
			"would never start recording until a restart")
	}
	if rows, _ := baselineTail(filepath.Join(dir, ".urnetwork", baselineFileName), 10); len(rows) != 0 {
		t.Errorf("wrote %d rows while the key was off, want 0", len(rows))
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("sampler did not stop when the provider context was cancelled")
	}
}

func TestBaselineMarkWithNoStateDirIsRefusedNotWrittenRelativeToCwd(t *testing.T) {
	prev := baselinePathFn
	baselinePathFn = func() string { return "" }
	t.Cleanup(func() { baselinePathFn = prev })
	if _, err := baselineMark("x"); err == nil {
		t.Fatal("baselineMark with no state directory succeeded; it would write relative to the cwd")
	}
}
