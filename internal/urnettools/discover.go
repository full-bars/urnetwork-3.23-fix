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
// binary. Matches on basename to be resilient to custom install paths.
func isProviderArg(arg string) bool {
	base := filepath.Base(arg)
	// Strip a trailing .exe (Windows) defensively.
	base = strings.TrimSuffix(base, ".exe")
	return knownBinaries[base]
}

// discoverSystemdUnits scans systemd for provider units (running or stopped)
// and fills in Provider records for any unit not already represented by a
// live process. Unit User= is read from the unit file; state dir follows the
// install convention.
func discoverSystemdUnits(running []Provider) []Provider {
	cmd := exec.Command("systemctl", "list-units", "--all", "--no-legend", "--no-pager")
	out, err := cmd.Output()
	if err != nil {
		return nil // no systemd (container/other init) — process scan is enough
	}
	seenPID := map[int]bool{}
	seenUnit := map[string]bool{}
	for _, p := range running {
		seenPID[p.PID] = true
	}
	// unitUser maps unit name -> User= value, resolved on demand.
	unitUser := func(unit string) string {
		if seenUnit[unit] {
			return "" // already resolved, no-op guard
		}
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
		base := unit
		if i := strings.IndexByte(base, '.'); i >= 0 {
			base = base[:i]
		}
		if !isProviderArg(base) {
			continue
		}
		// Skip units already backed by a running process (matched by unit
		// name via the provider's Unit field, set below).
		matched := false
		for i := range running {
			if running[i].Unit == unit {
				matched = true
				break
			}
		}
		if matched {
			continue
		}
		u := unitUser(unit)
		p := Provider{
			User:     u,
			StateDir: unitStateDir(u),
			Unit:     unit,
			Running:  false,
		}
		if p.StateDir == "" {
			// No resolvable state dir: skip the JWT read rather than
			// decoding from a relative "jwt" path (free-review major).
			out2 = append(out2, p)
			continue
		}
		p.Network, p.NetworkID, p.JWTExpires, _ = decodeJWT(filepath.Join(p.StateDir, "jwt"))
		out2 = append(out2, p)
	}
	return out2
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
