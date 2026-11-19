package connect

import "net/http"

const HeaderEgressProxy = "X-UR-Egress-Proxy"
const EgressProxyDeclared = "1"

func applyEgressProxyHeader(header http.Header, strategy *ClientStrategy) {
	if strategy != nil && strategy.ConnectSettings().ProxySettings != nil {
		header.Set(HeaderEgressProxy, EgressProxyDeclared)
	}
}
