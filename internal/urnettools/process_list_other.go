//go:build !windows

package urnettools

import "errors"

// listProviderProcesses is only used by the Windows uninstall path. Elsewhere
// a service manager owns the provider, so report an error (fail closed)
// rather than an empty, falsely reassuring list.
func listProviderProcesses() ([]providerProcess, error) {
	return nil, errors.New("provider process listing is only implemented on Windows")
}
