//go:build !freebsd

package urnettools

import "errors"

// bsdServiceControl is implemented in lifecycle_freebsd.go. Call sites are
// gated on runtime.GOOS, but the symbol must exist for every build, the same
// way the windows lifecycle stubs do.
func bsdServiceControl(_ Provider, _ string) error {
	return errors.New("rc.d service control is only available on FreeBSD")
}
