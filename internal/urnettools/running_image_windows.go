//go:build windows

package urnettools

import (
	"fmt"

	"golang.org/x/sys/windows"
)

const processQueryLimitedInformation = 0x1000

// runningImagePath returns the filesystem path of the image the process
// identified by pid is actually executing. Windows has no /proc, so this
// queries the image path recorded in the process's PEB via
// QueryFullProcessImageName — the equivalent of /proc/<pid>/exe. Callers
// must NOT substitute the on-disk binary path: after an update swaps the
// binary, the on-disk file is the NEW image while the running process may
// still execute the OLD one, so reading the on-disk file is tautological
// and proves nothing about restart verification.
func runningImagePath(pid int) (string, error) {
	if pid <= 0 {
		return "", fmt.Errorf("runningImagePath: invalid pid %d", pid)
	}
	// PROCESS_QUERY_LIMITED_INFORMATION is enough for the image path and
	// works across privilege boundaries that full query access would not.
	h, err := windows.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		return "", fmt.Errorf("runningImagePath: OpenProcess(%d): %w", pid, err)
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, windows.MAX_PATH)
	size := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &size); err != nil {
		return "", fmt.Errorf("runningImagePath: QueryFullProcessImageName(%d): %w", pid, err)
	}
	return windows.UTF16ToString(buf[:size]), nil
}
