package urnettools

import (
	"crypto/sha256"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"
)

func TestInstallRecovered_TargetMissing_Renames(t *testing.T) {
	dir := t.TempDir()
	tmp := filepath.Join(dir, "tmp.bin")
	binary := filepath.Join(dir, "provider")
	if err := os.WriteFile(tmp, []byte("recovered-image"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := installRecovered(tmp, binary); err != nil {
		t.Fatalf("installRecovered (target missing) should succeed, got %v", err)
	}
	if _, err := os.Stat(binary); err != nil {
		t.Fatalf("binary should exist after rename: %v", err)
	}
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Fatalf("temp file should be consumed by the rename, stat err=%v", err)
	}
}

// TestInstallRecovered_TargetAppeared_NoClobber is the regression guard for the
// stale-vs-fresh TOCTOU: if a target binary materializes between the initial
// missing-stat and the final install (a concurrent updater won the race),
// recovery must refuse rather than overwrite the fresh binary.
func TestInstallRecovered_TargetAppeared_NoClobber(t *testing.T) {
	dir := t.TempDir()
	tmp := filepath.Join(dir, "tmp.bin")
	binary := filepath.Join(dir, "provider")
	if err := os.WriteFile(tmp, []byte("stale-deleted-image"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binary, []byte("fresh-binary-from-updater"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := installRecovered(tmp, binary)
	if !errors.Is(err, errBinaryAppeared) {
		t.Fatalf("want errBinaryAppeared, got %v", err)
	}
	// The freshly-installed binary must be untouched.
	got, rerr := os.ReadFile(binary)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(got) != "fresh-binary-from-updater" {
		t.Fatalf("installRecovered clobbered the fresh binary: %q", got)
	}
	if _, serr := os.Stat(tmp); !os.IsNotExist(serr) {
		t.Fatalf("temp file should be cleaned up on refusal, stat err=%v", serr)
	}
}

// TestProvRecoverHelperProcess is a sleeper the integration test launches as a
// child, then deletes its own on-disk binary mid-run so the recovery path can
// exercise a genuine deleted-but-running process. Under normal `go test` it is
// a no-op (not driven by the env flag).
func TestProvRecoverHelperProcess(t *testing.T) {
	if os.Getenv("URNPROV_RECOVER_HELPER") != "1" {
		return
	}
	time.Sleep(60 * time.Second)
}

func TestEnsureBinaryRecoverable_WhenBinaryExists_NoOp(t *testing.T) {
	dir := t.TempDir()
	b := filepath.Join(dir, "provider")
	if err := os.WriteFile(b, []byte("x"), 0o700); err != nil {
		t.Fatal(err)
	}
	p := Provider{Binary: b, User: "nobody"}
	got, recovered, err := ensureBinaryRecoverable(p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != b {
		t.Fatalf("got binary %q, want %q", got, b)
	}
	if recovered {
		t.Fatal("recovered should be false when the binary already exists")
	}
}

func TestEnsureBinaryRecoverable_MissingAndNotRunning_PlainError(t *testing.T) {
	p := Provider{Binary: "/nonexistent/does-not-exist", PID: 0, Running: false}
	if _, _, err := ensureBinaryRecoverable(p); err == nil {
		t.Fatal("expected an error when the binary is missing and no process runs to recover it from")
	}
}

func TestEnsureBinaryRecoverable_MissingAndRunning_GuardRejectsPathMismatch(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux-only: uses /proc/self/exe")
	}
	if _, err := os.Stat("/proc/self/exe"); err != nil {
		t.Skip("no /proc/self/exe")
	}
	// Our own process is running, but its resolved exe path will NOT equal the
	// fabricated binary path -> the guard must refuse rather than write.
	p := Provider{Binary: "/nonexistent/never-this", PID: os.Getpid(), Running: true}
	if _, _, err := ensureBinaryRecoverable(p); err == nil {
		t.Fatal("expected the path-equivalence guard to refuse a mismatched /proc exe target")
	}
}

func TestRecoverDeletedBinary_RecreatesRunningImage(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux-only: uses /proc/<pid>/exe")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat("/proc/self/exe"); err != nil {
		t.Skip("no /proc/self/exe")
	}
	dir := t.TempDir()
	helperPath := filepath.Join(dir, "helper")
	src, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(helperPath, src, 0o755); err != nil {
		t.Fatal(err)
	}

	// Launch the sleeper child, then delete its on-disk binary while it runs so
	// the process lives on the (now-deleted) inode — the exact prod state.
	cmd := exec.Command(helperPath, "-test.run=TestProvRecoverHelperProcess")
	cmd.Env = append(os.Environ(), "URNPROV_RECOVER_HELPER=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	pid := cmd.Process.Pid
	if err := os.Remove(helperPath); err != nil {
		t.Fatal(err)
	}

	if err := recoverDeletedBinary(helperPath, pid, ""); err != nil {
		t.Fatalf("recoverDeletedBinary: %v", err)
	}

	// Verify the recreated file is byte-identical to the still-running image.
	live, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/exe")
	if err != nil {
		t.Fatalf("read live image: %v", err)
	}
	rec, err := os.ReadFile(helperPath)
	if err != nil {
		t.Fatalf("read recovered file: %v", err)
	}
	if sha256.Sum256(live) != sha256.Sum256(rec) {
		t.Fatal("recovered binary differs from the running image")
	}
	if m, err := os.Stat(helperPath); err != nil || m.Mode()&0o111 == 0 {
		t.Fatalf("recovered binary lost exec bit (mode %v, err %v)", m, err)
	}
}
