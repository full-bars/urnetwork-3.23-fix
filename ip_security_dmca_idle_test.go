package connect

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
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

// The provider's return path hands over the path of the flow (client to server),
// not of the return packet, so it must refresh the key as given. Reversing it, as
// the packet-oriented touch does, would match nothing.
func TestDmcaTouchFlowRefreshesInTheFlowsOwnOrientation(t *testing.T) {
	detector := newIdleDmcaDetector(nil, 10*time.Minute)
	path := dmcaIdleTestPath(6881)
	detector.classify(path, nil)
	key := path.ToIp6Path()

	ageFlow(t, detector, key, 11*time.Minute)
	detector.touchFlow(path)
	detector.evictIdle(time.Now())
	if got := detector.Testing_FlowCount(); got != 1 {
		t.Fatalf("a flow refreshed in its own orientation was reclaimed: %d flows", got)
	}

	// the wrong orientation refreshes nothing, so the flow ages out
	ageFlow(t, detector, key, 11*time.Minute)
	detector.touchFlow(path.Reverse())
	detector.evictIdle(time.Now())
	if got := detector.Testing_FlowCount(); got != 0 {
		t.Fatalf("a reversed path refreshed a flow: %d flows", got)
	}

	// no state is created, and a privileged destination is never tracked
	detector.touchFlow(dmcaIdleTestPath(6999))
	privileged := dmcaIdleTestPath(443)
	detector.touchFlow(privileged)
	if got := detector.Testing_FlowCount(); got != 0 {
		t.Fatalf("touch created state: %d flows", got)
	}
}

func TestProviderReturnPathKeepsALiveFlowFromBeingReclaimed(t *testing.T) {
	detector := newIdleDmcaDetector(nil, 10*time.Minute)
	provider := &RemoteUserNatProvider{securityPolicy: &egressSecurityPolicy{dmca: detector}}
	path := dmcaIdleTestPath(6881)
	detector.classify(path, nil)
	key := path.ToIp6Path()

	// a flow that is receiving but whose client has been silent
	ageFlow(t, detector, key, 11*time.Minute)
	provider.refreshEgressFlow(protocol.ProvideMode_Public, path)
	detector.evictIdle(time.Now())
	if got := detector.Testing_FlowCount(); got != 1 {
		t.Fatalf("the return path did not keep the flow alive: %d flows", got)
	}

	// network mode never reaches the detector on the egress side, so it must not refresh
	ageFlow(t, detector, key, 11*time.Minute)
	provider.refreshEgressFlow(protocol.ProvideMode_Network, path)
	detector.evictIdle(time.Now())
	if got := detector.Testing_FlowCount(); got != 0 {
		t.Fatalf("a network mode packet refreshed a flow: %d flows", got)
	}

	// a policy that cannot be refreshed, and a nil path, are ignored
	(&RemoteUserNatProvider{securityPolicy: DefaultIngressSecurityPolicy()}).refreshEgressFlow(protocol.ProvideMode_Public, path)
	provider.refreshEgressFlow(protocol.ProvideMode_Public, nil)
}

// touchSpyPolicy is a security policy that records which flows the provider's
// return path reported.
type touchSpyPolicy struct {
	smtpSeamPolicy
	touched atomic.Int64
}

func (self *touchSpyPolicy) touchFlow(*IpPath) { self.touched.Add(1) }

// The provider's Receive is the return path of every relayed packet; it must
// report the flow so the idle scan does not reclaim a live one.
func TestProviderReceiveReportsTheFlowToTheEgressPolicy(t *testing.T) {
	spy := &touchSpyPolicy{smtpSeamPolicy: smtpSeamPolicy{stats: DefaultSecurityPolicyStatsCollector()}}
	ctx, cancel := context.WithCancel(context.Background())
	clientSettings := DefaultClientSettings()
	clientSettings.SendBufferSettings.SequenceBufferSize = 0
	clientSettings.SendBufferSettings.AckBufferSize = 0
	clientSettings.ReceiveBufferSettings.SequenceBufferSize = 0
	clientSettings.ForwardBufferSettings.SequenceBufferSize = 0
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), clientSettings)
	localUserNat := NewLocalUserNatWithDefaults(ctx, "touch-spy-provider-test", nil)
	settings := DefaultRemoteUserNatProviderSettings()
	settings.WriteTimeout = 10 * time.Millisecond
	settings.EgressSecurityPolicyGenerator = func(context.Context, *SecurityPolicyStatsCollector) SecurityPolicy { return spy }
	provider := NewRemoteUserNatProvider(client, localUserNat, nil, settings)
	t.Cleanup(func() {
		provider.Close()
		localUserNat.Close()
		client.Cancel()
		cancel()
	})

	source := TransferPath{SourceId: NewId()} // a source mask, as the local user nat reports it
	path := dmcaIdleTestPath(6881)
	packet := []byte{0x45, 0, 0, 28, 0, 0, 0, 0, 64, 17, 0, 0, 198, 51, 100, 7, 10, 0, 0, 5, 0x1a, 0xe1, 0x9c, 0x40, 0, 8, 0, 0}
	provider.Receive(source, protocol.ProvideMode_Public, path, packet)
	if got := spy.touched.Load(); got != 1 {
		t.Fatalf("a public mode return packet reported %d flows, want 1", got)
	}
	provider.Receive(source, protocol.ProvideMode_Network, path, packet)
	if got := spy.touched.Load(); got != 1 {
		t.Fatalf("a network mode return packet reported a flow: %d total", got)
	}
}
