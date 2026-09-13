//go:build windows

package urnettools

import "errors"

// restartContainerProvider is unreachable on Windows: provider containers
// are Linux images, and inContainer only matches inside one.
func restartContainerProvider(p Provider) error {
	return errors.New("container provider restart is only supported inside a Linux container")
}
