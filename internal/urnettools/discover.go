package urnettools

import (
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

// providerCandidateUsers returns the usernames of users that show
// evidence of a provider install: a provider-looking unit under
// ~/.config/systemd/user or a ~/.urnetwork state dir. Best-effort.
// Usernames are returned (not home paths) because discoverUserUnits uses
// them as `systemctl --user -M <user>@` selectors.
func providerCandidateUsers() ([]string, error) {
	b, err := exec.Command("getent", "passwd").Output()
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
	all := discoverProcesses()
	// Platform hook for stopped-unit / lifecycle-based discovery. On Linux
	// this attaches systemd unit names to running providers and adds
	// stopped provider units; on macOS/Windows it is a no-op (those
	// platforms have no systemd units to enumerate).
	all = append(all, discoverStopped(all)...)
	sort.Slice(all, func(i, j int) bool {
		if all[i].User != all[j].User {
			return all[i].User < all[j].User
		}
		return all[i].Unit < all[j].Unit
	})
	return all
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
