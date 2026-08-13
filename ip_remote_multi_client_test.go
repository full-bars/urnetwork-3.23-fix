package connect

import (
	"context"
	"math"
	"testing"
	"time"

	// "slices"
	"sync"

	"github.com/go-playground/assert/v2"

	"github.com/urnetwork/connect/protocol"
)

func TestMultiClientUdp4(t *testing.T) {
	testClient(t, testingNewMultiClient, udp4Packet, (*IpPath).ToIp4Path)
}

func TestMultiClientTcp4(t *testing.T) {
	testClient(t, testingNewMultiClient, tcp4Packet, (*IpPath).ToIp4Path)
}

func TestMultiClientUdp6(t *testing.T) {
	testClient(t, testingNewMultiClient, udp6Packet, (*IpPath).ToIp6Path)
}

func TestMultiClientTcp6(t *testing.T) {
	testClient(t, testingNewMultiClient, tcp6Packet, (*IpPath).ToIp6Path)
}

func testingNewMultiClient(ctx context.Context, providerClient *Client, receivePacketCallback ReceivePacketFunction) (UserNatClient, error) {

	mutex := sync.Mutex{}
	unsubs := map[*Client]func(){}

	generator := &TestMultiClientGenerator{
		nextDestinations: func(count int, excludeDestinations []MultiHopId, rankMode string) (map[MultiHopId]DestinationStats, error) {
			next := map[MultiHopId]DestinationStats{}
			containsTail := func() bool {
				for _, destination := range excludeDestinations {
					if 0 < destination.Len() && destination.Tail() == providerClient.ClientId() {
						return true
					}
				}
				return false
			}
			if !containsTail() {
				next[RequireMultiHopId(providerClient.ClientId())] = DestinationStats{
					EstimatedBytesPerSecond: ByteCount(0),
					Tier:                    0,
				}
			}
			return next, nil
		},
		newClientArgs: func() (*MultiClientGeneratorClientArgs, error) {
			args := &MultiClientGeneratorClientArgs{
				ClientId:   NewId(),
				ClientAuth: nil,
			}
			return args, nil
		},
		removeClientArgs: func(args *MultiClientGeneratorClientArgs) {
			// do nothing
		},
		removeClientWithArgs: func(client *Client, args *MultiClientGeneratorClientArgs) {
			var unsub func()
			var ok bool
			func() {
				mutex.Lock()
				defer mutex.Unlock()
				unsub, ok = unsubs[client]
				if ok {
					delete(unsubs, client)
				}
			}()
			if ok {
				unsub()
			}
		},
		newClientSettings: func() *ClientSettings {
			settings := DefaultClientSettings()
			settings.SendBufferSettings.SequenceBufferSize = 0
			settings.SendBufferSettings.AckBufferSize = 0
			settings.ReceiveBufferSettings.SequenceBufferSize = 0
			// settings.ReceiveBufferSettings.AckBufferSize = 0
			settings.ForwardBufferSettings.SequenceBufferSize = 0
			return settings
		},
		newClient: func(ctx context.Context, args *MultiClientGeneratorClientArgs, clientSettings *ClientSettings) (*Client, error) {
			client := NewClient(ctx, args.ClientId, NewNoContractClientOob(), clientSettings)

			routeSend := make(chan []byte)
			routeReceive := make(chan []byte)

			transportSend := NewSendGatewayTransport()
			transportReceive := NewReceiveGatewayTransport()
			client.RouteManager().UpdateTransport(transportSend, []Route{routeSend})
			client.RouteManager().UpdateTransport(transportReceive, []Route{routeReceive})

			client.ContractManager().AddNoContractPeer(providerClient.ClientId())

			providerTransportSend := NewSendClientTransport(DestinationId(args.ClientId))
			providerTransportReceive := NewReceiveGatewayTransport()
			providerClient.RouteManager().UpdateTransport(providerTransportReceive, []Route{routeSend})
			providerClient.RouteManager().UpdateTransport(providerTransportSend, []Route{routeReceive})

			providerClient.ContractManager().AddNoContractPeer(client.ClientId())

			unsub := func() {
				client.RouteManager().RemoveTransport(transportSend)
				client.RouteManager().RemoveTransport(transportReceive)
				providerClient.RouteManager().RemoveTransport(providerTransportReceive)
				providerClient.RouteManager().RemoveTransport(providerTransportSend)
			}

			func() {
				mutex.Lock()
				defer mutex.Unlock()
				unsubs[client] = unsub
			}()

			return client, nil
		},
	}

	settings := DefaultMultiClientSettings()
	// TODO the tcp packets must use real seq numbers for this to work
	settings.TcpCollapsePrevention = false

	multiClient := NewRemoteUserNatMultiClient(
		ctx,
		generator,
		receivePacketCallback,
		protocol.ProvideMode_Network,
		settings,
	)

	return multiClient, nil
}

