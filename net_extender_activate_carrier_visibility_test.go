package connect

import (
	"strings"
	"testing"
)

func TestExtenderCarrierRecoveryLoggedOnce(t *testing.T) {
	log := &testActivateLogger{}
	fixture := newTestActivatorFixtureWithOperator(t, func(operator *testActivateOperator) {
		operator.setRefusedCarriers([]*ExtenderActivateCarrierRefusal{{Carrier: ExtenderCarrierQuic, Port: 443, Error: "no answer"}})
	}, func(settings *ExtenderActivatorSettings) { settings.ApiUrlV6 = ""; settings.Log = log })
	fixture.waitActivation()
	snapshot := fixture.activator.Status().Family(4)
	if snapshot == nil || len(snapshot.RefusedCarriers) != 1 {
		t.Fatalf("missing refusal: %+v", snapshot)
	}
	snapshot.RefusedCarriers[0].Port = 99
	if got := fixture.activator.Status().Family(4).RefusedCarriers[0].Port; got != 443 {
		t.Fatalf("snapshot aliased: %d", got)
	}
	fixture.operator.setRefusedCarriers(nil)
	fixture.networkChanged()
	fixture.waitActivation()
	lines := log.linesWithPrefix("🌉✅ [extender] IPv4: the platform now accepts ")
	if len(lines) != 1 || !strings.Contains(lines[0], "udp 443 (quic)") {
		t.Fatalf("recovery lines: %q", lines)
	}
	fixture.networkChanged()
	fixture.waitActivation()
	if lines = log.linesWithPrefix("🌉✅ [extender] IPv4: the platform now accepts "); len(lines) != 1 {
		t.Fatalf("repeated recovery: %q", lines)
	}
}
