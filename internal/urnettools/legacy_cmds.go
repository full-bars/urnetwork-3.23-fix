package urnettools

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	cmd := exec.Command(unitCommandArgs(p, action, extra...)[0], unitCommandArgs(p, action, extra...)[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// unitCommandArgs builds the systemctl argv for an action on the provider's
// unit: system units use "systemctl <action> <unit>"; user units are scoped
// to the owning user's session via "--user -M <user>@ <action> <unit>". The
// unit name is ALWAYS the final argument — systemctl errors "Too few
// arguments" without it (gauntlet finding: hot-restart printed that error;
// the pre-fix unitCommandArgs omitted the unit entirely).
func unitCommandArgs(p Provider, action string, extra ...string) []string {
	if p.Unit == "" {
		return []string{"systemctl", action}
	}
	if isUserUnit(p.Unit) && p.User != "" {
		args := []string{"systemctl", "--user", "-M", p.User + "@", action, p.Unit}
		return append(args, extra...)
	}
	args := []string{"systemctl", action, p.Unit}
	return append(args, extra...)
}

// isUserUnit reports whether a unit name is user-level (no systemd system
// unit file, or in the user's config dir). System units are the norm for
// fleet deployments; user units are the legacy install model.
func isUserUnit(unit string) bool {
	// The legacy installer places units under ~/.config/systemd/user/.
	// Heuristic: if it's NOT a system unit file, treat as user unit.
	// Check both the admin dir and the vendor/package dir — units shipped
	// by a package live under /usr/lib/systemd/system (free-review MEDIUM).
	for _, dir := range []string{"/etc/systemd/system", "/usr/lib/systemd/system", "/lib/systemd/system"} {
		if _, err := os.Stat(filepath.Join(dir, unit)); err == nil {
			return false
		}
	}
	return true
}

// cmdStart starts the provider's owning unit.
func cmdStart(args []string, force, dryRun bool) error {
	t, _, err := parseTargetFlags(args)
	if err != nil {
		return err
	}
	p, err := selectTarget(Discover(), t)
	if err != nil {
		return err
	}
	// -n/--dry-run is documented "safe anywhere": print the plan, do
	// nothing (free-review HIGH: start/stop previously discarded it and
	// executed for real).
	if dryRun {
		fmt.Printf("[dry-run] would start %s (unit=%s, user=%s)\n", providerLabel(p), p.Unit, p.User)
		return nil
	}
	return unitCommand(p, "start")
}
func cmdStop(args []string, force, dryRun bool) error {
	t, _, err := parseTargetFlags(args)
	if err != nil {
		return err
	}
	p, err := selectTarget(Discover(), t)
	if err != nil {
		return err
	}
	if dryRun {
		fmt.Printf("[dry-run] would stop %s (unit=%s, user=%s)\n", providerLabel(p), p.Unit, p.User)
		return nil
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

// discoverDockerFn is the docker-provider discovery function, as a var so
// errWithDockerHint's docker branch is testable without a live daemon.
var discoverDockerFn = DiscoverDocker

// errWithDockerHint wraps a no-provider error with a pointer to the docker
// variant when provider containers exist: the systemd/process tool cannot
// tail their logs, but `urnet-docker logs` can (its interactive picker
// lists them). Only fires when systemdProviderCount is ZERO — when systemd
// providers exist, a selectTarget error is a target problem (typo/
// ambiguity), not a wrong-tool problem, and pointing at docker would
// mislead (review MEDIUM). The count is threaded from the caller (which
// already fetched Discover()) to avoid re-running the discovery pipeline.
func errWithDockerHint(err error, systemdProviderCount int) error {
	if systemdProviderCount > 0 {
		return err
	}
	docker := discoverDockerFn()
	if len(docker) == 0 {
		return err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%v\n", err)
	fmt.Fprintf(&b, "provider(s) running in docker (use urnet-docker):\n")
	for _, p := range docker {
		fmt.Fprintf(&b, "  %s  net=%s\n", p.Unit, p.Network)
	}
	fmt.Fprintf(&b, "to view their logs: urnet-docker logs\n")
	return fmt.Errorf("%s", b.String())
}

// cmdLogs streams logs for the provider: RAMLOGS-aware (reads /dev/shm)
// when the unit has URNETWORK_RAMLOGS=1 / a RAM profile, else journald.
func cmdLogs(args []string) error {
	t, _, err := parseTargetFlags(args)
	if err != nil {
		return err
	}
	providers := Discover()
	p, narrowed, err := selectTargetOrSoleAccessible(providers, t)
	if err != nil {
		return errWithDockerHint(err, len(providers))
	}
	if narrowed {
		printNarrowedNote(len(providers), p, "logs")
	}
	if providerUsesRamlogs(p) {
		// Stream from the RAM buffer on the box.
		fmt.Printf("Streaming from RAM disk (/dev/shm/urnetwork.log) — provider %s\n", providerLabel(p))
		cmd := exec.Command("tail", "-n", "250", "-f", "/dev/shm/urnetwork.log")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}
	// journalctl is a standalone binary, not a systemctl verb — calling it
	// through unitCommand would execute `systemctl journalctl` (invalid,
	// free-review critical). Scope user units explicitly.
	cmd := exec.Command("journalctl", journalctlArgs(p)...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// journalctlArgs builds the journalctl argv for following a provider's
// unit: system units use "-fu <unit>"; user units are scoped to the owning
// user's session via "-M <user>@ --user-unit <unit> -f" (a plain "-fu"
// against a user unit name would query the SYSTEM journal instead).
func journalctlArgs(p Provider) []string {
	if isUserUnit(p.Unit) && p.User != "" {
		return []string{"-M", p.User + "@", "--user-unit", p.Unit, "-f"}
	}
	return []string{"-fu", p.Unit}
}

// providerUsesRamlogs checks the unit's Environment for RAM logging or a
// RAM profile (the same check the legacy show_logs does). User units are
// queried in the owning user's session, not the system manager
// (free-review major: RAMLOGS detection ignored user units).
func providerUsesRamlogs(p Provider) bool {
	if p.Unit == "" {
		return false
	}
	var out []byte
	var err error
	if isUserUnit(p.Unit) && p.User != "" {
		out, err = exec.Command("systemctl", "--user", "-M", p.User+"@", "show", p.Unit, "-p", "Environment").Output()
	} else {
		out, err = exec.Command("systemctl", "show", p.Unit, "-p", "Environment").Output()
	}
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
		return fmt.Errorf("hub requires a subcommand: set <url> | off | install | init | link | unlink | test | onboard-cmd | show-password | open-port | update")
	}
	sub := args[0]
	rest := args[1:]
	// LENIENT target parse: `hub install` defines its own --tag= flag
	// (opus5 F1: strict parsing rejected --tag= before cmdHubInstall saw
	// it). Unknown --flags are rejected per-subcommand below.
	t, rest, err := parseTargetFlagsLenient(rest)
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
	case "init":
		password := ""
		for i := 0; i < len(rest); i++ {
			if rest[i] == "--password" || rest[i] == "-p" {
				if i+1 < len(rest) {
					password = rest[i+1]
					i++
				} else {
					return fmt.Errorf("--password requires a value")
				}
			} else {
				return fmt.Errorf("unknown hub init argument %q", rest[i])
			}
		}
		ok, err := confirmGate("init hub on "+providerLabel(p), p, force, dryRun)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		return cmdHubInit(p, password)
	case "show-password":
		return cmdHubShowPassword(p)
	case "onboard-cmd":
		return cmdHubOnboardCmd(p)
	case "link":
		url, token := "", ""
		for i := 0; i < len(rest); i++ {
			if rest[i] == "--token" {
				if i+1 < len(rest) {
					token = rest[i+1]
					i++
				} else {
					return fmt.Errorf("--token requires a value")
				}
			} else if url == "" {
				url = rest[i]
			} else {
				return fmt.Errorf("unexpected hub link argument %q", rest[i])
			}
		}
		if url == "" {
			return fmt.Errorf("hub link requires a URL: hub link <https://hub-host:port> [--token <onboard-token>]")
		}
		return cmdHubLink(p, url, token, force)
	case "test":
		url := ""
		if len(rest) > 0 {
			url = rest[0]
		}
		return cmdHubTest(p, url)
	case "unlink":
		ok, err := confirmGate("unlink hub from "+providerLabel(p), p, force, dryRun)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		return cmdHubUnlink(p, force)
	case "update":
		ok, err := confirmGate("update hub on "+providerLabel(p), p, force, dryRun)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		return cmdHubUpdate(p, rest)
	case "open-port":
		if len(rest) < 1 {
			return fmt.Errorf("hub open-port requires a port, e.g. hub open-port 8443")
		}
		return cmdHubOpenPort(rest[0])
	default:
		return fmt.Errorf("unknown hub subcommand %q (set|off|install|init|link|unlink|test|onboard-cmd|show-password|open-port|update)", sub)
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
	content := mergeDropinEnvFile(path, envLine)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return err
	}
	fmt.Printf("Wrote %s\n", path)
	return restartAfterDropin(p)
}

// mergeDropinEnvFile returns the merged drop-in content for a new
// Environment= line: it reads the existing file, keeps lines whose env key
// differs, replaces same-key lines, and always re-emits exactly one
// [Service] header. Pure (no I/O beyond the read) so tests can pin the
// merge semantics without a real unit (coderabbit: tests must call
// production logic).
func mergeDropinEnvFile(path, envLine string) string {
	newKey := envLine
	if i := strings.IndexByte(envLine, '='); i > 0 {
		newKey = envLine[:i]
	}
	var kept []string
	if b, err := os.ReadFile(path); err == nil {
		for _, ln := range strings.Split(string(b), "\n") {
			trimmed := strings.TrimSpace(ln)
			if trimmed == "" || trimmed == "[Service]" {
				continue // header is re-emitted below — avoid duplicates
			}
			if strings.HasPrefix(trimmed, "Environment=") {
				val := strings.TrimPrefix(trimmed, "Environment=")
				val = strings.Trim(val, `"`)
				// Same key (e.g. URNETWORK_PROFILE) — replaced below.
				if strings.HasPrefix(val, newKey) && (len(val) == len(newKey) || val[len(newKey)] == '=') {
					continue
				}
			}
			kept = append(kept, trimmed)
		}
	}
	kept = append(kept, fmt.Sprintf("Environment=%q", envLine))
	return "[Service]\n" + strings.Join(kept, "\n") + "\n"
}

// removeDropinEnv removes a matching Environment line from a drop-in file
// (or the whole file if it becomes empty).
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
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	// Exact-key removal, NOT substring: envKey "URNETWORK_PROFILE" must not
	// drop a sibling "URNETWORK_PROFILE_EXTRA" line (free-review MAJOR).
	var kept []string
	for _, ln := range strings.Split(string(b), "\n") {
		trimmed := strings.TrimSpace(ln)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "Environment=") {
			val := strings.TrimPrefix(trimmed, "Environment=")
			val = strings.Trim(val, `"`)
			// Exact key match (key or key=value); anything else is kept.
			if val == envKey || (strings.HasPrefix(val, envKey+"=")) {
				continue // drop this line only
			}
		}
		if trimmed == "[Service]" {
			continue // header re-emitted below — avoid duplicates
		}
		kept = append(kept, trimmed)
	}
	if len(kept) == 0 {
		if err := os.Remove(path); err != nil {
			return err
		}
		fmt.Printf("Removed %s\n", path)
	} else {
		content := "[Service]\n" + strings.Join(kept, "\n") + "\n"
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			return err
		}
		fmt.Printf("Updated %s (removed %s)\n", path, envKey)
	}
	return restartAfterDropin(p)
}

// isELFExecutable reports whether path starts with the ELF magic bytes
// (0x7f 'E' 'L' 'F'). Used to sanity-check downloaded binaries WITHOUT
// executing them — running a freshly downloaded, unverified artifact is
// code execution of a remote file (coderabbit critical). Linux-only check;
// see isRecognizedExecutable for the platform-aware form.
func isELFExecutable(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	var magic [4]byte
	if _, err := io.ReadFull(f, magic[:]); err != nil {
		return false
	}
	return magic[0] == 0x7f && magic[1] == 'E' && magic[2] == 'L' && magic[3] == 'F'
}

// isMachOExecutable reports whether path starts with a Mach-O magic (darwin
// binaries: MH_MAGIC_64 0xFEEDFACF / MH_MAGIC 0xFEEDFACE, plus the byte-
// swapped and fat-binary forms 0xCEFAEDFE / 0xCFFAEDFE / 0xCAFEBABE /
// 0xBEBAFECA). The tool cross-compiles for darwin, so a downloaded darwin
// binary must pass a Mach-O check, not the ELF one.
func isMachOExecutable(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	var magic [4]byte
	if _, err := io.ReadFull(f, magic[:]); err != nil {
		return false
	}
	switch {
	case magic[0] == 0xfe && magic[1] == 0xed && magic[2] == 0xfa && (magic[3] == 0xce || magic[3] == 0xcf):
		return true // MH_CIGAM / MH_CIGAM_64 (byte-swapped big-endian)
	case magic[0] == 0xce && magic[1] == 0xfa && magic[2] == 0xed && magic[3] == 0xfe:
		return true // MH_MAGIC (little-endian on disk)
	case magic[0] == 0xcf && magic[1] == 0xfa && magic[2] == 0xed && magic[3] == 0xfe:
		// MH_MAGIC_64 — the little-endian on-disk byte sequence of the
		// host-order constant. This is the branch real Go darwin/amd64 and
		// darwin/arm64 binaries hit (verified by cross-compiling and dumping
		// the first bytes: cf fa ed fe). NOT a big-endian case.
		return true
	case magic[0] == 0xca && magic[1] == 0xfe && magic[2] == 0xba && magic[3] == 0xbe:
		return true // FAT_MAGIC (universal binary)
	case magic[0] == 0xbe && magic[1] == 0xba && magic[2] == 0xfe && magic[3] == 0xca:
		return true // FAT_CIGAM (universal binary, swapped)
	}
	return false
}

// isPEExecutable reports whether path starts with the MZ header of a PE
// (Windows) executable.
func isPEExecutable(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	var magic [2]byte
	if _, err := io.ReadFull(f, magic[:]); err != nil {
		return false
	}
	return magic[0] == 'M' && magic[1] == 'Z'
}

// isRecognizedExecutable is the platform-aware structural check for a
// downloaded binary: ELF on linux, Mach-O on darwin, PE on windows. It
// never executes the file — it only confirms the magic matches the platform
// we are about to install for (coderabbit critical: the provider path
// guards with this same ceiling). A wrong-format artifact (a shell script,
// a corrupt download, or a binary built for another OS) is refused before
// it can be swapped into place.
func isRecognizedExecutable(path string) bool {
	switch runtime.GOOS {
	case "darwin":
		return isMachOExecutable(path)
	case "windows":
		return isPEExecutable(path)
	default:
		return isELFExecutable(path)
	}
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
	// Same guard as unitCommand: an empty unit must be rejected before any
	// systemctl invocation (coderabbit minor on the coverage pass).
	if p.Unit == "" {
		return fmt.Errorf("provider %s has no owning systemd unit", providerLabel(p))
	}
	if isUserUnit(p.Unit) && p.User != "" {
		_ = exec.Command("systemctl", "--user", "-M", p.User+"@", "daemon-reload").Run()
		// Propagate the restart error like the system-unit branch below —
		// an operator writing a drop-in override must learn when the
		// provider never actually restarted (Sonnet MEDIUM-2).
		return exec.Command("systemctl", "--user", "-M", p.User+"@", "restart", p.Unit).Run()
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
	// chmod FIRST (the sanity check needs the execute bit), then verify the
	// file structurally WITHOUT executing it — running a freshly downloaded,
	// unverified binary is code execution of a remote artifact (coderabbit
	// critical; the provider-update path requires a digest for the same
	// reason, but the hub asset has no published digest to check against).
	if err := os.Chmod(hubBin, 0o755); err != nil {
		return err
	}
	if !isRecognizedExecutable(hubBin) {
		os.Remove(hubBin)
		return fmt.Errorf("hub download: %s is not a %s executable (corrupted or wrong asset)", hubBin, runtime.GOOS)
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
// to the legacy installer script's optimize when present). Platform-aware:
// Linux uses sysctl, Windows uses netsh/reg (no kernel to tune, but the
// network stack equivalents matter for proxy-scale connection churn).
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
	fmt.Println("optimize: applying golden-fleet network limits")
	return optimizeFor(runtime.GOOS)()
}

// optimizeFor returns the platform-appropriate optimize function for goos.
// Extracted so the dispatch itself is unit-testable without running the
// (root-requiring, host-mutating) implementations.
func optimizeFor(goos string) func() error {
	if goos == "windows" {
		return optimizeWindows
	}
	return optimizeLinux
}

// optimizeLinux applies the Linux sysctl set: socket buffers, FD limit, and
// the two connection-churn knobs that matter most for a proxy box — the
// ephemeral port pool (ip_local_port_range) and TIME_WAIT recycling
// (tcp_fin_timeout). Conservative; failures are logged, never fatal.
func optimizeLinux() error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("optimize: sysctl requires root (running as uid %d); run with sudo or as root", os.Geteuid())
	}
	// Buffer + FD settings mirror the legacy do_optimize; the port range
	// and TIME_WAIT knobs are new (proxy-scale outbound churn exhausts
	// the default ~28k ephemeral ports and parks sockets in TIME_WAIT).
	for _, args := range [][]string{
		{"-w", "net.core.rmem_max=134217728", "net.core.wmem_max=134217728"},
		{"-w", "fs.file-max=1000000"},
		{"-w", "net.ipv4.ip_local_port_range=1024 65535"},
		{"-w", "net.ipv4.tcp_fin_timeout=15"},
	} {
		cmd := exec.Command("sysctl", args...)
		if out, err := cmd.CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "optimize: warning: sysctl %v failed: %v (%s)\n", args, err, strings.TrimSpace(string(out)))
		}
	}
	fmt.Println("optimize: done")
	return nil
}

// optimizeWindows applies the Windows network-stack equivalents: a widened
// ephemeral port pool (netsh dynamicport) and a shorter TIME_WAIT
// (TcpTimedWaitDelay registry key). These need an elevated shell; failures
// are logged, never fatal.
func optimizeWindows() error {
	// netsh: widen the dynamic client port pool (default ~16k is too small
	// for a busy proxy box).
	if out, err := exec.Command("netsh", "int", "ipv4", "set", "dynamicport", "tcp", "start=1025", "num=64510").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "optimize: warning: netsh dynamicport failed: %v (%s)\n", err, strings.TrimSpace(string(out)))
	}
	// Registry: shorten TIME_WAIT so closed sockets free their ports faster
	// (default 120s on Windows). Takes effect after reboot.
	if out, err := exec.Command("reg", "add", `HKLM\SYSTEM\CurrentControlSet\Services\Tcpip\Parameters`,
		"/v", "TcpTimedWaitDelay", "/t", "REG_DWORD", "/d", "30", "/f").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "optimize: warning: reg TcpTimedWaitDelay failed: %v (%s)\n", err, strings.TrimSpace(string(out)))
	}
	fmt.Println("optimize: done (TcpTimedWaitDelay takes effect on reboot)")
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
