package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// oom_cap.go is the OOM-aware start cap: when the kernel OOM-killed this box
// since the previous start, the next start should run fewer proxies instead of
// the same load that just died, and relax slowly once it has been stable.
//
// This file is SHADOW ONLY for now: it decides and logs what it would do
// ("[oomcap] would ...") but nothing enforces the cap yet. Enforcement must be
// wired through the effective cap (min of the operator trim and this) and gated
// by URNETWORK_OOM_CAP=on once the shadow decisions have been reviewed on real
// boxes. The decision logic below is pure so it can be tested without a kernel.

const (
	oomCapReduceFactor   = 0.8            // next cap = 80% of the proxies that were running at death
	oomCapMinFloor       = 50             // never below this many proxies
	oomCapMaxReductions  = 3              // per window; then freeze and say so
	oomCapReductionWin   = 24 * time.Hour // sliding window for the reduction budget
	oomCapRelaxAfter     = 24 * time.Hour // clean time before relaxing
	oomCapRelaxNumerator = 11             // +10% per clean window
	oomCapRelaxDenom     = 10
)

// oomMarker is written at every start and read at the next one.
type oomMarker struct {
	BootID      string `json:"boot_id"`
	OOMKills    int64  `json:"oom_kills"` // /proc/vmstat oom_kill at that start
	Proxies     int    `json:"proxies"`   // proxies launched at that start
	StartedUnix int64  `json:"started_unix"`
}

// oomKilledSinceMarker reports whether an OOM kill happened since the previous
// start. A different boot id means a reboot (the counter reset), which is never
// attributed to an OOM; an unknown reading never claims one.
func oomKilledSinceMarker(m *oomMarker, bootID string, oomKills int64) bool {
	if m == nil || bootID == "" || m.BootID != bootID || oomKills < 0 || m.OOMKills < 0 {
		return false
	}
	return oomKills > m.OOMKills
}

// oomCapState persists between starts.
type oomCapState struct {
	Cap        int     `json:"cap"`        // 0 = no auto cap
	Reductions []int64 `json:"reductions"` // unix times of recent reductions
	SinceUnix  int64   `json:"since_unix"` // last reduction or relax
}

type oomCapDecision struct {
	Action string // none | reduce | frozen | relax | clear
	From   int
	To     int
}

func recentReductions(rs []int64, now time.Time) []int64 {
	var out []int64
	for _, r := range rs {
		if now.Sub(time.Unix(r, 0)) < oomCapReductionWin {
			out = append(out, r)
		}
	}
	return out
}

// oomCapOnOOM decides the cap after a start that followed an OOM kill.
// proxiesAtDeath is what the previous process had launched; desired is the
// current desired pool size (bounds the floor).
func oomCapOnOOM(st oomCapState, proxiesAtDeath, desired int, now time.Time) (oomCapState, oomCapDecision) {
	recent := recentReductions(st.Reductions, now)
	st.Reductions = recent
	if len(recent) >= oomCapMaxReductions {
		return st, oomCapDecision{Action: "frozen", From: st.Cap, To: st.Cap}
	}
	floor := max(oomCapMinFloor, desired/4)
	base := proxiesAtDeath
	if st.Cap > 0 && st.Cap < base {
		base = st.Cap // never reduce from a number larger than the standing cap
	}
	next := max(floor, int(float64(base)*oomCapReduceFactor))
	if st.Cap > 0 && next >= st.Cap {
		// Already at or below what this reduction would set (e.g. at the floor).
		return st, oomCapDecision{Action: "none", From: st.Cap, To: st.Cap}
	}
	d := oomCapDecision{Action: "reduce", From: st.Cap, To: next}
	st.Cap = next
	st.Reductions = append(st.Reductions, now.Unix())
	st.SinceUnix = now.Unix()
	return st, d
}

// oomCapOnCleanStart relaxes the cap by 10% once a full clean window has passed
// since the last change, and clears it when it reaches the desired size.
func oomCapOnCleanStart(st oomCapState, desired int, now time.Time) (oomCapState, oomCapDecision) {
	if st.Cap <= 0 {
		return st, oomCapDecision{Action: "none"}
	}
	if now.Sub(time.Unix(st.SinceUnix, 0)) < oomCapRelaxAfter {
		return st, oomCapDecision{Action: "none", From: st.Cap, To: st.Cap}
	}
	next := max(st.Cap+1, st.Cap*oomCapRelaxNumerator/oomCapRelaxDenom)
	st.SinceUnix = now.Unix()
	if desired > 0 && next >= desired {
		d := oomCapDecision{Action: "clear", From: st.Cap, To: 0}
		st.Cap = 0
		return st, d
	}
	d := oomCapDecision{Action: "relax", From: st.Cap, To: next}
	st.Cap = next
	return st, d
}

func oomCapDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".urnetwork"), nil
}

func oomReadJSON(path string, v any) bool {
	b, err := os.ReadFile(path)
	return err == nil && json.Unmarshal(b, v) == nil
}

func oomWriteJSON(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// readVmstatOOMKills returns the kernel's oom_kill counter since boot, -1 if
// unreadable.
func readVmstatOOMKills() int64 {
	b, err := os.ReadFile("/proc/vmstat")
	if err != nil {
		return -1
	}
	for _, line := range strings.Split(string(b), "\n") {
		if k, v, ok := strings.Cut(line, " "); ok && k == "oom_kill" {
			if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
				return n
			}
		}
	}
	return -1
}

func readBootID() string {
	b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// oomCapStartup runs once per start (SHADOW ONLY): reads the previous marker,
// decides what an OOM-aware cap would do, persists the state and this start's
// marker, and returns the log lines. It never changes the proxies launched.
func oomCapStartup(desired, launching int, bootID string, oomKills int64, now time.Time) []string {
	dir, err := oomCapDir()
	if err != nil {
		return nil
	}
	_ = os.MkdirAll(dir, 0o700)
	markerPath, statePath := filepath.Join(dir, "run.marker"), filepath.Join(dir, "oom_cap.json")

	var prev oomMarker
	havePrev := oomReadJSON(markerPath, &prev)
	var st oomCapState
	oomReadJSON(statePath, &st)

	var d oomCapDecision
	var msg string
	if havePrev && oomKilledSinceMarker(&prev, bootID, oomKills) {
		st, d = oomCapOnOOM(st, prev.Proxies, desired, now)
		msg = "OOM kill since the last start"
	} else {
		st, d = oomCapOnCleanStart(st, desired, now)
		msg = "no OOM since the last start"
	}
	_ = oomWriteJSON(statePath, st)
	_ = oomWriteJSON(markerPath, oomMarker{BootID: bootID, OOMKills: oomKills, Proxies: launching, StartedUnix: now.Unix()})

	if d.Action == "none" || d.Action == "" {
		return nil
	}
	return []string{"[oomcap] shadow: " + msg + ": would " + d.Action + " the automatic start cap " +
		strconv.Itoa(d.From) + " -> " + strconv.Itoa(d.To) + " (not enforced; running " + strconv.Itoa(launching) + " proxies)"}
}
