package main

import (
	"reflect"
	"testing"

	"github.com/urnetwork/connect"
	"golang.org/x/net/proxy"
)

func authSettingsFor(addr, user string) *connect.ProxySettings {
	return &connect.ProxySettings{Network: "tcp", Address: addr, Auth: &proxy.Auth{User: user, Password: "p"}}
}

// A tracked credentialed proxy that is still in the desired list must be
// neither "removed" nor "added". Diffing by bare address listed every one as
// both, and the operator confirmed a prompt full of raw identity keys.
func TestPlanProxyRefresh_CredentialedProxyInSyncIsNotAChange(t *testing.T) {
	desired := []*connect.ProxySettings{authSettingsFor("gw.example:1080", "u1"), authSettingsFor("gw.example:1080", "u2")}
	current := map[string]ProxyEntry{
		identityKey("gw.example:1080", "u1"): {ID: 1, Health: "up"},
		identityKey("gw.example:1080", "u2"): {ID: 2, Health: "up"},
	}
	added, removed := planProxyRefresh(desired, current)
	if len(added) != 0 || len(removed) != 0 {
		t.Fatalf("in-sync list must produce no changes, got added=%v removed=%v", added, removed)
	}
}

// One account leaving a shared gateway removes exactly that identity, and a
// new account is added by its own key, sorted for a stable prompt.
func TestPlanProxyRefresh_AccountAtSharedGatewayChanges(t *testing.T) {
	desired := []*connect.ProxySettings{authSettingsFor("gw.example:1080", "u2"), authSettingsFor("gw.example:1080", "u4"), authSettingsFor("gw.example:1080", "u3")}
	current := map[string]ProxyEntry{
		identityKey("gw.example:1080", "u1"): {ID: 1, Health: "dead"},
		identityKey("gw.example:1080", "u2"): {ID: 2, Health: "up"},
	}
	added, removed := planProxyRefresh(desired, current)

	wantAdded := []string{identityKey("gw.example:1080", "u3"), identityKey("gw.example:1080", "u4")}
	if !reflect.DeepEqual(added, wantAdded) {
		t.Fatalf("added=%q, want %q", added, wantAdded)
	}
	if len(removed) != 1 || removed[0].key != identityKey("gw.example:1080", "u1") || removed[0].entry.Health != "dead" {
		t.Fatalf("removed=%+v, want only the u1 account", removed)
	}
}

// Unauthenticated (internal-config) proxies keep their bare-address key and an
// empty health is classified, as before.
func TestPlanProxyRefresh_BareAddressAndHealthClassification(t *testing.T) {
	desired := []*connect.ProxySettings{{Network: "tcp", Address: "10.0.0.1:1080"}}
	current := map[string]ProxyEntry{"10.0.0.2:1080": {ID: 2}}
	added, removed := planProxyRefresh(desired, current)
	if !reflect.DeepEqual(added, []string{"10.0.0.1:1080"}) {
		t.Fatalf("added=%q", added)
	}
	if len(removed) != 1 || removed[0].key != "10.0.0.2:1080" || removed[0].entry.Health != "starting" {
		t.Fatalf("removed=%+v, want the bare-address proxy classified starting", removed)
	}
}
