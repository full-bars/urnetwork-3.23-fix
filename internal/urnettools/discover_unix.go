//go:build linux

package urnettools

import (
	"fmt"
	"os"
	osuser "os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
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
		// A running binary whose on-disk file was deleted (e.g. a prior
		// interrupted update) resolves to a "<path> (deleted)" target. Record
		// this BEFORE stripping the suffix so the update path can detect a
		// stale process whose binary was swapped out by a prior partial update.
		// Then strip the kernel's " (deleted)" marker so Provider.Binary stays
		// the canonical executable path; otherwise installBinary/backup later
		// write to a literal "... (deleted)" path and the service's real
		// binary stays missing.
		_, binaryDeleted := strings.CutSuffix(exe, " (deleted)")
		exe = strings.TrimSuffix(exe, " (deleted)")
		env := readEnviron(pid)
		user := env["USER"]
		if user == "" {
			user = env["LOGNAME"]
		}
		stateDir := stateDirFor(env)
		if user == "" || stateDir == "" {
			// environ is unreadable for another user's process (no
			// permission without root/CAP_SYS_PTRACE) — the common case
			// on a multi-provider box run by a non-root operator. Fall
			// back to the /proc/<pid> directory's own ownership, which is
			// always stat-able cross-user (how `ps`/`top` work without
			// root), so a provider under a different account is still
			// identified instead of showing up as an untargetable blank
			// row (--user/--state-dir would have nothing to match).
			if ownerUser, ownerHome := processOwner(pid); ownerUser != "" {
				if user == "" {
					user = ownerUser
				}
				if stateDir == "" && ownerHome != "" {
					stateDir = filepath.Join(ownerHome, ".urnetwork")
				}
			}
		}
		p := Provider{
			User:          user,
			StateDir:      stateDir,
			Binary:        exe,
			PID:           pid,
			Running:       true,
			BinaryDeleted: binaryDeleted,
		}
		// A provider may carry its own --state-dir flag; honor it. Both
		// the space ("--state-dir <path>") and equals
		// ("--state-dir=<path>") forms are accepted; the equals form
		// is the standard Unix convention and was previously missed
		// here, leaving state-dir-derived operations (set, fast-auth,
		// report, session save/load, proxy health/traffic, and
		// uninstall's RemoveAll) targeting the wrong directory.
		for i := 1; i < len(args); i++ {
			a := args[i]
			if a == "--state-dir" {
				if i+1 < len(args) {
					p.StateDir = args[i+1]
					break
				}
				continue
			}
			if v, ok := strings.CutPrefix(a, "--state-dir="); ok {
				p.StateDir = v
				break
			}
		}
		// H3 fix: validate --state-dir from foreign process argv. An attacker
		// can launch `exec -a provider ./x --state-dir=/home/victim` and the
		// tool would pick up that state-dir, leading to root RemoveAll into
		// an attacker-controlled path. Validate the path is under the
		// resolved process owner's home directory.
		//
		// Use processOwner (kernel uid via /proc/<pid>) instead of
		// osuser.Lookup(p.User) — the USER/LOGNAME env vars come from the
		// target process itself and are fully attacker-controlled. Also add
		// a separator guard so /var/lib/ur doesn't match /var/lib/urnetwork-evil.
		if p.StateDir != "" && p.PID > 0 {
			if _, home := processOwner(p.PID); home != "" {
				cleanHome := filepath.Clean(home)
				cleanState := filepath.Clean(p.StateDir)
				// Reject the state-dir if it is the owner's HOME itself (an
				// uninstall RemoveAll would wipe the entire home dir) OR not
				// under the home at all.
				if cleanState == cleanHome || !strings.HasPrefix(cleanState, cleanHome+string(filepath.Separator)) {
					// Invalid or dangerous state-dir (exact home, outside home).
					p.StateDir = ""
				}
			}
		}
		if p.StateDir == "" {
			// No state dir resolvable (HOME unset). Skip the JWT read
			// entirely rather than falling through to a relative "jwt"
			// path in the invoker's CWD.
			out = append(out, p)
			continue
		}
		p.Network, p.NetworkID, p.JWTExpires, _ = decodeJWT(filepath.Join(p.StateDir, "jwt"))
		// S-C1 fix: use buildinfo-only version resolution for discovered
		// processes. providerVersionFromExec execs the binary as root —
		// a local user can plant a real ELF via `exec -a urnetwork-provider
		// ./malicious` and the magic-byte check passes. Discovery must never
		// exec a foreign binary. Accept empty version for -trimpath builds.
		p.Version = providerVersionFromBuildinfo(p.Binary)
		out = append(out, p)
	}
	return out
}

