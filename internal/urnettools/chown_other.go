//go:build !unix

package urnettools

// chownLikeStateOwner is a no-op on platforms without POSIX ownership.
func chownLikeStateOwner(stateDir, path string) error {
	return nil
}

// chownFdLikeStateOwner is a no-op on non-unix platforms.
func chownFdLikeStateOwner(stateDir string, fd int) error {
	return nil
}
