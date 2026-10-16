package connect

import (
	"context"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

type unadmittedReturnFixture struct {
	provider *RemoteUserNatProvider
	sequence *SendSequence
	sourceId Id
	flows    *atomic.Int64
	released *atomic.Int64
	admit    *atomic.Bool
}

func newUnadmittedReturnFixture(t *testing.T, abandon time.Duration) *unadmittedReturnFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	settings := DefaultClientSettings()
	client := NewClient(ctx, NewId(), NewNoContractClientOob(), settings)
	localUserNat := NewLocalUserNatWithDefaults(ctx, "unadmitted-return-test", nil)
	providerSettings := DefaultRemoteUserNatProviderSettings()
	providerSettings.WriteTimeout = time.Millisecond
	providerSettings.ReturnSendAbandonTimeout = abandon
	providerSettings.EgressSecurityPolicyGenerator = func(*SecurityPolicyStatsCollector) SecurityPolicy {
		return DisableSecurityPolicy()
	}
	provider := NewRemoteUserNatProvider(client, localUserNat, nil, providerSettings)
	t.Cleanup(func() {
		provider.Close()
		localUserNat.Close()
		client.Cancel()
		cancel()
	})

	f := &unadmittedReturnFixture{
		provider: provider,
		sourceId: NewId(),
		flows:    &atomic.Int64{},
		released: &atomic.Int64{},
		admit:    &atomic.Bool{},
	}
	provider.backendDegradedTest = func() bool { return false }
	provider.returnSendForTest = f.admit.Load
	provider.releasedForTest = func(Id) { f.released.Add(1) }
	localUserNat.sourceCancelMutex.Lock()
	localUserNat.sourceCancels = append(localUserNat.sourceCancels, func(sourceId Id) int {
		if sourceId == f.sourceId {
			f.flows.Add(1)
			return 1
		}
		return 0
	})
	localUserNat.sourceCancelMutex.Unlock()

	sendBuffer := client.sendBuffer
	f.sequence = NewSendSequence(ctx, client, sendBuffer, DestinationId(f.sourceId), MultiHopId{}, true, false, sequenceTlsRoleServer, false, settings.SendBufferSettings)
	sendBuffer.mutex.Lock()
	sendBuffer.sendSequences[sendSequenceId{Destination: DestinationId(f.sourceId), CompanionContract: true}] = f.sequence
	sendBuffer.mutex.Unlock()
	return f
}

// one socket-owned TCP return toward the source through the real Receive path
func (f *unadmittedReturnFixture) tcpReturn(t *testing.T, sourceId Id) {
	t.Helper()
	packet := MessagePoolCopy(craftSecurityPacket(IpProtocolTcp, []byte{203, 0, 113, 7}, 8080, []byte{10, 0, 0, 9}, 42001, false, []byte("data")))
	defer MessagePoolReturn(packet)
	ipPath, err := ParseIpPath(packet)
	if err != nil {
		t.Fatal(err)
	}
	f.provider.Receive(
		TransferPath{SourceId: sourceId, StreamId: NewId()},
		protocol.ProvideMode_Public,
		ipPath,
		packet,
	)
}

func TestProviderReleasesSourceWhoseTcpReturnStaysUnadmitted(t *testing.T) {
	f := newUnadmittedReturnFixture(t, 40*time.Millisecond)
	// let the client's startup workers settle so the baseline is steady
	time.Sleep(500 * time.Millisecond)
	baseline := runtime.NumGoroutine()
	// a different source's unadmitted return must not release this one
	f.tcpReturn(t, NewId())

	deadline := time.Now().Add(5 * time.Second)
	for f.released.Load() == 0 && time.Now().Before(deadline) {
		f.tcpReturn(t, f.sourceId)
		time.Sleep(5 * time.Millisecond)
	}
	if f.released.Load() != 1 {
		t.Fatalf("released %d times, want 1", f.released.Load())
	}
	if f.flows.Load() != 1 {
		t.Fatalf("cancelled %d NAT flow sets, want 1", f.flows.Load())
	}
	if f.sequence.ctx.Err() == nil {
		t.Fatal("the released source's send sequence is still live")
	}
	// the record is cleared, so the next failure starts a new clock
	f.tcpReturn(t, f.sourceId)
	if f.released.Load() != 1 {
		t.Fatal("source released again immediately")
	}
	// the release adds no goroutines and leaves none behind
	settled := false
	for deadline = time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if runtime.NumGoroutine() <= baseline {
			settled = true
			break
		}
	}
	if !settled {
		t.Fatalf("goroutines %d did not return to baseline %d", runtime.NumGoroutine(), baseline)
	}
}

func TestProviderNeverReleasesSourceWhoseReturnsAreAdmitted(t *testing.T) {
	f := newUnadmittedReturnFixture(t, 40*time.Millisecond)
	// slow: one return admitted every 30 ms, between unadmitted attempts, for
	// several times the abandon timeout
	end := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(end) {
		f.admit.Store(false)
		f.tcpReturn(t, f.sourceId)
		time.Sleep(15 * time.Millisecond)
		f.admit.Store(true)
		f.tcpReturn(t, f.sourceId)
		time.Sleep(15 * time.Millisecond)
	}
	if f.released.Load() != 0 || f.flows.Load() != 0 {
		t.Fatalf("a source with admitted returns was released (%d, %d)", f.released.Load(), f.flows.Load())
	}
	if f.sequence.ctx.Err() != nil {
		t.Fatal("a live source's send sequence was cancelled")
	}
}

func TestProviderDoesNotReleaseUnadmittedSourceWhileBackendDegraded(t *testing.T) {
	f := newUnadmittedReturnFixture(t, 20*time.Millisecond)
	var degraded atomic.Bool
	degraded.Store(true)
	f.provider.backendDegradedTest = degraded.Load
	end := time.Now().Add(120 * time.Millisecond)
	for time.Now().Before(end) {
		f.tcpReturn(t, f.sourceId)
		time.Sleep(5 * time.Millisecond)
	}
	if f.released.Load() != 0 {
		t.Fatal("released while the backend was degraded")
	}
	degraded.Store(false)
	f.tcpReturn(t, f.sourceId)
	if f.released.Load() != 1 {
		t.Fatal("not released on the first failure after the backend recovered")
	}
}

func TestProviderUnadmittedReleaseDisabledByZeroTimeout(t *testing.T) {
	f := newUnadmittedReturnFixture(t, 0)
	for i := 0; i < 20; i++ {
		f.tcpReturn(t, f.sourceId)
		time.Sleep(3 * time.Millisecond)
	}
	if f.released.Load() != 0 || f.sequence.ctx.Err() != nil {
		t.Fatal("released with the abandon timeout disabled")
	}
	if len(f.provider.unadmitted) != 0 {
		t.Fatal("tracked a source with the release disabled")
	}
}

func TestProviderDefaultReturnSendAbandonTimeout(t *testing.T) {
	if got := DefaultRemoteUserNatProviderSettings().ReturnSendAbandonTimeout; got != 120*time.Second {
		t.Fatalf("default %s, want 120s", got)
	}
}
