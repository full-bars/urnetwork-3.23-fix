package connect

import (
	"context"
	"testing"
	"time"

	"github.com/go-playground/assert/v2"

	"github.com/urnetwork/connect/protocol"
)

// helper mirroring the minimal channel construction in
// TestMultiClientChannelWindowStats, but without any timing-dependent
// activity generation. This lets the bucket-omission math be tested
// deterministically instead of via wall-clock hammering.
func testingNewEmptyMultiClientChannel(t *testing.T, settings *MultiClientSettings) *multiClientChannel {
	ctx := context.Background()

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

	args, err := generator.NewClientArgs()
	assert.Equal(t, nil, err)

	channelArgs := &multiClientChannelArgs{
		MultiClientGeneratorClientArgs: *args,
		Destination:                    RequireMultiHopId(NewId()),
		DestinationStats:               DestinationStats{EstimatedBytesPerSecond: 0, Tier: 0},
	}

	clientChannel, err := newMultiClientChannel(
		ctx, channelArgs, generator,
		func(client *multiClientChannel, source TransferPath, provideMode protocol.ProvideMode, ipPath *IpPath, packet []byte) {
		},
		DefaultIngressSecurityPolicy(),
		func(contractStatus *ContractStatus) {},
		func(contractStatsEvents []*ContractStatsEvent) {},
		func() {},
		nil,
		settings,
	)
	assert.Equal(t, nil, err)

	return clientChannel
}

// windowStatsWithCoalesce omits the latest two event buckets because they
// may be partial (see coalesceEventBuckets / windowStatsWithCoalesce in
// ip_remote_multi_client.go). This test pins that behavior with synthetic
// buckets instead of relying on real elapsed time, so it can't flake the
// way TestMultiClientChannelWindowStats did under -short.
func TestWindowStatsCoalesceOmitsLatestTwoBucketsDeterministic(t *testing.T) {
	settings := DefaultMultiClientSettings()
	settings.StatsWindowBucketDuration = 100 * time.Millisecond
	settings.StatsWindowDuration = 10 * time.Second // large enough that no bucket ages out

	clientChannel := testingNewEmptyMultiClientChannel(t, settings)

	makeBuckets := func(n int) []*multiClientEventBucket {
		now := time.Now()
		buckets := make([]*multiClientEventBucket, n)
		for i := 0; i < n; i += 1 {
			b := newMultiClientEventBucket()
			// space buckets out so none of them collide with the window-start trim
			b.createTime = now.Add(time.Duration(i) * settings.StatsWindowBucketDuration)
			b.eventTime = b.createTime
			b.sendAckCount = 1
			buckets[i] = b
		}
		return buckets
	}

	cases := []struct {
		bucketsIn       int
		wantBucketCount int
	}{
		{0, 0},
		{1, 0}, // this is exactly the pre-fix flake: 1 bucket -> 0 after omission
		{2, 0},
		{3, 1},
		{5, 3},
	}

	for _, c := range cases {
		clientChannel.eventBuckets = makeBuckets(c.bucketsIn)
		stats, err := clientChannel.windowStatsWithCoalesce(false)
		assert.Equal(t, nil, err)
		assert.Equal(t, c.wantBucketCount, stats.bucketCount)
	}
}

// The bucketCount assertion in TestMultiClientChannelWindowStats is
// sensitive to wall-clock timing (see the -short fix). The packet counters
// (sendAckCount, etc) are not: they come from the channel-wide running
// packetStats total rather than from the windowed eventBuckets slice, so
// they are populated on the very first event and are not subject to the
// "omit the latest two buckets" trimming. Asserting on them is a
// timing-independent way to confirm activity was recorded.
func TestWindowStatsCountersRobustToPartialBuckets(t *testing.T) {
	settings := DefaultMultiClientSettings()
	settings.StatsWindowBucketDuration = 100 * time.Millisecond
	settings.StatsWindowDuration = 1 * time.Second

	clientChannel := testingNewEmptyMultiClientChannel(t, settings)

	// A single, immediate burst of activity: far too short to span even
	// one full bucket duration, so bucketCount will be 0 after the -2
	// omission (matching the {1,0} case above).
	// addSendAck cancels out one prior addSendNack (it represents the nack
	// being resolved), so two nacks are sent to leave a net count of 1.
	clientChannel.addSendNack(1)
	clientChannel.addSendNack(1)
	clientChannel.addSendAck(1)
	clientChannel.addReceiveAck(1)

	stats, err := clientChannel.windowStatsWithCoalesce(false)
	assert.Equal(t, nil, err)

	assert.Equal(t, 0, stats.bucketCount)
	assert.Equal(t, int64(1), int64(stats.sendAckCount))
	assert.Equal(t, int64(1), int64(stats.sendNackCount))
	assert.Equal(t, int64(1), int64(stats.receiveAckCount))
}