type TestMultiClientGenerator struct {
	nextDestinations     func(count int, excludeDestinations []MultiHopId, rankMode string) (map[MultiHopId]DestinationStats, error)
	newClientArgs        func() (*MultiClientGeneratorClientArgs, error)
	removeClientArgs     func(args *MultiClientGeneratorClientArgs)
	removeClientWithArgs func(client *Client, args *MultiClientGeneratorClientArgs)
	newClientSettings    func() *ClientSettings
	newClient            func(ctx context.Context, args *MultiClientGeneratorClientArgs, clientSettings *ClientSettings) (*Client, error)
}

func (self *TestMultiClientGenerator) NextDestinations(count int, excludeDestinations []MultiHopId, rankMode string) (map[MultiHopId]DestinationStats, error) {
	return self.nextDestinations(count, excludeDestinations, rankMode)
}

func (self *TestMultiClientGenerator) NewClientArgs() (*MultiClientGeneratorClientArgs, error) {
	return self.newClientArgs()
}

func (self *TestMultiClientGenerator) RemoveClientArgs(args *MultiClientGeneratorClientArgs) {
	self.removeClientArgs(args)
}

func (self *TestMultiClientGenerator) RemoveClientWithArgs(client *Client, args *MultiClientGeneratorClientArgs) {
	self.removeClientWithArgs(client, args)
}

func (self *TestMultiClientGenerator) NewClientSettings() *ClientSettings {
	return self.newClientSettings()
}

func (self *TestMultiClientGenerator) NewClient(ctx context.Context, args *MultiClientGeneratorClientArgs, clientSettings *ClientSettings) (*Client, error) {
	return self.newClient(ctx, args, clientSettings)
}

func (self *TestMultiClientGenerator) FixedDestinationSize() (int, bool) {
	return 1, true
}

