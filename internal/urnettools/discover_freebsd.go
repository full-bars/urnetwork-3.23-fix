//go:build freebsd

package urnettools

import (
	"os"
	osuser "os/user"
	"path/filepath"
	"strconv"
	"time"
)

// discoverProcesses enumerates running provider processes on FreeBSD.
//
// FreeBSD has no /proc, so the process table comes from procstat(1), which is
// in the base system and reports the KERNEL's view: the loaded image path and
// the process credentials. Both matter for trust. The Linux path reads
// /proc/<pid>/exe (a link to the executing inode) and /proc/<pid>'s file
// ownership for exactly this reason — nothing a process prints about itself
// can be believed.
//
// What does NOT carry over is the container filtering. Linux skips processes
// whose mount namespace differs from the tool's AND whose cgroup names a
// container runtime, because a containerized provider must not surface as a
// host provider with a guessed state dir (the "ghost root provider" incident).
// FreeBSD jails are the near analogue, but they are not the same boundary: a
// jail has no per-jail mount-namespace-inode comparable to /proc/<pid>/ns/mnt,
// and a provider inside a jail legitimately shares the host uid space. Guessing
// here would risk either hiding real providers or admitting jailed ones with a
// host state dir, so discovery makes no jail claim and this is recorded as a
// known gap rather than silently approximated.
func discoverProcesses() []Provider {
	// `-a` is REQUIRED: procstat without a command or a pid list prints its
	// usage and exits non-zero. Calling `procstat -b` alone therefore yields
	// empty output on every call, discovery silently found nothing, and the
	// install-location fallback below covered for it — so the live-process path
	// was broken while the tool still printed a plausible inventory row.
	// A FreeBSD CI run with a real provider-named process is what caught this.
	rows := parseProcstatBasic(runProcstat("-a", "-b"))
	if len(rows) == 0 {
		return nil
	}
	creds := parseProcstatCredentials(runProcstat("-a", "-s"))

	var out []Provider
	for _, r := range rows {
		// The kernel-reported binary path is what identifies the process.
		// A "-" means procstat could not read it for this user; such a row
		// carries no evidence of anything, so it is not a provider.
		if r.path == "" || r.path == "-" {
			continue
		}
		if !isProviderArg(r.path) {
			continue
		}
		pid, err := strconv.Atoi(r.pid)
		if err != nil || pid <= 0 {
			continue
		}

		ownerUser, ownerHome := freebsdProcessOwner(creds[r.pid])
		// The username is the kernel credential's passwd name and nothing
		// else. The environ is never consulted for it, which is the same
		// precedence rule the Linux path applies and the reason
		// resolveDiscoveredStateDir can trust ownerHome: a process launched
		// as `USER=root exec -a provider ./evil` cannot claim root here,
		// because nothing this code reads is a string the process printed.
		//
		// An unresolvable uid leaves User empty rather than falling back to a
		// claimed USER=root; the row stays discoverable via --state-dir.
		p := Provider{
			User:     ownerUser,
			StateDir: resolveDiscoveredStateDir(ownerHome, ""),
			Binary:   r.path,
			PID:      pid,
			Running:  true,
		}
		if p.StateDir != "" {
			p.StateHome = ownerHome
		}
		if p.StateDir == "" {
			// No state dir resolvable: skip the JWT read entirely rather
			// than falling through to a relative "jwt" path in the
			// invoker's CWD (same rule as the Linux path).
			out = append(out, p)
			continue
		}
		p.Network, p.NetworkID, p.JWTExpires, _ = decodeJWT(filepath.Join(p.StateDir, "jwt"))

		// Version: ask the provider first, then the on-disk binary. The
		// Linux path has a third source (the /proc/<pid>/exe handle) that
		// names the loaded inode; FreeBSD has no equivalent link, so this
		// deliberately does not invent one — after an update the on-disk
		// binary is the NEW image, which is why HotSwap's own preflight
		// treats an unreadable running image as "cannot determine currency".
		if v, ok := providerVersionFromSocket(p); ok {
			p.Version = v
		}
		if p.Version == "" {
			p.Version = providerVersionFromBuildinfo(p.Binary)
		}
		out = append(out, p)
	}

	// No live process at all: fall back to the standard install location so
	// lifecycle commands still have a target on a stopped install, the way
	// the Darwin path does.
	if len(out) == 0 {
		if exe := installedBSDProvider(); exe != "" {
			out = append(out, Provider{
				User:     currentUserName(),
				StateDir: bsdStateDir(),
				Binary:   exe,
				Running:  false,
			})
		}
	}
	return out
}

