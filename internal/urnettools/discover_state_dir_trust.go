package urnettools

import (
	"path/filepath"
	"strings"
)

// This file holds the state-dir TRUST helpers shared by every platform that
// discovers providers from live processes (Linux via /proc, FreeBSD via
// kinfo.proc). They are deliberately platform-independent: the rule that a
// process may only claim a state directory inside the home directory the
// KERNEL attributes to it is a security boundary, and a second copy of it on
// another platform is a second chance to get it subtly wrong.

// resolveDiscoveredStateDir picks the state dir to attribute to a discovered
// process. ownerHome is the home directory the KERNEL's uid maps to; envDir is
// the process's own HOME-derived candidate, which the process controls.
//
//   - No trusted owner home: "" (never attribute a directory the process merely
//     claims, e.g. HOME=/root from a process whose uid has no passwd entry).
//   - Otherwise ownerHome/.urnetwork, unless envDir is a strict subdirectory of
//     the owner home that also stays inside it after symlinks are resolved
//     (a lexical prefix check alone accepts HOME=/home/alice/link where link
//     points outside the home).
func resolveDiscoveredStateDir(ownerHome, envDir string) string {
	if ownerHome == "" {
		return ""
	}
	if envDir != "" && stateDirInsideHome(ownerHome, envDir) {
		return envDir
	}
	return filepath.Join(ownerHome, ".urnetwork")
}

// stateDirInsideHome reports whether dir is a strict subdirectory of home,
// both lexically and after resolving symlinks. Equal to home is rejected (an
// uninstall RemoveAll would wipe the whole home). An empty home never contains
// anything. dir need not exist yet: the longest existing ancestor is resolved.
//
// The check and the later use are separate pathname lookups, so an owner who
// swaps a component in between can still redirect a read; file access under
// the resolved dir goes through the no-follow helpers to limit that to
// directory components the owner already controls.
func stateDirInsideHome(home, dir string) bool {
	if home == "" || dir == "" {
		return false
	}
	cleanHome, cleanDir := filepath.Clean(home), filepath.Clean(dir)
	sep := string(filepath.Separator)
	if cleanDir == cleanHome || !strings.HasPrefix(cleanDir, cleanHome+sep) {
		return false
	}
	realHome, err := filepath.EvalSymlinks(cleanHome)
	if err != nil {
		return false
	}
	realDir := evalExistingPrefix(cleanDir)
	return realDir != realHome && strings.HasPrefix(realDir, realHome+sep)
}

// evalExistingPrefix resolves symlinks in the longest existing ancestor of p
// and re-appends the not-yet-existing remainder.
func evalExistingPrefix(p string) string {
	rest := ""
	for cur := p; ; {
		if real, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(real, rest)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}
