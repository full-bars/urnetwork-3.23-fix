package urnettools

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// OpenBSD lifecycle: an rc.d(8) script in /etc/rc.d driven by rcctl(8), plus
// the cron-driven auto-update shared with FreeBSD (lifecycle_bsd_cron_unix.go).
//
// rcctl is the OpenBSD analogue of systemctl. `rcctl enable <svc>` adds a
// non-base service to pkg_scripts in /etc/rc.conf.local, which is what
// setAutoStart toggles. FreeBSD's sysrc and `service` commands do not exist.

// bsdServiceRunFn runs a service-control command. Package var so tests can
// intercept the invocation without a live rc.d.
var bsdServiceRunFn = func(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).CombinedOutput()
}

// openbsdRcctl runs `rcctl <args...>`, escalating through doas or sudo when the
// caller is not root, and returns the command output.
func openbsdRcctl(args ...string) ([]byte, error) {
	_, doasErr := exec.LookPath("doas")
	_, sudoErr := exec.LookPath("sudo")
	name, argv, err := openbsdRcctlArgv(os.Geteuid(), doasErr == nil, sudoErr == nil, args...)
	if err != nil {
		return nil, err
	}
	return bsdServiceRunFn(name, argv...)
}

// openbsdRequireRcScript returns an error unless an rc.d script exists for the
// service and runs the provider's own binary. rcctl enable would otherwise add
// a service that does not exist to pkg_scripts, and every boot would then
// report it as failed. A script for another binary must never be driven on
// behalf of this provider.
func openbsdRequireRcScript(p Provider, service string) error {
	if err := openbsdCheckScript(openbsdRcScriptPath(service), p.Binary); err != nil {
		return fmt.Errorf("rc.d script %s unusable for provider %s: %w - install it first (the installer script)",
			openbsdRcScriptPath(service), providerLabel(p), err)
	}
	return nil
}

var openbsdServicePID = func() int {
	data, err := os.ReadFile("/var/run/rc.d/urnetwork")
	if err != nil {
		return 0
	}
	return openbsdManagedPID(string(data), func(table, pattern string) ([]byte, error) {
		return execWithTimeout(5*time.Second, "pgrep", "-T", table, "-xf", pattern)
	})
}

func openbsdRequireManaged(p Provider) error {
	if p.Supervisor != "openbsd-rcd" || p.PID <= 0 || openbsdServicePID() != p.PID {
		return fmt.Errorf("%s is not the uniquely matched rc.d-managed provider; refusing service action", providerLabel(p))
	}
	return nil
}

// bsdServiceControl runs `rcctl <action> <name>` for the provider's rc.d
// service. The name is shared with FreeBSD so the generic call sites in
// update, start, stop, restart and uninstall need no OpenBSD branch of their own.
func bsdServiceControl(p Provider, action string) error {
	if p.Binary == "" {
		return fmt.Errorf("%s has no provider binary; refusing service action", providerLabel(p))
	}
	if err := openbsdRequireManaged(p); err != nil {
		return err
	}
	service := bsdRcServiceName(p.Binary)
	if err := openbsdRequireRcScript(p, service); err != nil {
		return err
	}
	out, err := openbsdRcctl(action, service)
	if err != nil {
		return fmt.Errorf("rcctl %s %s: %w (%s)", action, service, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// setAutoStart enables or disables boot-time start with rcctl.
func setAutoStart(p Provider, on bool) error {
	service := bsdRcServiceName(p.Binary)
	if err := openbsdRequireRcScript(p, service); err != nil {
		return err
	}
	action := "disable"
	if on {
		action = "enable"
	}
	if out, err := openbsdRcctl(action, service); err != nil {
		return fmt.Errorf("auto-start %s: rcctl %s %s: %w (%s)",
			providerLabel(p), action, service, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// cleanupLifecycle removes the invoking user's auto-update cron entry, then
// disables the rc.d service with rcctl. Each step is best-effort and
// only warns on failure, so uninstall continues.
//
// Uninstall has already stopped the provider by the time this runs, so the
// live-PID check of bsdServiceControl cannot be repeated: the process is gone.
// The identity was proven before the stop and is carried by p.Supervisor. Here
// the rc.d script must also still serve the provider's own binary.
func cleanupLifecycle(p Provider) {
	if err := removeBSDUpdateCron(p); err != nil {
		fmt.Fprintf(os.Stderr, "uninstall: could not remove auto-update cron entry: %v\n", err)
	}
	if err := openbsdCleanupService(p); err != nil {
		fmt.Fprintf(os.Stderr, "uninstall: service was not disabled: %v; check rcctl configuration manually\n", err)
	}
}

// openbsdCleanupService disables the rc.d service of a provider that
// was proven rc.d-managed at discovery. It returns the first error and skips a
// provider that is not rc.d-managed, so a lookalike process never causes the
// real service to be switched off.
func openbsdCleanupService(p Provider) error {
	if p.Binary == "" || p.Supervisor != "openbsd-rcd" {
		return fmt.Errorf("%s is not the uniquely matched rc.d-managed provider", providerLabel(p))
	}
	service := bsdRcServiceName(p.Binary)
	if _, err := os.Stat(openbsdRcScriptPath(service)); err != nil {
		// No script means nothing was ever enabled: nothing to clean up.
		return nil
	}
	if err := openbsdRequireRcScript(p, service); err != nil {
		return err
	}
	// Uninstall proved the process exited before this runs, so only disable.
	if out, err := openbsdRcctl("disable", service); err != nil {
		return fmt.Errorf("rcctl disable %s: %w (%s)", service, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// renderOpenBSDStatus runs `rcctl check <name>`, the OpenBSD equivalent of
// `systemctl status <unit>`. It is installed into the shared seam so `status`
// prefers the real service view and falls back to the panel only when this
// returns an error. check needs no privilege.
func renderOpenBSDStatus(p Provider) error {
	service := bsdRcServiceName(p.Binary)
	if err := openbsdRequireRcScript(p, service); err != nil {
		return err
	}
	cmd := exec.Command("rcctl", "check", service)
	cmd.Stdout = stdoutWriter()
	cmd.Stderr = stderrWriter()
	return cmd.Run()
}

func init() {
	renderSystemctlStatus = renderOpenBSDStatus
}
