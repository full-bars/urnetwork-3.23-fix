//go:build unix

package urnettools

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// simulateContainer points container detection at a temp marker that exists
// (inside) or not (outside).
func simulateContainer(t *testing.T, inside bool) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dockerenv")
	if inside {
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	orig := containerMarkerPath
	containerMarkerPath = path
	t.Cleanup(func() { containerMarkerPath = orig })
}

func TestUpdateRefusedUnderPelican(t *testing.T) {
	t.Setenv("PELICAN", "yes")
	err := updateProvider(Provider{Binary: filepath.Join(t.TempDir(), "urnetwork")}, updateConfig{Tag: "v1", Digest: "abc"})
	if !errors.Is(err, errPelicanUpdatesDisabled) {
		t.Fatalf("updateProvider under PELICAN=yes: got %v, want errPelicanUpdatesDisabled", err)
	}
}

func TestSelfUpdateRefusedUnderPelican(t *testing.T) {
	t.Setenv("PELICAN", "YES")
	err := selfUpdateToolTo(filepath.Join(t.TempDir(), "urnet-tools"), updateConfig{Tag: "v1", ToolDigest: "abc"})
	if !errors.Is(err, errPelicanUpdatesDisabled) {
		t.Fatalf("selfUpdateToolTo under PELICAN=YES: got %v, want errPelicanUpdatesDisabled", err)
	}
}

func TestUpdateNotGatedWithoutPelican(t *testing.T) {
	t.Setenv("PELICAN", "")
	err := updateProvider(Provider{Binary: filepath.Join(t.TempDir(), "urnetwork")}, updateConfig{Tag: "v1"})
	if errors.Is(err, errPelicanUpdatesDisabled) {
		t.Fatal("update was gated without PELICAN set")
	}
	if err == nil || !strings.Contains(err.Error(), "no sha256 digest") {
		t.Fatalf("expected the next gate (missing digest), got %v", err)
	}
}

func TestLifecycleRefusedInsideContainer(t *testing.T) {
	simulateContainer(t, true)
	cmds := map[string]func([]string, bool, bool) error{"start": cmdStart, "stop": cmdStop, "restart": cmdRestart}
	for verb, fn := range cmds {
		err := fn(nil, true, false)
		if err == nil || !strings.Contains(err.Error(), "docker "+verb) {
			t.Errorf("%s inside a container: got %v, want guidance to use docker %s", verb, err, verb)
		}
	}
}

func TestRefuseInContainerOutside(t *testing.T) {
	simulateContainer(t, false)
	if err := refuseInContainer("restart"); err != nil {
		t.Fatalf("refused outside a container: %v", err)
	}
}

func TestHubContainerRefusal(t *testing.T) {
	for _, sub := range []string{"update", "install"} {
		err := hubContainerRefusal(sub)
		if err == nil || !strings.Contains(err.Error(), "docker pull") || !strings.Contains(err.Error(), "urnetwork-3.23-fix-hub") {
			t.Errorf("hub %s: got %v, want a docker pull of the hub image", sub, err)
		}
	}
	for _, sub := range []string{"init", "onboard-cmd", "show-password"} {
		err := hubContainerRefusal(sub)
		if err == nil || !strings.Contains(err.Error(), "docker exec") || !strings.Contains(err.Error(), "mint-onboard-token") {
			t.Errorf("hub %s: got %v, want docker exec guidance", sub, err)
		}
	}
	for _, sub := range []string{"link", "unlink", "set", "off"} {
		if err := hubContainerRefusal(sub); err != nil {
			t.Errorf("hub %s refused inside a container: %v", sub, err)
		}
	}
}

