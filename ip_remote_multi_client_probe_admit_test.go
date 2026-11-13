package connect

import (
	"context"
	"sync"
	"testing"
	"time"
)

type probeAdmitGenerator struct {
	MultiClientGenerator
	v6Destination MultiHopId

	mutex   sync.Mutex
	filters []IpFamilyFilter
}

func (self *probeAdmitGenerator) FixedDestinationSize() (int, bool) { return 0, false }

func (self *probeAdmitGenerator) NewClientArgs() (*MultiClientGeneratorClientArgs, error) {
	return &MultiClientGeneratorClientArgs{}, nil
}

func (self *probeAdmitGenerator) RemoveClientArgs(args *MultiClientGeneratorClientArgs) {}

func (self *probeAdmitGenerator) NextDestinationsWithIpFamily(
	count int,
	excludeDestinations []MultiHopId,
	rankMode string,
	ipFamilyFilter IpFamilyFilter,
) (map[MultiHopId]DestinationStats, error) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	self.filters = append(self.filters, ipFamilyFilter)
	if ipFamilyFilter == IpFamilyFilterV6Capable {
		return map[MultiHopId]DestinationStats{self.v6Destination: {}}, nil
	}
	return map[MultiHopId]DestinationStats{}, nil
}

func (self *probeAdmitGenerator) askedFilters() []IpFamilyFilter {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return append([]IpFamilyFilter{}, self.filters...)
}

// A family-swap probe the resize pass requests must reach the enumerator,
// which asks the platform for v6-capable destinations alone, marks the
// candidate ready for the resize pass while it waits for expand, and clears
// the mark once expand took it.
func TestIpv6ProbeMarksCandidateReadyUntilExpandTakesIt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	generator := &probeAdmitGenerator{v6Destination: RequireMultiHopId(NewId())}
	settings := DefaultMultiClientSettings()
	window := &multiClientWindow{
		ctx:               ctx,
		cancel:            cancel,
		log:               loggerOrDefault(settings.Log),
		generator:         generator,
		settings:          settings,
		windowType:        WindowTypeQuality,
		clientChannelArgs: make(chan *multiClientChannelArgs),
		clients:           map[Id]*multiClientChannel{},
		generatorMonitor:  NewMonitor(),
		resizeMonitor:     NewMonitor(),
		performanceProfile: &PerformanceProfile{
			WindowType: WindowTypeQuality,
		},
	}
	window.setIpv6Shortfall(1)

	// request before the enumerator starts so its first round is the probe
	window.requestIpv6Probe()
	if window.hasIpv6CandidateReady() {
		t.Fatal("no candidate is ready before the probe ran")
	}
	resizeNotify := window.resizeMonitor.NotifyChannel()
	go window.randomEnumerateClientArgs()

	// the candidate waits for expand, so the resize pass can see it
	deadline := time.Now().Add(5 * time.Second)
	for !window.hasIpv6CandidateReady() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !window.hasIpv6CandidateReady() {
		t.Fatal("the probe was never consumed by the enumerator")
	}

	select {
	case args := <-window.clientChannelArgs:
		if args.Destination != generator.v6Destination {
			t.Fatalf("expected the v6-capable candidate, got %v", args.Destination)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the candidate was never offered to expand")
	}

	select {
	case <-resizeNotify:
	case <-time.After(5 * time.Second):
		t.Fatal("the resize pass was not woken for the ready candidate")
	}

	deadline = time.Now().Add(5 * time.Second)
	for window.hasIpv6CandidateReady() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if window.hasIpv6CandidateReady() {
		t.Fatal("ready mark must clear once expand took the candidate")
	}

	// the probe round asked for v6-capable alone
	filters := generator.askedFilters()
	if len(filters) == 0 || filters[0] != IpFamilyFilterV6Capable {
		t.Fatalf("expected the probe to ask v6-capable first, got %v", filters)
	}
	if window.takeIpv6ProbeRequest() {
		t.Fatal("the probe request must be consumed")
	}
}
