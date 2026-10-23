package connect

import (
	"strings"
	"testing"
	"time"
)

// the discovery counters must count one channel per creation and one connect,
// with the elapsed time summed, and must render every window even at zero
func TestPathDiscoveryCountersAndRender(t *testing.T) {
	// a local counter: mutating the process-wide one while other tests run
	// would race under -race, which is how CI caught this
	var discovery pathDiscoveryCounters
	discovery.recordCreated(WindowTypeQuality)
	discovery.recordCreated(WindowTypeQuality)
	discovery.recordCreated(WindowTypeSpeed)
	discovery.recordConnected(WindowTypeQuality, 250*time.Millisecond)
	discovery.recordConnected(WindowTypeQuality, 150*time.Millisecond)

	text := discovery.metricsText()
	for _, want := range []string{
		`urnet_path_channels_total{window="quality"} 2`,
		`urnet_path_channels_total{window="speed"} 1`,
		`urnet_path_channels_total{window="auto"} 0`,
		`urnet_path_channel_connects_total{window="quality"} 2`,
		`urnet_path_channel_connect_millis_total{window="quality"} 400`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("metrics text missing %q:\n%s", want, text)
		}
	}
}
