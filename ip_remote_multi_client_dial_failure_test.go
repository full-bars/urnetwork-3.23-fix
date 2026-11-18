package connect

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// the client-side dial-failure gates: the channel intercept accepts only the
// exact signal packet a provider builds, a pre-data flow is unbound so the
// application's retransmit re-races it, an established flow is never
// disturbed, and the per-channel strike record warns a starving provider out
// of new-flow selection. Deterministic: no network, no sleeps.

func dialFailureFlowPath(version int) *IpPath {
	ipPath := &IpPath{Version: version, Protocol: IpProtocolTcp, SourcePort: 42001, DestinationPort: 443, Syn: true, SequenceNumber: 1}
	if version == 4 {
		ipPath.SourceIp = net.ParseIP("10.0.0.9").To4()
		ipPath.DestinationIp = net.ParseIP("203.0.113.7").To4()
	} else {
		ipPath.SourceIp = net.ParseIP("fd00::9")
		ipPath.DestinationIp = net.ParseIP("2001:db8::7")
	}
	return ipPath
}

// newDialFailureTestMultiClient is the smallest parent the consumer needs:
// settings, the two path maps and the per-client update sets.
func newDialFailureTestMultiClient(rerace bool) *RemoteUserNatMultiClient {
	settings := DefaultMultiClientSettings()
	settings.DialFailureRerace = rerace
	return &RemoteUserNatMultiClient{
		settings:       settings,
		ip4PathUpdates: map[Ip4Path]*multiClientChannelUpdate{},
		ip6PathUpdates: map[Ip6Path]*multiClientChannelUpdate{},
		clientUpdates:  map[*multiClientChannel]map[*multiClientChannelUpdate]bool{},
	}
}

func registerDialFailureTestFlow(mc *RemoteUserNatMultiClient, channel *multiClientChannel, update *multiClientChannelUpdate, ipPath *IpPath) {
	switch ipPath.Version {
	case 4:
		mc.ip4PathUpdates[ipPath.ToIp4Path()] = update
	case 6:
		mc.ip6PathUpdates[ipPath.ToIp6Path()] = update
	}
	updates, ok := mc.clientUpdates[channel]
	if !ok {
		updates = map[*multiClientChannelUpdate]bool{}
		mc.clientUpdates[channel] = updates
	}
	updates[update] = true
}

func TestMultiClientDialFailureReracesPreDataFlow(t *testing.T) {
	for _, version := range []int{4, 6} {
		mc := newDialFailureTestMultiClient(true)
		channel := &multiClientChannel{}
		ipPath := dialFailureFlowPath(version)
		update := newMultiClientChannelUpdate(context.Background(), ipPath)
		update.client = channel
		registerDialFailureTestFlow(mc, channel, update, ipPath)

		mc.clientDialFailure(channel, ipPath)

		if update.client != nil {
			t.Fatalf("v%d: the flow was not unbound for a re-race", version)
		}
		if _, ok := mc.clientUpdates[channel]; ok {
			t.Fatalf("v%d: the unbound update stayed in the client's flow set", version)
		}
		if mc.dialFailures.Load() != 1 || mc.flowsReraced.Load() != 1 {
			t.Fatalf("v%d: counters are dialFailures=%d flowsReraced=%d", version, mc.dialFailures.Load(), mc.flowsReraced.Load())
		}
		channel.stateLock.Lock()
		strikes := len(channel.dialFailureTimes)
		channel.stateLock.Unlock()
		if strikes != 1 {
			t.Fatalf("v%d: the exit was not struck (%d)", version, strikes)
		}
	}
}

func TestMultiClientDialFailureNeverDisturbsAnEstablishedFlow(t *testing.T) {
	for _, version := range []int{4, 6} {
		mc := newDialFailureTestMultiClient(true)
		channel := &multiClientChannel{}
		ipPath := dialFailureFlowPath(version)
		update := newMultiClientChannelUpdate(context.Background(), ipPath)
		update.client = channel
		update.receivedInbound.Store(true)
		registerDialFailureTestFlow(mc, channel, update, ipPath)

		mc.clientDialFailure(channel, ipPath)

		if update.client != channel {
			t.Fatalf("v%d: an established flow was unbound", version)
		}
		if _, ok := mc.clientUpdates[channel]; !ok {
			t.Fatalf("v%d: the established flow was removed from its client set", version)
		}
		if mc.flowsReraced.Load() != 0 {
			t.Fatalf("v%d: a stale signal re-raced a working flow", version)
		}
		if mc.dialFailures.Load() != 1 {
			t.Fatalf("v%d: the intercepted signal was not counted", version)
		}
		channel.stateLock.Lock()
		strikes := len(channel.dialFailureTimes)
		channel.stateLock.Unlock()
		if strikes != 0 {
			t.Fatalf("v%d: a stale signal struck the exit", version)
		}
	}
}

