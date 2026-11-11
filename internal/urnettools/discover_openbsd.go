package urnettools

import (
	"errors"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

// OpenBSD ps ucomm identifies a process by name, not its executable path.
// A standard install path is only an on-disk candidate, not proof of the
// running image. Never derive a path from process arguments.
func discoverProcesses() []Provider {
	out, err := execWithTimeout(5*time.Second, "ps", "-A", "-o", "pid=,uid=,ucomm=")
	if err != nil {
		return nil
	}
	var providers []Provider
	for _, row := range parseOpenBSDProcesses(string(out)) {
		p := Provider{PID: row.pid, Running: true}
		if u, err := user.LookupId(row.uid); err == nil {
			p.User = u.Username
			p.StateDir = resolveDiscoveredStateDir(u.HomeDir, "")
			if p.StateDir != "" {
				p.StateHome = u.HomeDir
				var jwtErr error
				p.Network, p.NetworkID, p.JWTExpires, jwtErr = decodeJWT(filepath.Join(p.StateDir, "jwt"))
				p.IdentityRestricted = errors.Is(jwtErr, fs.ErrPermission)
				p.Binary = installedOpenBSDProviderIn(u.HomeDir)
			}
		}
		providers = append(providers, p)
	}
	return providers
}

var openbsdLibexecProviderPath = "/usr/local/libexec/urnetwork-provider/bin/urnetwork"

func openbsdProviderCandidates(home string) []string {
	paths := []string{openbsdLibexecProviderPath}
	if home != "" {
		paths = append(paths,
			filepath.Join(home, ".local", "share", "urnetwork-provider", "bin", "urnetwork"),
			filepath.Join(home, ".urnetwork", "urnetwork"))
	}
	return paths
}

func installedOpenBSDProviderIn(home string) string {
	for _, path := range openbsdProviderCandidates(home) {
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if path == openbsdLibexecProviderPath && info.Mode().Perm()&0o001 == 0 {
			continue
		}
		if unix.Access(path, unix.X_OK) == nil {
			return path
		}
	}
	return ""
}

// Stopped services are not enumerated yet.
func discoverStopped(_ []Provider) []Provider { return nil }

func currentUserName() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return ""
}

func narrowToAccessible(providers []Provider) []Provider {
	name := currentUserName()
	var out []Provider
	for _, p := range providers {
		if name != "" && p.User == name {
			out = append(out, p)
		}
	}
	return out
}

func platformIsPrivileged() bool { return os.Geteuid() == 0 }
