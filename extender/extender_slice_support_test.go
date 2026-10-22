package extender

import (
	"fmt"
	"strings"
	"time"

	"github.com/urnetwork/connect"
)

// testProbeCarriers is the carriers the tcp-only slice of the port can dial
// from the tests. The client carrier layer (connect.DialExtender) is a later
// slice of the fork, so the quic and dns entries return with it; the probe
// tests that need a client carrier are skipped until then.
var testProbeCarriers = []string{connect.ExtenderCarrierTcp}

// nextNLayerError waits for the next attributed error of one stage, skipping
// the others. It is defined by the nlayer test in the pin; the admission test
// (which is a core test) uses it, so the core slice carries it until the
// nlayer slice moves it beside its own tests.
func nextNLayerError(fixture *extenderFixture, stage string) (error, bool) {
	deadline := time.After(10 * time.Second)
	for {
		select {
		case err := <-fixture.errors:
			if strings.HasPrefix(err.Error(), stage+":") {
				return err, true
			}
		case <-deadline:
			return nil, false
		}
	}
}

var _ = fmt.Sprintf

// testProbeConfig is the client config of one probe on a fixture carrier. It
// is defined by the probe latency test in the pin; the fixture ports test
// (a core test) uses it, so the core slice carries it until the probe client
// slice moves it beside its own tests.
func testProbeConfig(fixture *extenderFixture, carrier string, publicKey []byte) *connect.ExtenderConfig {
	return &connect.ExtenderConfig{
		Profile:   fixture.profile(carrier),
		Ip:        fixture.ip,
		PublicKey: publicKey,
	}
}
