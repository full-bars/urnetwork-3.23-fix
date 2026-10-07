package urnettools

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
// on the first unreadable key and rolls the whole set back, so a wrong name here
// means the command fails on every box. The TCP autotune ceilings
// (autosndbufmax / autorcvbufmax) would be the way to raise the maximum
// autotuned buffer; they are deliberately NOT set until their names are
// confirmed against a real kernel, because guessing costs a CI cycle and a red
// job, and an unverified key in this list is worse than no key.
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

// WHY /etc/sysctl.conf AND NOT sysrc(8): FreeBSD persists runtime sysctl
// tunables in /etc/sysctl.conf, which /etc/rc.d/sysctl applies at boot in
// multi-user mode (sysctl.conf(5)). sysrc(8) is NOT an alternative — it edits
// rc.conf(5), whose variables are SHELL variable names, and a dotted sysctl MIB
// is not a valid shell assignment. Writing `net.inet.tcp.recvspace="..."` into
// rc.conf produces a line no boot script can consume, so the live setting would
// revert at the next reboot. An earlier version persisted through sysrc on
// exactly that reasoning and persisted nothing while reporting success.
//
// kern.maxfiles and kern.maxfilesperproc are also settable at runtime and so
// belong in the same file; /boot/loader.conf is only needed when a value must
// be in place before any rc script runs.

// bsdSysctlConfPath is the file /etc/rc.d/sysctl reads at boot.
const bsdSysctlConfPath = "/etc/sysctl.conf"

// Markers delimiting the block this tool owns. Everything between them is
// rewritten on each run; lines outside it are never touched.
const (
	bsdConfBeginMarker = "# --- urnet-tools optimize (managed block) ---"
	bsdConfEndMarker   = "# --- end urnet-tools optimize ---"
)

// renderBSDConfLine returns the sysctl.conf line for one key. sysctl.conf uses
// the sysctl(8) syntax `mib=value` and treats '#' as a comment introducer, so a
// key carrying one could be silently truncated to a comment.
func renderBSDConfLine(key, value string) string {
	return key + "=" + value
}

// upsertBSDConfLines returns conf with every managed key set to its target
// value. A key already present is REPLACED in place, preserving the file's
// other content and comment order; a key absent is appended under a single
// marked block.
//
// Pure, so the file rewriting is testable without root. A value is rejected
// rather than written if it is not a plain decimal number, because sysctl.conf
// is sourced by the boot scripts and anything else is at best a syntax error.
func upsertBSDConfLines(conf string, writes [][]string) (string, error) {
	lines := strings.Split(conf, "\n")

	for _, w := range writes {
		key, value := w[0], w[1]
		if err := validateBSDConfValue(key, value); err != nil {
			return "", err
		}
		want := renderBSDConfLine(key, value)

		// Replace an existing assignment to this key, wherever it lives.
		replaced := false
		for i, l := range lines {
			trimmed := strings.TrimSpace(l)
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}
			if k, _, ok := strings.Cut(trimmed, "="); ok && strings.TrimSpace(k) == key {
				lines[i] = want
				replaced = true
				break
			}
		}
		if replaced {
			continue
		}

		// Otherwise append into the managed block, creating it on first use.
		if begin := indexOfLine(lines, bsdConfBeginMarker); begin >= 0 {
			end := indexOfLine(lines, bsdConfEndMarker)
			if end < begin {
				return "", fmt.Errorf("sysctl.conf has a begin marker with no end marker; refusing to edit")
			}
			block := make([]string, 0, end-begin+1)
			block = append(block, lines[begin+1:end]...)
			block = append(block, want)
			rest := append([]string{}, lines[end:]...)
			lines = append(append(append([]string{}, lines[:begin+1]...), block...), rest...)
			continue
		}

		// No managed block yet: create one, keeping a trailing newline tidy.
		if n := len(lines); n > 0 && lines[n-1] == "" {
			lines[n-1] = bsdConfBeginMarker
			lines = append(lines, want, bsdConfEndMarker, "")
		} else {
			lines = append(lines, "", bsdConfBeginMarker, want, bsdConfEndMarker)
		}
	}
	return strings.Join(lines, "\n"), nil
}

