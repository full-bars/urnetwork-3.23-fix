//go:build windows

package durablesync

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// The absorption is load-bearing only if this platform really does refuse.
// Without this check the rest of the file would pass on a Windows host that
// never refused anything, leaving the policy untested.
func TestWindowsRefusesToFlushADirectory(t *testing.T) {
	dir, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	raw := dir.Sync()
	if raw == nil {
		t.Skip("this Windows filesystem accepted a directory flush; the shim is inert here")
	}
	if !errors.Is(raw, windows.ERROR_ACCESS_DENIED) {
		t.Skipf("this Windows filesystem refused with %v rather than ERROR_ACCESS_DENIED", raw)
	}
	if err := Sync(dir); err != nil {
		t.Fatal("absorbing the platform's own refusal:", err)
	}
}

// A handle that is not a usable descriptor keeps its verdict. Absorption is
// scoped to the platform's missing primitive, not to failures in general.
func TestWindowsKeepsUnrelatedFailures(t *testing.T) {
	closed, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	if err := Sync(closed); err == nil {
		t.Fatal("a closed descriptor was reported as an ordered directory")
	}
}

// On Windows a read-only file handle fails FlushFileBuffers with the same
// errno a directory returns, so the errno alone cannot tell them apart. The
// shared precondition is what keeps them apart, and this records that the
// hazard is real on this platform rather than hypothetical.
func TestWindowsAFileFlushAlsoFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jwt")
	if err := os.WriteFile(path, []byte("token"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	raw := file.Sync()
	if raw == nil {
		t.Skip("this Windows filesystem flushed a read-only file; the hazard is inert here")
	}
	if !errors.Is(raw, windows.ERROR_ACCESS_DENIED) {
		t.Skipf("this Windows filesystem refused with %v rather than ERROR_ACCESS_DENIED", raw)
	}
	// Same errno as the directory case above, so only the precondition can
	// distinguish them.
	if err := Sync(file); !errors.Is(err, ErrNotDirectory) {
		t.Fatalf("a file must be refused before the flush policy applies, got %v", err)
	}
}

// The absorption policy is exercised directly rather than only through a real
// flush, because the errnos that must survive cannot be provoked on demand.
//
// Asserting against flushDirectory rather than only through syncDirectory is
// deliberate: the comparison has to see the error the flush produced.
// Comparing some other error obtained in the same function would make every
// flush failure succeed while every other test here still passed.
func TestWindowsAbsorbsOnlyTheDirectoryFlushRefusals(t *testing.T) {
	absorbed := []windows.Errno{
		windows.ERROR_ACCESS_DENIED,
		windows.ERROR_INVALID_FUNCTION,
		windows.ERROR_NOT_SUPPORTED,
	}
	for _, errno := range absorbed {
		wrapped := &os.PathError{Op: "sync", Path: `C:\dir`, Err: errno}
		if !absorbable(wrapped) {
			t.Errorf("errno %v is a directory-flush refusal but was not absorbed", errno)
		}
		if err := flushDirectory(wrapped); err != nil {
			t.Errorf("errno %v should have been absorbed, got %v", errno, err)
		}
	}

	kept := []windows.Errno{
		windows.ERROR_INVALID_HANDLE,
		windows.ERROR_FILE_NOT_FOUND,
		windows.ERROR_SHARING_VIOLATION,
		windows.ERROR_WRITE_PROTECT,
		windows.ERROR_DISK_FULL,
		windows.ERROR_CRC,
	}
	for _, errno := range kept {
		wrapped := &os.PathError{Op: "sync", Path: `C:\dir`, Err: errno}
		if absorbable(wrapped) {
			t.Errorf("errno %v carries a real fault and must not be absorbed", errno)
		}
		if err := flushDirectory(wrapped); !errors.Is(err, errno) {
			t.Errorf("errno %v must reach the caller unchanged, got %v", errno, err)
		}
	}

	if absorbable(nil) {
		t.Error("a nil error must never be reported as an absorbed refusal")
	}
	if err := flushDirectory(nil); err != nil {
		t.Errorf("a successful flush must stay nil, got %v", err)
	}
	if flushDirectory(errors.New("some unrelated failure")) == nil {
		t.Error("a non-errno failure must not be absorbed")
	}
}
