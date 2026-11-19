package connect

import (
	"reflect"
	"sort"
	"testing"
	"time"
)

func TestProxyHealthSnapshotOnlineIdentities(t *testing.T) {
	ResetProxyHealthForTesting()
	t.Cleanup(ResetProxyHealthForTesting)
	proxyHealthMu.Lock()
	proxyHealthByIndex = map[int]*proxyHealth{
		0: {key: "direct", currentlyUp: true},
		1: {key: "up", currentlyUp: true},
		2: {key: "degraded", everUp: true},
		3: {key: "connecting", connecting: true},
		4: {key: "dead"},
		5: {key: "old", everUp: true, downSince: time.Unix(1, 0)},
		6: {key: "held", currentlyUp: true},
	}
	proxyHealthMu.Unlock()
	_, _, _, _, _, online, proxied := ProxyHealthSnapshotWithIdentities(map[string]bool{"held": true})
	sort.Strings(online)
	if !proxied || !reflect.DeepEqual(online, []string{"degraded", "up"}) {
		t.Fatalf("online=%v proxied=%v", online, proxied)
	}
	ResetProxyHealthForTesting()
	proxyHealthMu.Lock()
	proxyHealthByIndex[0] = &proxyHealth{key: "direct", currentlyUp: true}
	proxyHealthMu.Unlock()
	_, _, _, _, _, online, proxied = ProxyHealthSnapshotWithIdentities(nil)
	if proxied || len(online) != 0 {
		t.Fatalf("direct only: online=%v proxied=%v", online, proxied)
	}
}
