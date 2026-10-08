//go:build windows

package urnettools

import (
	"os"
	"syscall"
)

// openFollowFile opens a log for following without blocking whoever owns it.
// os.Open on Windows shares the file for read and write but not for delete, so
// while a follower held the log open a rename or delete of it (a rotation, a
// clear, an update) failed with ERROR_SHARING_VIOLATION. Opening with
// FILE_SHARE_DELETE lets those proceed; the follower notices the replacement
// by path and reopens.
func openFollowFile(path string) (*os.File, error) {
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	handle, err := syscall.CreateFile(
		name,
		syscall.GENERIC_READ,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil,
		syscall.OPEN_EXISTING,
		syscall.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(handle), path), nil
}
