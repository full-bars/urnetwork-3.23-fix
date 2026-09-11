package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// withExecutableEnv swaps the package's executable-resolution seams and the
// captured install path for the duration of a test.
func withExecutableEnv(t *testing.T, captured string, running string, runningErr error) {
	t.Helper()
	origInstall := installPath
	origExecutable := executableFunc
	origStat := statFunc
	t.Cleanup(func() {
		installPath = origInstall
		executableFunc = origExecutable
		statFunc = origStat
	})
	installPath = captured
	executableFunc = func() (string, error) { return running, runningErr }
	statFunc = os.Stat
}

// The update flow installs the new build at the provider's binary path and
// moves the running one to a backup. From then on os.Executable() names the
// backup, so a handoff must launch the captured install path or it re-executes
// the build it was meant to replace.
func TestHotSwapExecutablePathPrefersInstallPathAfterBinaryMoved(t *testing.T) {
	dir := t.TempDir()
	install := filepath.Join(dir, "urnetwork")
	backup := filepath.Join(dir, "urnetwork.bak-20260911T100157Z")
	for _, p := range []string{install, backup} {
		if err := os.WriteFile(p, []byte("binary"), 0o755); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	withExecutableEnv(t, install, backup, nil)

	got, err := hotSwapExecutablePath()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != install {
		t.Fatalf("expected the handoff to launch the install path %s, got %s", install, got)
	}
}

// Nothing moved: both agree, and the answer is that path.
func TestHotSwapExecutablePathUnchangedBinary(t *testing.T) {
	dir := t.TempDir()
	install := filepath.Join(dir, "urnetwork")
	if err := os.WriteFile(install, []byte("binary"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	withExecutableEnv(t, install, install, nil)

	got, err := hotSwapExecutablePath()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != install {
		t.Fatalf("expected %s, got %s", install, got)
	}
}

// A missing install path aborts. Falling back to the running image would
// re-execute the build the handoff is meant to replace while reporting a clean
// zero-downtime swap, which is the failure this function exists to remove.
func TestHotSwapExecutablePathAbortsWhenInstallPathGone(t *testing.T) {
	dir := t.TempDir()
	install := filepath.Join(dir, "urnetwork")
	running := filepath.Join(dir, "urnetwork.bak")
	if err := os.WriteFile(running, []byte("binary"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	withExecutableEnv(t, install, running, nil)

	got, err := hotSwapExecutablePath()
	if err == nil {
		t.Fatalf("expected an abort when the install path is gone, got %s", got)
	}
	if got != "" {
		t.Fatalf("expected no path on abort, got %s", got)
	}
}

// An install path that exists but cannot be executed aborts too: an updater
// that has written the binary but not yet marked it executable would otherwise
// spawn a candidate that dies with a permission error.
func TestHotSwapExecutablePathAbortsWhenInstallPathNotExecutable(t *testing.T) {
	dir := t.TempDir()
	install := filepath.Join(dir, "urnetwork")
	if err := os.WriteFile(install, []byte("binary"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	withExecutableEnv(t, install, install, nil)

	if _, err := hotSwapExecutablePath(); err == nil {
		t.Fatal("expected an abort when the install path is not executable")
	}
}

// An install path replaced by a directory aborts rather than being spawned.
func TestHotSwapExecutablePathAbortsWhenInstallPathIsDirectory(t *testing.T) {
	dir := t.TempDir()
	install := filepath.Join(dir, "urnetwork")
	if err := os.MkdirAll(install, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	withExecutableEnv(t, install, install, nil)

	if _, err := hotSwapExecutablePath(); err == nil {
		t.Fatal("expected an abort when the install path is a directory")
	}
}

// readlink on /proc/self/exe appends " (deleted)" once the running binary is
// unlinked, and os.Executable hands that back with a nil error. The suffix must
// never reach exec.
func TestHotSwapExecutablePathStripsDeletedSuffix(t *testing.T) {
	dir := t.TempDir()
	running := filepath.Join(dir, "urnetwork")
	if err := os.WriteFile(running, []byte("binary"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	withExecutableEnv(t, "", running+" (deleted)", nil)

	got, err := hotSwapExecutablePath()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != running {
		t.Fatalf("expected the deleted suffix stripped, got %s", got)
	}
}

// Nothing was captured at startup, so the running image is all there is.
func TestHotSwapExecutablePathWithoutCapturedInstallPath(t *testing.T) {
	dir := t.TempDir()
	running := filepath.Join(dir, "urnetwork")
	if err := os.WriteFile(running, []byte("binary"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	withExecutableEnv(t, "", running, nil)

	got, err := hotSwapExecutablePath()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != running {
		t.Fatalf("expected the running image, got %s", got)
	}
}

// Neither path is resolvable, so the handoff must abort rather than guess.
func TestHotSwapExecutablePathErrorsWhenNothingResolves(t *testing.T) {
	dir := t.TempDir()
	withExecutableEnv(t, filepath.Join(dir, "missing"), "", errors.New("no exe"))

	if _, err := hotSwapExecutablePath(); err == nil {
		t.Fatal("expected an error when neither the install path nor the running image resolves")
	}
}

// The captured path must name the real binary, not a synthetic link, so a
// handoff can stat it and spawn it.
func TestInstallPathCapturedAtStartupIsAResolvedFile(t *testing.T) {
	if installPath == "" {
		t.Skip("no executable path available in this environment")
	}
	if _, err := os.Stat(installPath); err != nil {
		t.Fatalf("captured install path %s is not usable: %v", installPath, err)
	}
	if installPath == "/proc/self/exe" {
		t.Fatal("captured install path must be resolved, not the /proc symlink")
	}
}
