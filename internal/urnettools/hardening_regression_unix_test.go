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
