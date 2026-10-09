//go:build !windows

package urnettools

import "os"

// openFollowFile opens a log for following. On unix an open file never blocks a
// rename or delete of its path, so a plain open is enough.
func openFollowFile(path string) (*os.File, error) {
	return os.Open(path)
}
