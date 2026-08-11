package urnettools

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// knownBinaries are the binary basenames the tool recognizes as URnetwork
// providers. provider_beta is the beta-test build name used on fleet boxes.
var knownBinaries = map[string]bool{
	"urnetwork":     true,
	"provider_beta": true,
	"provider":      true,
}

// discoverProcesses scans /proc for running provider processes across ALL
// users. Each match yields a Provider with identity + location fields filled
// from the process environ and exe. The caller's own euid is irrelevant —
// as root everything is visible; as a normal user /proc only shows own
// processes (hidepid), which is the intended permission boundary.
func discoverProcesses() []Provider {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var out []Provider
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid := parsePID(e.Name())
		if pid == 0 {
			continue
		}
		cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		if err != nil || len(cmdline) == 0 {
			continue
		}
		args := strings.Split(strings.TrimRight(string(cmdline), "\x00"), "\x00")
		if len(args) == 0 || !isProviderArg(args[0]) {
			continue
		}
		exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
		if err != nil {
			exe = args[0] // fall back to argv[0]
		}
		env := readEnviron(pid)
		user := env["USER"]
		if user == "" {
			user = env["LOGNAME"]
		}
		p := Provider{
			User:     user,
			StateDir: stateDirFor(env),
			Binary:   exe,
			PID:      pid,
			Running:  true,
		}
		// A provider may carry its own --state-dir flag; honor it.
		for i := 1; i < len(args)-1; i++ {
			if args[i] == "--state-dir" {
				p.StateDir = args[i+1]
				break
			}
		}
		if p.StateDir == "" {
			// No state dir resolvable (HOME unset). Skip the JWT read
			// entirely rather than falling through to a relative "jwt"
			// path in the invoker's CWD (review finding L1).
			out = append(out, p)
			continue
		}
		p.Network, p.NetworkID, p.JWTExpires, _ = decodeJWT(filepath.Join(p.StateDir, "jwt"))
		p.Version = providerVersion(p.Binary)
		out = append(out, p)
	}
	return out
}

// isProviderArg reports whether an executable path/name is a known provider
// binary. Matches on basename to be resilient to custom install paths, and
// by PREFIX so suffixed unit names (urnetwork-native.service,
// provider_beta-custom) are recognized too (opus5 F2). Excludes the
// well-known non-provider suffixes (-hub, -update) so their units are not
// mistaken for providers.
func isProviderArg(arg string) bool {
	base := filepath.Base(arg)
	// Strip a trailing .exe (Windows) defensively.
	base = strings.TrimSuffix(base, ".exe")
	for known := range knownBinaries {
		if base == known || strings.HasPrefix(base, known+"-") {
			// provider-hub / provider-update are NOT providers.
			if strings.HasSuffix(base, "-hub") || strings.HasSuffix(base, "-update") {
				return false
			}
			return true
		}
	}
	return false
}

// discoverSystemdUnits scans systemd for provider units (running or stopped)
// and fills in Provider records for any unit not already represented by a
// live process. Unit User= is read from the unit file; state dir follows the
// install convention.
//
// The fork's install model places units under ~/.config/systemd/user and
// drives them with `systemctl --user`, which the SYSTEM-manager listing
// below never shows. So this also enumerates per-user managers for users
// that plausibly run a provider (a user unit that looks like a provider, or
// a .urnetwork state dir), bounded to those users (opus5 F2).
func discoverSystemdUnits(running []Provider) []Provider {
	out := discoverSystemUnits(running)
	out = append(out, discoverUserUnits(running)...)
	return out
}

// discoverSystemUnits scans the system manager's unit listing.
func discoverSystemUnits(running []Provider) []Provider {
	cmd := exec.Command("systemctl", "list-units", "--all", "--no-legend", "--no-pager")
	out, err := cmd.Output()
	if err != nil {
		return nil // no systemd (container/other init) — process scan is enough
	}
	// unitUser maps unit name -> User= value, resolved on demand.
	unitUser := func(unit string) string {
		c := exec.Command("systemctl", "show", unit, "-p", "User", "--value")
		b, err := c.Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(b))
	}
	var out2 []Provider
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		unit := fields[0]
		if !isProviderUnit(unit) {
			continue
		}
		// Skip units already backed by a running process (matched by unit
		// name via the provider's Unit field, set below).
		if unitIn(running, unit) {
			continue
		}
		u := unitUser(unit)
		out2 = append(out2, providerFromUnit(unit, u))
	}
	return out2
}

