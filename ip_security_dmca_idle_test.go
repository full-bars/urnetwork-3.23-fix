package connect

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// a client->destination path for the dmca flow table (both ports unprivileged)
func dmcaIdleTestPath(dstPort int) *IpPath {
	return &IpPath{
		Version:         4,
		Protocol:        IpProtocolUdp,
		SourceIp:        net.ParseIP("10.0.0.5"),
		SourcePort:      40000,
		DestinationIp:   net.ParseIP("198.51.100.7"),
		DestinationPort: dstPort,
	}
}

func newIdleDmcaDetector(ctx context.Context, flowTtl time.Duration) *dmcaDetector {
	settings := DefaultDmcaSecurityPolicySettings()
	settings.FlowTtl = flowTtl
	return newDmcaDetector(ctx, settings, newWebStandardDetector(DefaultWebStandardSettings()))
}

// ageFlow backdates a tracked flow's idle clock.
func ageFlow(t *testing.T, detector *dmcaDetector, key Ip6Path, age time.Duration) {
	t.Helper()
	shard := detector.shards[dmcaShardIndex(key)]
	shard.mu.RLock()
	st := shard.flows[key]
	shard.mu.RUnlock()
	if st == nil {
		t.Fatalf("flow %v is not tracked", key)
	}
	atomic.StoreInt64(&st.lastActivityUnixNanos, time.Now().Add(-age).UnixNano())
}

func TestDmcaIdleFlowIsEvictedAndActiveFlowSurvives(t *testing.T) {
	detector := newIdleDmcaDetector(nil, 10*time.Minute)
	oldPath := dmcaIdleTestPath(6881)
	freshPath := dmcaIdleTestPath(6882)
	detector.classify(oldPath, nil)
	detector.classify(freshPath, nil)
	if got := detector.Testing_FlowCount(); got != 2 {
		t.Fatalf("tracked flows = %d, want 2", got)
	}
	ageFlow(t, detector, oldPath.ToIp6Path(), 11*time.Minute)

	detector.evictIdle(time.Now())
	if got := detector.Testing_FlowCount(); got != 1 {
		t.Fatalf("after the scan: %d flows, want 1 (the idle one reclaimed)", got)
	}
	shard := detector.shards[dmcaShardIndex(freshPath.ToIp6Path())]
	shard.mu.RLock()
	_, ok := shard.flows[freshPath.ToIp6Path()]
	shard.mu.RUnlock()
	if !ok {
		t.Fatal("the active flow was evicted")
	}
}

func TestDmcaIngressTouchRefreshesOnlyTrackedUnprivilegedFlows(t *testing.T) {
	detector := newIdleDmcaDetector(nil, 10*time.Minute)
	path := dmcaIdleTestPath(6881)
	detector.classify(path, nil)
	key := path.ToIp6Path()

	// the return direction of a tracked flow keeps it from aging out
	ageFlow(t, detector, key, 11*time.Minute)
	detector.touchIngress(path.Reverse())
	detector.evictIdle(time.Now())
	if got := detector.Testing_FlowCount(); got != 1 {
		t.Fatalf("an ingress-refreshed flow was reclaimed: %d flows", got)
	}

	// a return packet whose source port is privileged cannot refresh anything:
	// the matching egress destination below 1024 is never tracked
	ageFlow(t, detector, key, 11*time.Minute)
	privileged := path.Reverse()
	privileged.SourcePort = 443
	detector.touchIngress(privileged)
	detector.evictIdle(time.Now())
	if got := detector.Testing_FlowCount(); got != 0 {
		t.Fatalf("a privileged-port return packet refreshed a flow: %d flows", got)
	}

	// a return packet for an untracked flow creates no state
	detector.touchIngress(dmcaIdleTestPath(6999).Reverse())
	if got := detector.Testing_FlowCount(); got != 0 {
		t.Fatalf("ingress touch created state: %d flows", got)
	}
}

func TestDmcaFlowTtlZeroDisablesTheReaper(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	detector := newIdleDmcaDetector(ctx, 0)
	select {
	case <-detector.runDone:
	default:
		t.Fatal("FlowTtl=0 must not start the scan goroutine")
	}
	path := dmcaIdleTestPath(6881)
	detector.classify(path, nil)
	ageFlow(t, detector, path.ToIp6Path(), 24*time.Hour)
	detector.evictIdle(time.Now())
	if got := detector.Testing_FlowCount(); got != 1 {
		t.Fatalf("FlowTtl=0 must not evict: %d flows", got)
	}
}

func TestDmcaNoContextMeansNoScan(t *testing.T) {
	detector := newIdleDmcaDetector(nil, time.Minute)
	select {
	case <-detector.runDone:
	default:
		t.Fatal("a detector built without a context must not run the scan")
	}
}

func TestDmcaIdleScanStopsWhenItsContextIsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	detector := newIdleDmcaDetector(ctx, time.Minute)
	select {
	case <-detector.runDone:
		t.Fatal("the scan goroutine exited before the context was canceled")
	default:
	}
	cancel()
	select {
	case <-detector.runDone:
	case <-time.After(10 * time.Second):
		t.Fatal("the scan goroutine did not stop after its context was canceled")
	}
}

// The packet path and the scan run concurrently in production: sends create and
// refresh flows, receives refresh them, and the scan deletes idle ones.
func TestDmcaRefreshAndReapAreRaceFree(t *testing.T) {
	detector := newIdleDmcaDetector(nil, 30*time.Second)
	// tracked but already idle: a reaper deletes these while the packet
	// goroutines re-touch them
	for i := 0; i < 64; i++ {
		path := dmcaIdleTestPath(20000 + i)
		detector.classify(path, nil)
		ageFlow(t, detector, path.ToIp6Path(), time.Minute)
	}
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		g := g
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				path := dmcaIdleTestPath(20000 + (g*64+i)%64)
				detector.classify(path, nil)
				detector.touchIngress(path.Reverse())
			}
		}()
	}
	for g := 0; g < 2; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				detector.evictIdle(time.Now())
			}
		}()
	}
	wg.Wait()
}

// --- packet-path benchmarks (run before and after the idle-flow port) ---

// the steady state for an established flow: one atomic load and a switch
func BenchmarkDmcaClassifyDecidedFlow(b *testing.B) {
	detector := newDmcaDetector(nil, DefaultDmcaSecurityPolicySettings(), newWebStandardDetector(DefaultWebStandardSettings()))
	path := dmcaIdleTestPath(6881)
	payload := encryptedPayload(512)
	for i := 0; i < 8; i++ {
		detector.classify(path, payload)
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		detector.classify(path, payload)
	}
}

// the state-creating path: a fresh flow per iteration, including the hash, the
// shard lock and the capacity-eviction bookkeeping
func BenchmarkDmcaClassifyNewFlow(b *testing.B) {
	detector := newDmcaDetector(nil, DefaultDmcaSecurityPolicySettings(), newWebStandardDetector(DefaultWebStandardSettings()))
	payload := encryptedPayload(512)
	paths := make([]*IpPath, 4096)
	for i := range paths {
		paths[i] = dmcaIdleTestPath(1024 + i)
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		detector.classify(paths[i%len(paths)], payload)
	}
}

// the new ingress refresh: shard read lock plus one lookup for a tracked flow
func BenchmarkDmcaTouchIngress(b *testing.B) {
	detector := newDmcaDetector(nil, DefaultDmcaSecurityPolicySettings(), newWebStandardDetector(DefaultWebStandardSettings()))
	path := dmcaIdleTestPath(6881)
	detector.classify(path, nil)
	ingress := path.Reverse()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		detector.touchIngress(ingress)
	}
}
