//go:build linux

package urnettools

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// openrcInitScriptKeys are the openrc-run assignments this package needs from
// a generated init script. Written literally by the installer as key="value"
// lines (scripts/Provider_Install_Linux.sh, install_openrc_units).
var openrcInitScriptKeys = map[string]bool{
	"command":      true,
	"command_user": true,
}

// discoverOpenRCService reports the installed OpenRC service as a STOPPED
// provider when no live process already represents it.
//
// Why this exists: discovery was process-scan plus systemd units, so on an
// OpenRC host a service that was installed but not yet started was invisible.
// Every command that picks a target through selectTarget — auth, update,
// uninstall, set, logs — then failed with "no providers found" on a fresh
// install, which is by definition stopped and unauthenticated. That is what
// made the first-run auth hop impossible and left `uninstall` unable to clean
// up a stopped service.
//
// The record it returns carries Supervisor: "openrc" and Unit: "", because
// OpenRC has no unit name the rest of the tool understands; the explicit
// Supervisor field is what distinguishes this from a bare process so cleanup
// can tell "the service's own provider" from "someone's manual launch".
//
// Parsing is deliberately narrow: only command= and command_user= are read,
// and the user's home is derived from the passwd entry rather than guessed from
// the binary path. A malformed or missing script yields (Provider{}, false) so
// discovery falls back to "no stopped service" instead of inventing a target.
func discoverOpenRCService(running []Provider) (Provider, bool) {
	if !openrcActive() || !openrcServiceInstalled() {
		return Provider{}, false
	}

	command, user := openrcInitScriptIdentity(openrcInitScriptPath)
	if command == "" {
		return Provider{}, false
	}

	// A live supervise-daemon child already covers this service in the running
	// list; do not report it twice.
	for _, p := range running {
		if p.Binary == command || (user != "" && p.User == user && p.Binary == command) {
			return Provider{}, false
		}
	}

	home := userHomeDir(user)
	if home == "" {
		return Provider{}, false
	}

	return Provider{
		User:       user,
		StateDir:   filepath.Join(home, ".urnetwork"),
		StateHome:  home,
		Binary:     command,
		Supervisor: "openrc",
		Running:    false,
		PID:        0,
	}, true
}

// openrcInitScriptIdentity extracts the provider binary path and the service
// user from an openrc-run script. Values may be quoted or bare; only the two
// keys in openrcInitScriptKeys are considered, and a `command_user` that is
// left at openrc-run's default (root) is reported as "root" rather than empty,
// because that is genuinely who the process runs as.
func openrcInitScriptIdentity(path string) (command, user string) {
	f, err := os.Open(path)
	if err != nil {
		return "", ""
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	// Init scripts are small; cap the line so a pathological file cannot make
	// the scanner allocate without bound.
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.Index(line, "=")
		if eq < 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		if !openrcInitScriptKeys[key] {
			continue
		}
		val := strings.TrimSpace(line[eq+1:])
		val = strings.Trim(val, `"'`)
		switch key {
		case "command":
			command = val
		case "command_user":
			user = val
		}
	}
	if user == "" {
		user = "root"
	}
	return command, user
}

// userHomeDir returns the home directory recorded in /etc/passwd for user, or
// "" when the user cannot be resolved. Reading passwd rather than inferring
// from the binary path keeps StateDir inside the user's own home, which is the
// invariant openStateDirIn enforces elsewhere.
func userHomeDir(user string) string {
	if user == "" {
		return ""
	}
	f, err := os.Open("/etc/passwd")
	if err != nil {
		return ""
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Split(sc.Text(), ":")
		if len(fields) < 6 || fields[0] != user {
			continue
		}
		home := fields[5]
		if home == "" || !filepath.IsAbs(home) {
			return ""
		}
		return home
	}
	return ""
}
