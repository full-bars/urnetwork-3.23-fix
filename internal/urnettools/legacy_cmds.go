package urnettools

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// This file ports the remaining legacy urnet-tools commands — service
// management (start/stop/restart/logs), hub linking (set/off/install),
// tuning profiles (turbo/eco/lowmode/ramlogs/auto/optimize), and proxy
// extras (health/traffic/remove-dead). Unlike the legacy shell tool, every
// command targets the RESOLVED provider (via targeting) — never a
// hardcoded $HOME path or a guessed unit.

// unitCommand runs a systemctl command against the provider's owning unit.
// Unit resolution is provider-aware: system-level units are managed via the
// system manager; user-level units via the owning user's session.
func unitCommand(p Provider, action string, extra ...string) error {
	if p.Unit == "" {
		return fmt.Errorf("provider %s has no owning systemd unit", providerLabel(p))
	}
	args := append([]string{action}, extra...)
	if isUserUnit(p.Unit) && p.User != "" {
		// systemctl --user -M <user>@ ... (session-scoped)
		args = append([]string{"--user", "-M", p.User + "@", action}, extra...)
	}
	args = append([]string{"systemctl"}, args...)
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// isUserUnit reports whether a unit name is user-level (no systemd system
// unit file, or in the user's config dir). System units are the norm for
// fleet deployments; user units are the legacy install model.
func isUserUnit(unit string) bool {
	// The legacy installer places units under ~/.config/systemd/user/.
	// Heuristic: if it's NOT a system unit file, treat as user unit.
	if _, err := os.Stat(filepath.Join("/etc/systemd/system", unit)); err == nil {
		return false
	}
	return true
}

// cmdStart starts the provider's owning unit.
func cmdStart(args []string) error {
	t, _, err := parseTargetFlags(args)
	if err != nil {
		return err
	}
	p, err := selectTarget(Discover(), t)
	if err != nil {
		return err
	}
	return unitCommand(p, "start")
}

// cmdStop stops the provider's owning unit.
func cmdStop(args []string) error {
	t, _, err := parseTargetFlags(args)
	if err != nil {
		return err
	}
	p, err := selectTarget(Discover(), t)
	if err != nil {
		return err
	}
	return unitCommand(p, "stop")
}

// cmdRestart restarts the provider's owning unit (destructive gate applies).
func cmdRestart(args []string, force, dryRun bool) error {
	t, _, err := parseTargetFlags(args)
	if err != nil {
		return err
	}
	p, err := selectTarget(Discover(), t)
	if err != nil {
		return err
	}
	ok, err := confirmGate("restart "+p.Unit, p, force, dryRun)
	if err != nil {
		return err
	}
	if !ok {
		return nil // dry-run
	}
	return unitCommand(p, "restart")
}

// cmdLogs streams logs for the provider: RAMLOGS-aware (reads /dev/shm)
// when the unit has URNETWORK_RAMLOGS=1 / a RAM profile, else journald.
func cmdLogs(args []string) error {
	t, _, err := parseTargetFlags(args)
	if err != nil {
		return err
	}
	p, err := selectTarget(Discover(), t)
	if err != nil {
		return err
	}
	if providerUsesRamlogs(p) {
		// Stream from the RAM buffer on the box.
		fmt.Printf("Streaming from RAM disk (/dev/shm/urnetwork.log) — provider %s\n", providerLabel(p))
		cmd := exec.Command("tail", "-n", "250", "-f", "/dev/shm/urnetwork.log")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}
	return unitCommand(p, "journalctl", "-fu", p.Unit)
}

// providerUsesRamlogs checks the unit's Environment for RAM logging or a
// RAM profile (the same check the legacy show_logs does).
func providerUsesRamlogs(p Provider) bool {
	if p.Unit == "" {
		return false
	}
	out, err := exec.Command("systemctl", "show", p.Unit, "-p", "Environment").Output()
	if err != nil {
		return false
	}
	env := string(out)
	return strings.Contains(env, "URNETWORK_RAMLOGS=1") ||
		strings.Contains(env, "URNETWORK_PROFILE=lowmem") ||
		strings.Contains(env, "URNETWORK_PROFILE=eco")
}

// cmdHub implements hub set/off/install: writes the URNETWORK_REPORT_URL
// drop-in for the targeted provider's unit, or installs the hub binary.
func cmdHub(args []string, force, dryRun bool) error {
	if len(args) == 0 {
		return fmt.Errorf("hub requires a subcommand: set <url> | off | install")
	}
	sub := args[0]
	rest := args[1:]
	t, rest, err := parseTargetFlags(rest)
	if err != nil {
		return err
	}
	p, err := selectTarget(Discover(), t)
	if err != nil {
		return err
	}

	switch sub {
	case "set":
		if len(rest) < 1 {
			return fmt.Errorf("hub set requires a URL, e.g. urnet-tools hub set http://192.0.2.10:8080")
		}
		url := rest[0]
		if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
			return fmt.Errorf("invalid URL %q: must begin with http:// or https://", url)
		}
		ok, err := confirmGate(fmt.Sprintf("set hub report URL on %s to %s", providerLabel(p), url), p, force, dryRun)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		return writeDropinEnv(p, "hub.conf", "URNETWORK_REPORT_URL="+url)
	case "off":
		ok, err := confirmGate("remove hub report URL from "+providerLabel(p), p, force, dryRun)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		return removeDropinEnv(p, "hub.conf", "URNETWORK_REPORT_URL")
	case "install":
		return cmdHubInstall(p, rest)
	default:
		return fmt.Errorf("unknown hub subcommand %q (set|off|install)", sub)
	}
}