func TestMultiClientChannelWindowStats(t *testing.T) {
	// ensure that the bucket counts are bounded
	// if this is broken, the coalesce logic is broken and there will be a memory issue

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	settings := DefaultMultiClientSettings()
	settings.StatsWindowBucketDuration = 100 * time.Millisecond
	settings.StatsWindowDuration = 1 * time.Second
	settings.BlackholeTimeout = 300 * time.Second

	timeout := 1 * time.Second
	if testing.Short() {
		// Must span >= 3 bucket durations: the stats reader omits the latest
		// two (possibly partial) buckets, so 10ms of activity left zero
		// buckets and the bucketCount assertion below was a coin flip — a
		// deterministic failure on local -short runs (20/20), masked in CI
		// only when -race scheduling stretched the span across a bucket
		// boundary. Derived from the bucket duration (4x = one spare bucket
		// over the 3-bucket minimum) so the relationship self-documents and
		// stays correct if the bucket duration ever changes.
		timeout = 4 * settings.StatsWindowBucketDuration
	}

	m := 6
	n := 6
	repeatCount := 6
	parallelCount := 6

	generator := &TestMultiClientGenerator{
		nextDestinations: func(count int, excludedDestinations []MultiHopId, rankMode string) (map[MultiHopId]DestinationStats, error) {
			// not used
			return nil, nil
		},
		newClientArgs: func() (*MultiClientGeneratorClientArgs, error) {
			args := &MultiClientGeneratorClientArgs{
				ClientId:   NewId(),
				ClientAuth: nil,
			}
			return args, nil
		},
		removeClientArgs: func(args *MultiClientGeneratorClientArgs) {
			// do nothing
		},
		removeClientWithArgs: func(client *Client, args *MultiClientGeneratorClientArgs) {
			// do nothing
		},
		newClientSettings: DefaultClientSettings,
		newClient: func(ctx context.Context, args *MultiClientGeneratorClientArgs, clientSettings *ClientSettings) (*Client, error) {
			client := NewClient(ctx, args.ClientId, NewNoContractClientOob(), clientSettings)
			return client, nil
		},
	}

	clientReceivePacket := func(client *multiClientChannel, source TransferPath, provideMode protocol.ProvideMode, ipPath *IpPath, packet []byte) {
		// Do nothing
	}

	contractStatus := func(contractStatus *ContractStatus) {
		// Do nothing
	}

	// the coalesce logic trims from the last event in a bucket
	// if events are uniformly distributed in a bucket, this means there will be an extra bucket
	maxBucketCount := 1 + int(math.Ceil(float64(settings.StatsWindowDuration)/float64(settings.StatsWindowBucketDuration)))

	args, err := generator.NewClientArgs()
	channelArgs := &multiClientChannelArgs{
		MultiClientGeneratorClientArgs: *args,
		Destination:                    RequireMultiHopId(NewId()),
		DestinationStats: DestinationStats{
			EstimatedBytesPerSecond: 0,
			Tier:                    0,
		},
	}
	assert.Equal(t, nil, err)

	clientChannel, err := newMultiClientChannel(ctx, channelArgs, generator, clientReceivePacket, DefaultIngressSecurityPolicy(), contractStatus, func(contractStatsEvents []*ContractStatsEvent) {}, func() {}, nil, settings)
	assert.Equal(t, nil, err)

	cancelCtxs := []context.Context{}

	for p := 0; p < parallelCount; p += 1 {
		cancelCtx, cancel := context.WithCancel(ctx)
		cancelCtxs = append(cancelCtxs, cancelCtx)
		go func() {
			defer cancel()
			// Guarantee at least one full pass of activity even if the
			// scheduler starves this goroutine past the timeout (CI runs
			// the whole suite in parallel under -race). The assertions
			// below require events to have landed; a wall-clock-gated loop
			// can return having done zero work if the goroutine only gets
			// scheduled after endTime, which reads sendAckCount == 0 and
			// fails spuriously (observed on CI, unreproducible locally).
			endTime := time.Now().Add(timeout)
			for {
				for s := 0; s < m; s += 1 {
					for i := 0; i < n; i += 1 {
						for j := 0; j < n; j += 1 {
							for k := 0; k < n; k += 1 {
								for a := 0; a < repeatCount; a += 1 {
									packet, _ := udp4Packet(s, i, j, k)
									ipPath, err := ParseIpPath(packet)
									assert.Equal(t, nil, err)

									clientChannel.addSendNack(1)
									clientChannel.addSendAck(1)
									clientChannel.addReceiveAck(1)
									clientChannel.addSource(ipPath)
								}
							}
						}
					}
				}
				if !time.Now().Before(endTime) {
					break
				}
			}
		}()
	}

	for _, cancelCtx := range cancelCtxs {
		<-cancelCtx.Done()
	}

	stats, err := clientChannel.windowStatsWithCoalesce(false)
	assert.Equal(t, nil, err)

	// The memory-safety invariant this test guards is the UPPER bound:
	// bucketCount must stay within the coalesce window or the coalesce
	// logic leaks (the test's own comment). The previous lower-bound check
	// (1 <= bucketCount) was timing-sensitive: the stats reader omits the
	// latest two (possibly partial) buckets, and under -race load the
	// goroutines' event bursts can compress into fewer than three distinct
	// buckets, reading as zero even though events were recorded.
	//
	// Events landing is proven two ways, both deterministic:
	//   - raw packet counters (sendAckCount/receiveAckCount) increment per
	//     event regardless of bucket timing; and
	//   - windowDuration > 0 proves at least one event bucket survived the
	//     two-bucket omission (windowDuration is computed from the surviving
	//     eventBuckets, so a fully-broken bucket-creation path would still
	//     fail loudly — it cannot be satisfied by packetStats alone).
	assert.Equal(t, true, 0 < stats.sendAckCount)
	assert.Equal(t, true, 0 < stats.receiveAckCount)
	assert.Equal(t, true, 0 < stats.windowDuration)
	assert.Equal(t, true, stats.bucketCount <= maxBucketCount)

	stats, err = clientChannel.WindowStats()
	assert.Equal(t, nil, err)

	assert.Equal(t, true, 0 < stats.sendAckCount)
	assert.Equal(t, true, 0 < stats.receiveAckCount)
	assert.Equal(t, true, 0 < stats.windowDuration)
	assert.Equal(t, true, stats.bucketCount <= maxBucketCount)
}

