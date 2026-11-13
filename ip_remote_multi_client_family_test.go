package connect

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/go-playground/assert/v2"

	"github.com/urnetwork/connect/protocol"
)

func TestWindowGeneratorCallReturnsResult(t *testing.T) {
	value, err := windowGeneratorCall(context.Background(), time.Second, func() (int, error) {
		return 7, nil
	}, nil)
	assert.Equal(t, err, nil)
	assert.Equal(t, value, 7)

	_, err = windowGeneratorCall(context.Background(), time.Second, func() (int, error) {
		return 0, errors.New("boom")
	}, nil)
	assert.Equal(t, err != nil, true)
}

func TestWindowGeneratorCallZeroTimeoutCallsDirect(t *testing.T) {
	value, err := windowGeneratorCall(context.Background(), 0, func() (int, error) {
		return 3, nil
	}, nil)
	assert.Equal(t, err, nil)
	assert.Equal(t, value, 3)
}

func TestWindowGeneratorCallTimeoutAbandonsAndDrainsLate(t *testing.T) {
	release := make(chan struct{})
	late := make(chan int, 1)
	_, err := windowGeneratorCall(context.Background(), 20*time.Millisecond, func() (int, error) {
		<-release
		return 9, nil
	}, func(value int, err error) {
		late <- value
	})
	assert.Equal(t, err != nil, true)
	assert.Equal(t, err.Error() != "generator call canceled", true)

	close(release)
	select {
	case v := <-late:
		assert.Equal(t, v, 9)
	case <-time.After(2 * time.Second):
		t.Fatal("late result not drained")
	}
}

func TestWindowGeneratorCallCancelIsDistinctFromTimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	release := make(chan struct{})
	defer close(release)
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, err := windowGeneratorCall(ctx, time.Minute, func() (int, error) {
		<-release
		return 0, nil
	}, nil)
	assert.Equal(t, err != nil, true)
	assert.Equal(t, err.Error(), "generator call canceled")
	assert.Equal(t, time.Since(start) < 5*time.Second, true)
}

func TestWindowGeneratorCallPanicBecomesError(t *testing.T) {
	_, err := windowGeneratorCall(context.Background(), time.Second, func() (int, error) {
		panic("generator exploded")
	}, nil)
	assert.Equal(t, err != nil, true)
}

func TestReplyIpv6NoRouteDeliversPacket(t *testing.T) {
	var got [][]byte
	var gotPath *IpPath
	multiClient := &RemoteUserNatMultiClient{
		receivePacketCallback: func(source TransferPath, provideMode protocol.ProvideMode, ipPath *IpPath, packet []byte) {
			got = append(got, packet)
			gotPath = ipPath
		},
	}

	assert.Equal(t, multiClient.replyIpv6NoRoute(nil), false)
	assert.Equal(t, multiClient.replyIpv6NoRoute(&IpPath{Version: 4}), false)

	ipPath := &IpPath{
		Version:         6,
		Protocol:        IpProtocolTcp,
		SourceIp:        net.ParseIP("fd00::2"),
		SourcePort:      40000,
		DestinationIp:   net.ParseIP("2001:db8::1"),
		DestinationPort: 443,
	}
	assert.Equal(t, multiClient.replyIpv6NoRoute(ipPath), true)
	assert.Equal(t, len(got), 1)
	assert.Equal(t, len(got[0]) > 0, true)
	assert.Equal(t, gotPath == ipPath, true)
	assert.Equal(t, multiClient.Ipv6NoRouteCount(), uint64(1))
}

func TestPruneConnectSuccessesCutsVersionsInStep(t *testing.T) {
	now := time.Now()
	channel := &multiClientChannel{
		connectSuccessTimes:    []time.Time{now.Add(-3 * time.Minute), now.Add(-2 * time.Minute), now},
		connectSuccessVersions: []int{4, 6, 4},
	}
	channel.pruneConnectSuccessesWithLock(now.Add(-time.Minute))
	assert.Equal(t, len(channel.connectSuccessTimes), 1)
	assert.Equal(t, channel.connectSuccessVersions, []int{4})
}

func TestPruneConnectSuccessesClampsShortVersions(t *testing.T) {
	now := time.Now()
	channel := &multiClientChannel{
		connectSuccessTimes: []time.Time{now.Add(-3 * time.Minute), now},
	}
	channel.pruneConnectSuccessesWithLock(now.Add(-time.Minute))
	assert.Equal(t, len(channel.connectSuccessTimes), 1)
	assert.Equal(t, len(channel.connectSuccessVersions), 0)
}

func TestRequestDualstackTargetCount(t *testing.T) {
	quality := WindowTypeQuality
	speed := WindowTypeSpeed
	size := WindowSizeSettings{WindowSizeMinIpv6Capable: 0}
	// auto: only the quality window needs one
	assert.Equal(t, requestDualstackTargetCount(WindowTypeQuality, nil, size), 1)
	assert.Equal(t, requestDualstackTargetCount(WindowTypeSpeed, nil, size), 0)
	// fixed profile: only its window needs one
	assert.Equal(t, requestDualstackTargetCount(WindowTypeSpeed, &speed, size), 1)
	assert.Equal(t, requestDualstackTargetCount(WindowTypeQuality, &speed, size), 0)
	// the setting wins when larger
	size.WindowSizeMinIpv6Capable = 3
	assert.Equal(t, requestDualstackTargetCount(WindowTypeSpeed, &quality, size), 3)
	// the limited window has no ipv6 need
	assert.Equal(t, requestDualstackTargetCount(WindowTypeLimited, nil, size), 0)
}

func TestRequestRankMode(t *testing.T) {
	window := &multiClientWindow{windowType: WindowTypeSpeed}
	assert.Equal(t, window.requestRankMode(), "speed")
	limited := &multiClientWindow{windowType: WindowTypeLimited}
	assert.Equal(t, limited.requestRankMode(), "quality")
	limited.setRequestSizing(5, 2)
	assert.Equal(t, limited.requestTargetCount, 5)
	assert.Equal(t, limited.requestDualstackTargetCount, 2)
}
