package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/urnetwork/connect"
	"golang.org/x/net/proxy"
)

// identityKey is the key reload writes into proxy.state for a credentialed
// proxy at addr.
func identityKey(addr, user string) string {
	return (&connect.ProxySettings{Address: addr, Auth: &proxy.Auth{User: user, Password: "p"}}).Key()
}

// TestPaidProxyGrader_GradesCredentialedFileProxyByIdentityKey pins that the
// grader finds a credentialed file proxy when proxy.state is keyed by
// identity (address+user), the way reload writes it. Before the fix the
// desired set was keyed by bare address, so the identity-keyed entry was
// never in it and the proxy was silently never graded.
func TestPaidProxyGrader_GradesCredentialedFileProxyByIdentityKey(t *testing.T) {
	home := withTempHome(t)
	writePaidGradeProbeOverride(t, true)

	addr, connects, cleanup := listenSocks5Sequenced(t, func(n int) byte { return 0x00 })
	defer cleanup()
	seedProbeDNSForAddress(t, addr, tableProbePassCounter.Load())

	src := filepath.Join(home, "paid.txt")
	if err := os.WriteFile(src, []byte(addr+":u:p\n"), 0600); err != nil {
		t.Fatal(err)
	}
	key := identityKey(addr, "u")
	if err := writeProxyState(&ProxyState{
		Source:  src,
		Proxies: map[string]ProxyEntry{key: {ID: 7, Health: "up", Source: "file"}},
	}); err != nil {
		t.Fatal(err)
	}

	runPaidProxyGradeOnce(context.Background(), "1.2.3.4", 443)

	if n := connects.Load(); n != 5 {
		t.Fatalf("expected 5 CONNECTs (4 table + 1 stage-0) to the dial address, got %d", n)
	}
	state, err := readProxyState()
	if err != nil {
		t.Fatal(err)
	}
	e, ok := state.Proxies[key]
	if !ok {
		t.Fatalf("identity-keyed entry must remain in proxy.state, have %v", state.Proxies)
	}
	if !e.Graded || e.Score != 1.0 {
		t.Errorf("expected grade 1.0 persisted under the identity key, got graded=%v score=%v", e.Graded, e.Score)
	}
	if _, bare := state.Proxies[addr]; bare {
		t.Errorf("grader must not resurrect a bare-address entry %q", addr)
	}
}

// TestPaidProxyGrader_UnreadableSourceDialsAddressNotKey pins the fileOK=false
// fallback: with the source file unreadable the collector still grades tracked
// entries, and must dial the address part of the identity key, never the raw
// key (which embeds the \x1f separator and the user).
func TestPaidProxyGrader_UnreadableSourceDialsAddressNotKey(t *testing.T) {
	home := withTempHome(t)
	writePaidGradeProbeOverride(t, true)

	addr, connects, cleanup := listenSocks5Sequenced(t, func(n int) byte { return 0x00 })
	defer cleanup()
	seedProbeDNSForAddress(t, addr, tableProbePassCounter.Load())

	key := identityKey(addr, "u")
	if err := writeProxyState(&ProxyState{
		Source:  filepath.Join(home, "missing.txt"),
		Proxies: map[string]ProxyEntry{key: {ID: 7, Health: "up", Source: "file"}},
	}); err != nil {
		t.Fatal(err)
	}

	runPaidProxyGradeOnce(context.Background(), "1.2.3.4", 443)

	if n := connects.Load(); n == 0 {
		t.Fatalf("collector dialed nothing: the identity key was not resolved to its dial address")
	}
	state, err := readProxyState()
	if err != nil {
		t.Fatal(err)
	}
	if e, ok := state.Proxies[key]; !ok || e.LastGraded.IsZero() {
		t.Errorf("entry under identity key should have advanced LastGraded, got ok=%v entry=%+v", ok, e)
	}
}