// discoverUserUnits enumerates user-manager units for users that plausibly
// run a provider: those already seen running (their user manager may hold a
// stopped sibling) plus any user whose home has a provider-looking unit
// under ~/.config/systemd/user or a .urnetwork state dir. Each candidate
// user's manager is queried once with `systemctl --user -M <user>@`.
func discoverUserUnits(running []Provider) []Provider {
	users := map[string]bool{}
	for _, p := range running {
		if p.User != "" {
			users[p.User] = true
		}
	}
	// Broaden to any user with provider-ish files in their home (bounded:
	// only users with evidence, never all of /etc/passwd).
	if homes, err := providerCandidateHomes(); err == nil {
		for _, h := range homes {
			users[h] = true
		}
	}
	var out []Provider
	for user := range users {
		cmd := exec.Command("systemctl", "--user", "-M", user+"@", "list-units", "--all", "--no-legend", "--no-pager")
		b, err := cmd.Output()
		if err != nil {
			continue // no session bus / user manager for this user
		}
		for _, line := range strings.Split(string(b), "\n") {
			fields := strings.Fields(line)
			if len(fields) == 0 {
				continue
			}
			unit := fields[0]
			if !isProviderUnit(unit) {
				continue
			}
			if unitIn(running, unit) {
				continue
			}
			out = append(out, providerFromUnit(unit, user))
		}
	}
	return out
}

// providerCandidateHomes returns home directories of users that show
// evidence of a provider install: a provider-looking unit under
// ~/.config/systemd/user or a ~/.urnetwork state dir. Best-effort.
func providerCandidateHomes() ([]string, error) {
	b, err := exec.Command("getent", "passwd").Output()
	if err != nil {
		return nil, err
	}
	var homes []string
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Split(line, ":")
		if len(fields) < 6 || fields[5] == "" {
			continue
		}
		home := fields[5]
		// User-level unit dir with a provider unit, or a state dir.
		found := false
		for known := range knownBinaries {
			unitGlob := filepath.Join(home, ".config/systemd/user", known+"*.service")
			if matches, _ := filepath.Glob(unitGlob); len(matches) > 0 {
				homes = append(homes, home)
				found = true
				break
			}
		}
		if found {
			continue
		}
		if _, err := os.Stat(filepath.Join(home, ".urnetwork")); err == nil {
			homes = append(homes, home)
		}
	}
	return homes, nil
}

// isProviderUnit reports whether a systemd unit name looks like a provider
// unit (basename matches a known binary, optionally suffixed).
func isProviderUnit(unit string) bool {
	base := unit
	if i := strings.IndexByte(base, '.'); i >= 0 {
		base = base[:i]
	}
	return isProviderArg(base)
}

// unitIn reports whether any running provider carries the unit name.
func unitIn(running []Provider, unit string) bool {
	for i := range running {
		if running[i].Unit == unit {
			return true
		}
	}
	return false
}

// providerFromUnit builds a Provider record for a (possibly stopped) unit.
func providerFromUnit(unit, user string) Provider {
	p := Provider{
		User:     user,
		StateDir: unitStateDir(user),
		Unit:     unit,
		Running:  false,
	}
	if p.StateDir == "" {
		return p // no resolvable state dir — listed, but ungraded
	}
	p.Network, p.NetworkID, p.JWTExpires, _ = decodeJWT(filepath.Join(p.StateDir, "jwt"))
	return p
}

// Discover returns every provider on the box: running processes across all
// users plus stopped systemd units. Sorted by user then unit for stable
// output.
func Discover() []Provider {
	procs := discoverProcesses()
	// Attach unit names to running processes where systemd owns them.
	attachUnits(procs)
	units := discoverSystemdUnits(procs)
	all := append(procs, units...)
	sort.Slice(all, func(i, j int) bool {
		if all[i].User != all[j].User {
			return all[i].User < all[j].User
		}
		return all[i].Unit < all[j].Unit
	})
	return all
}

// attachUnits assigns a systemd unit name to each running provider by
// matching the unit's User= + ExecStart binary against the process. This is
// best-effort: processes started outside systemd keep Unit="".
func attachUnits(procs []Provider) {
	for i := range procs {
		p := &procs[i]
		if p.PID == 0 {
			continue
		}
		// Resolve the cgroup to find the owning unit.
		cg, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", p.PID))
		if err != nil {
			continue
		}
		s := string(cg)
		// cgroup v2 path ends with the unit name, e.g.
		// .../system.slice/urnetwork-native.service
		if idx := strings.LastIndex(s, ".service"); idx >= 0 {
			start := strings.LastIndex(s[:idx], "/")
			if start >= 0 {
				p.Unit = s[start+1 : idx+len(".service")]
			}
		}
	}
}
