//go:build !linux

package nicstat

import (
	"errors"
	"runtime"
	"testing"
)

func TestUnsupported(t *testing.T) {
	_, err := Read()
	var unsupported *UnsupportedError
	if !errors.As(err, &unsupported) || unsupported.OS != runtime.GOOS {
		t.Fatalf("Read: %v", err)
	}
	_, err = DefaultRoute()
	if !errors.As(err, &unsupported) || unsupported.OS != runtime.GOOS {
		t.Fatalf("DefaultRoute: %v", err)
	}
}
