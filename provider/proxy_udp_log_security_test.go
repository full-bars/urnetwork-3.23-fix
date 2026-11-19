package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/urnetwork/connect"
)

func TestProxyUDPLogRedactsErrors(t *testing.T) {
	const secret = "test-only-proxy-secret"
	settings := &connect.ProxySettings{Address: "socks5://alice:" + secret + "@127.0.0.1:1080/path"}
	for _, status := range []proxyUDPStatus{proxyUDPNoRelay, proxyUDPUnknown} {
		t.Run(status.String(), func(t *testing.T) {
			out := captureTlog(t, func() {
				logProxyUDPResult(settings, nil, proxyUDPResult{Status: status, Err: errors.New("dial failed: " + settings.Address)})
			})
			if strings.Contains(out, secret) || strings.Contains(out, "/path") {
				t.Fatal("proxy error leaked sensitive text")
			}
		})
	}
}