func TestMultiClientDialFailureUnmatchedSignalIsCountedOnly(t *testing.T) {
	mc := newDialFailureTestMultiClient(true)
	channel := &multiClientChannel{}
	ipPath := dialFailureFlowPath(4)

	mc.clientDialFailure(channel, ipPath)

	if mc.dialFailures.Load() != 1 || mc.flowsReraced.Load() != 0 {
		t.Fatalf("counters are dialFailures=%d flowsReraced=%d", mc.dialFailures.Load(), mc.flowsReraced.Load())
	}
	channel.stateLock.Lock()
	strikes := len(channel.dialFailureTimes)
	channel.stateLock.Unlock()
	if strikes != 0 {
		t.Fatalf("an unmatched signal struck the exit")
	}
}

func TestMultiClientDialFailureReraceOffForwardsTheSignal(t *testing.T) {
	for _, version := range []int{4, 6} {
		mc := newDialFailureTestMultiClient(false)
		channel := &multiClientChannel{}
		ipPath := dialFailureFlowPath(version)
		update := newMultiClientChannelUpdate(context.Background(), ipPath)
		update.client = channel
		registerDialFailureTestFlow(mc, channel, update, ipPath)
		// the flow sent its syn and the send path captured its generation;
		// the provider's signal carries no sequence of its own
		update.setSynGeneration(4711)
		ipPath.SequenceNumber = 0

		var forwarded *IpPath
		var forwardedPacket []byte
		mc.receivePacketCallback = func(source TransferPath, provideMode protocol.ProvideMode, forwardIpPath *IpPath, packet []byte) {
			forwarded = forwardIpPath
			forwardedPacket = packet
		}

		mc.clientDialFailure(channel, ipPath)

		if update.client != channel {
			t.Fatalf("v%d: the knob-off path must not unbind", version)
		}
		if forwarded == nil || forwardedPacket == nil {
			t.Fatalf("v%d: the failure was not forwarded to the app", version)
		}
		recovered, ok := ipParseIcmpUnreachable(forwardedPacket)
		if !ok || recovered.ToIp4Path() != ipPath.ToIp4Path() && recovered.ToIp6Path() != ipPath.ToIp6Path() {
			t.Fatalf("v%d: the forwarded packet does not name the flow", version)
		}
		// the embed must carry the flow's syn generation, not the signal's
		// own (zero) sequence, or a kernel stack drops it as out of window
		if recovered.SequenceNumber != 4711 {
			t.Fatalf("v%d: the forwarded embed carries sequence %d, want the flow's syn generation 4711", version, recovered.SequenceNumber)
		}
		if mc.flowsReraced.Load() != 0 || mc.dialFailures.Load() != 1 {
			t.Fatalf("v%d: counters are dialFailures=%d flowsReraced=%d", version, mc.dialFailures.Load(), mc.flowsReraced.Load())
		}
		MessagePoolReturn(forwardedPacket)
	}
}

func TestMultiClientChannelDialStarvedWindow(t *testing.T) {
	channel := &multiClientChannel{}

	channel.addDialFailure("203.0.113.7")
	channel.addDialFailure("203.0.113.7")
	if channel.dialStarved() {
		t.Fatal("two failures must not count as starved")
	}
	// three failures spanning two distinct destinations with no successes
	channel.addDialFailure("203.0.113.8")
	if !channel.dialStarved() {
		t.Fatal("three failures across two destinations with no successes must count as starved")
	}
	// one proven connect in the window clears it
	channel.addConnectSuccess()
	if channel.dialStarved() {
		t.Fatal("a proven connect must clear starvation")
	}

	// and the window prunes: failures older than the window do not count
	channel.stateLock.Lock()
	channel.dialFailureTimes = []time.Time{time.Now().Add(-2 * dialStrikeWindow)}
	channel.stateLock.Unlock()
	if channel.dialStarved() {
		t.Fatal("stale failures must prune out of the window")
	}
	channel.stateLock.Lock()
	remaining := len(channel.dialFailureTimes)
	channel.stateLock.Unlock()
	if remaining != 0 {
		t.Fatalf("the stale failures were not pruned (%d left)", remaining)
	}
}

