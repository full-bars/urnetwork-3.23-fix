//go:build windows

package urnettools

import "errors"

// triggerHotSwap on Windows is a stub until the named pipe adapter is connected.
func triggerHotSwap(p Provider) error {
	return errors.New("hotswap not yet implemented on windows")
}