// writeDropinEnv writes (or appends) an Environment= line to a drop-in
// override file for the provider's unit, then reloads/restarts it.
func writeDropinEnv(p Provider, name, envLine string) error {
	dropDir, err := unitDropinDir(p)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dropDir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(dropDir, name)
	content := fmt.Sprintf("[Service]\nEnvironment=%q\n", envLine)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return err
	}
	fmt.Printf("Wrote %s\n", path)
	return restartAfterDropin(p)
}

// removeDropinEnv removes a drop-in file (or a matching Environment line).
func removeDropinEnv(p Provider, name, envKey string) error {
	dropDir, err := unitDropinDir(p)
	if err != nil {
		return err
	}
	path := filepath.Join(dropDir, name)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		fmt.Printf("No %s found for %s\n", name, providerLabel(p))
		return nil
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	fmt.Printf("Removed %s\n", path)
	return restartAfterDropin(p)
}

// unitDropinDir returns the drop-in dir for the provider's unit.
func unitDropinDir(p Provider) (string, error) {
	if p.Unit == "" {
		return "", fmt.Errorf("provider %s has no owning unit", providerLabel(p))
	}
	if isUserUnit(p.Unit) && p.User != "" {
		home := homeForUser(p.User)
		if home == "" {
			return "", fmt.Errorf("cannot resolve home for user %s", p.User)
		}
		return filepath.Join(home, ".config/systemd/user/"+p.Unit+".d"), nil
	}
	return "/etc/systemd/system/" + p.Unit + ".d", nil
}

// restartAfterDropin reloads systemd and restarts the provider's unit.
func restartAfterDropin(p Provider) error {
	if isUserUnit(p.Unit) && p.User != "" {
		_ = exec.Command("systemctl", "--user", "-M", p.User+"@", "daemon-reload").Run()
		_ = exec.Command("systemctl", "--user", "-M", p.User+"@", "restart", p.Unit).Run()
		return nil
	}
	_ = exec.Command("systemctl", "daemon-reload").Run()
	return exec.Command("systemctl", "restart", p.Unit).Run()
}

