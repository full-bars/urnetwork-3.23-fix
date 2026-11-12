//go:build !openbsd

package urnettools

import "os"

func readJWTFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}