func TestMirrorURLFor(t *testing.T) {
	gh := githubReleaseDownloadPrefix + "v3.23.0-fix.31.1/urnetwork-provider-v3.23.0-fix.31.1.tar.gz"
	if got, want := mirrorURLFor(gh), mirrorReleaseDownloadPrefix+"v3.23.0-fix.31.1/urnetwork-provider-v3.23.0-fix.31.1.tar.gz"; got != want {
		t.Fatalf("mirrorURLFor(github) = %q, want %q", got, want)
	}
	if got := mirrorURLFor("https://example.com/custom.tar.gz"); got != "" {
		t.Fatalf("custom URL was mirrored: %q", got)
	}
}

// stubDownloads serves content per URL and records the order of requests.
func stubDownloads(t *testing.T, bodies map[string]string, fail map[string]bool) *[]string {
	t.Helper()
	var calls []string
	orig := downloadFileFunc
	downloadFileFunc = func(u, path string) error {
		calls = append(calls, u)
		if fail[u] {
			return fmt.Errorf("simulated failure for %s", u)
		}
		return os.WriteFile(path, []byte(bodies[u]), 0o644)
	}
	t.Cleanup(func() { downloadFileFunc = orig })
	return &calls
}

func digestOf(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func TestDownloadVerifiedFallsBackWhenMirrorFails(t *testing.T) {
	gh := githubReleaseDownloadPrefix + "v1/a.tar.gz"
	mirror := mirrorURLFor(gh)
	calls := stubDownloads(t, map[string]string{gh: "good"}, map[string]bool{mirror: true})
	path := filepath.Join(t.TempDir(), "a.tar.gz")
	if err := downloadVerified(gh, path, digestOf("good"), "download"); err != nil {
		t.Fatalf("downloadVerified: %v", err)
	}
	if fmt.Sprint(*calls) != fmt.Sprint([]string{mirror, gh}) {
		t.Fatalf("download order = %v, want mirror then GitHub", *calls)
	}
}

func TestDownloadVerifiedRetriesGitHubOnMirrorDigestMismatch(t *testing.T) {
	gh := githubReleaseDownloadPrefix + "v1/a.tar.gz"
	mirror := mirrorURLFor(gh)
	calls := stubDownloads(t, map[string]string{mirror: "tampered", gh: "good"}, nil)
	path := filepath.Join(t.TempDir(), "a.tar.gz")
	if err := downloadVerified(gh, path, digestOf("good"), "download"); err != nil {
		t.Fatalf("downloadVerified: %v", err)
	}
	if len(*calls) != 2 {
		t.Fatalf("expected a GitHub retry after the mirror digest mismatch, calls = %v", *calls)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "good" {
		t.Fatalf("kept %q, want the verified GitHub copy", got)
	}
}

func TestDownloadVerifiedUsesMirrorWhenGood(t *testing.T) {
	gh := githubReleaseDownloadPrefix + "v1/a.tar.gz"
	mirror := mirrorURLFor(gh)
	calls := stubDownloads(t, map[string]string{mirror: "good"}, nil)
	if err := downloadVerified(gh, filepath.Join(t.TempDir(), "a"), digestOf("good"), "download"); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(*calls) != fmt.Sprint([]string{mirror}) {
		t.Fatalf("calls = %v, want the mirror only", *calls)
	}
}

func TestDownloadVerifiedCustomURLNotMirrored(t *testing.T) {
	custom := "https://example.com/provider.tar.gz"
	calls := stubDownloads(t, map[string]string{custom: "good"}, nil)
	if err := downloadVerified(custom, filepath.Join(t.TempDir(), "p"), digestOf("good"), "download"); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(*calls) != fmt.Sprint([]string{custom}) {
		t.Fatalf("calls = %v, want only the custom URL", *calls)
	}
}

// startChild runs a process to stand in for the provider and reaps it in
// the background, so pidIsAlive sees it exit instead of a zombie.
func startChild(t *testing.T, script string) int {
	t.Helper()
	cmd := exec.Command("sh", "-c", script)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	return cmd.Process.Pid
}

func stubRespawn(t *testing.T, stateDir string, pid int) {
	t.Helper()
	origDiscover, origStop, origKill, origRespawn := containerDiscover, containerStopWait, containerKillWait, containerRespawnWait
	containerDiscover = func() []Provider { return []Provider{{PID: pid, StateDir: stateDir}} }
	containerStopWait, containerKillWait, containerRespawnWait = 500*time.Millisecond, 2*time.Second, 2*time.Second
	t.Cleanup(func() {
		containerDiscover, containerStopWait, containerKillWait, containerRespawnWait = origDiscover, origStop, origKill, origRespawn
	})
}

func TestRestartContainerProviderStopsAndClearsMarker(t *testing.T) {
	dir := t.TempDir()
	pid := startChild(t, "sleep 30")
	stubRespawn(t, dir, os.Getpid())

	if err := restartContainerProvider(Provider{PID: pid, StateDir: dir}); err != nil {
		t.Fatalf("restartContainerProvider: %v", err)
	}
	if pidIsAlive(pid) {
		t.Fatal("old provider process is still running")
	}
	if _, err := os.Stat(filepath.Join(dir, containerUpdatePendingMarker)); !os.IsNotExist(err) {
		t.Fatalf("update-pending marker left behind (stat err=%v)", err)
	}
}

func TestRestartContainerProviderEscalatesToSIGKILL(t *testing.T) {
	dir := t.TempDir()
	pid := startChild(t, `trap "" TERM; while :; do sleep 1; done`)
	time.Sleep(200 * time.Millisecond) // let the shell install its trap
	stubRespawn(t, dir, os.Getpid())

	if err := restartContainerProvider(Provider{PID: pid, StateDir: dir}); err != nil {
		t.Fatalf("restartContainerProvider: %v", err)
	}
	if pidIsAlive(pid) {
		t.Fatal("a provider ignoring SIGTERM was not killed")
	}
}

func TestRestartContainerProviderReportsMissingRespawn(t *testing.T) {
	dir := t.TempDir()
	pid := startChild(t, "sleep 30")
	stubRespawn(t, dir, 0)
	containerDiscover = func() []Provider { return nil }

	err := restartContainerProvider(Provider{PID: pid, StateDir: dir})
	if err == nil || !strings.Contains(err.Error(), "did not relaunch") {
		t.Fatalf("got %v, want a relaunch timeout error", err)
	}
	if _, err := os.Stat(filepath.Join(dir, containerUpdatePendingMarker)); !os.IsNotExist(err) {
		t.Fatal("update-pending marker left behind after a failed relaunch")
	}
}

func TestRestartProviderRoutesContainerProvider(t *testing.T) {
	simulateContainer(t, true)
	dir := t.TempDir()
	pid := startChild(t, "sleep 30")
	stubRespawn(t, dir, os.Getpid())

	if err := restartProvider(Provider{PID: pid, StateDir: dir}); err != nil {
		t.Fatalf("restartProvider for a unit-less provider in a container: %v", err)
	}
}

// TestUpdateCommandsRefuseUnderPelicanFirst: every update entry point must
// refuse before a release lookup, a confirmation prompt or an idle wait, as
// the shell tool did ("never reaches arch detection").
func TestUpdateCommandsRefuseUnderPelicanFirst(t *testing.T) {
	t.Setenv("PELICAN", "yes")
	orig := downloadFileFunc
	downloadFileFunc = func(u, path string) error {
		t.Errorf("network download attempted under Pelican: %s", u)
		return nil
	}
	t.Cleanup(func() { downloadFileFunc = orig })

	cmds := map[string]func([]string, bool, bool) error{
		"update":      cmdUpdate,
		"self-update": cmdSelfUpdate,
		"reinstall":   cmdReinstall,
	}
	for name, fn := range cmds {
		if err := fn(nil, false, false); !errors.Is(err, errPelicanUpdatesDisabled) {
			t.Errorf("%s under PELICAN=yes: got %v, want errPelicanUpdatesDisabled", name, err)
		}
	}
}
