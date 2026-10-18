package connect

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
