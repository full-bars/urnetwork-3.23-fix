//go:build linux

package urnettools

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// This file is the OpenRC lifecycle backend: on hosts where OpenRC is the
// running init system (Alpine and friends) and systemd is not available,
// lifecycle commands route through rc-service / rc-update, and the
// auto-update schedule is a busybox crond periodic entry instead of the
// systemd timer. The systemd path (lifecycle_unix.go) is untouched: the
// backend is selected by RUNTIME DETECTION, never by GOOS — OpenRC is still
// linux, so only the running init system distinguishes the two.
//
// Explicit limit (matches issue #786): there is no zero-downtime HotSwap
// under OpenRC. supervise-daemon has no sd_notify MainPID handoff, so an
// in-process baton handoff would orphan the successor while supervise-daemon
// respawns a second provider. Updates therefore decline HotSwap cleanly
// (openrcHotSwapDecline) and restart the service stop/start.

const (
	// openrcServiceName is the OpenRC service name. It matches the init
	// script name the installer writes (/etc/init.d/urnetwork), so
	// `rc-service urnetwork ...` addresses exactly the supervised provider.
	openrcServiceName = "urnetwork"

	// openrcUpdateCronName is the periodic entry that runs the auto-update.
	openrcUpdateCronName = "urnetwork-update"
)

var (
	// openrcInitScriptPath marks the service as installed. A var so tests
	// can point it at a temp file.
	openrcInitScriptPath = "/etc/init.d/" + openrcServiceName
	// openrcPeriodicBaseDir is busybox crond's periodic queue root. The
	// auto-update entry is dropped into its daily/weekly/monthly subdirs;
	// Alpine's stock /etc/crontabs/root runs those dirs via run-parts.
	// A var so tests never touch the real /etc.
	openrcPeriodicBaseDir = "/etc/periodic"

	// Detection and execution seams (tests override).
	openrcProbeFn      = probeOpenRC
	openrcLookPathFn   = exec.LookPath
	openrcStatFn       = os.Stat
	systemdRunningFn   = systemdRunning
	openrcRunFn        = runOpenRCCommand
	openrcToolPathFn   = openrcToolPath
	openrcExecutableFn = os.Executable
	openrcCommandRunFn = runOpenRCCommand // used for the crond status probe

	// providerSupervisedByOpenRCFn reports whether a provider process is the
	// direct child of supervise-daemon. Seam for tests.
	providerSupervisedByOpenRCFn = providerSupervisedByOpenRC
)

// systemdRunning reports whether systemd is the running init system. The
// standard marker is /run/systemd/system, which systemd creates at boot; the
// mere presence of a systemctl binary is not enough (containers and
// non-systemd hosts ship stubs).
func systemdRunning() bool {
	_, err := openrcStatFn("/run/systemd/system")
	return err == nil
}

// openrcPathExists is the stat probe used by the detection helpers.
func openrcPathExists(path string) bool {
	_, err := openrcStatFn(path)
	return err == nil
}

// probeOpenRC reports whether this host has an operational OpenRC. rc-service
// must be resolvable (it is the command every lifecycle operation runs),
// corroborated by at least one OpenRC runtime marker: /run/openrc (present on
// any booted OpenRC host) or the openrc-run runner binary.
func probeOpenRC() bool {
	haveRcService := false
	if _, err := openrcLookPathFn("rc-service"); err == nil {
		haveRcService = true
	}
	if !haveRcService {
		for _, p := range []string{"/sbin/rc-service", "/usr/sbin/rc-service"} {
			if openrcPathExists(p) {
				haveRcService = true
				break
			}
		}
	}
	if !haveRcService {
		return false
	}
	for _, p := range []string{"/run/openrc", "/sbin/openrc-run", "/usr/sbin/openrc-run"} {
		if openrcPathExists(p) {
			return true
		}
	}
	return false
}

// openrcActive reports whether lifecycle commands should route through the
// OpenRC backend. Systemd wins whenever it is the RUNNING init: a host where
// systemd is up keeps every existing systemd behavior byte-for-byte, even if
// an OpenRC package happens to be installed alongside.
func openrcActive() bool {
	if !openrcProbeFn() {
		return false
	}
	return !systemdRunningFn()
}

