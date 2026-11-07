//go:build !windows

package urnettools

import (
	"os"
	"testing"
)

// openAppendShared opens path for appending, creating it if needed.
func openAppendShared(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// replaceFile puts replacement at path in one atomic step, as the provider's
// trim does, while other handles may have path open.
func replaceFile(t *testing.T, replacement, path string) {
	t.Helper()
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
}
