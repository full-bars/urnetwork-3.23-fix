package connect

import (
	"context"
	"errors"
	"testing"
)

type stickyTestGenerator struct {
	MultiClientGenerator
	answer map[MultiHopId]DestinationStats
	err    error
	asked  []Id
}

func (self *stickyTestGenerator) FixedDestinationSize() (int, bool) { return 0, false }

func (self *stickyTestGenerator) NextDestinationsForClientId(clientId Id, excludeDestinations []MultiHopId, rankMode string) (map[MultiHopId]DestinationStats, error) {
	self.asked = append(self.asked, clientId)
	return self.answer, self.err
}

func stickyTestWindow(generator MultiClientGenerator, fixedSize int) *multiClientWindow {
	settings := DefaultMultiClientSettings()
	return &multiClientWindow{
		ctx:       context.Background(),
		generator: generator,
		settings:  settings,
		performanceProfile: &PerformanceProfile{
			WindowType: WindowTypeQuality,
			WindowSize: WindowSizeSettings{FixedWindowSize: fixedSize},
		},
	}
}

func TestStickyRedialTakeClearsAndForgetDrops(t *testing.T) {
	var memory stickyRedial
	if _, _, ok := memory.Take(); ok {
		t.Fatal("zero value must hold nothing")
	}
	destination := RequireMultiHopId(NewId())
	memory.Remember(destination, DestinationStats{})
	if got, _, ok := memory.Take(); !ok || got != destination {
		t.Fatal("expected the remembered exit")
	}
	if _, _, ok := memory.Take(); ok {
		t.Fatal("Take must clear the exit")
	}
	memory.Remember(destination, DestinationStats{})
	memory.Forget()
	if _, _, ok := memory.Take(); ok {
		t.Fatal("Forget must drop the exit")
	}
}

func TestEnumerateStickyRedialDialsRememberedExit(t *testing.T) {
	destination := RequireMultiHopId(NewId())
	generator := &stickyTestGenerator{answer: map[MultiHopId]DestinationStats{destination: {}}}
	window := stickyTestWindow(generator, 1)
	window.stickyRedial.Remember(destination, DestinationStats{})

	redial, ok, err := window.enumerateStickyRedial(nil, "")
	if err != nil || !ok || !redial.stickyRedial || redial.destination != destination {
		t.Fatalf("expected a sticky redial, got %+v ok=%v err=%v", redial, ok, err)
	}
	if len(generator.asked) != 1 || generator.asked[0] != destination.Tail() {
		t.Fatalf("expected one lookup by tail id, got %v", generator.asked)
	}
}

func TestEnumerateStickyRedialFallsBack(t *testing.T) {
	destination := RequireMultiHopId(NewId())

	// the platform answers without the provider
	generator := &stickyTestGenerator{answer: map[MultiHopId]DestinationStats{}}
	window := stickyTestWindow(generator, 1)
	window.stickyRedial.Remember(destination, DestinationStats{})
	if _, ok, err := window.enumerateStickyRedial(nil, ""); ok || err != nil {
		t.Fatalf("expected fallback to discovery, ok=%v err=%v", ok, err)
	}
	if _, _, pending := window.stickyRedial.Take(); pending {
		t.Fatal("an unavailable exit must not stay pending")
	}

	// the platform cannot be reached: the exit stays pending
	generator = &stickyTestGenerator{err: errors.New("down")}
	window = stickyTestWindow(generator, 1)
	window.stickyRedial.Remember(destination, DestinationStats{})
	if _, ok, err := window.enumerateStickyRedial(nil, ""); ok || err == nil {
		t.Fatalf("expected the lookup error, ok=%v err=%v", ok, err)
	}
	if _, _, pending := window.stickyRedial.Take(); !pending {
		t.Fatal("a failed lookup must keep the exit pending")
	}

	// a window that is not sticky never redials
	generator = &stickyTestGenerator{answer: map[MultiHopId]DestinationStats{destination: {}}}
	window = stickyTestWindow(generator, 2)
	window.stickyRedial.Remember(destination, DestinationStats{})
	if _, ok, _ := window.enumerateStickyRedial(nil, ""); ok || len(generator.asked) != 0 {
		t.Fatal("a non-sticky window must not look up")
	}
}

func TestWindowClientStatsNoCompatible(t *testing.T) {
	var stats WindowClientStats
	stats.addNoCompatible(&IpPath{Version: 6, Protocol: IpProtocolTcp})
	stats.addNoCompatible(&IpPath{Version: 6, Protocol: IpProtocolTcp})
	stats.addNoCompatible(&IpPath{Version: 4, Protocol: IpProtocolUdp})
	stats.addNoCompatible(&IpPath{Version: 4, Protocol: 99})
	stats.addNoCompatible(nil)

	counts := map[string]uint64{}
	stats.VisitNoCompatible(func(ipVersion string, protocol string, count uint64) {
		counts[ipVersion+"/"+protocol] = count
	})
	if counts["6/tcp"] != 2 || counts["4/udp"] != 1 || len(counts) != 6 {
		t.Fatalf("unexpected counts %v", counts)
	}

	var nilStats *WindowClientStats
	nilStats.addNoCompatible(&IpPath{Version: 4, Protocol: IpProtocolTcp})
}

func TestParentWindowClientStatsIsShared(t *testing.T) {
	parent := &RemoteUserNatMultiClient{}
	parent.windowClientStats().addNoCompatible(&IpPath{Version: 4, Protocol: IpProtocolTcp})
	got := uint64(0)
	parent.windowClientStats().VisitNoCompatible(func(v string, p string, c uint64) {
		if v == "4" && p == "tcp" {
			got = c
		}
	})
	if got != 1 {
		t.Fatalf("expected 1, got %d", got)
	}
}
