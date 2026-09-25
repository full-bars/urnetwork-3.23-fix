//go:build !unix

package urnettools

// chownConfigToDirOwner is a no-op on platforms without a unix ownership
// model (the config dir belongs to the invoking user on Windows already).
func chownConfigToDirOwner(path string) {}