// openrcServiceInstalled reports whether the OpenRC service exists (its init
// script is on disk). Without it there is nothing to start/stop/autostart and
// callers fall back to the unitless/bare-process paths.
func openrcServiceInstalled() bool {
	return openrcPathExists(openrcInitScriptPath)
}

// openrcToolPath resolves an OpenRC tool (rc-service, rc-update) to an
// absolute path: PATH first, then the standard sbin locations. A provider
// user's login PATH routinely lacks /sbin, but the tool must still find
// rc-service to manage the system service.
func openrcToolPath(name string) string {
	if p, err := openrcLookPathFn(name); err == nil {
		return p
	}
	for _, dir := range []string{"/sbin", "/usr/sbin"} {
		p := filepath.Join(dir, name)
		if openrcPathExists(p) {
			return p
		}
	}
	return name
}

// runOpenRCCommand runs an OpenRC tool with stdio passthrough so the operator
// sees its output (including rc-service's "already started" / "already
// stopped" notices).
func runOpenRCCommand(args ...string) error {
	if len(args) == 0 {
		return errors.New("empty OpenRC command")
	}
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	return cmd.Run()
}

// openrcServiceArgv is the rc-service argv for an action on the service.
func openrcServiceArgv(action string) []string {
	return []string{openrcToolPathFn("rc-service"), openrcServiceName, action}
}

// openrcUpdateArgv is the rc-update argv for adding/removing the service.
// The runlevel is ALWAYS named explicitly: `rc-update del <service>` without
// one targets sysinit, not default, and reports "not in the runlevel".
func openrcUpdateArgv(action string) []string {
	return []string{openrcToolPathFn("rc-update"), action, openrcServiceName, "default"}
}

// openrcPeriodicIntervals are the busybox crond queues the auto-update entry
// can live in, mapped from the interval names the CLI already accepts.
var openrcPeriodicIntervals = []string{"daily", "weekly", "monthly"}

// openrcValidInterval reports whether interval names a periodic queue.
func openrcValidInterval(interval string) bool {
	for _, d := range openrcPeriodicIntervals {
		if d == interval {
			return true
		}
	}
	return false
}

// openrcTargetApplies reports whether a parsed target names the OpenRC
// service (or names nothing at all). Any other explicit unit is a different
// provider and must not be hijacked by the OpenRC backend; --user /
// --state-dir / --network selectors are accepted because on an OpenRC host
// the service is the only supervised provider deployment.
func openrcTargetApplies(t Target) bool {
	return t.Unit == "" || t.Unit == openrcServiceName
}

// openrcGateProvider is the provider record used for the restart confirm gate
// and the restart-reason marker on an OpenRC host: the service name, enriched
// best-effort with the sole discovered provider so the audit line shows the
// real user/state when they are unambiguous.
func openrcGateProvider() Provider {
	p := Provider{Unit: openrcServiceName}
	if provs := discoverSystemdFn(); len(provs) == 1 {
		p.User = provs[0].User
		p.StateDir = provs[0].StateDir
		p.StateHome = provs[0].StateHome
		p.Network = provs[0].Network
		p.PID = provs[0].PID
	}
	return p
}

