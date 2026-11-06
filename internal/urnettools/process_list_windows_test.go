//go:build windows

package urnettools

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestHelperProcessSleep is the body of the fake provider process spawned
// by TestListProviderProcessesSeesOwnProcess; it does nothing otherwise.
func TestHelperProcessSleep(t *testing.T) {
	if os.Getenv("URNETTOOLS_HELPER_SLEEP") != "1" {
		t.Skip("helper process only")
	}
	time.Sleep(60 * time.Second)
}

// The real Windows lister must see a provider-named process with its full
// path, owned by the caller. The process is a copy of this test binary
// named like a provider, in a temp dir; it is killed at the end.
func TestListProviderProcessesSeesOwnProcess(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(t.TempDir(), "urnetwork-listtest.exe")
	src, err := os.Open(self)
	if err != nil {
		t.Fatal(err)
	}
	dst, err := os.Create(fake)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		t.Fatal(err)
	}
	src.Close()
	dst.Close()

	cmd := exec.Command(fake, "-test.run=^TestHelperProcessSleep$")
	cmd.Env = append(os.Environ(), "URNETTOOLS_HELPER_SLEEP=1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start fake provider: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	procs, err := listProviderProcesses()
	if err != nil {
		t.Fatalf("listProviderProcesses: %v", err)
	}
	for _, pp := range procs {
		if pp.PID != cmd.Process.Pid {
			continue
		}
		if !strings.EqualFold(filepath.Clean(pp.Exe), filepath.Clean(fake)) {
			t.Errorf("exe = %q, want %q", pp.Exe, fake)
		}
		if !pp.OwnerKnown || !pp.OwnedByCaller {
			t.Errorf("owner known=%v mine=%v, want both true for our own process", pp.OwnerKnown, pp.OwnedByCaller)
		}
		if !pp.mayShareProfile() {
			t.Errorf("own process must count as sharing the profile")
		}
		return
	}
	t.Fatalf("fake provider pid %d not in the listing (%d provider processes seen)", cmd.Process.Pid, len(procs))
}