// one repeatedly failing destination must not starve-warn a channel: the
// strikes have to span distinct destinations before they are evidence about
// the exit rather than about a destination
func TestMultiClientChannelDialStarvedRequiresDistinctDestinations(t *testing.T) {
	channel := &multiClientChannel{}

	for i := 0; i < 4; i++ {
		channel.addDialFailure("203.0.113.7")
	}
	if channel.dialStarved() {
		t.Fatal("one destination's repeated failures must not starve-warn the exit")
	}

	// a second destination makes the strikes evidence about the exit
	channel.addDialFailure("198.51.100.9")
	if !channel.dialStarved() {
		t.Fatal("strikes spanning two destinations with no successes must count as starved")
	}

	// the parallel destinations stay entry-aligned under the window prune
	channel.stateLock.Lock()
	channel.dialFailureTimes[0] = time.Now().Add(-2 * dialStrikeWindow)
	channel.dialFailureTimes[1] = time.Now().Add(-2 * dialStrikeWindow)
	channel.dialFailureTimes[2] = time.Now().Add(-2 * dialStrikeWindow)
	channel.stateLock.Unlock()
	if channel.dialStarved() {
		t.Fatal("pruned strikes must not starve")
	}
	channel.stateLock.Lock()
	times := len(channel.dialFailureTimes)
	destinations := len(channel.dialFailureDestinations)
	channel.stateLock.Unlock()
	if times != destinations {
		t.Fatalf("times and destinations fell out of alignment: %d vs %d", times, destinations)
	}
}

// the intercept wiring: a signal packet fed through the channel's receive
// path reaches the parent callback, and ordinary garbage still drops.
func TestMultiClientChannelDialFailureIntercept(t *testing.T) {
	ipPath := dialFailureFlowPath(4)
	signal, ok := ipOosUnreachable(ipPath)
	if !ok {
		t.Fatal("the signal builder failed")
	}
	defer MessagePoolReturn(signal)

	var intercepted *IpPath
	channel := &multiClientChannel{
		ctx: context.Background(),
		log: DefaultLogger(),
		dialFailureCallback: func(sourceClient *multiClientChannel, egressIpPath *IpPath) {
			intercepted = egressIpPath
		},
	}

	frame, err := ToFrame(&protocol.IpPacketFromProvider{
		IpPacket: &protocol.IpPacket{PacketBytes: MessagePoolShareReadOnly(signal)},
	}, DefaultProtocolVersion)
	if err != nil {
		t.Fatal(err)
	}
	defer MessagePoolReturn(frame.MessageBytes)
	channel.clientReceive(TransferPath{}, []*protocol.Frame{frame}, Peer{ProvideMode: protocol.ProvideMode_Network})

	if intercepted == nil {
		t.Fatal("the signal did not reach the dial-failure callback")
	}
	if intercepted.ToIp4Path() != ipPath.ToIp4Path() {
		t.Fatalf("the intercepted path is %+v, want the flow", intercepted)
	}

	// garbage through the same path reaches nothing
	intercepted = nil
	garbage := MessagePoolCopy([]byte("not an ip packet"))
	garbageFrame, err := ToFrame(&protocol.IpPacketFromProvider{
		IpPacket: &protocol.IpPacket{PacketBytes: garbage},
	}, DefaultProtocolVersion)
	if err != nil {
		t.Fatal(err)
	}
	defer MessagePoolReturn(garbageFrame.MessageBytes)
	channel.clientReceive(TransferPath{}, []*protocol.Frame{garbageFrame}, Peer{ProvideMode: protocol.ProvideMode_Network})
	if intercepted != nil {
		t.Fatal("garbage was intercepted as a signal")
	}
}
