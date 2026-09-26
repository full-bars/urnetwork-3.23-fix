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
// It defaults to SHADOW mode: it decides and logs what it would do, and only
// URNETWORK_OOM_CAP=on enforces the cap, through effectiveTrimCap (the tightest
// of the operator trim and this cap). The decision logic below is pure so it can
// be tested without a kernel.

const (
	oomCapReduceFactor   = 0.8            // next cap = 80% of the proxies that were running at death
	oomCapMinFloor       = 50             // never below this many proxies
	oomCapMaxReductions  = 3              // per window; then freeze and say so
	oomCapReductionWin   = 24 * time.Hour // sliding window for the reduction budget
	oomCapRelaxAfter     = 24 * time.Hour // clean time before relaxing
	oomCapRelaxNumerator = 11             // +10% per clean window
	oomCapRelaxDenom     = 10
)

type oomCapModeKind int

const (
	oomCapShadow oomCapModeKind = iota // default: decide and log, enforce nothing
	oomCapOff
	oomCapOn
)

func parseOOMCapMode(v string) (oomCapModeKind, bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "off", "0", "false", "no":
		return oomCapOff, true
	case "on", "1", "true", "yes":
		return oomCapOn, true
	case "shadow":
		return oomCapShadow, true
	}
	return oomCapShadow, false
}

// oomCapMode combines the URNETWORK_OOM_CAP environment variable with the
// persisted control value (`urnet-tools set oom-cap on|off|shadow`). ANY source
// saying off wins, so the kill switch always works even against an env "on";
// otherwise any source saying on wins; otherwise shadow, the safe default of
// deciding and logging without enforcing.
func oomCapMode() oomCapModeKind {
	sources := []string{os.Getenv("URNETWORK_OOM_CAP")}
	if v, ok := globalControlState.get("oom_cap"); ok {
		sources = append(sources, v)
	}
	mode := oomCapShadow
	for _, v := range sources {
		m, ok := parseOOMCapMode(v)
		if !ok {
			continue
		}
		if m == oomCapOff {
			return oomCapOff
		}
		if m == oomCapOn {
			mode = oomCapOn
		}
	}
	return mode
}

// effectiveTrimCap is the cap the launch and reload paths enforce: the tightest
// positive of the operator's trim file (never written by the auto logic) and the
// automatic OOM cap, which counts only in "on" mode. 0 means no cap.
func effectiveTrimCap() (int, error) {
	operator, err := readTrimTarget()
	if err != nil {
		return 0, err
	}
	if oomCapMode() != oomCapOn {
		return operator, nil
	}
	var st oomCapState
	if dir, derr := oomCapDir(); derr == nil {
		oomReadJSON(filepath.Join(dir, "oom_cap.json"), &st)
	}
	switch {
	case st.Cap > 0 && (operator == 0 || st.Cap < operator):
		return st.Cap, nil
	}
	return operator, nil
}

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

// oomCapDecide runs once per start BEFORE the launch selection: it reads the
// previous marker, decides what an OOM-aware cap does, persists the cap state and
// returns the log lines. In "on" mode the persisted cap is then enforced by
// effectiveTrimCap for this very start; otherwise it is only reported.
func oomCapDecide(desired int, bootID string, oomKills int64, now time.Time) []string {
	mode := oomCapMode()
	if mode == oomCapOff {
		return nil
	}
	dir, err := oomCapDir()
	if err != nil {
		return nil
	}
	_ = os.MkdirAll(dir, 0o700)
	statePath := filepath.Join(dir, "oom_cap.json")

	var prev oomMarker
	havePrev := oomReadJSON(filepath.Join(dir, "run.marker"), &prev)
	var st oomCapState
	oomReadJSON(statePath, &st)

	var d oomCapDecision
	var msg string
	if havePrev && oomKilledSinceMarker(&prev, bootID, oomKills) {
		st, d = oomCapOnOOM(st, prev.Proxies, desired, now)
		msg = "OOM kill since the last start (peak running " + strconv.Itoa(prev.Proxies) + ")"
	} else {
		st, d = oomCapOnCleanStart(st, desired, now)
		msg = "no OOM since the last start"
	}
	_ = oomWriteJSON(statePath, st)

	if d.Action == "none" || d.Action == "" {
		return nil
	}
	modeName := "shadow"
	if mode == oomCapOn {
		modeName = "on"
	}
	ledgerRecord(ledgerEntry{Actor: "oomcap", Action: d.Action, From: d.From, To: d.To, Mode: modeName, Reason: msg})
	verb, tail := "shadow: "+msg+": would ", " (not enforced; set URNETWORK_OOM_CAP=on to enforce)"
	if mode == oomCapOn {
		verb, tail = "applied: "+msg+": ", ""
	}
	return []string{"[oomcap] " + verb + d.Action + " the automatic start cap " +
		strconv.Itoa(d.From) + " -> " + strconv.Itoa(d.To) + tail}
}

// oomMarkerWithPeak raises the marker's proxy count to the observed running
// count when that is higher (the peak within this start), and reports whether it
// changed. The count only ever rises, so a partial ramp never lowers it.
func oomMarkerWithPeak(m oomMarker, running int) (oomMarker, bool) {
	if running <= m.Proxies {
		return m, false
	}
	m.Proxies = running
	return m, true
}

// oomCapUpdatePeak records a higher running count in this start's marker. Cheap:
// it only writes when the peak rises.
func oomCapUpdatePeak(running int) {
	if oomCapMode() == oomCapOff {
		return
	}
	dir, err := oomCapDir()
	if err != nil {
		return
	}
	path := filepath.Join(dir, "run.marker")
	var m oomMarker
	if !oomReadJSON(path, &m) {
		return
	}
	if next, changed := oomMarkerWithPeak(m, running); changed {
		_ = oomWriteJSON(path, next)
	}
}

// oomCapRecordStart writes this start's marker (after the launch selection, so
// it records what was actually launched).
func oomCapRecordStart(launching int, bootID string, oomKills int64, now time.Time) {
	if oomCapMode() == oomCapOff {
		return
	}
	dir, err := oomCapDir()
	if err != nil {
		return
	}
	_ = os.MkdirAll(dir, 0o700)
	_ = oomWriteJSON(filepath.Join(dir, "run.marker"), oomMarker{BootID: bootID, OOMKills: oomKills, Proxies: launching, StartedUnix: now.Unix()})
}

// oomCapStartup is decide + record in one call (tests, and callers that do not
// need the decision to take effect before their own launch selection).
func oomCapStartup(desired, launching int, bootID string, oomKills int64, now time.Time) []string {
	lines := oomCapDecide(desired, bootID, oomKills, now)
	oomCapRecordStart(launching, bootID, oomKills, now)
	return lines
}
