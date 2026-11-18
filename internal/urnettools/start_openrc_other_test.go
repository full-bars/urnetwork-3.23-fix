//go:build !linux

package urnettools

import "testing"

func stubOpenRCForStartTest(t *testing.T) {
	t.Helper()
}