// openrcRouteLifecycle intercepts start/stop/restart on OpenRC hosts. handled
// is false when the OpenRC backend does not apply and the caller must run its
// normal systemd/unitless path (a different explicit unit, leftover
// positionals — which the normal path refuses with its precise message — or
// no OpenRC service installed).
func openrcRouteLifecycle(verb string, args []string, force, dryRun bool) (bool, error) {
	if !openrcActive() || !openrcServiceInstalled() {
		return false, nil
	}
	t, rest, err := parseTargetFlags(args)
	if err != nil {
		return false, nil // the normal path reports the parse error identically
	}
	if len(rest) > 0 {
		return false, nil
	}
	if !openrcTargetApplies(t) {
		return false, nil
	}
	if dryRun {
		fmt.Printf("[dry-run] would %s %s (OpenRC service)\n", verb, openrcServiceName)
		return true, nil
	}
	if verb == "restart" {
		gateProvider := openrcGateProvider()
		ok, err := confirmGate("restart "+openrcServiceName+" (OpenRC service)", gateProvider, force, dryRun)
		if err != nil {
			return true, err
		}
		if !ok {
			return true, nil
		}
		// Leave the reason for the provider's next start (best effort).
		recordRestartReason(gateProvider, restartReasonManual)
	}
	fmt.Printf("running rc-service %s %s...\n", openrcServiceName, verb)
	if err := openrcRunFn(openrcServiceArgv(verb)...); err != nil {
		return true, fmt.Errorf("rc-service %s %s: %w", openrcServiceName, verb, err)
	}
	fmt.Printf("%s %s (OpenRC service)\n", openrcVerbPast(verb), openrcServiceName)
	return true, nil
}

// openrcVerbPast returns the past-tense form for the lifecycle messages.
func openrcVerbPast(verb string) string {
	switch verb {
	case "start":
		return "started"
	case "stop":
		return "stopped"
	case "restart":
		return "restarted"
	}
	return verb
}

// openrcRouteAutoStart intercepts `auto-start on|off` on OpenRC hosts:
// rc-update add|del <service> default.
func openrcRouteAutoStart(mode string, rest []string, force, dryRun bool) (bool, error) {
	if !openrcActive() || !openrcServiceInstalled() {
		return false, nil
	}
	t, _, err := parseTargetFlags(rest)
	if err != nil {
		return false, nil
	}
	if !openrcTargetApplies(t) {
		return false, nil
	}
	if dryRun {
		fmt.Printf("[dry-run] would set auto-start %s for %s (OpenRC service)\n", mode, openrcServiceName)
		return true, nil
	}
	action := "add"
	if mode == "off" {
		action = "del"
	}
	if err := openrcRunFn(openrcUpdateArgv(action)...); err != nil {
		return true, fmt.Errorf("rc-update %s %s: %w", action, openrcServiceName, err)
	}
	return true, nil
}

// openrcRouteAutoUpdate intercepts `auto-update daily|weekly|monthly|off` on
// OpenRC hosts: a busybox crond periodic entry replaces the systemd timer.
// Unlike auto-start it does not require the init script — the entry is a
// plain cron drop-in — but it runs the SAME urnet-tools binary the operator
// uses, resolved from the provider's install dir when possible.
func openrcRouteAutoUpdate(interval string, rest []string, dryRun bool) (bool, error) {
	if !openrcActive() {
		return false, nil
	}
	t, _, err := parseTargetFlags(rest)
	if err != nil {
		return false, nil
	}
	if !openrcTargetApplies(t) {
		return false, nil
	}
	if dryRun {
		fmt.Printf("[dry-run] would set auto-update %s for %s (busybox crond periodic entry)\n", interval, openrcServiceName)
		return true, nil
	}
	if err := openrcSetAutoUpdate(interval, openrcUpdateToolPath(openrcGateProvider())); err != nil {
		return true, err
	}
	if interval != "off" {
		if note := openrcCrondNote(); note != "" {
			fmt.Fprintf(os.Stderr, "note: %s\n", note)
		}
	}
	return true, nil
}

// openrcUpdateToolPath resolves the urnet-tools binary the auto-update cron
// entry must run. Preference order: the tool beside the provider binary (the
// canonical install layout the shell installer creates), then this running
// binary, then PATH.
func openrcUpdateToolPath(p Provider) string {
	if p.Binary != "" {
		cand := filepath.Join(filepath.Dir(p.Binary), "urnet-tools")
		if fi, err := openrcStatFn(cand); err == nil && !fi.IsDir() {
			return cand
		}
	}
	if self, err := openrcExecutableFn(); err == nil && self != "" {
		return self
	}
	if p, err := openrcLookPathFn("urnet-tools"); err == nil {
		return p
	}
	return "urnet-tools"
}

