package urnettools

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ramlogsFreshWindow is how recently the RAM buffer must have been written for
// its existence to count as evidence. The provider syncs it twice a second
// while running, so a live redirect is always well inside this. The window
// exists so a stale file left behind by a provider that has since been
// reconfigured, or by a container that no longer runs, is not mistaken for an
// active redirect.
const ramlogsFreshWindow = 2 * time.Minute

// ramlogsEnvKey is the environment variable the provider itself reads to
// decide whether to redirect its output into /dev/shm.
const ramlogsEnvKey = "URNETWORK_RAMLOGS"

// ramLogPathsFor returns the RAM buffer paths to consider for a provider,
// newest convention first. The provider writes /dev/shm/<binary basename>.log
// so multi-provider boxes do not conflate outputs, and older builds wrote the
// shared /dev/shm/urnetwork.log.
func ramLogPathsFor(p Provider) []string {
	paths := []string{}
	if p.Binary != "" {
		paths = append(paths, "/dev/shm/"+filepath.Base(p.Binary)+".log")
	}
	return append(paths, "/dev/shm/urnetwork.log")
}

// ramlogFileActive reports whether a RAM buffer for this provider exists and
// has been written to recently.
//
// This is the provider's own observable behavior rather than an inference from
// stored configuration, which is why it is consulted before the environment:
// whatever the config says, a file being appended to twice a second is where
// the logs are actually going. Only meaningful for a running provider, since a
// stopped one is not writing anywhere.
func ramlogFileActive(p Provider) bool {
	for _, path := range ramLogPathsFor(p) {
		if ramlogFileFresh(path, p.Running) {
			return true
		}
	}
	return false
}

// ramlogFileFresh holds the decision rules for one path so they can be tested
// without writing into the host's /dev/shm: the provider must be running, the
// buffer must exist with content, and it must have been written to inside
// ramlogsFreshWindow.
func ramlogFileFresh(path string, running bool) bool {
	if !running {
		return false
	}
	fi, err := os.Stat(path)
	if err != nil || fi.IsDir() || fi.Size() == 0 {
		return false
	}
	return time.Since(fi.ModTime()) <= ramlogsFreshWindow
}

// unitEnvironment returns the unit's effective Environment= assignments, which
// cover the unit body and every drop-in. It deliberately does not try to read
// EnvironmentFile= contents; systemd does not expand those into this property,
// and the file check above already covers a provider that is redirecting for
// any reason at all.
func unitEnvironment(p Provider) (string, error) {
	if p.Unit == "" {
		return "", nil
	}
	var args []string
	if isUserUnit(p.Unit) && p.User != "" {
		args = append([]string{"systemctl"}, systemctlUserArgs(p.User)...)
		args = append(args, "show", "-p", "Environment", "--value", p.Unit)
	} else {
		args = []string{"systemctl", "show", "-p", "Environment", "--value", p.Unit}
	}
	out, err := exec.Command(args[0], args[1:]...).Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// ramlogsEnvEnabled parses a systemd Environment= value and reports whether it
// turns RAMLOGS on. The property is a single space-separated line of KEY=VALUE
// pairs, with values quoted when they contain spaces.
func ramlogsEnvEnabled(environment string) bool {
	for _, field := range strings.Fields(environment) {
		field = strings.Trim(field, `"'`)
		key, value, found := strings.Cut(field, "=")
		if !found || strings.TrimSpace(key) != ramlogsEnvKey {
			continue
		}
		if truthyOn(strings.ToLower(strings.Trim(strings.TrimSpace(value), `"'`))) {
			return true
		}
	}
	return false
}