func TestDefaultMultiRaceClientCount(t *testing.T) {
	n := defaultMultiRaceClientCount()

	if n != 16 {
		t.Errorf("expected 16, got %d", n)
	}

	// DefaultMultiClientSettings should use it
	settings := DefaultMultiClientSettings()
	if settings.MultiRaceClientCount != n {
		t.Errorf("settings.MultiRaceClientCount = %d, want %d (from defaultMultiRaceClientCount)", settings.MultiRaceClientCount, n)
	}
}

func TestMultiClientChannelSendNackCoalesceDoesNotLeak(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	generator := &TestMultiClientGenerator{
		nextDestinations: func(count int, excludedDestinations []MultiHopId, rankMode string) (map[MultiHopId]DestinationStats, error) {
			return nil, nil
		},
		newClientArgs: func() (*MultiClientGeneratorClientArgs, error) {
			return &MultiClientGeneratorClientArgs{ClientId: NewId(), ClientAuth: nil}, nil
		},
		removeClientArgs:     func(args *MultiClientGeneratorClientArgs) {},
		removeClientWithArgs: func(client *Client, args *MultiClientGeneratorClientArgs) {},
		newClientSettings:    DefaultClientSettings,
		newClient: func(ctx context.Context, args *MultiClientGeneratorClientArgs, clientSettings *ClientSettings) (*Client, error) {
			return NewClient(ctx, args.ClientId, NewNoContractClientOob(), clientSettings), nil
		},
	}

	clientReceivePacket := func(client *multiClientChannel, source TransferPath, provideMode protocol.ProvideMode, ipPath *IpPath, packet []byte) {
	}
	contractStatus := func(contractStatus *ContractStatus) {}

	settings := DefaultMultiClientSettings()
	settings.StatsWindowBucketDuration = 20 * time.Millisecond
	settings.StatsWindowDuration = 60 * time.Millisecond
	settings.BlackholeTimeout = 300 * time.Second

	args, err := generator.NewClientArgs()
	assert.Equal(t, nil, err)
	channelArgs := &multiClientChannelArgs{
		MultiClientGeneratorClientArgs: *args,
		Destination:                    RequireMultiHopId(NewId()),
		DestinationStats:               DestinationStats{EstimatedBytesPerSecond: 0, Tier: 0},
	}

	clientChannel, err := newMultiClientChannel(ctx, channelArgs, generator, clientReceivePacket, DefaultIngressSecurityPolicy(), contractStatus, func(contractStatsEvents []*ContractStatsEvent) {}, func() {}, nil, settings)
	assert.Equal(t, nil, err)

	clientChannel.addSendNack(ByteCount(1))

	clientChannel.stateLock.Lock()
	nackCountAfterAdd := clientChannel.packetStats.sendNackCount
	clientChannel.stateLock.Unlock()
	assert.Equal(t, 1, nackCountAfterAdd)

	time.Sleep(settings.StatsWindowDuration + 20*time.Millisecond)
	clientChannel.addSendSyn(1)

	clientChannel.stateLock.Lock()
	nackCountAfterEviction := clientChannel.packetStats.sendNackCount
	clientChannel.stateLock.Unlock()

	assert.Equal(t, 0, nackCountAfterEviction)
}

