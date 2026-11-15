package urnettools

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// sigpipeRestartRisk reports whether the effective unit for p would leave a
// SIGPIPE-killed provider dead. It asks systemd for the effective properties
// of the unit (including every drop-in directory via systemctl show), and an
// empty RestartForceExitStatus= resets the list as systemd does. On other
// OSes, or for providers not owned by systemd (OpenRC, launchd, Windows,
// bare process), there is no systemd equivalent to migrate and no risk to
// report: false, nil. Returned error only when systemd itself fails.
//
// A unit leaves a SIGPIPE death dead when its effective Restart= policy does
// not restart on clean exit (not "always"/"on-success") and nothing forces
// restart on SIGPIPE: RestartForceExitStatus must contain SIGPIPE, and
// RestartPreventExitStatus must not contain it.
func sigpipeRestartRisk(p Provider) (bool, error) {
	if runtime.GOOS != "linux" || p.Unit == "" || p.Supervisor == "openrc" {
		return false, nil
	}
	args := []string{}
	if isUserUnit(p.Unit) && p.User != "" {
		args = append(args, systemctlUserArgs(p.User)...)
	}
	args = append(args, "show",
		"-p", "Restart",
		"-p", "RestartForceExitStatus",
		"-p", "RestartPreventExitStatus",
		"-p", "SuccessExitStatus",
		p.Unit,
	)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "systemctl", args...).Output()
	if err != nil {
		return false, err
	}
	props := make(map[string]string)
	for _, line := range strings.Split(string(out), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok {
			props[key] = value
		}
	}
	for _, key := range []string{"Restart", "RestartForceExitStatus", "RestartPreventExitStatus", "SuccessExitStatus"} {
		if _, ok := props[key]; !ok {
			return false, fmt.Errorf("systemctl omitted %s", key)
		}
	}
	hasPipe := func(value string) bool {
		for _, v := range strings.Fields(value) {
			if v == "SIGPIPE" {
				return true
			}
		}
		return false
	}
	if hasPipe(props["RestartPreventExitStatus"]) {
		return true, nil
	}
	if hasPipe(props["RestartForceExitStatus"]) {
		return false, nil
	}
	return props["Restart"] != "always" && props["Restart"] != "on-success", nil
}
