package urnettools

import "testing"

// `urnet-tools set baseline off` must be sent to the provider as the VALUE
// "off", not as the generic clear. The clear path drops the override and
// re-applies the provider's live default for the key, and for baseline that
// default is "on". So the documented way to stop recording printed "cleared ...
// applied live" while the recorder kept writing, and the persisted state then
// read as no value, which the provider treats as on at every start. The
// provider-side tests set the flag directly and so never saw it.
func TestBaselineOffIsSentAsAValueNotAClear(t *testing.T) {
	for _, key := range []string{"baseline", "baseline_recorder", "Baseline"} {
		c, ok := canonicalControlKey(key)
		if !ok || c != "baseline" {
			t.Fatalf("canonicalControlKey(%q) = %q, %v; want baseline", key, c, ok)
		}
		if treatsOffAsClear(c) {
			t.Errorf("%q maps to baseline, which must NOT take the off-as-clear path: "+
				"clearing restores the default, which is on", key)
		}
	}
}

// Any key whose live default is not "off" needs the same treatment, because a
// clear restores that default. This pins it for every key the provider lists in
// liveDefaults with a non-off default, so a new default-on key cannot repeat
// the mistake. The urnettools side has no access to the provider's map, so the
// known default-on keys are listed here and must stay in step with it.
func TestDefaultOnKeysNeverTakeTheOffAsClearPath(t *testing.T) {
	for _, k := range []string{"baseline", "oom_cap"} {
		if treatsOffAsClear(k) {
			t.Errorf("%s has a live default that is not off, so off must be sent as a value", k)
		}
	}
}