// cmdHubInstall downloads and installs the hub binary + user unit.
func cmdHubInstall(p Provider, rest []string) error {
	// The hub binary asset follows the provider release pattern.
	tag := ""
	if len(rest) > 0 {
		tag = strings.TrimPrefix(rest[0], "--tag=")
	}
	if tag == "" {
		if rel, err := latestRelease(); err == nil {
			tag = rel.Tag
		} else {
			return err
		}
	}
	arch := runtimeGOARCH()
	url := fmt.Sprintf("https://github.com/full-bars/urnetwork-3.23-fix/releases/download/%s/urnetwork-hub-%s-linux-%s", tag, tag, arch)
	binDir := filepath.Dir(p.Binary)
	hubBin := filepath.Join(binDir, "urnetwork-hub")
	fmt.Printf("Downloading hub %s -> %s\n", url, hubBin)
	if err := downloadFile(url, hubBin); err != nil {
		return fmt.Errorf("hub download: %w", err)
	}
	if err := os.Chmod(hubBin, 0o755); err != nil {
		return err
	}
	// User-level systemd unit for the hub.
	home := homeForUser(p.User)
	if home == "" {
		return fmt.Errorf("cannot resolve home for user %s", p.User)
	}
	unitPath := filepath.Join(home, ".config/systemd/user/urnetwork-hub.service")
	content := fmt.Sprintf(`[Unit]
Description=URnetwork Hub

[Service]
ExecStart=%s
Restart=always
RestartSec=5
LimitNOFILE=65536

[Install]
WantedBy=default.target
`, hubBin)
	if err := os.MkdirAll(filepath.Dir(unitPath), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(unitPath, []byte(content), 0o644); err != nil {
		return err
	}
	fmt.Printf("Installed %s\n", unitPath)
	return nil
}

// runtimeGOARCH mirrors runtime.GOARCH without importing runtime in helpers.
func runtimeGOARCH() string {
	return strings.ToLower(goarch())
}

// goarch returns the build GOARCH (amd64/arm64) for asset naming.
func goarch() string {
	// runtime.GOARCH is the cleanest source; keep this as a tiny wrapper
	// so tests can stub it if needed.
	return goArchValue
}

// goArchValue is set at init from the runtime.
var goArchValue = func() string {
	switch os.Getenv("GOARCH") {
	case "amd64", "arm64", "386", "arm":
		return os.Getenv("GOARCH")
	}
	// Fall back to uname -m (best effort, avoids importing runtime).
	out, err := exec.Command("uname", "-m").Output()
	if err != nil {
		return "amd64"
	}
	switch strings.TrimSpace(string(out)) {
	case "x86_64":
		return "amd64"
	case "aarch64":
		return "arm64"
	case "armv7l":
		return "arm"
	case "i386", "i686":
		return "386"
	default:
		return "amd64"
	}
}()

// cmdTune implements the tuning profile commands (turbo/eco/lowmode/ramlogs/
// auto/optimize) by writing URNETWORK_PROFILE / env drop-ins for the
// targeted provider. Mode names match the legacy tool.
func cmdTune(profile string, args []string, force, dryRun bool) error {
	if len(args) == 0 {
		return fmt.Errorf("%s requires a mode: on | off (or v4/v8/off for turbo)", profile)
	}
	mode := args[0]
	rest := args[1:]
	t, _, err := parseTargetFlags(rest)
	if err != nil {
		return err
	}
	p, err := selectTarget(Discover(), t)
	if err != nil {
		return err
	}
	ok, err := confirmGate(fmt.Sprintf("set %s=%s on %s", profile, mode, providerLabel(p)), p, force, dryRun)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}

	var envLine string
	switch profile {
	case "ramlogs":
		if mode == "on" {
			envLine = "URNETWORK_RAMLOGS=1"
		} else {
			return removeDropinEnv(p, "tuning.conf", "URNETWORK_RAMLOGS")
		}
	case "eco":
		if mode == "on" {
			envLine = "URNETWORK_PROFILE=eco"
		} else {
			return removeDropinEnv(p, "tuning.conf", "URNETWORK_PROFILE")
		}
	case "lowmode":
		if mode == "on" {
			envLine = "URNETWORK_PROFILE=lowmem"
		} else {
			return removeDropinEnv(p, "tuning.conf", "URNETWORK_PROFILE")
		}
	case "turbo":
		if mode == "v4" || mode == "v8" {
			envLine = "URNETWORK_PROFILE=turbo-" + mode
		} else {
			return removeDropinEnv(p, "tuning.conf", "URNETWORK_PROFILE")
		}
	case "auto":
		if mode == "on" {
			envLine = "URNETWORK_PROFILE=auto"
		} else {
			return removeDropinEnv(p, "tuning.conf", "URNETWORK_PROFILE")
		}
	default:
		return fmt.Errorf("unknown profile %q", profile)
	}
	return writeDropinEnv(p, "tuning.conf", envLine)
}

