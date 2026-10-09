package connect

import (
	"context"
	"os"
	"reflect"
	"sort"
	"testing"
)

// The detector tests exercise the privileged-port signature check, which
// production leaves off by default. Opt in for the whole test binary; tests that
// check the production default clear the variable themselves.
func init() {
	os.Setenv("URNETWORK_DPI_PRIVILEGED_BT", "1")
}

func TestDpiEnvOptIn(t *testing.T) {
	cases := []struct {
		admits, privileged     string
		wantAdmits, wantPrivBt bool
	}{
		{"", "", true, false},
		{"off", "0", false, false},
		{"on", "", true, false},
		{"OFF", "1", false, true},
		{"", "1", true, true},
		{"false", "true", true, false},
	}
	t.Cleanup(resetDpiAdmitsEnvForTest)
	for _, c := range cases {
		t.Setenv("URNETWORK_DPI_ADMITS", c.admits)
		t.Setenv("URNETWORK_DPI_PRIVILEGED_BT", c.privileged)
		resetDpiAdmitsEnvForTest()
		if got := DpiAdmitsEnabled(); got != c.wantAdmits {
			t.Fatalf("ADMITS=%q: got %t, want %t", c.admits, got, c.wantAdmits)
		}
		if got := DpiPrivilegedBtEnabled(); got != c.wantPrivBt {
			t.Fatalf("PRIVILEGED_BT=%q: got %t, want %t", c.privileged, got, c.wantPrivBt)
		}
	}
}

// The production default with nothing set: admits on, privileged check off.
func TestDpiProductionDefaults(t *testing.T) {
	t.Setenv("URNETWORK_DPI_ADMITS", "")
	t.Setenv("URNETWORK_DPI_PRIVILEGED_BT", "")
	resetDpiAdmitsEnvForTest()
	t.Cleanup(resetDpiAdmitsEnvForTest)
	if !DpiAdmitsEnabled() {
		t.Fatal("application-standard admits must default on")
	}
	if DpiPrivilegedBtEnabled() {
		t.Fatal("privileged-port BitTorrent check must default off")
	}
}

// URNETWORK_DPI_ADMITS=off must restore the previous verdicts through every
// default constructor. A detector added later as another optional setting
// would silently escape the switch, so the set of optional detector fields on
// the DMCA settings is pinned: adding one fails this test until the switch
// covers it.
func TestDpiAdmitsOffCoversEveryDefaultConstructor(t *testing.T) {
	t.Setenv("URNETWORK_DPI_ADMITS", "off")
	resetDpiAdmitsEnvForTest()
	t.Cleanup(resetDpiAdmitsEnvForTest)

	dmca := DefaultDmcaSecurityPolicySettings()
	if dmca.App != nil || dmca.Gaming != nil || dmca.Messaging != nil {
		t.Fatalf("DMCA settings keep a detector with admits off: app=%v gaming=%v messaging=%v", dmca.App, dmca.Gaming, dmca.Messaging)
	}
	web := DefaultWebStandardSettings()
	if web.Turn || web.Rtp || web.Rtcp {
		t.Fatalf("web standard settings keep an admit with admits off: turn=%t rtp=%t rtcp=%t", web.Turn, web.Rtp, web.Rtcp)
	}
	if DefaultCfaaSecurityPolicySettings().AllowTelegramCalls {
		t.Fatal("CFAA settings keep the Telegram call exception with admits off")
	}

	optional := []string{}
	typ := reflect.TypeOf(DmcaSecurityPolicySettings{})
	for i := 0; i < typ.NumField(); i++ {
		if typ.Field(i).Type.Kind() == reflect.Ptr {
			optional = append(optional, typ.Field(i).Name)
		}
	}
	sort.Strings(optional)
	if want := []string{"App", "Gaming", "Messaging"}; !reflect.DeepEqual(optional, want) {
		t.Fatalf("optional detector fields on DmcaSecurityPolicySettings = %v, want %v: wire the new one into applyDpiAdmitsEnvToDmca and update this list", optional, want)
	}
}

// The detector tests opt in to the privileged-port BitTorrent check for the
// whole binary (see init), so the production default needs its own proof: with
// nothing set, BitTorrent on a privileged port is allowed without inspection, as
// before this change, and with URNETWORK_DPI_PRIVILEGED_BT=1 it is an incident.
func TestPrivilegedPortBittorrentFollowsTheSwitch(t *testing.T) {
	t.Cleanup(resetDpiAdmitsEnvForTest)
	for _, name := range []string{"bittorrent-tcp-443", "bittorrent-tcp-80", "dht-udp-443"} {
		fixture := loadSecurityFixture(t, "testdata/ipsecurity/"+name+".json")
		for _, tc := range []struct {
			value string
			want  SecurityPolicyResult
		}{
			{"", SecurityPolicyResultAllow},
			{"1", SecurityPolicyResultIncident},
		} {
			t.Setenv("URNETWORK_DPI_PRIVILEGED_BT", tc.value)
			resetDpiAdmitsEnvForTest()
			policy := DefaultSecurityPolicy(context.Background())
			results := replayFixture(t, policy, fixture, 49000)
			if last := results[len(results)-1]; last != tc.want {
				t.Fatalf("%s with URNETWORK_DPI_PRIVILEGED_BT=%q: last verdict %v, want %v", name, tc.value, last, tc.want)
			}
		}
	}
}
