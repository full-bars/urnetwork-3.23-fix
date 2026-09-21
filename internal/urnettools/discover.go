package urnettools

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// knownBinaries are the binary basenames the tool recognizes as URnetwork
// providers. provider_beta is the beta-test build name used on fleet boxes.
var knownBinaries = map[string]bool{
	"urnetwork":      true,
	"urnet-provider": true,
	"provider_beta":  true,
	"provider":       true,
}

// providerCandidateUsers returns the usernames of users that show
// evidence of a provider install: a provider-looking unit under
// ~/.config/systemd/user or a ~/.urnetwork state dir. Best-effort.
// Usernames are returned (not home paths) because discoverUserUnits uses
// them as `systemctl --user -M <user>@` selectors.
func providerCandidateUsers() ([]string, error) {
	b, err := execWithTimeout(5*time.Second, "getent", "passwd")
	if err != nil {
		return nil, err
	}
	var users []string
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Split(line, ":")
		if len(fields) < 6 || fields[5] == "" {
			continue
		}
		user := fields[0]
		home := fields[5]
		// User-level unit dir with a provider unit, or a state dir.
		found := false
		for known := range knownBinaries {
			unitGlob := filepath.Join(home, ".config/systemd/user", known+"*.service")
			if matches, _ := filepath.Glob(unitGlob); len(matches) > 0 {
				users = append(users, user)
				found = true
				break
			}
		}
		if found {
			continue
		}
		if _, err := os.Stat(filepath.Join(home, ".urnetwork")); err == nil {
			users = append(users, user)
		}
	}
	return users, nil
}

// isProviderUnit reports whether a systemd unit name looks like a provider
// unit (basename matches a known binary, optionally suffixed).
func isProviderUnit(unit string) bool {
	// Only a .service can be a provider. Timers, sockets, paths and targets
	// share the provider's name prefix on an ordinary install
	// (urnetwork-update.timer ships with it) and on any box running sibling
	// tooling, and a name-only match swept them in as phantom providers:
	// observed live 2026-09-09, where `urnet-tools logs` listed four
	// "providers" on a box with one, two of them timers.
	base, ok := strings.CutSuffix(unit, ".service")
	if !ok {
		return false
	}
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

// alreadyBackedByRunning reports whether a systemd unit is already
// represented by a running provider.  Checks unit name first (fast),
// then falls back to state-dir matching for user-level systemd where
// attachUnits cannot parse the cgroup to set Provider.Unit.
func alreadyBackedByRunning(running []Provider, unit string, userFor func(string) string) bool {
	if unitIn(running, unit) {
		return true
	}
	if userFor == nil {
		return false
	}
	user := userFor(unit)
	sd := unitStateDir(user)
	if sd == "" {
		return false
	}
	for i := range running {
		if running[i].Unit == "" && running[i].StateDir == sd && running[i].User == user {
			return true
		}
	}
	return false
}

// providerFromUnit builds a Provider record for a (possibly stopped) unit.
//
// binary is the unit's ExecStart executable, resolved by the caller (it needs
// the right systemctl scope: system, --user, or --user -M <user>@). Without
// it a stopped provider carries an empty Provider.Binary, and every operation
// that has to touch the executable refuses outright -- `urnet-tools update`
// aborts with "no resolvable binary path -- nothing updated", so a provider
// that is merely stopped cannot be updated at all.
func providerFromUnit(unit, user, binary string) Provider {
	p := Provider{
		User:     user,
		StateDir: unitStateDir(user),
		Unit:     unit,
		Binary:   binary,
		Running:  false,
	}
	if p.StateDir == "" {
		return p // no resolvable state dir — listed, but ungraded
	}
	jwtPath := filepath.Join(p.StateDir, "jwt")
	netName, netID, exp, jwtErr := decodeJWT(jwtPath)
	p.Network, p.NetworkID, p.JWTExpires = netName, netID, exp
	if jwtErr != nil {
		// Distinguish "no identity" from "identity unreadable": a
		// permission error on another account's state dir (or on the jwt
		// file itself) must not print as a blank-but-valid net= field
		// (LA1 6c). Go 1.27 maps os.ReadFile's *os.PathError to
		// fs.ErrPermission, so a permission-denied on the jwt file itself
		// is now caught directly — the old readableByCurrentUser probe
		// missed that case (it only checked the parent dir). Any other
		// decode failure (missing/corrupt) keeps the old silent-empty
		// behavior.
		if errors.Is(jwtErr, fs.ErrPermission) {
			p.IdentityRestricted = true
		}
	}
	return p
}

// discoverProcessesFn and discoverStoppedFn are stub seams so tests can
// isolate Discover() from the host's /proc scan and systemd enumeration.
var (
	discoverProcessesFn = discoverProcesses
	discoverStoppedFn   = discoverStopped
)

// Discover returns every provider on the box: running processes across all
// users plus stopped systemd units. Sorted by user then unit for stable
// output.
func Discover() []Provider {
	all := discoverProcessesFn()
	// Platform hook for stopped-unit / lifecycle-based discovery. On Linux
	// this attaches systemd unit names to running providers and adds
	// stopped provider units; on macOS/Windows it is a no-op (those
	// platforms have no systemd units to enumerate).
	all = append(all, discoverStoppedFn(all)...)
	all = dedupRunningProviders(all)
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].User != all[j].User {
			return all[i].User < all[j].User
		}
		if all[i].Unit != all[j].Unit {
			return all[i].Unit < all[j].Unit
		}
		if all[i].StateDir != all[j].StateDir {
			return all[i].StateDir < all[j].StateDir
		}
		return all[i].PID < all[j].PID
	})
	return all
}

