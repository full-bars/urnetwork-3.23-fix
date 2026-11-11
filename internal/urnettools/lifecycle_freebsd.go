//go:build freebsd

package urnettools

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// FreeBSD lifecycle: an rc.d(8) service script plus a periodic auto-update
// driven from cron(8).
//
// rc.d is the analogue of a systemd service unit, and it is the right shape to
// target: `service urnetwork start|stop|status` is what a FreeBSD operator
// expects, and the rc subsystem keeps the enabled flag in /etc/rc.conf, which is
// what setAutoStart toggles through sysrc(8).
//
// The auto-update scheduler has no timer equivalent. systemd supplies
// OnCalendar timers; FreeBSD supplies cron, so a schedule is a crontab line.
// That is weaker in one specific way worth stating: cron has no missed-run
// catch-up, so a box that was down at the scheduled minute does not update on
// the next boot the way a systemd timer would have.

// bsdServiceRunFn runs a service-control command. Package var so tests can
// intercept the invocation without a live rc.d.
var bsdServiceRunFn = func(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).CombinedOutput()
}

// bsdRequireRcScript returns an error unless an rc.d script exists for the
// service. Without this check, sysrc happily writes `<name>_enable=YES` for a
// service that does not exist and reports success, and `service <name> start`
// fails with a bare "not found" that reads like a transient problem.
func bsdRequireRcScript(p Provider, service string) error {
	if _, err := os.Stat(bsdRcScriptPath(service)); err != nil {
		return fmt.Errorf("no rc.d script at %s for provider %s — install it first (urnet-tools install-service, or the installer script)",
			bsdRcScriptPath(service), providerLabel(p))
	}
	return nil
}

// bsdServiceControl runs `service <name> <action>` for the provider's rc.d
// service, escalating through sudo when not already root.
//
// rc.d start/stop need root (the script's own mkdir/chown/daemon -u require
// it), so a non-root invocation that silently fails would look like success.
// Output is returned in the error so a failure is diagnosable.
func bsdServiceControl(p Provider, action string) error {
	service := bsdRcServiceName(p.Binary)
	if err := bsdRequireRcScript(p, service); err != nil {
		return err
	}
	var name string
	var args []string
	if os.Geteuid() == 0 {
		name, args = "service", []string{service, action}
	} else {
		if _, err := exec.LookPath("sudo"); err != nil {
			return fmt.Errorf("`service %s %s` needs root and sudo is not installed — run it as root", service, action)
		}
		name, args = "sudo", []string{"service", service, action}
	}
	out, err := bsdServiceRunFn(name, args...)
	if err != nil {
		return fmt.Errorf("service %s %s: %w (%s)", service, action, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// setAutoStart enables or disables boot-time start by flipping the service's
// rc.conf enable flag with sysrc(8).
//
// sysrc is used rather than editing /etc/rc.conf directly: it rewrites the file
// atomically and preserves its formatting, which a hand-written redirect cannot
// guarantee on a file every other service on the box also depends on.
func setAutoStart(p Provider, on bool) error {
	service := bsdRcServiceName(p.Binary)
	// A service that does not exist must not get an rc.conf flag: sysrc would
	// write it, succeed, and enable nothing.
	if err := bsdRequireRcScript(p, service); err != nil {
		return err
	}
	value := "NO"
	if on {
		value = "YES"
	}
	if out, err := exec.Command("sysrc", service+"_enable="+value).CombinedOutput(); err != nil {
		return fmt.Errorf("auto-start %s: sysrc %s_enable=%s: %w (%s)",
			providerLabel(p), service, value, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// cleanupLifecycle removes the auto-update cron entry on uninstall. The rc.d
// script is removed by the uninstall command's own service handling.
func cleanupLifecycle(p Provider) {
	_ = removeBSDUpdateCron(p)
}

// renderBSDStatus runs `service <name> status`, the FreeBSD equivalent of
// `systemctl status <unit>`. It is installed into the shared seam so `status`
// prefers the real service view and falls back to the panel only when this
// returns an error.
func renderBSDStatus(p Provider) error {
	service := bsdRcServiceName(p.Binary)
	cmd := exec.Command("service", service, "status")
	cmd.Stdout = stdoutWriter()
	cmd.Stderr = stderrWriter()
	return cmd.Run()
}

func init() {
	renderSystemctlStatus = renderBSDStatus
}
