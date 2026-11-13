package urnettools

import (
	"errors"
	"io/fs"
	"os"
	"os/user"
	"strings"
	"syscall"
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
	managedPID := openbsdServicePID()
	var providers []Provider
	for _, row := range parseOpenBSDProcesses(string(out)) {
		p := Provider{PID: row.pid, Running: true}
		// Only the process rc.d uniquely matches owns the installed service
		// binary. Any other process named like the provider (a manual run
		// by another user) gets no binary, so service control can never be
		// routed from its row to the installed service.
		managed := managedPID > 0 && row.pid == managedPID
		if managed {
			p.Supervisor = "openbsd-rcd"
			p.Binary = installedOpenBSDProvider()
		}
		if u, err := user.LookupId(row.uid); err == nil {
			p.User = u.Username
			p.StateDir = resolveDiscoveredStateDir(u.HomeDir, "")
			if p.StateDir != "" {
				p.StateHome = u.HomeDir
				// Pin the state directory to an open handle before reading the
				// JWT, so a symlink swapped in after any check cannot redirect
				// the read to another account's home.
				h, herr := openStateDirIn(p.StateHome, p.StateDir)
				if herr != nil {
					p.StateDir = ""
				} else {
					raw, rerr := h.readFile("jwt", maxJWTBytes)
					h.Close()
					if rerr != nil {
						p.IdentityRestricted = errors.Is(rerr, fs.ErrPermission)
					} else {
						net, id, exp, derr := decodeJWTBytes(raw)
						p.Network = net
						p.NetworkID = id
						p.JWTExpires = exp
						p.IdentityRestricted = derr != nil && errors.Is(derr, fs.ErrPermission)
					}
				}
			}
		}
		providers = append(providers, p)
	}
	return providers
}

// openbsdLibexecProviderPath is the only binary path accepted. urnet-tools
// often runs as root, so a binary in a user-writable home must never be
// resolved and executed.
var openbsdLibexecProviderPath = "/usr/local/libexec/urnetwork-provider/bin/urnetwork"

// openbsdProviderOwnerUID is the uid that must own the provider binary. It is
// a var so tests, which cannot create root-owned files, can override it.
var openbsdProviderOwnerUID uint32 = 0

func installedOpenBSDProvider() string {
	path := openbsdLibexecProviderPath
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return ""
	}
	// Must be owned by root: a binary owned by another user could be swapped
	// by that user and then executed by root.
	if st, ok := info.Sys().(*syscall.Stat_t); !ok || st.Uid != openbsdProviderOwnerUID {
		return ""
	}
	// Must be executable by others and not writable by group or others.
	perm := info.Mode().Perm()
	if perm&0o001 == 0 || perm&0o022 != 0 {
		return ""
	}
	if unix.Access(path, unix.X_OK) == nil {
		return path
	}
	return ""
}

// stateDirIsSymlink reports whether dir itself is a symbolic link.
func stateDirIsSymlink(dir string) bool {
	if dir == "" {
		return false
	}
	fi, err := os.Lstat(dir)
	return err == nil && fi.Mode()&os.ModeSymlink != 0
}

// openbsdRcDaemonUser returns the account the installed rc.d script runs the
// provider as, or "" when the script is missing or its value is not a plain
// account name. It is a var so tests need no /etc/rc.d.
var openbsdRcDaemonUser = func() string {
	data, err := os.ReadFile(openbsdRcScriptPath("urnetwork"))
	if err != nil {
		return ""
	}
	return parseOpenBSDRcDaemonUser(string(data))
}

// parseOpenBSDRcDaemonUser extracts daemon_user="<name>" from an rc.d script.
func parseOpenBSDRcDaemonUser(script string) string {
	for _, line := range strings.Split(script, "\n") {
		v, ok := strings.CutPrefix(line, `daemon_user="`)
		if !ok {
			continue
		}
		name, ok := strings.CutSuffix(v, `"`)
		if !ok || name == "" || name == "root" {
			return ""
		}
		for i, r := range name {
			if r >= 'a' && r <= 'z' || r == '_' || i > 0 && (r >= '0' && r <= '9' || r == '-') {
				continue
			}
			return ""
		}
		return name
	}
	return ""
}

// discoverStopped returns the installed rc.d service as a stopped provider so
// start has a target. It adds a row only when no running provider is already
// the rc.d-managed one, the binary passes the root-ownership checks, and the
// script names a real service user that owns a non-symlink state directory.
func discoverStopped(running []Provider) []Provider {
	for _, r := range running {
		if r.Supervisor == "openbsd-rcd" {
			return nil
		}
	}
	bin := installedOpenBSDProvider()
	if bin == "" {
		return nil
	}
	name := openbsdRcDaemonUser()
	if name == "" {
		return nil
	}
	u, err := user.Lookup(name)
	if err != nil {
		return nil
	}
	stateDir := resolveDiscoveredStateDir(u.HomeDir, "")
	if stateDir == "" || stateDirIsSymlink(stateDir) {
		return nil
	}
	p := Provider{User: u.Username, StateDir: stateDir, StateHome: u.HomeDir, Binary: bin, Supervisor: "openbsd-rcd"}
	h, err := openStateDirIn(p.StateHome, p.StateDir)
	if err == nil {
		var raw []byte
		raw, err = h.readFile("jwt", maxJWTBytes)
		h.Close()
		if err == nil {
			p.Network, p.NetworkID, p.JWTExpires, _ = decodeJWTBytes(raw)
		}
	}
	p.IdentityRestricted = errors.Is(err, fs.ErrPermission)
	return []Provider{p}
}

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