// dedupRunningProviders collapses RUNNING rows that share one
// (User, StateDir) into a single row. One state dir is owned by at most one
// live provider — the provider's own state lock makes a second process on
// the same state dir impossible — so a duplicate RUNNING row is either the
// same provider discovered through two sources (process + attached unit) or
// a same-prefix non-provider process that borrowed the default state-dir
// attribution (live case: `provider_tracker` next to the real provider shows
// an identical user/net/state-dir row and jams every selection path with
// false ambiguity). Survivor preference, strongest evidence first:
//  1. unit-backed row (systemd is the management contract, so the survivor
//     must keep the unit name for logs/stop routing)
//  2. exact known provider binary
//  3. control socket reachable (only a real provider listens there)
//  4. first row wins on a tie (stable)
//
// STOPPED rows are never collapsed: a stopped unit alongside a running
// manual process legitimately shares the state dir and both must stay
// visible.
func dedupRunningProviders(providers []Provider) []Provider {
	type key struct{ user, stateDir string }
	out := make([]Provider, 0, len(providers))
	byKey := map[key]int{}
	for _, p := range providers {
		if !p.Running || p.StateDir == "" || p.User == "" {
			out = append(out, p)
			continue
		}
		k := key{p.User, p.StateDir}
		if j, ok := byKey[k]; ok {
			// A better-evidenced challenger replaces the survivor.
			if runningProviderRank(out[j]) < runningProviderRank(p) {
				out[j] = p
			}
			continue
		}
		byKey[k] = len(out)
		out = append(out, p)
	}
	return out
}

// runningProviderRank orders the survival evidence for
// dedupRunningProviders; higher wins. A unit-backed row is the strongest
// (systemd owns lifecycle and logs), then an exact known binary name, then
// a reachable control socket.
func runningProviderRank(p Provider) int {
	if p.Unit != "" {
		return 30
	}
	if isExactProviderBinary(p.Binary) {
		return 20
	}
	if controlSocketReachable(p) {
		return 10
	}
	return 0
}

