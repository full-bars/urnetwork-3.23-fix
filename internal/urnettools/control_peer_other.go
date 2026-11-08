//go:build !linux

package urnettools

import (
	"fmt"
	"net"
)

// socketPeerPID is unavailable off Linux, so callers that need to know who
// answered a control socket fail closed there.
func socketPeerPID(conn net.Conn) (int, error) {
	return 0, fmt.Errorf("socket peer credentials are not available on this platform")
}
