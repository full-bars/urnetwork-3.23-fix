package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
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

// oomCapModeName is parseOOMCapMode's inverse for logging: the canonical name
// of a mode value.
func oomCapModeName(m oomCapModeKind) string {
	switch m {
	case oomCapOff:
		return "off"
	case oomCapOn:
		return "on"
	default:
		return "shadow"
	}
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

// trimCapOperator and trimCapOOM name the source of the binding cap.
const (
	trimCapOperator = "operator"
	trimCapOOM      = "oomcap"
)

// effectiveTrimCapSource is the cap the launch and reload paths enforce and the
// source that binds: the tightest positive of the operator's trim file (never
// written by the auto logic) and the automatic OOM cap, which counts only in
// "on" mode. cap 0 means no cap and the source is "". Logs and the action ledger
// use the source so a shed driven by the automatic cap is not attributed to the
// operator.
//
// The automatic cap is evaluated FIRST and independently of the operator's file:
// the two come from different places and one unreadable file must not silence
// the other. A proxy_trim that cannot be read for any reason other than "not
// there" (permissions, a directory where a file belongs, a broken disk) used to
// return an error here, and both callers (the reload's trim block and the
// startup selection) skip capping entirely on an error, so a single unreadable
// file silently disabled the OOM protection on exactly the boxes short of
// memory. Here the operator cap reads as 0, says so once, and the automatic cap
// still applies.
func effectiveTrimCapSource() (int, string, error) {
	auto := 0
	if oomCapMode() == oomCapOn {
		var st oomCapState
		if dir, derr := oomCapDir(); derr == nil {
			oomReadJSON(filepath.Join(dir, "oom_cap.json"), &st)
		}
		auto = st.Cap
	}
	operator, err := readTrimTarget()
	if err != nil {
		// Same shape as the invalid-value warning in readTrimTarget: the ramlog
		// line is immediate, the events.log write is queued for after whatever
		// lock this caller holds, and a repeat of the same error says nothing
		// at all (not even a blank line).
		if w := trimUnreadableWarning(err); w != "" {
			tlog("%s\n", w)
			deferCritWrite(w)
		}
		if auto > 0 {
			return auto, trimCapOOM, nil
		}
		return 0, "", nil
	}
	switch {
	case auto > 0 && (operator == 0 || auto < operator):
		return auto, trimCapOOM, nil
	case operator > 0:
		return operator, trimCapOperator, nil
	}
	return 0, "", nil
}

// trimUnreadableSeen is the proxy_trim read error already warned about, so a
// file that stays unreadable is reported once instead of on every reload.
var trimUnreadableSeen atomic.Value

// trimUnreadableReset clears the warned-on error. Tests only: without it one
// test's unreadable file silences the warning in every test after it.
func trimUnreadableReset() { trimUnreadableSeen.Store("") }

// trimUnreadableWarning returns the warning line for an unreadable proxy_trim
// the first time that exact error is seen, and "" for a repeat.
func trimUnreadableWarning(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if prev, _ := trimUnreadableSeen.Load().(string); prev == msg {
		return ""
	}
	trimUnreadableSeen.Store(msg)
	return fmt.Sprintf("[proxy][trim] warn: cannot read proxy_trim (%s); ignoring the operator cap, the automatic cap still applies", msg)
}

// effectiveTrimCap is effectiveTrimCapSource without the source.
func effectiveTrimCap() (int, error) {
	c, _, err := effectiveTrimCapSource()
	return c, err
}

// oomMarker is written at every start and read at the next one.
type oomMarker struct {
	// BootID is the counter's epoch (see oomKillEpoch): the boot id, plus the
	// cgroup scope when OOMKills was read from a cgroup's memory.events.
	BootID      string `json:"boot_id"`
	OOMKills    int64  `json:"oom_kills"` // the oom_kill counter at that start
	Proxies     int    `json:"proxies"`   // proxies launched at that start
	StartedUnix int64  `json:"started_unix"`
	// LastSeenUnix is a heartbeat refreshed by the periodic pressure-loop path
	// (oomCapUpdatePeak) at most once per oomHeartbeatInterval, while this
	// process is alive. StartedUnix alone freezes at process start, so a
	// long-running provider's marker looked stale (see oomMarkerMaxAge) and any
	// OOM kill after oomMarkerMaxAge of uptime was silently never blamed. Zero
	// means an old marker written before this field existed; age-checks fall
	// back to StartedUnix in that case.
	LastSeenUnix int64 `json:"last_seen_unix,omitempty"`
}

// oomHeartbeatInterval bounds how often oomCapUpdatePeak refreshes
// LastSeenUnix: often enough that oomMarkerMaxAge age-checks the process's
// actual uptime, not so often that a routine 30s pressure tick becomes a disk
// write.
const oomHeartbeatInterval = time.Hour

// oomKilledSinceMarker reports whether an OOM kill happened since the previous
// start. A different boot id means a reboot (the counter reset), which is never
// attributed to an OOM; an unknown reading never claims one. A marker older than
// oomMarkerMaxAge is stale: the box may have been down for days (or running with
// the cap off) while ANY other workload on the same boot bumped the counter
// (the host-wide one where no cgroup counter is readable; see oomKillEpoch),
// and the marker's proxy count no longer describes this process's load. Staleness is judged against the heartbeat (how recently this
// process was last seen running), not StartedUnix (when it started): a
// long-running process refreshes the heartbeat and is never blamed for a kill
// that happened while it was legitimately still up. A marker with no heartbeat
// (written before this field existed) falls back to StartedUnix.
const oomMarkerMaxAge = 72 * time.Hour

func oomKilledSinceMarker(m *oomMarker, bootID string, oomKills int64, now time.Time) bool {
	if m == nil || bootID == "" || m.BootID != bootID || oomKills < 0 || m.OOMKills < 0 {
		return false
	}
	lastSeen := m.LastSeenUnix
	if lastSeen == 0 {
		lastSeen = m.StartedUnix
	}
	if lastSeen > 0 && now.Sub(time.Unix(lastSeen, 0)) > oomMarkerMaxAge {
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
		// Frozen: no more reductions today, but the box was OOM-killed NOW.
		// The clean-day clock must restart from this OOM, or a start after
		// the original window would relax (grow) the cap immediately after
		// a fresh kill.
		st.SinceUnix = now.Unix()
		return st, oomCapDecision{Action: "frozen", From: st.Cap, To: st.Cap}
	}
	floor := max(oomCapMinFloor, desired/4)
	base := proxiesAtDeath
	if st.Cap > 0 && st.Cap < base {
		base = st.Cap // never reduce from a number larger than the standing cap
	}
	next := max(floor, int(float64(base)*oomCapReduceFactor))
	if next >= proxiesAtDeath || (st.Cap > 0 && next >= st.Cap) {
		// Either already at or below what this reduction would set (e.g. at
		// the floor), or the floor itself (max(oomCapMinFloor, desired/4)) is
		// not tighter than what actually died -- a small pool (desired under
		// roughly 4x the floor) would otherwise get a cap AT OR ABOVE its
		// death size, an "applied: reduce" that changes nothing or grows the
		// cap. Still an OOM kill: restart the clean-day clock so the cap does
		// not relax immediately after the box died.
		st.SinceUnix = now.Unix()
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

// oomReadJSONChecked is oomReadJSON that also says when the file EXISTS but
// could not be read or parsed (torn, hand-edited, disk full). A missing file is
// normal (first start) and returns no warning. The caller proceeds as if there
// were no state, so without the warning a standing cap or an OOM attribution
// would silently vanish.
func oomReadJSONChecked(path string, v any) (ok bool, warning string) {
	if oomReadJSON(path, v) {
		return true, ""
	}
	// Only a genuinely absent file is normal. Any OTHER stat failure (EACCES on
	// a parent directory, EIO, ENOTDIR) means the file may well be there and we
	// simply could not look, so the warning has to stand: a standing cap or an
	// OOM kill recorded there is not applied, and saying so is the whole point
	// of this check.
	if _, err := os.Stat(path); err != nil && os.IsNotExist(err) {
		return false, ""
	}
	return false, "[oomcap] warn: " + filepath.Base(path) + " exists but could not be read, so it is treated as empty (a standing cap or an OOM kill recorded there is not applied)"
}

func oomWriteJSON(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	// A HotSwap parent and candidate both write these files at start. Without
	// the lock one can truncate the shared .tmp while the other renames it, and
	// the state file ends up holding a partial write.
	release, err := acquireJWTStoreLock(path)
	if err != nil {
		return err
	}
	defer release()
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

// parseCgroupOOMKills reads the oom_kill field out of a cgroup v2 memory.events
// file. ok is false when the field is absent or unparseable.
func parseCgroupOOMKills(events string) (int64, bool) {
	for _, line := range strings.Split(events, "\n") {
		if k, v, ok := strings.Cut(line, " "); ok && k == "oom_kill" {
			if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil && n >= 0 {
				return n, true
			}
		}
	}
	return 0, false
}

// readCgroupOOMKills returns the OOM-kill counter of the cgroup subtree this
// process runs in, and a scope label naming which cgroup it was read from.
//
// memory.events is hierarchical, so a cgroup's counter includes every kill in
// its descendants. The process's own cgroup is not used: systemd removes a
// service's cgroup when it stops and creates a new one on restart, so its
// counter resets exactly when we need to compare across a restart. The parent
// (or the nearest ancestor with a readable file) persists, and still counts only
// kills inside that subtree, not the whole host's. When the process IS the mount
// root (a container), its own file is the persistent one and is used.
//
// What the subtree covers, honestly: for a system unit that parent is
// /system.slice, which also holds every OTHER service on the box, and with the
// systemd Docker driver it holds the containers too. A beta provider container
// running on the same host can therefore bump the counter this provider reads
// and be blamed on it here. That is a deliberate trade for a counter that
// survives a restart; the scope label travels with the reading so an operator
// reading the ledger can see which subtree the number covered.
// ok is false when no memory.events can be read (cgroup v1, no cgroup).
func readCgroupOOMKills(mount, selfCgroup string) (kills int64, scope string, ok bool) {
	rel := ""
	for _, line := range strings.Split(selfCgroup, "\n") {
		if strings.HasPrefix(line, "0::") {
			rel = strings.TrimSpace(strings.TrimPrefix(line, "0::"))
			break
		}
	}
	if rel == "" {
		return 0, "", false
	}
	mount = filepath.Clean(mount)
	dir := filepath.Join(mount, rel)
	if dir != mount {
		dir = filepath.Dir(dir) // start at the parent: it outlives our own cgroup
	}
	for {
		if b, err := os.ReadFile(filepath.Join(dir, "memory.events")); err == nil {
			if n, found := parseCgroupOOMKills(string(b)); found {
				at := strings.TrimPrefix(dir, mount)
				if at == "" {
					at = "/"
				}
				return n, "cgroup:" + at, true
			}
		}
		if dir == mount || len(dir) <= len(mount) {
			return 0, "", false
		}
		dir = filepath.Dir(dir)
	}
}

// oomKillEpoch returns the (epoch, counter) pair the marker stores and compares.
// The epoch is the boot id, plus the scope the counter covers when it comes from
// a cgroup: a reading is only comparable to a marker taken from the same counter
// on the same boot, and oomKilledSinceMarker already refuses to compare across
// different epochs. With a readable cgroup file the counter covers this
// process's ANCESTOR cgroup's subtree (see readCgroupOOMKills on what that
// subtree includes: for a system unit, /system.slice and everything under it),
// so a kill anywhere in that subtree counts, and a kill elsewhere on the host
// does not. Otherwise it is the host-wide /proc/vmstat counter with the plain
// boot id, as before. The first start after an upgrade has a plain-boot-id
// marker, so it is not compared (never a false attribution).
func oomKillEpoch(bootID, mount, selfCgroup string, vmstatKills int64) (string, int64) {
	if bootID != "" {
		if n, scope, ok := readCgroupOOMKills(mount, selfCgroup); ok {
			return bootID + "|" + scope, n
		}
	}
	return bootID, vmstatKills
}

// readOOMKillEpoch is oomKillEpoch for the running process.
func readOOMKillEpoch() (string, int64) {
	self, _ := os.ReadFile("/proc/self/cgroup")
	return oomKillEpoch(readBootID(), "/sys/fs/cgroup", string(self), readVmstatOOMKills())
}

// oomCapResetOnOff forgets the standing automatic cap. Turning the kill switch
// off has to mean the automatic cap is GONE, not merely not enforced while off:
// nothing ages the cap while off (the reduce and relax logic never runs), so a
// cap set days ago is still there, still binding the moment the switch goes back
// on, and it can shed proxies on the very start the operator turned the switch
// back on to let the pool grow. The reduction history is kept: it is what stops
// a flapping box from cutting the pool again on the very next OOM.
//
// Deliberately NOT an age check on SinceUnix. A long-running provider
// legitimately holds the same cap for days (SinceUnix only moves at a reduction
// or a relax, and a relax is evaluated at start), so dropping a cap merely
// because it is old would shed proxies mid-run on a healthy box. "off" is the
// operator saying stop, and stop means no standing cap.
func oomCapResetOnOff() []string {
	dir, err := oomCapDir()
	if err != nil {
		return nil
	}
	statePath := filepath.Join(dir, "oom_cap.json")
	var st oomCapState
	if !oomReadJSON(statePath, &st) || st.Cap <= 0 {
		return nil
	}
	from := st.Cap
	st.Cap = 0
	if werr := oomWriteJSON(statePath, st); werr != nil {
		importantLogf("[oomcap] warn: could not clear the automatic cap %d (%v); it stays in place and is not enforced while the mode is off\n", from, werr)
		return nil
	}
	ledgerRecord(ledgerEntry{Actor: "oomcap", Action: "clear", From: from, To: 0, Mode: oomCapModeName(oomCapOff),
		Reason: "kill switch off"})
	return []string{"[oomcap] cleared the automatic start cap " + strconv.Itoa(from) + " -> none (the kill switch forgets the standing cap)"}
}

// oomCapDecide runs once per start BEFORE the launch selection: it reads the
// previous marker, decides what an OOM-aware cap does, persists the cap state and
// returns the log lines. In "on" mode the persisted cap is then enforced by
// effectiveTrimCap for this very start; otherwise it is only reported.
func oomCapDecide(desired int, bootID string, oomKills int64, now time.Time) []string {
	mode := oomCapMode()
	if mode == oomCapOff {
		return oomCapResetOnOff()
	}
	dir, err := oomCapDir()
	if err != nil {
		return nil
	}
	_ = os.MkdirAll(dir, 0o700)
	statePath := filepath.Join(dir, "oom_cap.json")

	var warnings []string
	var prev oomMarker
	havePrev, w := oomReadJSONChecked(filepath.Join(dir, "run.marker"), &prev)
	if w != "" {
		warnings = append(warnings, w)
	}
	var st oomCapState
	if _, w := oomReadJSONChecked(statePath, &st); w != "" {
		warnings = append(warnings, w)
	}

	var d oomCapDecision
	var msg string
	if havePrev && oomKilledSinceMarker(&prev, bootID, oomKills, now) {
		st, d = oomCapOnOOM(st, prev.Proxies, desired, now)
		msg = "OOM kill since the last start (peak running " + strconv.Itoa(prev.Proxies) + ")"
	} else {
		st, d = oomCapOnCleanStart(st, desired, now)
		msg = "no OOM since the last start"
	}
	_ = oomWriteJSON(statePath, st)

	if d.Action == "none" || d.Action == "" {
		return warnings
	}
	modeName := "shadow"
	if mode == oomCapOn {
		modeName = "on"
	}
	ledgerRecord(ledgerEntry{Actor: "oomcap", Action: d.Action, From: d.From, To: d.To, Mode: modeName, Reason: msg})
	verb, tail := "shadow: "+msg+": ", " (not enforced; set URNETWORK_OOM_CAP=on to enforce)"
	if mode == oomCapOn {
		verb, tail = "applied: "+msg+": ", ""
	}
	return append(warnings, "[oomcap] "+verb+oomCapActionPhrase(d, mode == oomCapOn)+tail)
}

// oomCapActionPhrase words a decision for the log: "would reduce ..." in shadow,
// "reduced ..." when enforced. A frozen decision changes nothing, so it reads as
// holding the cap rather than as a move between two equal numbers.
func oomCapActionPhrase(d oomCapDecision, applied bool) string {
	from := "none"
	if d.From > 0 {
		from = strconv.Itoa(d.From)
	}
	verb := map[bool]map[string]string{
		false: {"reduce": "would reduce", "relax": "would relax", "clear": "would clear", "frozen": "would hold"},
		true:  {"reduce": "reduced", "relax": "relaxed", "clear": "cleared", "frozen": "held"},
	}[applied][d.Action]
	if verb == "" {
		verb = d.Action
	}
	if d.Action == "frozen" {
		return verb + " the automatic start cap at " + strconv.Itoa(d.To) + " (reduction limit reached)"
	}
	to := "none"
	if d.To > 0 {
		to = strconv.Itoa(d.To)
	}
	return verb + " the automatic start cap " + from + " -> " + to
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

// oomMarkerWithHeartbeat refreshes the marker's LastSeenUnix to now when it has
// never been set or oomHeartbeatInterval has passed since it was last
// refreshed, and reports whether it changed. Bounding the rate keeps this off
// the hot path of the 30s pressure tick that calls it.
func oomMarkerWithHeartbeat(m oomMarker, now time.Time) (oomMarker, bool) {
	if m.LastSeenUnix != 0 && now.Sub(time.Unix(m.LastSeenUnix, 0)) < oomHeartbeatInterval {
		return m, false
	}
	m.LastSeenUnix = now.Unix()
	return m, true
}

// oomCapUpdatePeak records a higher running count in this start's marker and
// refreshes its heartbeat at a bounded rate, so a long-running provider's
// marker is never judged stale by oomKilledSinceMarker just because it started
// more than oomMarkerMaxAge ago. Cheap: it only writes when the peak rises or
// the heartbeat is due.
//
// The heartbeat runs even with the kill switch off, and only the peak update is
// skipped. The heartbeat exists to prove "this process is still up", and
// oomKilledSinceMarker ages the marker against oomMarkerMaxAge. Gating the
// heartbeat on the mode meant a box left off for a maintenance weekend came
// back with a frozen marker, and the first real OOM kill after re-enabling was
// refused as stale: no reduction, no ledger entry, no log line. Refusing a kill
// is the one failure this whole mechanism must never make.
func oomCapUpdatePeak(running int, now time.Time) {
	off := oomCapMode() == oomCapOff
	dir, err := oomCapDir()
	if err != nil {
		return
	}
	path := filepath.Join(dir, "run.marker")
	var m oomMarker
	if !oomReadJSON(path, &m) {
		return
	}
	next := m
	var peakChanged bool
	if !off {
		next, peakChanged = oomMarkerWithPeak(next, running)
	}
	next, heartbeatChanged := oomMarkerWithHeartbeat(next, now)
	if peakChanged || heartbeatChanged {
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