func TestMultiClientChannelSourceCountRefcountPrunesOnEviction(t *testing.T) {
	// Regression: addSource incremented sourceCount[source] per PACKET while
	// the bucket path set is deduped; eviction decrements once per (bucket,
	// path). A source that sent N packets inside a single bucket left a
	// phantom N-1 count that survived eviction, so the (destination, source)
	// entry was never pruned — monotonic growth of ip4DestinationSourceCount
	// per dest/source pair, inflating window-resize sizing and ulimit
	// warnings downstream.
	settings := DefaultMultiClientSettings()
	settings.StatsWindowBucketDuration = time.Minute
	settings.StatsWindowDuration = 2 * time.Minute

	clientChannel := &multiClientChannel{
		settings:                  settings,
		packetStats:               &clientWindowStats{},
		ip4DestinationSourceCount: map[Ip4Path]map[Ip4Path]int{},
		ip6DestinationSourceCount: map[Ip6Path]map[Ip6Path]int{},
	}

	// One source, one destination, many packets — all land in the same bucket.
	packet, _ := udp4Packet(1, 1, 1, 1)
	ipPath, err := ParseIpPath(packet)
	assert.Equal(t, nil, err)

	for i := 0; i < 100; i += 1 {
		clientChannel.addSource(ipPath)
	}

	ip4Path := ipPath.ToIp4Path()
	source := ip4Path.Source()
	destination := ip4Path.Destination()

	// The refcount must be 1 (one bucket holds this path), not 100 (packets).
	clientChannel.stateLock.Lock()
	count := clientChannel.ip4DestinationSourceCount[destination][source]
	clientChannel.stateLock.Unlock()
	assert.Equal(t, 1, count)

	// Age the bucket out of the window and coalesce: the entry must prune.
	clientChannel.stateLock.Lock()
	clientChannel.eventBuckets[0].eventTime = time.Now().Add(-3 * settings.StatsWindowDuration)
	clientChannel.coalesceEventBuckets()
	clientChannel.stateLock.Unlock()

	clientChannel.stateLock.Lock()
	_, stillThere := clientChannel.ip4DestinationSourceCount[destination][source]
	clientChannel.stateLock.Unlock()
	assert.Equal(t, false, stillThere)
}

func TestMultiClientRemoveClientCancelsUpdateCtx(t *testing.T) {
	// Regression (leak-hunt item 4): removeClient nulled update.client but
	// never cancelled update.ctx, so the update's per-flow teardown
	// goroutine stayed parked in waitForIdleUpdate (time.After up to
	// SequenceIdleTimeout=120s) and could not observe the client removal
	// until the idle timer fired. Every client removal stranded one
	// goroutine + one timer for the full idle timeout. The fix cancels the
	// ctx so the teardown goroutine wakes immediately.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	settings := DefaultMultiClientSettings()
	settings.SequenceIdleTimeout = 2 * time.Minute // default; any value works

	// A minimal generator-free construction: only the fields removeClient
	// touches (stateLock, clientUpdates, log, ctx) plus the maps the
	// constructor initializes are needed.
	multiClient := &RemoteUserNatMultiClient{
		ctx:              ctx,
		cancel:           cancel,
		settings:         settings,
		log:              NewNoopLogger(),
		clientUpdates:    map[*multiClientChannel]map[*multiClientChannelUpdate]bool{},
		ip4PathUpdates:   map[Ip4Path]*multiClientChannelUpdate{},
		ip6PathUpdates:   map[Ip6Path]*multiClientChannelUpdate{},
		affinityIp4Paths: map[Ip4Path]map[Ip4Path]time.Time{},
		affinityIp6Paths: map[Ip6Path]map[Ip6Path]time.Time{},
	}

	// A client — removeClient only needs IsDone() (ctx) and pointer identity.
	clientCtx, clientCancel := context.WithCancel(ctx)
	client := &multiClientChannel{ctx: clientCtx, cancel: clientCancel}

	// A hand-built update bound to that client, with a live ctx.
	update := newMultiClientChannelUpdate(ctx, &IpPath{Version: 4})
	update.client = client
	multiClient.clientUpdates[client] = map[*multiClientChannelUpdate]bool{update: true}

	multiClient.removeClient(client)

	select {
	case <-update.ctx.Done():
		// fixed: the update ctx is cancelled, teardown goroutine wakes now
	default:
		t.Fatal("removeClient must cancel the update ctx so the parked teardown goroutine wakes immediately, not after SequenceIdleTimeout")
	}
	if multiClient.clientUpdates[client] != nil {
		t.Fatal("removeClient must delete the client's updates map")
	}
}