// openrcSetAutoUpdate writes or removes the busybox crond periodic entry that
// runs `urnet-tools update -f` at the requested interval.
//
// Why /etc/periodic/<interval>/ and not a line in /etc/crontabs/root:
// Alpine's stock /etc/crontabs/root already contains the run-parts lines for
// /etc/periodic/{daily,weekly,monthly} (verified on alpine:latest), so a
// drop-in file is idempotent, needs no crontab parsing or rewriting, cannot
// race or corrupt an operator-managed crontab, and is removed with a single
// rm on `off`. The interval maps directly onto the periodic queue. crond
// itself must be running for the entry to fire; openrcCrondNote reports when
// it is not.
func openrcSetAutoUpdate(interval, toolPath string) error {
	if interval == "off" {
		for _, dir := range openrcPeriodicIntervals {
			path := filepath.Join(openrcPeriodicBaseDir, dir, openrcUpdateCronName)
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("auto-update off: remove %s: %w", path, err)
			}
		}
		return nil
	}
	if !openrcValidInterval(interval) {
		return fmt.Errorf("invalid interval %q: daily|weekly|monthly|off", interval)
	}
	// Move semantics: clear the other queues so an interval change never
	// leaves two entries firing.
	for _, dir := range openrcPeriodicIntervals {
		if dir == interval {
			continue
		}
		path := filepath.Join(openrcPeriodicBaseDir, dir, openrcUpdateCronName)
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("auto-update %s: remove stale %s: %w", interval, path, err)
		}
	}
	dir := filepath.Join(openrcPeriodicBaseDir, interval)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("auto-update: create %s: %w", dir, err)
	}
	path := filepath.Join(dir, openrcUpdateCronName)
	// run-parts executes only regular executable files; writeFileAtomic
	// applies the mode through the held fd (temp + rename), so a crash never
	// leaves a half-written entry.
	if err := writeFileAtomic(path, []byte(openrcUpdateCronScript(toolPath)), 0o755); err != nil {
		return fmt.Errorf("auto-update: write %s: %w", path, err)
	}
	return nil
}

// openrcUpdateCronScript is the periodic entry's content. `update -f` is the
// unattended form: no confirmation prompts, and a failed restart surfaces in
// the cron output.
func openrcUpdateCronScript(toolPath string) string {
	return "#!/bin/sh\n" +
		"# URnetwork provider auto-update — installed by `urnet-tools auto-update`.\n" +
		"# Runs from busybox crond's periodic queue (see /etc/crontabs/root).\n" +
		"# Zero-downtime HotSwap is unavailable under OpenRC (no sd_notify):\n" +
		"# the update performs a stop/start service restart.\n" +
		"exec \"" + toolPath + "\" update -f\n"
}

// openrcCrondNote returns an actionable note when the periodic entry would
// never fire, so `auto-update on` does not silently schedule into a queue
// nothing runs. Empty when crond has an OpenRC service that reports started.
func openrcCrondNote() string {
	crondInit := "/etc/init.d/crond"
	if !openrcPathExists(crondInit) {
		return "the crond OpenRC service is not installed (on Alpine: apk add busybox-openrc); the auto-update entry will not fire until crond runs"
	}
	if err := openrcCommandRunFn(openrcToolPathFn("rc-service"), "crond", "status"); err != nil {
		return "crond is not started; enable it so the auto-update entry fires: rc-update add crond default && rc-service crond start"
	}
	return ""
}