// cmdOptimize applies golden-fleet kernel/OS limits (best-effort; delegates
// to the legacy installer script's optimize when present).
func cmdOptimize(args []string, force, dryRun bool) error {
	t, _, err := parseTargetFlags(args)
	if err != nil {
		return err
	}
	p, err := selectTarget(Discover(), t)
	if err != nil {
		return err
	}
	ok, err := confirmGate("apply golden-fleet OS/kernel limits to "+providerLabel(p), p, force, dryRun)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	// The legacy do_optimize writes sysctl/limits; mirror the essentials.
	// This is intentionally conservative — full parity lives in the legacy
	// installer until the Go tool is validated.
	fmt.Println("optimize: applying conservative kernel limits (sysctl net.core.rmem/wmem, fs.file-max)")
	_ = exec.Command("sysctl", "-w", "net.core.rmem_max=134217728", "net.core.wmem_max=134217728").Run()
	_ = exec.Command("sysctl", "-w", "fs.file-max=1000000").Run()
	fmt.Println("optimize: done")
	return nil
}

// cmdProxyHealthTarget prints the provider's proxy health state + streams
// the event log (state files in the provider's state dir). Takes a resolved
// Provider (targeting happens in the caller).
func cmdProxyHealthTarget(p Provider) error {
	state := filepath.Join(p.StateDir, "proxy_health.state")
	logf := filepath.Join(p.StateDir, "proxy_health.log")
	if b, err := os.ReadFile(state); err == nil {
		fmt.Printf("Current proxy health (%s):\n%s\n", state, b)
	} else {
		fmt.Printf("No snapshot yet at %s (waiting for first heartbeat?)\n", state)
	}
	if _, err := os.Stat(logf); err == nil {
		fmt.Printf("Streaming proxy health events (%s). Ctrl-C to stop.\n", logf)
		cmd := exec.Command("tail", "-n", "20", "-f", logf)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}
	fmt.Printf("No event log yet at %s.\n", logf)
	return nil
}

// cmdProxyTrafficTarget prints the provider's proxy traffic snapshot. Takes
// a resolved Provider (targeting happens in the caller).
func cmdProxyTrafficTarget(p Provider) error {
	state := filepath.Join(p.StateDir, "proxy_traffic.state")
	if b, err := os.ReadFile(state); err == nil {
		fmt.Printf("Current proxy traffic (%s):\n%s\n", state, b)
	} else {
		fmt.Printf("No traffic snapshot yet at %s.\n", state)
	}
	return nil
}

// cmdProxyRemoveDead delegates remove-dead to the provider binary.
func cmdProxyRemoveDead(args []string) error {
	t, rest, err := parseTargetFlags(args)
	if err != nil {
		return err
	}
	p, err := selectTarget(Discover(), t)
	if err != nil {
		return err
	}
	return providerSubcommand(p, append([]string{"proxy", "remove-dead"}, rest...)...)
}