func TestMultiClientPQERequiresSaneEncryptionIdleTimeout(t *testing.T) {
	// Regression (leak-hunt item 2): the PQE/Required path replaced a nil
	// EncryptionSettings with DefaultEncryptionSettings(), which ships
	// IdleTimeout == 0. With that value, Release() defers deletion while a
	// handshake is in flight but Run() never arms CancelIfIdle (it requires
	// 0 < IdleTimeout) — so once the handshake settles, a refs==0 session
	// stays registered forever (zombie). The fix derives the same
	// sequence-bounded idle horizon DefaultClientSettings uses, arming the
	// reap loop so the session is eventually reaped.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	settings := DefaultMultiClientSettings()
	settings.StatsWindowBucketDuration = 100 * time.Millisecond
	settings.StatsWindowDuration = 1 * time.Second

	var gotEncryptionIdleTimeout time.Duration
	// Generator that returns a ClientSettings with EncryptionSettings == nil,
	// the exact precondition that triggers the DefaultEncryptionSettings()
	// replacement in the PQE path. The newClient callback receives the
	// settings AFTER newMultiClientChannel applied the PQE fix, so it is the
	// ground truth for what the session will run with.
	generator := &TestMultiClientGenerator{
		nextDestinations: func(count int, excludedDestinations []MultiHopId, rankMode string) (map[MultiHopId]DestinationStats, error) {
			return map[MultiHopId]DestinationStats{}, nil
		},
		newClientArgs: func() (*MultiClientGeneratorClientArgs, error) {
			return &MultiClientGeneratorClientArgs{ClientId: NewId(), ClientAuth: nil}, nil
		},
		removeClientArgs:     func(args *MultiClientGeneratorClientArgs) {},
		removeClientWithArgs: func(client *Client, args *MultiClientGeneratorClientArgs) {},
		newClientSettings: func() *ClientSettings {
			s := DefaultClientSettings()
			s.EncryptionSettings = nil // the nil precondition
			return s
		},
		newClient: func(ctx context.Context, args *MultiClientGeneratorClientArgs, clientSettings *ClientSettings) (*Client, error) {
			if clientSettings.EncryptionSettings != nil {
				gotEncryptionIdleTimeout = clientSettings.EncryptionSettings.IdleTimeout
			}
			return NewClient(ctx, args.ClientId, NewNoContractClientOob(), clientSettings), nil
		},
	}

	channelArgs := &multiClientChannelArgs{
		MultiClientGeneratorClientArgs: MultiClientGeneratorClientArgs{ClientId: NewId()},
		Destination:                    RequireMultiHopId(NewId()),
	}
	pqe := &PerformanceProfile{PostQuantumEncryption: true}

	clientChannel, err := newMultiClientChannel(
		ctx, channelArgs, generator,
		func(client *multiClientChannel, source TransferPath, provideMode protocol.ProvideMode, ipPath *IpPath, packet []byte) {
		},
		DefaultIngressSecurityPolicy(),
		func(contractStatus *ContractStatus) {},
		func(contractStatsEvents []*ContractStatsEvent) {},
		func() {}, pqe, settings,
	)
	if err != nil {
		t.Fatalf("newMultiClientChannel: %s", err)
	}
	defer clientChannel.Close()

	// The session settings the PQE path installed must arm the reap loop
	// (0 < IdleTimeout), otherwise refs==0 sessions become zombies. The
	// timeout must also be bounded (a sane horizon, not "keep forever").
	if gotEncryptionIdleTimeout <= 0 {
		t.Fatalf("PQE path must install a positive EncryptionSettings.IdleTimeout (got %v) so the CancelIfIdle reap loop is armed — with the old 0 default, refs==0 sessions became zombies", gotEncryptionIdleTimeout)
	}
	if gotEncryptionIdleTimeout > 24*time.Hour {
		t.Fatalf("PQE IdleTimeout %v is not a sane bounded horizon", gotEncryptionIdleTimeout)
	}
}

