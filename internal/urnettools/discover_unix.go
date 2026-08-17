//go:build linux

package urnettools

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
)

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

// discoverStopped on Linux restores the F2 stopped-provider discovery: it
// attaches systemd unit names to the running providers, then scans the
// system and user managers for provider units that are not backed by a live
// process. The running list is passed through so unit scans can avoid
// duplicating providers already represented by a process (unitIn checks the
// attached Unit field).
func discoverStopped(running []Provider) []Provider {
	attachUnits(running)
	return discoverSystemdUnits(running)
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
		// .../system.slice/urnetwork-native.service. Only accept names that
		// look like a provider unit (isProviderUnit) — on a GH runner the
		// provider inherits the runner's cgroup, and an unfiltered name would
		// make hot-restart systemctl-restart the wrong unit (review S3).
		if idx := strings.LastIndex(s, ".service"); idx >= 0 {
			start := strings.LastIndex(s[:idx], "/")
			if start >= 0 {
				unit := s[start+1 : idx+len(".service")]
				if isProviderUnit(unit) {
					p.Unit = unit
				}
			}
		}
	}
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
	// --plain strips the leading "●"/space state column so fields[0] is the
	// unit name; without it a loaded-failed unit parses as "●" and is never
	// matched (CI unix-lifecycle: fake unit installed but undiscoverable).
	cmd := exec.Command("systemctl", "--plain", "list-units", "--all", "--no-legend", "--no-pager")
	out, err := cmd.Output()
	if err != nil {
		return nil // no systemd (container/other init) — process scan is enough
	}
	// list-units --all misses never-started units that exist on disk;
	// list-unit-files scans the unit paths and sees them. Merge both so a
	// freshly-installed (stopped) provider is discoverable.
	if fb, ferr := exec.Command("systemctl", "--plain", "list-unit-files", "--no-legend", "--no-pager").Output(); ferr == nil {
		out = append(out, '\n')
		out = append(out, fb...)
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
// user's manager is queried once with `systemctl --user -M <user>@` — except
// the CURRENT user, where plain `systemctl --user` is used: the -M form goes
// through machined/loginctl and can fail on CI runners (no cross-user
// session bus) even when the local user manager is fully functional.
func discoverUserUnits(running []Provider) []Provider {
	users := map[string]bool{}
	for _, p := range running {
		if p.User != "" {
			users[p.User] = true
		}
	}
	// Broaden to any user with provider-ish files in their home (bounded:
	// only users with evidence, never all of /etc/passwd).
	if usersByFile, err := providerCandidateUsers(); err == nil {
		for _, u := range usersByFile {
			users[u] = true
		}
	}
	// The current user's manager is reachable via the session bus /
	// XDG_RUNTIME_DIR socket; other users require -M <user>@ (machined).
	current := currentUserName()
	var out []Provider
	for user := range users {
		var b []byte
		var err error
		if user == current {
			// --plain strips the leading "●"/space state column so
			// fields[0] is the unit name (see discoverSystemUnits).
			b, err = exec.Command("systemctl", "--user", "--plain", "list-units", "--all", "--no-legend", "--no-pager").Output()
			if err == nil {
				// list-units --all misses never-started units that exist
				// on disk (a fresh fake/stopped provider); list-unit-files
				// scans the unit paths and sees them. Merge both.
				if fb, ferr := exec.Command("systemctl", "--user", "--plain", "list-unit-files", "--no-legend", "--no-pager").Output(); ferr == nil {
					b = append(b, '\n')
					b = append(b, fb...)
				}
			}
		} else {
			// Cross-user query goes through machined/loginctl, which can
			// be unavailable on CI runners even though the local user
			// manager works. Fall back to the caller's own manager when
			// the -M form fails so a same-user provider is still found.
			b, err = exec.Command("systemctl", "--user", "-M", user+"@", "--plain", "list-units", "--all", "--no-legend", "--no-pager").Output()
			if err != nil {
				b, err = exec.Command("systemctl", "--user", "--plain", "list-units", "--all", "--no-legend", "--no-pager").Output()
			}
		}
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

// currentUserName returns the invoking user's login name, used to decide
// whether a user-manager query needs -M <user>@ (cross-user) or can use the
// local session bus. os/user.Current() is authoritative; USER/LOGNAME are a
// fallback for stripped environments (non-login CI shells often lack them).
func currentUserName() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return os.Getenv("LOGNAME")
}
