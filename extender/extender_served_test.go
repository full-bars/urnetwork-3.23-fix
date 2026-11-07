// What counts as a request served for a client (see provider/extender_tally.go).
//
// The service counters (ServiceCounts) are incremented when a header decodes,
// before it is authorized and admitted, and the `proxy` counter is incremented
// for every ordinary non-extender http request. Neither says a client was helped.
// ServedCounts counts a request only once it has passed authorization and
// admission and the stream has been handed over.

package extender

import (
	"io"
	"net/http"
	"testing"

	"github.com/urnetwork/connect"
)

// A relayed forward is served, on every carrier.
func TestExtenderServedCountsAnAcceptedForward(t *testing.T) {
	endpoint := newRelayEndpoint(t, "127.0.0.1")
	fixture := newRelayStatsFixture(t, endpoint)

	if served := fixture.server.ServedCounts(); len(served) != 0 {
		t.Fatalf("a server that has served nothing counts %v", served)
	}
	for i, carrier := range []string{
		connect.ExtenderCarrierTcp,
		connect.ExtenderCarrierQuic,
		connect.ExtenderCarrierDns,
	} {
		if err := relayTestSession(fixture, carrier); err != nil {
			t.Fatalf("%s: %v", carrier, err)
		}
		if got := fixture.server.ServedCounts()["forward"]; got != uint64(i+1) {
			t.Fatalf("after %s: served forwards = %d, want %d", carrier, got, i+1)
		}
	}
}

// An ordinary web request to the port (a scanner, a browser) is answered like a
// plain website and is not a client: it counts as the `proxy` service but not as
// served.
func TestExtenderServedIgnoresOrdinaryWebRequests(t *testing.T) {
	fixture := newExtenderFixture(t, "127.0.0.1", nil)
	for _, protocol := range testProxyProtocols {
		response := getThroughExtender(t, fixture, protocol, testSpoofName, "/")
		io.Copy(io.Discard, response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("%s status = %d", protocol, response.StatusCode)
		}
	}
	if fixture.server.ServiceCounts()["proxy"] == 0 {
		t.Fatal("the decoy proxy requests were not counted as the proxy service")
	}
	if served := fixture.server.ServedCounts(); len(served) != 0 {
		t.Fatalf("ordinary web requests were counted as served: %v", served)
	}
}

// A forward to a host that is not on the allow list is refused, so it is not
// served, even though its header decoded and counted as the forward service.
func TestExtenderServedIgnoresARefusedForward(t *testing.T) {
	endpoint := newRelayEndpoint(t, "127.0.0.1")
	fixture := newRelayStatsFixture(t, endpoint)

	// the allow list holds testRelayHost; any other destination is refused
	if err := relayTestSessionTo(fixture, connect.ExtenderCarrierTcp, "not-allowed.example"); err == nil {
		t.Fatal("a forward to a host that is not allowed was not refused")
	}
	if got := fixture.server.ServiceCounts()["forward"]; got != 1 {
		t.Fatalf("service count for the refused forward = %d, want 1 (it is counted when the header decodes)", got)
	}
	if served := fixture.server.ServedCounts(); len(served) != 0 {
		t.Fatalf("a refused forward was counted as served: %v", served)
	}
	if refused := fixture.server.RefusalCounts()["destination authorization"]; refused != 1 {
		t.Fatalf("destination authorization refusals = %d, want 1", refused)
	}
}