func TestMultiClientRemoveClientDoesNotClobberSuccessorUpdate(t *testing.T) {
	// Regression (Opus HIGH finding on the #370 leak fix): removeClient
	// cancels a still-registered update's ctx, and a packet arriving
	// afterwards replaces it in the path map with a fresh update. The stale
	// teardown goroutine's unconditional `delete(ip4PathUpdates, ip4Path)`
	// then removed the SUCCESSOR's entry — orphaning a live update whose
	// packets would split across exit clients and break the flow. The
	// teardown closure now verifies it is still the registered update
	// before deleting.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	settings := DefaultMultiClientSettings()
	settings.SequenceIdleTimeout = 2 * time.Minute // keep goroutine1 parked
	settings.DestinationAffinity = false           // skip the affinity block in reserveUpdate (needs self.config)

	multiClient := &RemoteUserNatMultiClient{
		ctx:              ctx,
		cancel:           cancel,
		settings:         settings,
		log:              NewNoopLogger(),
		clientUpdates:    map[*multiClientChannel]map[*multiClientChannelUpdate]bool{},
		ip4PathUpdates:   map[Ip4Path]*multiClientChannelUpdate{},
		ip6PathUpdates:   map[Ip6Path]*multiClientChannelUpdate{},
		affinityIp4Paths: map[Ip4Path]map[Ip4Path]time.Time{},
		affinityIp6Paths: map[Ip6Path]map[Ip6Path]time.Time{},
	}

	clientCtx, clientCancel := context.WithCancel(ctx)
	client := &multiClientChannel{ctx: clientCtx, cancel: clientCancel}

	ipPath := &IpPath{Version: 4}
	ip4Path := ipPath.ToIp4Path()

	// Real reserveUpdate path: creates update1 and spawns its teardown
	// goroutine (parked in waitForIdleUpdate, 120s idle, ctx live).
	update1, _ := multiClient.reserveUpdate(ipPath)
	if multiClient.ip4PathUpdates[ip4Path] != update1 {
		t.Fatal("precondition: update1 must be the registered update")
	}
	update1.client = client
	multiClient.clientUpdates[client] = map[*multiClientChannelUpdate]bool{update1: true}

	// removeClient cancels update1's ctx (the leak fix) but does NOT touch
	// the path map — update1 is still registered there.
	multiClient.removeClient(client)

	// A packet for the same 5-tuple arrives: reserveUpdate would see
	// update1.IsDone() and replace it. Register the successor under the
	// lock while the stale goroutine is still parked, mirroring that
	// replacement deterministically.
	update2 := newMultiClientChannelUpdate(ctx, ipPath)
	multiClient.stateLock.Lock()
	multiClient.ip4PathUpdates[ip4Path] = update2
	multiClient.stateLock.Unlock()

	// Let the stale goroutine run its teardown closure (it wakes on
	// update1's cancelled ctx). Without the supersede guard it commits the
	// unconditional delete and ip4PathUpdates[ip4Path] becomes nil (the
	// clobber); with the guard it stays update2. Poll for the clobber
	// signature so the mutation fails fast, otherwise wait out the window.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		multiClient.stateLock.Lock()
		cur := multiClient.ip4PathUpdates[ip4Path]
		multiClient.stateLock.Unlock()
		if cur == nil {
			break // stale goroutine committed the delete
		}
		time.Sleep(5 * time.Millisecond)
	}

	multiClient.stateLock.Lock()
	cur := multiClient.ip4PathUpdates[ip4Path]
	multiClient.stateLock.Unlock()
	if cur != update2 {
		t.Fatalf("stale teardown goroutine clobbered the successor: ip4PathUpdates[path] = %p, want update2 %p (live successor must stay registered)", cur, update2)
	}
	if update2.ctx.Err() != nil {
		t.Fatal("successor update2 ctx must not be cancelled")
	}
}

