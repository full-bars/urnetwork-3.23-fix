package urnettools

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/urnetwork/connect/internal/tui"
)

func TestUDPInvariantOutputOmitsProxyIdentity(t *testing.T) {
	var udp SnapshotProxyUDP
	if err := json.Unmarshal([]byte(`{"ok":3,"unchecked":4,"identity":"192.0.2.66:24001 fixture-account"}`), &udp); err != nil {
		t.Fatal(err)
	}
	s := loadSnapshotFixture(t, "node_snapshot_v1.json")
	s.Proxies.UDP = &udp
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	p := Provider{User: "u", StateDir: t.TempDir(), Running: true}
	stubStatus(t, []Provider{p}, map[string]json.RawMessage{"": raw}, true)
	status := captureStdout(t, func() {
		if err := cmdStatus([]string{"--plain"}); err != nil {
			t.Fatal(err)
		}
	})
	model := newTestModel(tui.DefaultTheme(), topProviders(1), &fakeClock{t: topBase})
	model.apply(0, s, nil)
	top, _ := showOnSim(t, model, 120, 40)
	for _, output := range []string{status, top, renderLiveBlock(s, liveOpts{}, "")} {
		if !strings.Contains(output, "3 ok") {
			t.Fatal("UDP coverage row missing")
		}
		for _, secret := range []string{"192.0.2.66", "24001", "fixture-account"} {
			if strings.Contains(output, secret) {
				t.Fatal("UDP output contains proxy identity")
			}
		}
	}
}