// freebsdProcessOwner maps a procstat credential record to a passwd username
// and home directory.
//
// The EUID is preferred over the RUID because it is the identity the process
// actually runs with, matching how ps/top attribute a process and how the
// Linux path uses the /proc directory owner. When the EUID is unreadable the
// RUID is used if it is readable; when neither resolves to a passwd entry the
// owner is reported as unknown ("", "") rather than as root, so an
// unresolvable uid never becomes an unrestricted record.
func freebsdProcessOwner(c procstatCreds) (username, home string) {
	if !c.haveEUID && !c.haveRUID {
		return "", ""
	}
	var uid uint32
	switch {
	case c.haveEUID:
		uid = c.euid
	case c.haveRUID:
		uid = c.ruid
	default:
		return "", ""
	}
	u, err := osuser.LookupId(strconv.FormatUint(uint64(uid), 10))
	if err != nil || u.Username == "" {
		return "", ""
	}
	return u.Username, u.HomeDir
}

// runProcstat runs procstat with the given flags and returns its stdout.
// A missing or failing procstat yields empty output rather than an error:
// discovery then reports no providers, which is the honest outcome when the
// process table cannot be read, and keeps the caller free of error plumbing.
func runProcstat(args ...string) string {
	out, err := execWithTimeout(5*time.Second, "procstat", args...)
	if err != nil {
		return ""
	}
	return string(out)
}

// installedBSDProvider locates the provider in the standard install location
// used by the FreeBSD installer script.
func installedBSDProvider() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	for _, c := range bsdProviderCandidates(home) {
		if info, err := os.Stat(c); err == nil && !info.IsDir() {
			return c
		}
	}
	return ""
}

// bsdProviderCandidates are the install paths bsdInstallerPaths installs to,
// in priority order. Kept as a pure function of the home directory so the
// installer script and this lookup cannot drift apart silently.
func bsdProviderCandidates(home string) []string {
	if home == "" {
		return nil
	}
	return []string{
		filepath.Join(home, ".local", "share", "urnetwork-provider", "bin", "urnetwork"),
		filepath.Join(home, ".urnetwork", "urnetwork"),
	}
}

// bsdStateDir is the provider state dir on FreeBSD.
func bsdStateDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".urnetwork")
}

// discoverStopped on FreeBSD returns nil for the same reason Darwin's does:
// rc.d services have no enumeration the tool can read the way systemd user
// units can, and a stopped install is already covered by the standard
// install-location fallback in discoverProcesses.
func discoverStopped(running []Provider) []Provider {
	return nil
}

// currentUserName returns the invoking user's login name.
func currentUserName() string {
	if u, err := osuser.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return os.Getenv("LOGNAME")
}

// narrowToAccessible filters providers to those the invoking user owns, so a
// read-only command can auto-pick the sole reachable provider instead of
// refusing with "N providers found" when the other N-1 belong to accounts the
// caller cannot act on.
//
// A blank p.User means the owner's credential lookup failed — the owner is
// unknown, not unrestricted — so blank rows are excluded rather than treated
// as belonging to the current user.
func narrowToAccessible(providers []Provider) []Provider {
	current := currentUserName()
	if current == "" {
		return nil
	}
	var out []Provider
	for _, p := range providers {
		if p.User != "" && p.User == current {
			out = append(out, p)
		}
	}
	return out
}

// platformIsPrivileged on freebsd: euid==0 (root).
func platformIsPrivileged() bool {
	return os.Geteuid() == 0
}
