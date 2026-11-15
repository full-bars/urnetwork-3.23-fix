package urnettools

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

type sigpipeState struct {
	restart string
	force   string
	prevent string
}

func parseSigpipeState(output string) (sigpipeState, error) {
	props := make(map[string]string)
	for _, line := range strings.Split(output, "\n") {
		if key, value, ok := strings.Cut(line, "="); ok {
			props[key] = value
		}
	}
	for _, key := range []string{"Restart", "RestartForceExitStatus", "RestartPreventExitStatus", "SuccessExitStatus"} {
		if _, ok := props[key]; !ok {
			return sigpipeState{}, fmt.Errorf("systemctl omitted %s", key)
		}
	}
	return sigpipeState{props["Restart"], props["RestartForceExitStatus"], props["RestartPreventExitStatus"]}, nil
}

func sigpipeContains(value string) bool {
	for _, token := range strings.Fields(value) {
		if token == "SIGPIPE" {
			return true
		}
	}
	return false
}

func (s sigpipeState) risk() bool {
	if sigpipeContains(s.prevent) {
		return true
	}
	return !sigpipeContains(s.force) && s.restart != "always" && s.restart != "on-success"
}

func (s sigpipeState) String() string {
	return fmt.Sprintf("Restart=%s RestartForceExitStatus=%q RestartPreventExitStatus=%q", s.restart, s.force, s.prevent)
}

type sigpipeRunner func(context.Context, ...string) ([]byte, error)

func runSigpipeSystemctl(ctx context.Context, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, "systemctl", args...).Output()
}

func showSigpipeState(p Provider, run sigpipeRunner) (sigpipeState, error) {
	var args []string
	if p.User != "" && isUserUnit(p.Unit) {
		args = append(args, systemctlUserArgs(p.User)...)
	}
	args = append(args, "show", "-p", "Restart", "-p", "RestartForceExitStatus", "-p", "RestartPreventExitStatus", "-p", "SuccessExitStatus", p.Unit)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := run(ctx, args...)
	if err != nil {
		return sigpipeState{}, fmt.Errorf("systemctl show: %w", err)
	}
	return parseSigpipeState(string(out))
}
