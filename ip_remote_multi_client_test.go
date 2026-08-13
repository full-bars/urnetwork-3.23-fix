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
			for endTime := time.Now().Add(timeout); time.Now().Before(endTime); {
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
