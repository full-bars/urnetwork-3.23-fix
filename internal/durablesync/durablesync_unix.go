//go:build !windows

package durablesync

import "os"

// Nothing is absorbed on Unix. fsync(2) on a directory descriptor is the only
// thing that makes a rename durable there, so every refusal carries a real
// durability verdict and must reach the caller.
//
// Go retries an interrupted call, and on Darwin retries an ENOTSUP from
// F_FULLFSYNC with fsync(2). Those are retries of the same operation rather
// than absorptions: a refusal that survives them is reported.
func syncDirectory(directory *os.File) error {
	return directory.Sync()
}
