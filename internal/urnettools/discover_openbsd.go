package urnettools

import (
	"errors"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"time"
)

// OpenBSD ps ucomm is the kernel accounting name, not the mutable argv.
// It does not expose a trustworthy executable path, so Binary stays empty:
// update and uninstall must not guess a path from process arguments.
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
			}
		}
		providers = append(providers, p)
	}
	return providers
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
