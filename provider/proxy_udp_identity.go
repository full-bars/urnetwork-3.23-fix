package main

import (
	"crypto/sha256"
	"fmt"

	"github.com/urnetwork/connect"
)

// udpProxyIdentity identifies the proxy without printing its address or account.
func udpProxyIdentity(settings *connect.ProxySettings) string {
	sum := sha256.Sum256([]byte(settings.Key()))
	return fmt.Sprintf("proxy[%d] (%x)", settings.Index, sum[:6])
}
