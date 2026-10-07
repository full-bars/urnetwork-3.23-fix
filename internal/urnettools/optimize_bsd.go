package urnettools

import (
	"fmt"
	"os"
	"os/exec"
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
	rcVars := bsdRcConfVars()
	for i, w := range writes {
		key := w[0]
		v, ok := rcVars[key]
		if !ok {
			continue
		}
		if out, err := exec.Command("sysrc", v+"="+w[1]).CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "optimize: warning: sysrc %s failed: %v (%s); rolling back\n",
				v, err, strings.TrimSpace(string(out)))
			rollbackSysctls(writes[:i+1], prior)
			return fmt.Errorf("optimize: persist %s failed (%v); rolled back live settings", v, err)
		}
	}

	fmt.Println("optimize: done (live + rc.conf persisted)")
	return nil
}
