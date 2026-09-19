//go:build unix

package urnettools

// TestWriteStateFileRejectsFIFO: writeStateFile must not block forever on a
// planted FIFO (a plain O_WRONLY open of a FIFO with no reader blocks).
// The helper opens with O_NONBLOCK to detect FIFOs, so this must return an
// error quickly instead of hanging the caller (root) indefinitely. Unix-only
// because FIFOs require mkfifo(2).

import (
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestWriteStateFileRejectsFIFO(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "target")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("cannot create fifo: %v", err)
	}
	if err := writeStateFile(dir, "target", []byte("x"), 0o600); err == nil {
		t.Fatal("writeStateFile on a FIFO returned nil; must refuse (O_NONBLOCK open fails) instead of blocking forever")
	}
}

// TestWriteStateFileRejectsFIFOWithReader: a FIFO that HAS a reader opens
// successfully under O_NONBLOCK, so the open alone does not catch it. The
// write must still be refused (fstat: not a regular file) instead of pushing
// state into whatever is on the other end.
func TestWriteStateFileRejectsFIFOWithReader(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "target")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("cannot create fifo: %v", err)
	}
	rfd, err := unix.Open(fifo, unix.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		t.Skipf("cannot open fifo reader: %v", err)
	}
	defer unix.Close(rfd)

	err = writeStateFile(dir, "target", []byte("secret"), 0o600)
	if err == nil {
		t.Fatal("writeStateFile wrote into a FIFO with a reader; must refuse")
	}
	if !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("unexpected error: %v", err)
	}
	buf := make([]byte, 16)
	if n, _ := unix.Read(rfd, buf); n > 0 {
		t.Fatalf("reader received %q from the refused write", buf[:n])
	}
}
