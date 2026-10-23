package connect

// vless_refusal_test.go — a family-pinned direct strategy refuses VLESS
// servers whatever path they arrive by, ported from the pin's
// net_http_refusal_test.go (TestDirectClientStrategyRefusesLaterVlessAndCustomExtenders).

import (
	"context"
	"net/netip"
	"testing"
)

// A family-pinned direct strategy keeps its extenders and VLESS servers
// dropped after construction: a later `SetVlessConfigs` or
// `SetCustomExtenders` adds none.
func TestDirectClientStrategyRefusesLaterVlessAndCustomExtenders(t *testing.T) {
	settings := DefaultClientStrategySettings()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	strategy := NewDirectClientStrategy(ctx, settings, 4)
	defer strategy.Close()

	strategy.SetVlessConfigs([]*VlessConfig{{
		Address:  "192.0.2.1",
		Port:     443,
		Id:       testVlessUserId,
		Network:  VlessNetworkTcp,
		Security: VlessSecurityNone,
	}})
	if vlessConfigs := strategy.VlessConfigs(); len(vlessConfigs) != 0 {
		t.Fatalf("direct strategy took %d VLESS dialers after construction", len(vlessConfigs))
	}
	strategy.SetCustomExtenders(map[netip.Addr]string{netip.MustParseAddr("198.51.100.7"): "secret"})
	if customExtenders := strategy.CustomExtenders(); len(customExtenders) != 0 {
		t.Fatalf("direct strategy took %d custom extenders after construction", len(customExtenders))
	}
	if settings.DisableVless || settings.DisableManualExtenders {
		t.Fatal("the caller's settings must not change")
	}
}