// indexOfLine returns the index of the first line equal to want, or -1.
func indexOfLine(lines []string, want string) int {
	for i, l := range lines {
		if strings.TrimSpace(l) == want {
			return i
		}
	}
	return -1
}

// validateBSDConfValue rejects a value that is not a plain decimal number.
// sysctl.conf is consumed by shell at boot; a value carrying a quote, a space,
// a newline or a '#' would either be a syntax error or truncate the line into a
// comment, and either way the boot value would not be the one asked for.
func validateBSDConfValue(key, value string) error {
	if value == "" {
		return fmt.Errorf("refusing to persist an empty value for %s", key)
	}
	for _, c := range value {
		if c < '0' || c > '9' {
			return fmt.Errorf("refusing to persist non-numeric value %q for %s", value, key)
		}
	}
	return nil
}

// optimizeFreeBSD applies the FreeBSD equivalents of the golden-fleet tuning.
//
// The shape matches optimizeLinux deliberately: snapshot the prior live values,
// apply, roll back on any failure, then persist. Persisting writes
// /etc/sysctl.conf through a temp file and a rename, so an interrupted run cannot
// leave a half-written boot file.
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

	// Snapshot the boot file too, so a persist failure restores it exactly. The
	// bytes are the source of truth, not a re-render: re-rendering would lose
	// entries this tool does not manage.
	confBefore, confExisted, err := readBSDConf()
	if err != nil {
		return fmt.Errorf("optimize: cannot read %s (%v); aborting", bsdSysctlConfPath, err)
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

	newConf, err := upsertBSDConfLines(confBefore, writes)
	if err != nil {
		// The file is untouched at this point, so only the live values need
		// reverting — but they must be, or the box runs tuned until reboot.
		fmt.Fprintf(os.Stderr, "optimize: warning: rendering %s failed: %v; rolling back\n",
			bsdSysctlConfPath, err)
		rollbackSysctls(writes, prior)
		return fmt.Errorf("optimize: persist %s failed (%v); rolled back live settings",
			bsdSysctlConfPath, err)
	}
	if err := writeBSDConf(newConf, confExisted); err != nil {
		fmt.Fprintf(os.Stderr, "optimize: warning: writing %s failed: %v; rolling back\n",
			bsdSysctlConfPath, err)
		rollbackBSDConf(confBefore, confExisted)
		rollbackSysctls(writes, prior)
		return fmt.Errorf("optimize: persist %s failed (%v); rolled back live and boot settings",
			bsdSysctlConfPath, err)
	}

	fmt.Println("optimize: done (live + " + bsdSysctlConfPath + " persisted)")
	return nil
}

// readBSDConf returns the current contents of the boot file and whether it
// existed. A missing file is not an error: it will be created.
func readBSDConf() (content string, existed bool, err error) {
	data, err := os.ReadFile(bsdSysctlConfPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, err
	}
	return string(data), true, nil
}

// writeBSDConf replaces the boot file atomically, preserving its existing mode
// when there is one. The temp file is created in the same directory so the
// rename stays on one filesystem and is therefore atomic.
func writeBSDConf(content string, existed bool) error {
	mode := os.FileMode(0o644)
	if existed {
		if fi, err := os.Stat(bsdSysctlConfPath); err == nil {
			mode = fi.Mode().Perm()
		}
	}
	dir := filepath.Dir(bsdSysctlConfPath)
	tmp, err := os.CreateTemp(dir, ".sysctl.conf.urnet")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)

	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, bsdSysctlConfPath)
}

// rollbackBSDConf restores the exact prior bytes of the boot file, or removes it
// if it did not exist before. Leaving behind a file this tool created and then
// failed to write would change the box's boot behaviour even though the command
// reported a rollback.
func rollbackBSDConf(content string, existed bool) {
	if !existed {
		if err := os.Remove(bsdSysctlConfPath); err != nil && !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "optimize: warning: removing %s failed: %v\n",
				bsdSysctlConfPath, err)
		}
		return
	}
	if err := writeBSDConf(content, true); err != nil {
		fmt.Fprintf(os.Stderr, "optimize: warning: restoring %s failed: %v\n",
			bsdSysctlConfPath, err)
	}
}
