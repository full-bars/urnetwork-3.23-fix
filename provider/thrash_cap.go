package main

// thrash_cap.go is the thrash-escape memory: when the thrash watchdog restarts
// the provider for swap thrash, the next start must come up SMALLER instead of
// rebuilding the same non-fitting proxy set. On LA7 a plain restart alone did
// not fix the own-memory thrash — the same evening's redegrade proved the pool
// rebuilds the same wall — so escalation writes a reduced start cap that the
// launch path, the reload trim and the pool controller all honour through
// effectiveTrimCapSource (the tighter of operator trim, OOM cap, thrash cap).
//
// State lives at ~/.urnetwork/thrash_cap.json and expires 24h after the last
// escalation: a clean day relaxes the cap. A new escalation re-arms it, appends
// to the restarts ring, and pushes the expiry out. This file is portable: only
// effectiveTrimCapSource reads it, and that runs on every platform.

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const (
	// trimCapThrash names the automatic thrash cap as the binding source in
	// trim logs and the action ledger (mirrors trimCapOOM).
	trimCapThrash = "thrashcap"

	thrashCapFileName = "thrash_cap.json"

	// thrashCapHold is how long an escalation's cap survives without a new
	// escalation.
	thrashCapHold = 24 * time.Hour

	// thrashCapFraction is how much of the pre-restart running count the next
	// start may rebuild; the rest is headroom for the same growth the escape
	// could not contain.
	thrashCapFraction = 0.6

	// thrashMaxRestarts24h is the hard ceiling on escapes per day. Past it the
	// watchdog logs and stops acting (the cap stays in force from the last one).
	thrashMaxRestarts24h = 3
)

// thrashRestartBackoff is the re-arm schedule between escapes, indexed by how
// many escapes already happened in the last 24h. At the current max of 3 per
// day the third rung (6h) is unreachable (a third restart is denied outright);
// it stays for spec parity and goes live if the daily max ever rises.
var thrashRestartBackoff = []time.Duration{
	30 * time.Minute,
	2 * time.Hour,
	6 * time.Hour,
}

// thrashCapState is the persisted escape memory.
type thrashCapState struct {
	Cap         int     `json:"cap"`          // next-start running-proxy cap; 0 = none
	SetUnix     int64   `json:"set_unix"`     // when the cap was last (re-)armed
	ExpiresUnix int64   `json:"expires_unix"` // the cap stops applying after this
	Restarts    []int64 `json:"restarts"`     // unix seconds of recent escapes
}

// thrashCapPath returns the state file path, beside the other oom_cap files.
func thrashCapPath() (string, error) {
	dir, err := oomCapDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, thrashCapFileName), nil
}

// readThrashCapState reads the persisted state; a missing file is the zero
// value (never an error the caller must handle).
func readThrashCapState() thrashCapState {
	var st thrashCapState
	if path, err := thrashCapPath(); err == nil {
		oomReadJSON(path, &st)
	}
	return st
}

// activeThrashCap returns the cap in force at now, if any. Expiry is checked
// here so every reader agrees on when a quiet day has relaxed the cap.
func activeThrashCap(now time.Time) (int, bool) {
	st := readThrashCapState()
	if st.Cap <= 0 || st.ExpiresUnix <= 0 {
		return 0, false
	}
	if now.Unix() >= st.ExpiresUnix {
		return 0, false
	}
	return st.Cap, true
}

// thrashRestartsWithin prunes the restarts ring to the 24h window ending at
// now. Order is preserved (oldest first).
func thrashRestartsWithin(st thrashCapState, now time.Time) []int64 {
	cutoff := now.Add(-thrashCapHold).Unix()
	out := make([]int64, 0, len(st.Restarts))
	for _, ts := range st.Restarts {
		if ts < cutoff {
			continue
		}
		if ts > now.Unix() {
			// A backwards clock step left a future-dated entry: keep the
			// restart (dropping it would silently bypass the cooldown) but
			// clamp it to now.
			ts = now.Unix()
		}
		out = append(out, ts)
	}
	return out
}

// thrashCapEscalationAllowed applies the anti-loop ladder: at most
// thrashMaxRestarts24h escapes per day, spaced by thrashRestartBackoff.
// Returns a stable code plus human reason when denied, and the count of
// escapes in the last 24h. The code is what callers dedupe on; the reason
// embeds changing durations and must never be compared.
func thrashCapEscalationAllowed(st thrashCapState, now time.Time) (bool, string, string, int) {
	recent := thrashRestartsWithin(st, now)
	n := len(recent)
	if n >= thrashMaxRestarts24h {
		return false, "cap-reached", fmt.Sprintf("restart cap reached (%d of max %d restarts in 24h); not restarting again today", n, thrashMaxRestarts24h), n
	}
	if n > 0 {
		idx := n - 1
		if idx >= len(thrashRestartBackoff) {
			idx = len(thrashRestartBackoff) - 1
		}
		wait := thrashRestartBackoff[idx]
		since := now.Sub(time.Unix(recent[n-1], 0))
		if since < wait {
			return false, "rearm", fmt.Sprintf("re-arming: last restart was %s ago, next allowed in %s", roundDur(since), roundDur(wait-since)), n
		}
	}
	return true, "", "", n
}

// thrashCapForNextStart sizes the refit cap: a fraction of what was running
// when the box started thrashing. 0 running means nothing to protect, so no
// cap is written.
func thrashCapForNextStart(running int) int {
	if running <= 0 {
		return 0
	}
	cap := int(float64(running) * thrashCapFraction)
	if cap < 1 {
		cap = 1
	}
	return cap
}

// recordThrashEscalation persists the escape: cap for the next start, the
// restarts ring with this escape appended, and a fresh expiry. Errors are
// returned so the caller can log; the escalation itself never depends on the
// write succeeding (the exit proceeds either way).
func recordThrashEscalation(cap int, now time.Time) error {
	path, err := thrashCapPath()
	if err != nil {
		return err
	}
	// The write lock lives beside the file: the directory must exist before
	// oomWriteJSON tries to lock (same order ledgerAppend uses).
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	prev := readThrashCapState()
	recent := thrashRestartsWithin(prev, now)
	// The restart is ALWAYS appended: the ring is the anti-loop's accounting,
	// and skipping it when the computed cap was 0 (nothing running) once
	// allowed unthrottled restart loops. A cap of 0 keeps any cap the previous
	// escalation left standing instead of erasing it — the next start still
	// benefits from fitting better.
	writeCap := cap
	// Preserve/inherit the previous cap only while it is still UNEXPIRED, and
	// never loosen: the tighter of the two wins ("tighter automatic wins").
	if prev.Cap > 0 && prev.ExpiresUnix > now.Unix() && (writeCap <= 0 || prev.Cap < writeCap) {
		writeCap = prev.Cap
	}
	st := thrashCapState{
		Cap:         writeCap,
		SetUnix:     now.Unix(),
		ExpiresUnix: now.Add(thrashCapHold).Unix(),
		Restarts:    append(recent, now.Unix()),
	}
	return oomWriteJSON(path, st)
}

// roundDur renders a duration in whole human units ("29m", "1h58m") for the
// re-arm reason lines.
func roundDur(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	d = d.Round(time.Minute)
	if h := d / time.Hour; h > 0 {
		return fmt.Sprintf("%dh%02dm", h, (d%time.Hour)/time.Minute)
	}
	return fmt.Sprintf("%dm", d/time.Minute)
}
