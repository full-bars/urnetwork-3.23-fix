package main

import (
	"fmt"

	"github.com/urnetwork/connect"
)

// udpProxyIdentity names the proxy in a UDP message the way the rest of the
// provider does: the slot and the address it dials. The account is not part of
// it, and the address never holds a credential (parseProxyAddress keeps those in
// Auth), so the line says which proxy it is about without printing either.
func udpProxyIdentity(settings *connect.ProxySettings) string {
	return fmt.Sprintf("proxy[%d] (%s)", settings.Index, redactedProxyAddress(settings.Address))
}
