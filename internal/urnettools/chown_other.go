//go:build !unix

package urnettools

// chownLikeStateOwner is a no-op on platforms without POSIX ownership.
func chownLikeStateOwner(stateDir, path string) error {
	return nil
}
