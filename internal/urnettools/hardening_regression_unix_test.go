//go:build !windows

package urnettools

import (
	"archive/tar"
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestReadStateFileNoFollowRejectsSymlink: readStateFileNoFollow must refuse
// to follow a symlink planted at the state-file path — a TOCTOU-free check
// (O_NOFOLLOW open on unix) means the link cannot be swapped between check
// and read.
func TestReadStateFileNoFollowRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(dir, "jwt")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	if _, err := readStateFileNoFollow(dir, "jwt"); err == nil {
		t.Fatal("readStateFileNoFollow followed a symlink; must refuse")
	}
	if b, _ := os.ReadFile(victim); string(b) != "secret" {
		t.Fatal("victim file was modified")
	}
}

// TestContainerReadFileDecodesCPTar: containerReadFile must decode the
// `docker cp -` tar stream and return the extracted file content, not raw
// tar bytes.
func TestContainerReadFileDecodesCPTar(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	content := []byte("jwt-content\n")
	if err := tw.WriteHeader(&tar.Header{Name: "jwt", Mode: 0o600, Size: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	tarPath := filepath.Join(dir, "cp-output.tar")
	if err := os.WriteFile(tarPath, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	shim := filepath.Join(dir, "docker")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"cp\" ]; then cat \"$TEST_TAR\"; exit 0; fi\n" +
		"echo \"$*\" >> \"$DOCKER_SHIM_LOG\"\nexit 0\n"
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	setDockerTestBin(shim)
	t.Cleanup(func() { setDockerTestBin("") })
	t.Setenv("TEST_TAR", tarPath)

	out, err := containerReadFile(dockerContainer{
		ID:    "abc123",
		Name:  "urnet-test",
		Image: "urnetwork:latest",
		State: "running",
	}, "/root/.urnetwork/jwt")
	if err != nil {
		t.Fatalf("containerReadFile: %v", err)
	}
	if strings.TrimSpace(out) != "jwt-content" {
		t.Fatalf("containerReadFile returned tar bytes instead of raw file: %q", out)
	}
}

// The docker shims below are POSIX shell scripts, so these tests are
// unix-only.

func TestContainerFollowFileUsesName(t *testing.T) {
	dir := t.TempDir()
	shim := filepath.Join(dir, "docker")
	if err := os.WriteFile(shim, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$DOCKER_SHIM_LOG\"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(dir, "args")
	t.Setenv("DOCKER_SHIM_LOG", log)
	setDockerTestBin(shim)
	t.Cleanup(func() { setDockerTestBin("") })
	if err := containerFollowFile("provider", "/dev/shm/urnetwork.log", 50); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if want := "exec\nprovider\ntail\n-n\n50\n-F\n/dev/shm/urnetwork.log\n"; string(got) != want {
		t.Fatalf("arguments %q, want %q", got, want)
	}
}

// TestContainerReadFilePrefersCP validates the CP-first read path used for
// stopped-container identity discovery.
func TestContainerReadFilePrefersCP(t *testing.T) {
	log := filepath.Join(t.TempDir(), "docker.log")
	shim := filepath.Join(t.TempDir(), "docker")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"cp\" ]; then\n" +
		"  tf=$(mktemp); printf 'cp-data' > \"$tf\"; tar cf - -C \"$(dirname \"$tf\")\" \"$(basename \"$tf\")\"; rm -f \"$tf\"; exit 0; fi\n" +
		"if [ \"$1\" = \"exec\" ]; then echo 'exec-data'; exit 0; fi\n" +
		"echo \"$*\" >> \"$DOCKER_SHIM_LOG\"\nexit 0\n"
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	orig := dockerCLI
	setDockerTestBin(shim)
	t.Cleanup(func() { setDockerTestBin("") })
	t.Setenv("DOCKER_SHIM_LOG", log)
	c := dockerContainer{ID: "abc123", Name: "urnet-test", Image: "urnetwork:latest", State: "running"}
	out, err := containerReadFile(c, "/root/.urnetwork/jwt")
	if err != nil {
		t.Fatalf("containerReadFile: %v", err)
	}
	if strings.TrimSpace(out) != "cp-data" {
		t.Errorf("containerReadFile = %q, want cp-data (cp preferred over exec)", out)
	}
	_ = orig
}

// TestRunCappedTimeoutKillsSlowCommand pins the timed read helper that the
// docker reads rely on: a command that never finishes must be killed when the
// deadline passes, and the call must return errCommandTimeout rather than
// hanging or buffering forever (a hung dockerd would otherwise hang every
// docker command indefinitely).
func TestRunCappedTimeoutKillsSlowCommand(t *testing.T) {
	start := time.Now()
	out, _, err := runCappedTimeout(exec.Command("/bin/sleep", "30"), 1<<20, 300*time.Millisecond)
	_ = out
	if !errors.Is(err, errCommandTimeout) {
		t.Fatalf("runCappedTimeout(sleep 30, 300ms) err = %v, want errCommandTimeout", err)
	}
	if el := time.Since(start); el > 10*time.Second {
		t.Fatalf("runCappedTimeout took %s to give up; the process was not killed", el)
	}
}

// TestContainerReadFileCPTooLargeSkipsExecFallback pins that a docker cp
// which exceeds the output cap is NOT retried through docker exec: the
// fallback would put the same bad container through a second full wait
// (DiscoverDocker reads containers serially, so a hung or flooding
// dockerd doubles the delay per container), and an oversized entry
// exceeds containerFileMax either way.
func TestContainerReadFileCPTooLargeSkipsExecFallback(t *testing.T) {
	log := filepath.Join(t.TempDir(), "docker.log")
	shim := filepath.Join(t.TempDir(), "docker")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"cp\" ]; then dd if=/dev/zero bs=1048576 count=2 2>/dev/null | tr '\\0' 'x'; exit 0; fi\n" +
		"echo \"$*\" >> \"$DOCKER_SHIM_LOG\"\nexit 0\n"
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	setDockerTestBin(shim)
	t.Cleanup(func() { setDockerTestBin("") })
	t.Setenv("DOCKER_SHIM_LOG", log)
	c := dockerContainer{ID: "abc123", Name: "urnet-test", Image: "urnetwork:latest", State: "running"}
	if _, err := containerReadFile(c, "/root/.urnetwork/jwt"); err == nil {
		t.Fatalf("containerReadFile with an oversized cp reply = nil error, want error")
	}
	if b, _ := os.ReadFile(log); strings.Contains(string(b), "exec") {
		t.Fatalf("oversized cp fell back to docker exec (shim log %q); must return immediately", b)
	}
}

// TestContainerReadFileExecFallbackUsesDashDash pins the exec-fallback argv:
// the container path must be passed to sh as $1 after a cat -- separator, so
// a HOME-derived path beginning with "-" is never read as a cat option, and
// the sh -c wrapper's `test -f` keeps a FIFO from leaving a stuck cat in the
// container when the client is killed on timeout.
func TestContainerReadFileExecFallbackUsesDashDash(t *testing.T) {
	log := filepath.Join(t.TempDir(), "docker.log")
	shim := filepath.Join(t.TempDir(), "docker")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = \"cp\" ]; then exit 1; fi\n" +
		"echo \"$*\" >> \"$DOCKER_SHIM_LOG\"\nexit 0\n"
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	setDockerTestBin(shim)
	t.Cleanup(func() { setDockerTestBin("") })
	t.Setenv("DOCKER_SHIM_LOG", log)
	_, _ = containerReadFile(dockerContainer{ID: "abc123", Name: "urnet-test", Image: "urnetwork:latest", State: "running"}, "/tmp/-dash-file")
	b, _ := os.ReadFile(log)
	if !strings.Contains(string(b), "exec abc123 sh") {
		t.Fatalf("exec fallback did not run through sh -c: shim got %q", b)
	}
	// `test -f "$1" && exec cat -- "$1"` then `sh` as $0, then the path.
	if !strings.Contains(string(b), "cat --") {
		t.Fatalf("exec fallback lacks the cat -- separator: shim got %q", b)
	}
	if !strings.Contains(string(b), "cat --") || !strings.Contains(string(b), "/tmp/-dash-file") {
		t.Fatalf("path did not arrive behind the -- separator: shim got %q", b)
	}
}

// TestExtractSingleFileContentRefusesOversizedEntry pins that a docker cp
// tar stream carrying a file larger than containerFileMax is rejected with an
// error instead of being silently truncated at 1 MiB (identity files are
// small; anything bigger is either corruption or a container that must not
// feed credentials into discovery).
func TestExtractSingleFileContentRefusesOversizedEntry(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	oversize := containerFileMax + 1
	if err := tw.WriteHeader(&tar.Header{Name: "jwt", Mode: 0o600, Size: int64(oversize)}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(make([]byte, oversize)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := extractSingleFileContent(buf.Bytes()); err == nil {
		t.Fatalf("extractSingleFileContent accepted a %d byte entry; must error", oversize)
	}
}

// TestContainerReadFileCapsOutput: a container file larger than the cap must
// be refused while it is being read, not buffered whole. The shim streams
// far more than the cap for both the cp and the exec-cat paths.
func TestContainerReadFileCapsOutput(t *testing.T) {
	dir := t.TempDir()
	shim := filepath.Join(dir, "docker")
	script := "#!/bin/sh\nhead -c 5000000 /dev/zero\nexit 0\n"
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	setDockerTestBin(shim)
	t.Cleanup(func() { setDockerTestBin("") })

	out, err := containerReadFile(dockerContainer{ID: "abc123", Name: "urnet-test", State: "running"}, "/root/.urnetwork/jwt")
	if err == nil {
		t.Fatalf("oversized container file was accepted (%d bytes)", len(out))
	}
	if !errors.Is(err, errOutputTooLarge) {
		t.Fatalf("expected errOutputTooLarge, got %v", err)
	}
}

// TestRunCappedWithinLimit: output at or under the cap is returned intact and
// stderr is captured for error messages.
func TestRunCappedWithinLimit(t *testing.T) {
	out, stderr, err := runCapped(exec.Command("sh", "-c", "printf abcde; printf oops >&2"), 5)
	if err != nil {
		t.Fatalf("runCapped: %v", err)
	}
	if string(out) != "abcde" || stderr != "oops" {
		t.Fatalf("out=%q stderr=%q", out, stderr)
	}
	if _, _, err := runCapped(exec.Command("sh", "-c", "printf abcdef"), 5); !errors.Is(err, errOutputTooLarge) {
		t.Fatalf("6 bytes with cap 5: want errOutputTooLarge, got %v", err)
	}
}

// TestParseUnixIDRejectsOutOfRange: a uid/gid above uint32 must be an error,
// not wrap (4294967296 wraps to 0, i.e. root).
func TestParseUnixIDRejectsOutOfRange(t *testing.T) {
	for _, bad := range []string{"4294967296", "99999999999", "-1", "", "12a"} {
		if _, err := parseUnixID(bad); err == nil {
			t.Errorf("parseUnixID(%q) accepted; must reject", bad)
		}
	}
	for s, want := range map[string]uint32{"0": 0, "1000": 1000, "4294967295": 4294967295} {
		got, err := parseUnixID(s)
		if err != nil || got != want {
			t.Errorf("parseUnixID(%q) = %d, %v; want %d", s, got, err, want)
		}
	}
}
