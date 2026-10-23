package connect

// net_tls_hello_gate_test.go -- the runtime gate for the tls client hello
// (net_tls_hello_gate.go), proven on the real dialers rather than on the
// handshaker's fields: each test dials the shared local endpoint, captures the
// hello the endpoint received, and compares it with the committed Chrome
// golden. A gate that were read once at construction would present the same
// hello on both dials and fail here.

import (
	"context"
	"fmt"
	"net"
	"slices"
	"testing"

	"github.com/urnetwork/connect/fingerprint"
)

// tlsHelloGateForTest leaves the process gate unset when the test ends, so the
// next test in this package starts from its strategy's own fingerprint again.
func tlsHelloGateForTest(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { tlsHelloGate.Store(tlsHelloUnset) })
}

// SetTlsHelloGateForTest drives the process gate for the external conformance
// tests, which cannot reach it otherwise, and leaves it unset when the test
// ends. The gate is process-wide, so a test that sets it must not run beside
// another that relies on the base fingerprint.
func SetTlsHelloGateForTest(t *testing.T, chrome bool) {
	t.Helper()
	tlsHelloGateForTest(t)
	SetTlsClientHelloChrome(chrome)
}

// tlsHelloGateSettings is strategy settings whose dials reach endpoint whatever
// name they dial, trusting its private ca, with base as the strategy's
// fingerprint (the value the gate overrides).
func tlsHelloGateSettings(t *testing.T, endpoint *fingerprint.Endpoint, base string) *ClientStrategySettings {
	t.Helper()
	tlsConfig, err := DefaultTlsConfig()
	if err != nil {
		t.Fatal(err)
	}
	tlsConfig.RootCAs = endpoint.CaCertPool()
	settings := DefaultClientStrategySettings()
	settings.TlsConfig = tlsConfig
	settings.TlsClientHelloFingerprint = base
	addr := endpoint.Addr()
	settings.ConnectSettings.DialContextSettings = &DialContextSettings{
		DialContext: func(ctx context.Context, network string, address string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp4", addr)
		},
	}
	return settings
}

// tlsHelloGateWebSocketHello dials the endpoint's websocket through one fixture
// dialer under settings and returns the client hello the endpoint captured for
// that dial.
func tlsHelloGateWebSocketHello(
	t *testing.T,
	endpoint *fingerprint.Endpoint,
	dialer testTlsHelloDialer,
	settings *ClientStrategySettings,
) *fingerprint.ClientHelloFingerprint {
	t.Helper()
	authority := fmt.Sprintf("%s:%d", fingerprint.ServerName, endpoint.Port())
	testTlsHelloWebSocket(t, dialer.clientDialer(settings), authority)
	captures := endpoint.CapturedClientHellos()
	capture := captures[len(captures)-1]
	parsed, err := fingerprint.ParseClientHello(capture.Message)
	if err != nil {
		t.Fatal(err)
	}
	parsed.RecordCount = capture.RecordCount
	return parsed
}

// The gate reports whether the Chrome hello was on before, and an unset gate
// reads as off (the hello the default presents), so a first `on` logs
// `(was off)` rather than a transition out of a gate nobody set.
func TestTlsClientHelloGateSetReportsPrevious(t *testing.T) {
	tlsHelloGateForTest(t)

	if TlsClientHelloChrome() {
		t.Fatal("a fresh gate must read off")
	}
	if previous := SetTlsClientHelloChrome(true); previous {
		t.Error("the first `on` reported a previous on")
	}
	if !TlsClientHelloChrome() {
		t.Error("the gate did not turn on")
	}
	if previous := SetTlsClientHelloChrome(true); !previous {
		t.Error("a second `on` did not report the previous on")
	}
	if previous := SetTlsClientHelloChrome(false); !previous {
		t.Error("`off` did not report the previous on")
	}
	if TlsClientHelloChrome() {
		t.Error("the gate did not turn off")
	}
	if previous := SetTlsClientHelloChrome(false); previous {
		t.Error("a second `off` reported a previous on")
	}
}

// The gate is read on every dial, by both the normal and the resilient dialers:
// with the strategy's base set to Go's hello, a dial with the gate off presents
// Go's hello, the gate flips, and the next dial presents the committed Chrome
// hello. The same comparison run the other way proves the gate also overrides a
// base that asks for Chrome, which is what makes `tls_hello off` a real kill
// switch and not only a fallback.
func TestTlsClientHelloGateReadPerDial(t *testing.T) {
	golden, err := fingerprint.LoadGolden(fingerprint.GoldenChrome133Synthetic)
	if err != nil {
		t.Fatal(err)
	}

	for _, testDialer := range testTlsHelloDialers {
		t.Run(testDialer.description, func(t *testing.T) {
			endpoint, err := fingerprint.NewEndpoint(fingerprint.EndpointOptions{Handler: testTlsHelloHandler()})
			if err != nil {
				t.Fatal(err)
			}
			defer endpoint.Close()

			// the websocket path offers http/1.1 alone, so no alps beside it
			diffOptions := fingerprint.DiffOptions{
				ExpectedServerName:    fingerprint.ServerName,
				ExpectedAlpnProtocols: clientWebSocketNextProtos,
			}
			if !testDialer.fragment {
				// the fragmenting dialers reshape records on purpose
				diffOptions.ExpectRecordCount = 1
			}

			tlsHelloGateForTest(t)
			settings := tlsHelloGateSettings(t, endpoint, TlsClientHelloFingerprintGo)

			SetTlsClientHelloChrome(false)
			goHello := tlsHelloGateWebSocketHello(t, endpoint, testDialer, settings)
			SetTlsClientHelloChrome(true)
			chromeHello := tlsHelloGateWebSocketHello(t, endpoint, testDialer, settings)

			if slices.Equal(goHello.CipherSuites, chromeHello.CipherSuites) {
				t.Fatalf("both dials presented the same cipher suites %v: the gate was not read per dial", goHello.CipherSuites)
			}
			if drifts := fingerprint.Diff(golden.Fingerprint, chromeHello, diffOptions); len(drifts) != 0 {
				t.Errorf("gate on: %s", fingerprint.FormatDrift(fingerprint.GoldenChrome133Synthetic, drifts))
			}
			drifts := fingerprint.Diff(golden.Fingerprint, goHello, diffOptions)
			if len(drifts) == 0 {
				t.Fatal("gate off presented the Chrome hello, so off does not switch the hello")
			}
			fields := make([]string, len(drifts))
			for i, drift := range drifts {
				fields[i] = drift.Field
			}
			if !slices.Contains(fields, "cipher_suites") {
				t.Errorf("gate off drifted in %v, want Go's cipher list named", fields)
			}

			// the same gate off must beat a base that asks for Chrome, as the
			// provider's `tls_hello off` must beat an env or a strategy default
			chromeBaseSettings := tlsHelloGateSettings(t, endpoint, TlsClientHelloFingerprintChrome)
			SetTlsClientHelloChrome(false)
			chromeBaseHello := tlsHelloGateWebSocketHello(t, endpoint, testDialer, chromeBaseSettings)
			if drifts := fingerprint.Diff(golden.Fingerprint, chromeBaseHello, diffOptions); len(drifts) == 0 {
				t.Error("gate off kept the Chrome hello of a chrome base fingerprint")
			}
		})
	}
}