// nonProviderSiblingSuffixes are "-<suffix>" segments that follow a known
// provider binary name in a unit/exe name but denote a DIFFERENT, non-
// provider service that happens to share the prefix — never a provider
// itself, so isProviderArg must exclude it rather than match it. Checked as
// a suffix (-update) or as the segment immediately after the "-"
// (-dashboard, -dashboard-py, -dashboard-rs, ...) since dashboard apps get
// their own per-language unit names.
//
// dashboard: live fleet false-positive (2026-08-17) — a box running
// provider-dashboard{,-py,-rs}.service (unrelated monitoring services, not
// providers) had them swept into discovery, flooding the same-user
// candidate list and permanently blocking narrowToAccessible's auto-pick.
// tracker: live fleet false-positive (2026-09-20) — a box running
// provider_tracker (the provider-dashboard-rs companion process) showed up
// as a duplicate user@mesocyclone provider, confusing default/update/paste
// and blocking narrowToAccessible's auto-pick.
var nonProviderSiblingSuffixes = []string{"hub", "update", "sentinel", "dashboard", "tracker"}

// isProviderArg reports whether an executable path/name is a known provider
// binary. Matches on basename to be resilient to custom install paths, and
// by PREFIX so suffixed unit names (urnetwork-native.service,
// provider_beta-custom) are recognized too. Excludes known
// non-provider siblings (see nonProviderSiblingSuffixes) so their units are
// not mistaken for providers.
// NOTE: a bare "provider" binary is NOT corroborated by state-dir or unit
// here — that happens downstream in Process() when homeForUser/state-dir
// argv fail; this is a deliberate design choice for custom install paths.
func isProviderArg(arg string) bool {
	base := filepath.Base(arg)
	// Strip a trailing .exe (Windows) defensively.
	base = strings.TrimSuffix(base, ".exe")
	// Match case-insensitively. Windows process names (toolhelp) report the
	// on-disk exe name, which is case-insensitive on NTFS — a binary installed
	// or renamed as "Urnetwork.exe" / "URNETWORK.EXE" must still match.
	// Lowercasing the input is safe on all platforms: the knownBinaries keys
	// are themselves lowercase, and Linux names we care about are lowercase.
	base = strings.ToLower(base)
	for known := range knownBinaries {
		// Both separators are in use. Systemd units and custom installs use a
		// hyphen (urnetwork-native), while the Docker image names the binary
		// by architecture and flavour with underscores
		// (urnetwork_amd64_stable). Matching only the hyphen form meant the
		// tool found nothing at all inside a container.
		if base != known &&
			!strings.HasPrefix(base, known+"-") &&
			!strings.HasPrefix(base, known+"_") {
			continue
		}
		rest := strings.TrimPrefix(base, known)
		// Normalize the separator before the sibling check, so a sibling
		// cannot slip through by being written with the other one.
		rest = strings.ReplaceAll(rest, "_", "-")
		for _, sibling := range nonProviderSiblingSuffixes {
			if rest == "-"+sibling || strings.HasPrefix(rest, "-"+sibling+"-") {
				return false
			}
		}
		return true
	}
	return false
}

// isExactProviderBinary reports whether arg matches a known binary name
// exactly (case-insensitive), without the prefix-with-suffix pattern that
// isProviderArg allows.  Strips .service (systemd unit) and .exe (Windows)
// before comparing.  Used to decide whether the name-alone fallback is
// trustworthy: an exact match (e.g. "urnetwork") is a provider by
// definition, while a prefix match (e.g. "urnetwork-rebind-fairfax2")
// needs corroborating evidence.
func isExactProviderBinary(arg string) bool {
	base := filepath.Base(arg)
	base = strings.TrimSuffix(base, ".exe")
	base = strings.TrimSuffix(base, ".service")
	base = strings.ToLower(base)
	return knownBinaries[base]
}

// dirExists reports whether path exists and is a directory.  Used as
// secondary evidence that a unit is a provider when ExecStart is not
// readable: a service without a .urnetwork state dir is not a provider
// regardless of its name.
func dirExists(path string) bool {
	if path == "" {
		return false
	}
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}