// openrcCleanup removes the OpenRC lifecycle artifacts for an uninstall: the
// service is stopped and de-registered, the init script is removed, and the
// periodic auto-update entry is cleared. Every step is best-effort with a
// warning: uninstall must not abort halfway on a permission error, and the
// caller may not be root (the system service needs root to remove).
func openrcCleanup() {
	if err := openrcRunFn(openrcServiceArgv("stop")...); err != nil {
		fmt.Fprintf(os.Stderr, "uninstall: warning: rc-service %s stop: %v\n", openrcServiceName, err)
	}
	if err := openrcRunFn(openrcUpdateArgv("del")...); err != nil {
		fmt.Fprintf(os.Stderr, "uninstall: warning: rc-update del %s: %v\n", openrcServiceName, err)
	}
	if err := os.Remove(openrcInitScriptPath); err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "uninstall: warning: could not remove %s: %v (run: sudo rm -f %s)\n", openrcInitScriptPath, err, openrcInitScriptPath)
	}
	for _, dir := range openrcPeriodicIntervals {
		path := filepath.Join(openrcPeriodicBaseDir, dir, openrcUpdateCronName)
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "uninstall: warning: could not remove %s: %v (run: sudo rm -f %s)\n", path, err, path)
		}
	}
}

// providerSupervisedByOpenRC reports whether the provider process is the
// direct child of OpenRC's supervise-daemon, i.e. the process the urnetwork
// service supervises. The check reads the kernel's parent pid
// (/proc/<pid>/stat field 4) and the parent's comm — the kernel truncates
// comm to 15 bytes, so "supervise-daemon" is stored as "supervise-daemo".
// Nothing in the process's own environment or argv is trusted.
func providerSupervisedByOpenRC(p Provider) bool {
	if p.PID <= 0 {
		return false
	}
	ppid := parentPID(p.PID)
	if ppid <= 1 {
		return false
	}
	comm, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", ppid))
	if err != nil {
		return false
	}
	return isSuperviseDaemonComm(strings.TrimSpace(string(comm)))
}

// isSuperviseDaemonComm matches the kernel's comm for supervise-daemon,
// including its 15-byte truncation.
func isSuperviseDaemonComm(comm string) bool {
	return comm != "" && strings.HasPrefix("supervise-daemon", comm)
}

// parentPID returns the parent pid from /proc/<pid>/stat. comm may contain
// spaces and parentheses, so only the fields after the LAST ')' are parsed
// (state ppid pgrp ...).
func parentPID(pid int) int {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0
	}
	fields := strings.Fields(s[i+1:])
	if len(fields) < 2 {
		return 0
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0
	}
	return ppid
}

// openrcRestartService restarts the OpenRC service when the running provider
// is the process supervise-daemon supervises. handled=false otherwise: a
// systemd-unit provider is never routed through OpenRC, and a bare process
// (manual launch) is not the service's — restarting the service would not
// restart it, so the caller's normal path (with its actionable error) runs.
func openrcRestartService(p Provider) (bool, error) {
	if !openrcActive() || !openrcServiceInstalled() {
		return false, nil
	}
	if p.Unit != "" {
		return false, nil
	}
	if !providerSupervisedByOpenRCFn(p) {
		return false, nil
	}
	fmt.Printf("restarting %s (OpenRC service, pid %d)...\n", openrcServiceName, p.PID)
	if err := openrcRunFn(openrcServiceArgv("restart")...); err != nil {
		return true, fmt.Errorf("rc-service %s restart: %w", openrcServiceName, err)
	}
	fmt.Printf("restarted %s (OpenRC service)\n", openrcServiceName)
	return true, nil
}

// openrcHotSwapDecline returns a clean decline when the running provider is
// supervised by OpenRC's supervise-daemon. An in-process baton handoff has no
// way to hand the supervised MainPID over (no sd_notify), so the successor
// would be orphaned from supervise-daemon, which would then respawn a SECOND
// provider. Declining makes the update fall back to a stop/start service
// restart, the documented OpenRC trade.
func openrcHotSwapDecline(p Provider) error {
	if !providerSupervisedByOpenRCFn(p) {
		return nil
	}
	return ErrHotSwapOpenRC
}

// renderOpenRCStatus prints the service's supervisor view for `urnet-tools
// status` on an OpenRC host; the caller then falls through to the table,
// which carries the live control-socket view. rc-service status exits 3 for
// a stopped service (0 for started) — a normal state, not a failure — so the
// exit code is deliberately ignored and the command's own output is the view.
func renderOpenRCStatus() error {
	_ = openrcRunFn(openrcServiceArgv("status")...)
	return nil
}
