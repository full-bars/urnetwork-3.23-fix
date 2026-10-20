package connect

import (
	"context"
	"crypto/tls"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// Every control dial in this package must reach the network through the
// egress-aware machinery (ConnectSettings.NetDialer / DialContext /
// resolveEgressUDPAddr), or its socket/name resolution falls into the tunnel
// this process provides on Windows (the connect-hang root cause). This test
// pins that statically: raw net dials, raw OS-resolver lookups, and the
// default http client/transport are forbidden in package-root non-test files,
// except at the sites audited below.
//
// If this test fails on new code, dial through ConnectSettings (NetDialer or
// DialContext) or resolveEgressUDPAddr instead -- or, if the new site really
// is exempt (a connect()-only local-address probe, the egress machinery
// itself), add it to the allowlist WITH the reason.
//
// Port note (v2026 engine -> 3.23 generation): the header and the first three
// entries are the pinned engine's. The remaining entries are this generation's
// still-raw sites: the dial-family and transport slices own converting them
// (the pinned engine runs them through resolveEgressUDPAddr / the egress
// resolver), so this slice audits them here instead of converting them. An
// entry goes stale harmlessly once its site is converted; remove it then.
var allowedRawDialSites = map[string]map[string]string{
	"ice_net.go": {
		"net.Dial": "connect()-only UDP local-address probe; no packet is sent",
	},
	"egress_dial.go": {
		"net.DefaultResolver": "the context-aware no-egress path inside resolveEgressUDPAddr itself, " +
			"resolving to a list so pickControlIPAddr can honor the family policy; and dialResolver, " +
			"the same fallback for the hostname dial race in ConnectSettings.DialContext",
	},
	"ice_resolve_net.go": {
		"net.DefaultResolver": "the lifecycle-aware fallback inside the per-peer egress resolver",
	},
	"net.go": {
		"net.DefaultResolver": "the warmup target DNS cache resolves before any control dial exists; " +
			"moving it onto the egress-aware resolver belongs to the dial-family slice",
	},
	"net_ipv6_detect.go": {
		"net.DialTimeout": "startup IPv6 global-unicast detection against a fixed public address; " +
			"no hostname resolution and not a control dial",
	},
	"stun_probe.go": {
		"net.DefaultResolver": "STUN target hostnames come from config, not from a control dial; " +
			"the probe cycle belongs to the ICE/probe wiring",
		"net.DialTimeout": "the STUN binding probe dials a resolved target IP; " +
			"the ICE/probe wiring owns moving it onto the egress path",
	},
	"transport.go": {
		"net.ResolveUDPAddr": "the H3, H3-over-DNS, and DNS-pump server-name resolution sites that " +
			"resolveEgressUDPAddr replaces in the pinned engine; the transport slice lands that change",
	},
	"transport_p2p_webrtc.go": {
		"net.DialTimeout": "one-time IPv6 STUN reachability probe that gates IPv6 ICE candidates; " +
			"the ICE slice owns moving it onto the egress path",
	},
}

var forbiddenNetCalls = map[string]bool{
	"Dial":            true,
	"DialTimeout":     true,
	"DialUDP":         true,
	"DialTCP":         true,
	"DialIP":          true,
	"ResolveUDPAddr":  true,
	"ResolveTCPAddr":  true,
	"ResolveIPAddr":   true,
	"LookupIP":        true,
	"LookupHost":      true,
	"LookupIPAddr":    true,
	"DefaultResolver": true,
}

var forbiddenHttpIdents = map[string]bool{
	"DefaultClient":    true,
	"DefaultTransport": true,
}

func TestControlDialSitesUseTheBoundDialer(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var violations []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		allowed := allowedRawDialSites[name]
		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			var call string
			switch {
			case pkg.Name == "net" && forbiddenNetCalls[sel.Sel.Name]:
				call = "net." + sel.Sel.Name
			case pkg.Name == "http" && forbiddenHttpIdents[sel.Sel.Name]:
				call = "http." + sel.Sel.Name
			default:
				return true
			}
			if _, ok := allowed[call]; ok {
				return true
			}
			pos := fset.Position(sel.Pos())
			violations = append(violations, fmt.Sprintf("%s:%d: %s bypasses the egress-aware dial path", pos.Filename, pos.Line, call))
			return true
		})
	}
	if 0 < len(violations) {
		t.Fatalf("raw dial/resolve sites outside the audited allowlist:\n%s", strings.Join(violations, "\n"))
	}
}

// The normal strategy dial used to be a raw tls.Dialer wrapping
// ConnectSettings.NetDialer(). It now dials through ConnectSettings.DialContext
// (net_http.go, the "normal" dialer's tlsDialContext), and that path must still
// apply the same socket Control: NetDialer installs DialControl, and DialControl
// is where the forced-interface binding (egressControl) and the Windows VPN
// service self-exclusion live. If it were dropped, this process's own control
// dials would route back into the tunnel it provides.
//
// The dial below is the strategy's own "normal" dialer with no proxy and no
// injected DialContextSettings, which is exactly the
// `DialContextSettings == nil && ProxySettings == nil` branch of
// ConnectSettings.DialContext: that branch builds a plain NetDialer and calls
// net.Dialer.DialContext on it. The control wrapper records that it ran and
// that it was handed a live socket, then chains egressControl the way
// production installs it.
func TestNormalStrategyDialPassesThroughEgressControl(t *testing.T) {
	index4, index6 := EgressInterfaceIndex()
	defer SetEgressInterfaceIndex(index4, index6)
	// zero leaves egressControl a no-op on every platform, so this test asserts
	// the plumbing, not a platform-specific bind
	SetEgressInterfaceIndex(0, 0)

	serverTls := selfSignedTlsConfig(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tlsListener := tls.NewListener(listener, serverTls)
	defer tlsListener.Close()
	go func() {
		for {
			serverConn, err := tlsListener.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				handshakeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = conn.(*tls.Conn).HandshakeContext(handshakeCtx)
			}(serverConn)
		}
	}()

	var controlCalls atomic.Int64
	var controlFds atomic.Int64
	var controlNetwork atomic.Value
	settings := DefaultClientStrategySettings()
	settings.TlsConfig = &tls.Config{InsecureSkipVerify: true}
	// the plain branch under test
	settings.DialContextSettings = nil
	settings.ProxySettings = nil
	settings.DialControl = func(network string, address string, c syscall.RawConn) error {
		controlCalls.Add(1)
		controlNetwork.Store(network)
		if err := c.Control(func(fd uintptr) {
			if fd != 0 {
				controlFds.Add(1)
			}
		}); err != nil {
			return err
		}
		return egressControl(network, address, c)
	}

	strategy := NewClientStrategy(context.Background(), settings)
	var dialTls DialTlsContextFunction
	for dialer := range strategy.dialers {
		if dialer.description == "normal" {
			dialTls = dialer.dialTlsContext
		}
	}
	if dialTls == nil {
		t.Fatal("the normal dialer is not registered")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := dialTls(ctx, "tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("normal strategy dial: %v", err)
	}
	conn.Close()

	if got := controlCalls.Load(); got != 1 {
		t.Fatalf("the egress control ran %d times on the normal strategy dial, want 1: "+
			"ConnectSettings.DialContext reached the network without NetDialer's Control", got)
	}
	if got := controlFds.Load(); got != 1 {
		t.Fatalf("the egress control saw %d socket fds, want 1: it did not run on the dialed socket", got)
	}
	if network, _ := controlNetwork.Load().(string); !strings.HasPrefix(network, "tcp") {
		t.Fatalf("the egress control saw network %q, want a tcp dial", network)
	}
}
