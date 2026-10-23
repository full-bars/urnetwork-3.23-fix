package connect

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Path discovery is where a provider decides which other providers can carry
// its traffic. These counters answer the two questions an operator actually
// asks: what is each window building, and how long does a channel take to
// become usable. They are updated once per channel, never per packet, and read
// with one mutex at scrape time.
type pathDiscoveryCounters struct {
	mu            sync.Mutex
	channels      map[string]uint64
	connects      map[string]uint64
	connectMillis map[string]uint64
}

var globalPathDiscovery pathDiscoveryCounters

func pathDiscoveryWindowName(window WindowType) string {
	if name := window.RankMode(); name != "" {
		return name
	}
	return "auto"
}

func (self *pathDiscoveryCounters) initLocked() {
	if self.channels == nil {
		self.channels = map[string]uint64{}
		self.connects = map[string]uint64{}
		self.connectMillis = map[string]uint64{}
	}
}

func (self *pathDiscoveryCounters) recordCreated(window WindowType) {
	name := pathDiscoveryWindowName(window)
	self.mu.Lock()
	self.initLocked()
	self.channels[name]++
	self.mu.Unlock()
}

func (self *pathDiscoveryCounters) recordConnected(window WindowType, elapsed time.Duration) {
	name := pathDiscoveryWindowName(window)
	millis := uint64(elapsed / time.Millisecond)
	self.mu.Lock()
	self.initLocked()
	self.connects[name]++
	self.connectMillis[name] += millis
	self.mu.Unlock()
}

func recordPathChannelCreated(window WindowType) {
	globalPathDiscovery.recordCreated(window)
}

func recordPathChannelConnected(window WindowType, elapsed time.Duration) {
	globalPathDiscovery.recordConnected(window, elapsed)
}

// metricsText renders the discovery counters for /metrics. Every window is
// emitted even before it builds anything, so a dashboard reads a stable zero
// rather than a gap.
func (self *pathDiscoveryCounters) metricsText() string {
	self.mu.Lock()
	channels := make(map[string]uint64, len(self.channels))
	for window, count := range self.channels {
		channels[window] = count
	}
	connects := make(map[string]uint64, len(self.connects))
	for window, count := range self.connects {
		connects[window] = count
	}
	connectMillis := make(map[string]uint64, len(self.connectMillis))
	for window, millis := range self.connectMillis {
		connectMillis[window] = millis
	}
	self.mu.Unlock()

	windows := []string{"quality", "speed", "auto"}
	emit := func(b *strings.Builder, name string, counts map[string]uint64, help string) {
		fmt.Fprintf(b, "# HELP %s %s\n", name, help)
		fmt.Fprintf(b, "# TYPE %s counter\n", name)
		for _, window := range windows {
			fmt.Fprintf(b, "%s{window=%s} %d\n", name, PrometheusLabelValue(window), counts[window])
		}
		var extra []string
		for window := range counts {
			if window != "quality" && window != "speed" && window != "auto" {
				extra = append(extra, window)
			}
		}
		sort.Strings(extra)
		for _, window := range extra {
			fmt.Fprintf(b, "%s{window=%s} %d\n", name, PrometheusLabelValue(window), counts[window])
		}
	}

	var b strings.Builder
	emit(&b, "urnet_path_channels_total", channels, "Provider channels created, by window.")
	emit(&b, "urnet_path_channel_connects_total", connects, "Provider channels that reached a connected provider, by window.")
	emit(&b, "urnet_path_channel_connect_millis_total", connectMillis, "Sum of channel connect times in milliseconds, by window; divide by the connects for the mean.")
	return b.String()
}

// PathDiscoveryMetricsText renders the process-wide discovery counters for
// /metrics.
func PathDiscoveryMetricsText() string {
	return globalPathDiscovery.metricsText()
}
