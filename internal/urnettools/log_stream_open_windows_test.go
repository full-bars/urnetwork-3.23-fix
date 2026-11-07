//go:build windows

package urnettools

import (
	"os"
	"syscall"
	"testing"
)

// openAppendShared opens path for appending, creating it if needed, with
// FILE_SHARE_DELETE so the file can still be moved aside while the handle is
// open. os.OpenFile does not set that flag on Windows.
func openAppendShared(t *testing.T, path string) *os.File {
	t.Helper()
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	h, err := syscall.CreateFile(
		name,
		syscall.FILE_APPEND_DATA,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil,
		syscall.OPEN_ALWAYS,
		syscall.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		t.Fatal(err)
	}
	return os.NewFile(uintptr(h), path)
}
