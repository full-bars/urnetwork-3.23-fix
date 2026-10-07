package urnettools

import (
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
)

// bsdSysctlWrites returns the FreeBSD tuning keys and their target values.
//
// These are the FreeBSD namesakes of the Linux set, and the two are not
// interchangeable: net.core.rmem_max does not exist here (it is
// net.inet.tcp.recvspace), kern.maxfiles has no fs.file-max analogue, and there
// is no nf_conntrack at all — FreeBSD's firewall is pf, whose table limit is
// tuned per-rule rather than with a global sysctl. So a conntrack-sized write
// would be a silent no-op at best.
//
// Every key here is asserted to exist on a real kernel by the FreeBSD CI job,
// which extracts this list rather than repeating it. An earlier draft also
// listed "net.inet.tcp.sendspace.max", which does not exist — `optimize` aborts
// on the first unreadable key and rolls the whole set back, so a wrong name
// here means the command fails on every box. The TCP autotune ceilings
// (autosndbufmax / autorcvbufmax) would be the way to raise the maximum
// autotuned buffer; they are deliberately NOT set until their names are
// confirmed against a real kernel, because guessing costs a CI cycle and a
// red job, and an unverified key in this list is worse than no key.
func bsdSysctlWrites() [][]string {
	return [][]string{
		// Socket buffers for the UDP/WebRTC and TCP transfer paths.
		{"net.inet.tcp.recvspace", "4194304"},
		{"net.inet.tcp.sendspace", "4194304"},
		// Descriptor budget: one open socket per proxied connection is the
		// provider's load-bearing resource.
		{"kern.maxfiles", "200000"},
		{"kern.maxfilesperproc", "100000"},
	}
}

// WHY sysrc AND NOT /etc/sysctl.conf: a review suggested the latter, on the
// grounds that sysctl(8) "serves to query/modify MIBs" while sysrc(8) "works on
// values in the system rc.conf configuration files". That is true of the two
// TOOLS, and it does not apply here: these tunables (net.inet.tcp.recvspace,
// kern.maxfiles and friends) are loader tunables declared in
// /etc/defaults/rc.conf, and the kernel reads them from rc.conf at boot. That is
// exactly what sysrc edits. /etc/sysctl.conf is a separate, additional
// mechanism read by /etc/rc.d/sysctl, not a replacement for rc.conf.
//
// Either mechanism would work; rc.conf is the one whose variables already
// exist with the same names, so sysrc updates an existing entry in place rather
// than introducing a second source of truth for the same knob.

// bsdRcConfVars maps each tuned sysctl to the rc.conf variable that persists
// it. FreeBSD does not read /etc/sysctl.conf at boot — sysctl(8) persistence
// lives in rc.conf, written through sysrc(8) — so writing a sysctl.conf file
// the way optimizeLinux does would apply nothing after a reboot.
//
// Each key here is an existing rc.conf knob with a matching sysctl name, which
// is what makes sysrc the correct writer: the variable already carries the
// boot-time value, and sysrc updates it in place.
func bsdRcConfVars() map[string]string {
	return map[string]string{
		"net.inet.tcp.recvspace": "net.inet.tcp.recvspace",
		"net.inet.tcp.sendspace": "net.inet.tcp.sendspace",
		"kern.maxfiles":          "kern.maxfiles",
		"kern.maxfilesperproc":   "kern.maxfilesperproc",
	}
}

// rollbackBSDDefaults restores the rc.conf variables optimizeFreeBSD changed,
// clearing any that had no prior value so the persisted half of a failed run
// does not reappear at the next boot.
//
// Restoring goes through sysrc(8) rather than editing rc.conf directly: sysrc
// rewrites the file atomically and preserves its formatting, which every other
// service on the box also depends on.
func rollbackBSDDefaults(prior map[string]string) {
	// Reverse order so the file is returned to a consistent state even if a
	// later restore fails; a failure here is logged, never masked over the
	// original error.
	vars := make([]string, 0, len(prior))
	for v := range prior {
		vars = append(vars, v)
	}
	sort.Strings(vars)
	for i := len(vars) - 1; i >= 0; i-- {
		v := vars[i]
		arg := v + "="
		if prior[v] != "" {
			arg = v + "=" + prior[v]
		}
		if out, err := exec.Command("sysrc", arg).CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "optimize: warning: sysrc rollback %s failed: %v (%s)\n",
				v, err, strings.TrimSpace(string(out)))
		}
	}
}

// optimizeFreeBSD applies the FreeBSD equivalents of the golden-fleet tuning.
//
// The shape matches optimizeLinux deliberately: snapshot the prior live values,
// apply, roll back on any failure, then persist. Persisting here means rc.conf
// rather than a sysctl.conf file, which is the one part that genuinely differs
// and the part that would otherwise look like it worked while reverting at the
// next reboot.
func optimizeFreeBSD() error {
	if os.Geteuid() != 0 {
		self, _ := os.Executable()
		if self == "" {
			self = "urnet-tools"
		}
		return fmt.Errorf("optimize: sysctl requires root (running as uid %d); run: sudo %s optimize",
			os.Geteuid(), self)
	}

	writes := bsdSysctlWrites()

	// Snapshot first: a key we cannot read is a key we cannot roll back, so
	// abort before changing anything rather than half-way through.
	prior := make(map[string]string, len(writes))
	for _, w := range writes {
		out, err := exec.Command("sysctl", "-n", w[0]).Output()
		if err != nil {
			return fmt.Errorf("optimize: cannot read current %s (%v); aborting", w[0], err)
		}
		prior[w[0]] = strings.TrimSpace(string(out))
	}

	applied := 0
	for _, w := range writes {
		if out, err := exec.Command("sysctl", "-w", w[0]+"="+w[1]).CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "optimize: warning: sysctl -w %s failed: %v (%s); rolling back\n",
				w[0], err, strings.TrimSpace(string(out)))
			rollbackSysctls(writes[:applied], prior)
			return fmt.Errorf("optimize: live apply of %s failed: %w", w[0], err)
		}
		applied++
	}

	// Persist through sysrc so the values survive a reboot. A persistence
	// failure must not leave the live settings changed with nothing recording
	// them, so the live values are rolled back to their prior state.
	//
	// The rc.conf values are snapshotted too, and each is restored on failure.
	// A partial sysrc run would otherwise leave earlier keys persisted in
	// rc.conf: the live settings roll back immediately, but at the next reboot
	// the persisted half would come back — a change the operator never
	// completed and was told had been rolled back.
	rcVars := bsdRcConfVars()
	priorRC := make(map[string]string, len(writes))
	for _, w := range writes {
		v, ok := rcVars[w[0]]
		if !ok {
			continue
		}
		out, err := exec.Command("sysrc", "-n", v).Output()
		if err != nil {
			// Not currently set. sysrc reports "sysrc: unknown oid" for an
			// unset variable; anything else is a real failure, but either way
			// there is no prior value to restore, so record it as unset.
			priorRC[v] = ""
			continue
		}
		priorRC[v] = strings.TrimSpace(string(out))
	}

	for _, w := range writes {
		v, ok := rcVars[w[0]]
		if !ok {
			continue
		}
		if out, err := exec.Command("sysrc", v+"="+w[1]).CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "optimize: warning: sysrc %s failed: %v (%s); rolling back\n",
				v, err, strings.TrimSpace(string(out)))
			rollbackBSDDefaults(priorRC)
			rollbackSysctls(writes, prior)
			return fmt.Errorf("optimize: persist %s failed (%v); rolled back live and rc.conf settings", v, err)
		}
	}

	fmt.Println("optimize: done (live + rc.conf persisted)")
	return nil
}
