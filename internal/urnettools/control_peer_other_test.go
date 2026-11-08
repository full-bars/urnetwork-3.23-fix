//go:build !linux && !windows

package urnettools

import (
	"os"
	"path/filepath"
	"testing"
)

// Unsupported image resolution already prevented verification before the
// socket fallback existed. A socket response cannot change that on these
// platforms because the listener PID is unavailable to this implementation.
func TestPeerVersionUnsupportedPlatformFailsClosed(t *testing.T) {
	dir := t.TempDir()
	p := Provider{StateDir: dir}
	startVersionMockServer(t, filepath.Join(dir, "provider.sock"), controlResponse{OK: true, BuildVersion: "new"})
	if _, err := runningImageHandle(os.Getpid()); err == nil {
		t.Fatal("running image unexpectedly supported")
	}
	if v, ok := providerVersionFromSocketForPID(p, os.Getpid()); ok {
		t.Fatalf("unverified socket accepted: %q", v)
	}
}