// processOwner resolves the OS username and home directory that own pid via
// the /proc/<pid> directory's file ownership, not its environ. Reading
// another user's /proc/<pid>/environ requires root or CAP_SYS_PTRACE and
// fails for an unprivileged caller; stat on /proc/<pid> itself has no such
// restriction (it's how ps/top enumerate other users' processes), so this
// still identifies the process owner when environ is unreadable.
func processOwner(pid int) (username, home string) {
	fi, err := os.Stat(fmt.Sprintf("/proc/%d", pid))
	if err != nil {
		return "", ""
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return "", ""
	}
	u, err := osuser.LookupId(strconv.FormatUint(uint64(st.Uid), 10))
	if err != nil {
		return "", ""
	}
	return u.Username, u.HomeDir
}

// narrowToAccessible filters providers to those the current OS user can act
// on without root: cross-user systemd/journalctl queries need root or a
// machined session an unprivileged operator typically doesn't have. Used to
// auto-pick the sole reachable provider for read-only commands (logs)
// instead of refusing with "N providers found" when the other N-1 are
// running under different accounts the caller can't select anyway.
//
// A blank p.User means processOwner's owner lookup itself failed (e.g. a
// numeric UID with no matching passwd entry) — the owner is unknown, not
// unrestricted — so blank rows are excluded rather than treated as
// belonging to the current user.
func narrowToAccessible(providers []Provider) []Provider {
	current := currentUserName()
	var out []Provider
	for _, p := range providers {
		if p.User != "" && p.User == current {
			out = append(out, p)
		}
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
// a .urnetwork state dir), bounded to those users.
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
	out, err := execWithTimeout(5*time.Second, "systemctl", "--plain", "list-units", "--all", "--no-legend", "--no-pager")
	if err != nil {
		return nil // no systemd (container/other init) — process scan is enough
	}
	// list-units --all misses never-started units that exist on disk;
	// list-unit-files scans the unit paths and sees them. Merge both so a
	// freshly-installed (stopped) provider is discoverable.
	if fb, ferr := execWithTimeout(5*time.Second, "systemctl", "--plain", "list-unit-files", "--no-legend", "--no-pager"); ferr == nil {
		out = append(out, '\n')
		out = append(out, fb...)
	}
	// unitUser maps unit name -> User= value, resolved on demand.
	unitUser := func(unit string) string {
		b, err := execWithTimeout(5*time.Second, "systemctl", "show", unit, "-p", "User", "--value")
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(b))
	}
	return parseUnitLines(string(out), running, unitUser)
}

// parseUnitLines parses `systemctl list-units`/`list-unit-files` output
// (one unit per line, unit name in the first field) into deduplicated
// Provider entries, skipping non-provider units and units already backed by
// a running process. userFor resolves the User= value for a given unit name.
//
// Both discoverSystemUnits and discoverUserUnits merge list-units with
// list-unit-files by concatenating the two outputs (list-units --all misses
// never-started units that list-unit-files sees) — so any unit that is both
// loaded AND has a file on disk appears in both listings and would yield two
// identical Provider rows without the dedup here (observed live on a fleet
// box: every enabled-but-inactive unit doubled in `urnet-tools logs`'s
// ambiguity list).
func parseUnitLines(text string, running []Provider, userFor func(unit string) string) []Provider {
	var out []Provider
	seen := map[string]bool{}
	for _, line := range strings.Split(text, "\n") {
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
		if seen[unit] {
			continue
		}
		seen[unit] = true
		out = append(out, providerFromUnit(unit, userFor(unit)))
	}
	return out
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
			b, err = execWithTimeout(5*time.Second, "systemctl", "--user", "--plain", "list-units", "--all", "--no-legend", "--no-pager")
			if err == nil {
				// list-units --all misses never-started units that exist
				// on disk (a fresh fake/stopped provider); list-unit-files
				// scans the unit paths and sees them. Merge both.
				if fb, ferr := execWithTimeout(5*time.Second, "systemctl", "--user", "--plain", "list-unit-files", "--no-legend", "--no-pager"); ferr == nil {
					b = append(b, '\n')
					b = append(b, fb...)
				}
			}
		} else {
			// Cross-user query goes through machined/loginctl. If -M
			// fails (no machined, no lingering session — the norm for
			// service accounts), skip this user entirely. The old code
			// fell back to the current user's manager and labelled every
			// unit with the wrong user, creating ghost providers.
			b, err = execWithTimeout(10*time.Second, "systemctl", "--user", "-M", user+"@", "--plain", "list-units", "--all", "--no-legend", "--no-pager")
			if err != nil {
				continue
			}
		}
		if err != nil {
			continue // no session bus / user manager for this user
		}
		out = append(out, parseUnitLines(string(b), running, func(string) string { return user })...)
	}
	return out
}

// currentUserName returns the invoking user's login name, used to decide
// whether a user-manager query needs -M <user>@ (cross-user) or can use the
// local session bus. os/osuser.Current() is authoritative; USER/LOGNAME are a
// fallback for stripped environments (non-login CI shells often lack them).
func currentUserName() string {
	if u, err := osuser.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return os.Getenv("LOGNAME")
}

// platformIsPrivileged on linux: euid==0 (root). Non-root users are
// unprivileged and get the auto-default.
func platformIsPrivileged() bool {
	return os.Geteuid() == 0
}