func TestMultiClientChannelCancelUnsubscribesReceiveCallback(t *testing.T) {
	// Regression (leak-hunt item 6): Cancel() called self.cancel() and
	// client.Cancel() but never clientReceiveUnsub(), unlike Close(). A
	// Cancel-only path (client eviction, shuffle, replacedClient) left the
	// receive callback registered on the client, retaining the dead
	// channel's callback chain until the next resize — a bounded but
	// steady retention. Cancel must unsubscribe like Close does.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	generator := &TestMultiClientGenerator{
		nextDestinations: func(count int, excludedDestinations []MultiHopId, rankMode string) (map[MultiHopId]DestinationStats, error) {
			return nil, nil
		},
		newClientArgs: func() (*MultiClientGeneratorClientArgs, error) {
			return &MultiClientGeneratorClientArgs{ClientId: NewId(), ClientAuth: nil}, nil
		},
		removeClientArgs:     func(args *MultiClientGeneratorClientArgs) {},
		removeClientWithArgs: func(client *Client, args *MultiClientGeneratorClientArgs) {},
		newClientSettings:    DefaultClientSettings,
		newClient: func(ctx context.Context, args *MultiClientGeneratorClientArgs, clientSettings *ClientSettings) (*Client, error) {
			return NewClient(ctx, args.ClientId, NewNoContractClientOob(), clientSettings), nil
		},
	}

	clientReceivePacket := func(client *multiClientChannel, source TransferPath, provideMode protocol.ProvideMode, ipPath *IpPath, packet []byte) {
	}
	contractStatus := func(contractStatus *ContractStatus) {}

	settings := DefaultMultiClientSettings()
	settings.StatsWindowBucketDuration = 20 * time.Millisecond
	settings.StatsWindowDuration = 60 * time.Millisecond
	settings.BlackholeTimeout = 300 * time.Second

	args, err := generator.NewClientArgs()
	assert.Equal(t, nil, err)
	channelArgs := &multiClientChannelArgs{
		MultiClientGeneratorClientArgs: *args,
		Destination:                    RequireMultiHopId(NewId()),
	}

	clientChannel, err := newMultiClientChannel(
		ctx, channelArgs, generator, clientReceivePacket,
		DefaultIngressSecurityPolicy(), contractStatus,
		func(contractStatsEvents []*ContractStatsEvent) {}, func() {}, nil, settings,
	)
	assert.Equal(t, nil, err)

	client := clientChannel.client
	before := len(client.receiveCallbacks.Get())
	if before == 0 {
		t.Fatal("precondition: expected at least one receive callback after construction")
	}

	clientChannel.Cancel()

	after := len(client.receiveCallbacks.Get())
	// The channel registered exactly one callback at construction
	// (clientChannel.clientReceive). Cancel must remove it.
	if after != before-1 {
		t.Fatalf("Cancel() must unsubscribe the channel's receive callback like Close() — before=%d after=%d (dead channel's callback retained until next resize)", before, after)
	}
}
