package urnettools

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
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
	_, hasDoas := exec.LookPath("doas")
	_, doasConfigErr := os.Stat("/etc/doas.conf")
	_, hasSudo := exec.LookPath("sudo")
	name, argv, err := openbsdRcctlArgv(os.Geteuid(), hasDoas == nil && doasConfigErr == nil, hasSudo == nil, args...)
	if err != nil {
		return nil, err
	}
	return bsdServiceRunFn(name, argv...)
}

// openbsdRequireRcScript returns an error unless an rc.d script exists for the
// service. rcctl enable would otherwise add a service that does not exist to
// pkg_scripts, and every boot would then report it as failed.
func openbsdRequireRcScript(p Provider, service string) error {
	if _, err := os.Stat(openbsdRcScriptPath(service)); err != nil {
		return fmt.Errorf("no rc.d script at %s for provider %s - install it first (the installer script)",
			openbsdRcScriptPath(service), providerLabel(p))
	}
	return nil
}

// bsdServiceControl runs `rcctl <action> <name>` for the provider's rc.d
// service. The name is shared with FreeBSD so the generic call sites in
// update, start, stop, restart and uninstall need no OpenBSD branch of their own.
func bsdServiceControl(p Provider, action string) error {
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
// stops and disables the rc.d service with rcctl. Each step is best-effort and
// only warns on failure, so uninstall continues.
func cleanupLifecycle(p Provider) {
	if err := removeBSDUpdateCron(p); err != nil {
		fmt.Fprintf(os.Stderr, "uninstall: could not remove auto-update cron entry: %v\n", err)
	}
	service := bsdRcServiceName(p.Binary)
	if _, err := os.Stat(openbsdRcScriptPath(service)); err != nil {
		return
	}
	for _, action := range []string{"stop", "disable"} {
		if out, err := openbsdRcctl(action, service); err != nil {
			fmt.Fprintf(os.Stderr, "uninstall: rcctl %s %s failed: %v (%s)\n",
				action, service, err, strings.TrimSpace(string(out)))
		}
	}
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
