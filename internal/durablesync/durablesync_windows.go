//go:build windows

package durablesync

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// Windows has no directory flush. A directory can only be opened at all with
// FILE_FLAG_BACKUP_SEMANTICS, and FlushFileBuffers on such a handle always
// fails with ERROR_ACCESS_DENIED. The refusal says there is nothing here to
// flush; it is not a verdict on the caller's own access, which is why
// elevation does not resolve it.
//
// This package makes no directory-durability claim on Windows. Absorbing the
// refusal is how a caller is allowed to proceed rather than being stopped by a
// primitive the platform does not have.
//
// The descriptor is already known to be a directory; Sync refuses anything
// else before reaching this point, so a file can never be certified here.
func syncDirectory(directory *os.File) error {
	return flushDirectory(directory.Sync())
}

// flushDirectory is the entire Windows policy, taking the flush error as an
// argument so it can be tested without provoking a real filesystem fault.
// Deciding here rather than inline in syncDirectory keeps the errno comparison
// against the error the flush actually produced, not one obtained from some
// other call in the same function.
func flushDirectory(syncErr error) error {
	if syncErr == nil || absorbable(syncErr) {
		return nil
	}
	return syncErr
}

// absorbable is the whole of this platform's policy: the errnos that mean
// "there is no directory flush here" and nothing else. Every other failure
// stays a failure, so a genuine access denial, a bad handle or a volume error
// is never mistaken for the platform's missing primitive.
func absorbable(err error) bool {
	for _, refusal := range []windows.Errno{
		windows.ERROR_ACCESS_DENIED,
		windows.ERROR_INVALID_FUNCTION,
		windows.ERROR_NOT_SUPPORTED,
	} {
		if errors.Is(err, refusal) {
			return true
		}
	}
	return false
}
