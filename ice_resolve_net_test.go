//go:build !js

// This file pins native ICE name resolution to the lifetime of the exact peer
// generation that initiated it.
//
// Ported to urnetwork-3.23-fix from the v2026 engine,
// github.com/full-bars/connect@v0.0.0-20261017075543-c8d8cdf536a8
// (ice_resolve_net_test.go). Two adaptations, both test scaffolding only:
//
//   - PIN's dual-stack helpers (forEachIpVersion, testUdpNetwork,
//     testTcpNetwork in test_dualstack_test.go, including its IPv6 loopback
//     precondition) do not exist in this generation. The lookups exercised
//     here are synthetic and resolve through a blocked Dial, so the two
//     families are driven by the local table below with no loopback
//     precondition.
//   - The peer-lifecycle test used PIN's webRtcPeerConnectionFactory,
//     WebRtcSettings.UseLoopbackOnlyIceInterfaces / EnableDatagramFastPath and
//     peerConn.teardown() / teardownDone / workers. This generation has no
//     factory and no separate teardown step: it retires a peer by canceling
//     its context and Run returns after Pion's local description unblocks, so
//     the test drives peer.Cancel() and waits on a local run-done channel.
package connect

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/transport/v4/stdnet"

	"github.com/urnetwork/connect/protocol"
)

// blockingIceResolver exposes entry into a real net.Resolver lookup and holds
// its wire dial until the lookup context is canceled.
type blockingIceResolver struct {
	resolver *net.Resolver
	entered  chan struct{}
	exited   chan struct{}
}

// newBlockingIceResolver creates an explicit cancellation barrier without a
// reachable DNS server or scheduler-dependent packet timeout.
func newBlockingIceResolver() *blockingIceResolver {
	entered := make(chan struct{})
	exited := make(chan struct{})
	var enteredOnce sync.Once
	var exitedOnce sync.Once
	resolver := &net.Resolver{
		PreferGo:     true,
		StrictErrors: true,
		Dial: func(ctx context.Context, network string, address string) (
			net.Conn,
			error,
		) {
			enteredOnce.Do(func() { close(entered) })
			<-ctx.Done()
			exitedOnce.Do(func() { close(exited) })
			return nil, context.Cause(ctx)
		},
	}
	return &blockingIceResolver{
		resolver: resolver,
		entered:  entered,
		exited:   exited,
	}
}

// waitForIceResolveTestSignal bounds a positive synchronization edge; elapsed
// time is not part of the behavior being asserted.
func waitForIceResolveTestSignal(
	t *testing.T,
	signal <-chan struct{},
	description string,
) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", description)
	}
}

// iceResolveTestFamilies are the transport network families the resolver is
// exercised on, standing in for PIN's testIpVersions plus testUdpNetwork /
// testTcpNetwork.
var iceResolveTestFamilies = []struct {
	name string
	udp  string
	tcp  string
}{
	{name: "v4", udp: "udp4", tcp: "tcp4"},
	{name: "v6", udp: "udp6", tcp: "tcp6"},
}

// iceResolveTestSignalSender discards the peer's signals: the lifecycle test
// only needs Pion's candidate gathering to start, not a negotiated peer.
type iceResolveTestSignalSender struct{}

func (iceResolveTestSignalSender) SendSignal(TransferPath, *protocol.Frame) {}

// restoreStunTallyWindowForTest leaves the process-global STUN aggregate as
// this test found it.
//
// The real peer connection below fails Pion's "Failed to resolve STUN host"
// line, which pionLeveledLogger counts into stunTally whether or not logging is
// enabled (see stun_tally.go). That one synthetic pulse starts the aggregate's
// emit window, so a later test that asserts on a before/after delta
// (stun_tally_test.go) would find its own pulse emitting and resetting the
// counters instead of incrementing them. No production behavior is involved:
// the counters are log-only, and the test's pulse describes a hostname that
// cannot resolve by construction.
func restoreStunTallyWindowForTest() {
	stunTally.mu.Lock()
	defer stunTally.mu.Unlock()
	stunTally.ok = 0
	stunTally.fail = 0
	stunTally.lastLogAt = time.Now()
	stunTally.nextLogAt = time.Time{}
}

// TestPeerConnectionResolveNetCancelsStunAndTurnLookups covers both resolver
// entry points used by Pion gathering. The old transport.Net calls had no
// cancellation input and remained parked in DNS after their peer was gone.
func TestPeerConnectionResolveNetCancelsStunAndTurnLookups(t *testing.T) {
	for _, family := range iceResolveTestFamilies {
		t.Run(family.name, func(t *testing.T) {
			base, err := stdnet.NewNet()
			if err != nil {
				t.Fatal(err)
			}
			cases := []struct {
				network string
				address string
				resolve func(*peerConnectionResolveNet) error
			}{
				{
					network: family.udp,
					address: "stun.blocked.invalid:3478",
					resolve: func(network *peerConnectionResolveNet) error {
						_, resolveErr := network.ResolveUDPAddr(family.udp, "stun.blocked.invalid:3478")
						return resolveErr
					},
				},
				{
					network: family.tcp,
					address: "turn.blocked.invalid:3478",
					resolve: func(network *peerConnectionResolveNet) error {
						_, resolveErr := network.ResolveTCPAddr(family.tcp, "turn.blocked.invalid:3478")
						return resolveErr
					},
				},
			}
			for _, testCase := range cases {
				blockedResolver := newBlockingIceResolver()
				network, cancel := newPeerConnectionResolveNet(
					context.Background(),
					base,
					blockedResolver.resolver,
					time.Hour,
				)
				if network.resolver == blockedResolver.resolver {
					t.Fatal("peer resolver aliases the shared resolver")
				}
				if !network.resolver.PreferGo || !network.resolver.StrictErrors {
					t.Fatal("peer resolver did not preserve resolver policy")
				}
				resolveResult := make(chan error, 1)
				go func() {
					resolveResult <- testCase.resolve(network)
				}()
				waitForIceResolveTestSignal(
					t,
					blockedResolver.entered,
					testCase.network+" resolver entry for "+testCase.address,
				)
				cancel()
				select {
				case resolveErr := <-resolveResult:
					if !errors.Is(resolveErr, context.Canceled) {
						t.Fatalf(
							"%s resolve error = %v, want context cancellation",
							testCase.network,
							resolveErr,
						)
					}
				case <-time.After(5 * time.Second):
					t.Fatalf("%s resolve ignored peer cancellation", testCase.network)
				}
				waitForIceResolveTestSignal(
					t,
					blockedResolver.exited,
					testCase.network+" resolver exit",
				)
			}
		})
	}
}

// TestPreferredAddress pins the family-agnostic preference: the first IPv4
// answer wins even when a dual-stack name returns its AAAA first, and a
// family-filtered lookup keeps the resolver's own order.
func TestPreferredAddress(t *testing.T) {
	v6 := netip.MustParseAddr("2001:db8::1")
	v4a := netip.MustParseAddr("192.0.2.10")
	v4b := netip.MustParseAddr("192.0.2.11")
	mapped := netip.MustParseAddr("::ffff:192.0.2.12")

	cases := []struct {
		name      string
		lookup    string
		addresses []netip.Addr
		want      netip.Addr
	}{
		{"dual stack prefers the first v4", "ip", []netip.Addr{v6, v4a, v4b}, v4a},
		{"v4-mapped answer counts as v4", "ip", []netip.Addr{v6, mapped}, mapped},
		{"v6 only falls back to the first answer", "ip", []netip.Addr{v6}, v6},
		{"ip4 keeps the resolver order", "ip4", []netip.Addr{v4a, v4b}, v4a},
		{"ip6 keeps the resolver order", "ip6", []netip.Addr{v6}, v6},
	}
	for _, testCase := range cases {
		if got := preferredAddress(testCase.addresses, testCase.lookup); got != testCase.want {
			t.Fatalf("%s: preferredAddress = %v, want %v", testCase.name, got, testCase.want)
		}
	}
}

// TestResolveIPAddrLiteralFamily covers the literal branch, which never reaches
// DNS: the requested family must still reject the other family, shaped like
// net.Resolve*'s own error.
func TestResolveIPAddrLiteralFamily(t *testing.T) {
	base, err := stdnet.NewNet()
	if err != nil {
		t.Fatal(err)
	}
	network, cancel := newPeerConnectionResolveNet(context.Background(), base, nil, 0)
	defer cancel()

	cases := []struct {
		network string
		host    string
		wantErr bool
		wantV4  bool
	}{
		{network: "udp4", host: "::1", wantErr: true},
		{network: "tcp4", host: "2001:db8::1", wantErr: true},
		{network: "udp6", host: "1.2.3.4", wantErr: true},
		{network: "udp", host: "::1"},
		{network: "udp", host: "1.2.3.4", wantV4: true},
		{network: "udp4", host: "::ffff:1.2.3.4", wantV4: true},
		{network: "udp6", host: "::ffff:1.2.3.4", wantErr: true},
	}
	for _, testCase := range cases {
		address, resolveErr := network.ResolveIPAddr(testCase.network, testCase.host)
		if testCase.wantErr {
			var addrErr *net.AddrError
			if !errors.As(resolveErr, &addrErr) {
				t.Fatalf("%s %q: error = %v, want *net.AddrError", testCase.network, testCase.host, resolveErr)
			}
			if addrErr.Err != "no suitable address found" {
				t.Fatalf("%s %q: AddrError.Err = %q", testCase.network, testCase.host, addrErr.Err)
			}
			continue
		}
		if resolveErr != nil {
			t.Fatalf("%s %q: unexpected error %v", testCase.network, testCase.host, resolveErr)
		}
		if gotV4 := address.IP.To4() != nil; gotV4 != testCase.wantV4 {
			t.Fatalf("%s %q: IPv4 = %v, want %v (%v)", testCase.network, testCase.host, gotV4, testCase.wantV4, address.IP)
		}
	}

	// The UDP entry point keeps the same family check after splitting the port.
	if _, err := network.ResolveUDPAddr("udp4", "[::ffff:1.2.3.4]:3478"); err != nil {
		t.Fatalf("mapped literal under udp4: %v", err)
	}
	if _, err := network.ResolveUDPAddr("udp4", "[::1]:3478"); err == nil {
		t.Fatal("udp4 accepted an IPv6 literal through ResolveUDPAddr")
	}
}

// TestResolveIPAddrPrefersIPv4ForDualStackLookup closes the wiring gap for F6:
// it drives the order a dual-stack name can return (AAAA first) through the
// real entry point, which a fake DNS server cannot stage deterministically
// because LookupNetIP sorts answers by RFC 6724.
func TestResolveIPAddrPrefersIPv4ForDualStackLookup(t *testing.T) {
	base, err := stdnet.NewNet()
	if err != nil {
		t.Fatal(err)
	}
	v6 := netip.MustParseAddr("2001:db8::1")
	v4 := netip.MustParseAddr("192.0.2.10")
	for _, testCase := range []struct {
		name      string
		addresses []netip.Addr
		wantV4    bool
	}{
		{name: "dual stack prefers v4", addresses: []netip.Addr{v6, v4}, wantV4: true},
		{name: "v6 only keeps the first answer", addresses: []netip.Addr{v6}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			network, cancel := newPeerConnectionResolveNet(context.Background(), base, nil, 0)
			defer cancel()
			lookups := 0
			network.lookupNetIP = func(_ context.Context, lookupNetwork string, host string) ([]netip.Addr, error) {
				lookups++
				if lookupNetwork != "ip" {
					t.Fatalf("lookup network = %q, want ip", lookupNetwork)
				}
				if host != "stun.example" {
					t.Fatalf("lookup host = %q", host)
				}
				return testCase.addresses, nil
			}
			address, resolveErr := network.ResolveUDPAddr("udp", "stun.example:3478")
			if resolveErr != nil {
				t.Fatal(resolveErr)
			}
			if address.Port != 3478 {
				t.Fatalf("port = %d, want 3478", address.Port)
			}
			if gotV4 := address.IP.To4() != nil; gotV4 != testCase.wantV4 {
				t.Fatalf("IPv4 = %v, want %v (%v)", gotV4, testCase.wantV4, address.IP)
			}
			if lookups != 1 {
				t.Fatalf("lookups = %d, want 1", lookups)
			}
		})
	}
}

// TestResolveIPAddrLookupNetwork pins the network mapping every lookup passes
// to the resolver, so a family-filtered endpoint cannot ask for the wrong one.
func TestResolveIPAddrLookupNetwork(t *testing.T) {
	base, err := stdnet.NewNet()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		network string
		want    string
	}{
		{"ip", "ip"}, {"udp", "ip"}, {"tcp", "ip"},
		{"ip4", "ip4"}, {"udp4", "ip4"}, {"tcp4", "ip4"},
		{"ip6", "ip6"}, {"udp6", "ip6"}, {"tcp6", "ip6"},
	}
	for _, testCase := range cases {
		t.Run(testCase.network, func(t *testing.T) {
			network, cancel := newPeerConnectionResolveNet(context.Background(), base, nil, 0)
			defer cancel()
			got := ""
			network.lookupNetIP = func(_ context.Context, lookupNetwork string, _ string) ([]netip.Addr, error) {
				got = lookupNetwork
				return []netip.Addr{netip.MustParseAddr("192.0.2.10")}, nil
			}
			if _, err := network.ResolveIPAddr(testCase.network, "host.example"); err != nil {
				t.Fatal(err)
			}
			if got != testCase.want {
				t.Fatalf("lookup network = %q, want %q", got, testCase.want)
			}
		})
	}
}

// TestResolveIPAddrPreservesLiteralZone keeps the literal branch zoned and out
// of DNS, which is what link-local ICE hosts depend on.
func TestResolveIPAddrPreservesLiteralZone(t *testing.T) {
	base, err := stdnet.NewNet()
	if err != nil {
		t.Fatal(err)
	}
	network, cancel := newPeerConnectionResolveNet(context.Background(), base, nil, 0)
	defer cancel()
	network.lookupNetIP = func(context.Context, string, string) ([]netip.Addr, error) {
		t.Fatal("a literal reached DNS")
		return nil, nil
	}
	address, err := network.ResolveIPAddr("udp6", "fe80::1%eth0")
	if err != nil {
		t.Fatal(err)
	}
	if address.Zone != "eth0" {
		t.Fatalf("zone = %q, want eth0", address.Zone)
	}
	if address.IP.To4() != nil {
		t.Fatalf("link-local literal resolved as IPv4: %v", address.IP)
	}
}

// TestResolveIPAddrErrorPaths covers the two refusal branches and lookup error
// propagation that the happy-path tables cannot reach.
func TestResolveIPAddrErrorPaths(t *testing.T) {
	base, err := stdnet.NewNet()
	if err != nil {
		t.Fatal(err)
	}
	network, cancel := newPeerConnectionResolveNet(context.Background(), base, nil, 0)
	defer cancel()

	var unknown net.UnknownNetworkError
	if _, err := network.ResolveIPAddr("udp9", "host.example"); !errors.As(err, &unknown) {
		t.Fatalf("unknown network error = %v, want net.UnknownNetworkError", err)
	}

	network.lookupNetIP = func(context.Context, string, string) ([]netip.Addr, error) {
		return nil, nil
	}
	if _, err := network.ResolveIPAddr("udp", "empty.example"); err == nil || !strings.Contains(err.Error(), "no addresses") {
		t.Fatalf("empty lookup error = %v, want a no-addresses error", err)
	}

	sentinel := errors.New("synthetic lookup failure")
	network.lookupNetIP = func(context.Context, string, string) ([]netip.Addr, error) {
		return nil, sentinel
	}
	if _, err := network.ResolveIPAddr("udp", "fail.example"); !errors.Is(err, sentinel) {
		t.Fatalf("lookup failure = %v, want the resolver error", err)
	}
}

// TestWebRtcPeerTeardownCancelsBlockedStunResolution reproduces the production
// shutdown ordering: SetLocalDescription starts a STUN hostname lookup, then
// the peer retires while that lookup is blocked. Teardown must cancel DNS
// before PeerConnection.Close joins the ICE gatherer's WaitGroup.
func TestWebRtcPeerTeardownCancelsBlockedStunResolution(t *testing.T) {
	defer restoreStunTallyWindowForTest()

	blockedResolver := newBlockingIceResolver()
	settings := DefaultWebRtcSettings()
	settings.Log = NewNoopLogger()
	settings.StunGatherTimeout = time.Hour
	settings.IceServerUrls = []string{"stun:stun.blocked.invalid:3478"}
	settings.iceResolverForTest = blockedResolver.resolver
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	peer, err := newPeerConn(
		ctx,
		peerConnKey{PeerId: NewId(), StreamId: NewId()},
		NewId(),
		true,
		iceResolveTestSignalSender{},
		settings,
	)
	if err != nil {
		t.Fatal(err)
	}
	if peer.cancelIceResolve == nil {
		t.Fatal("peer connection has no cancelable ICE resolver")
	}

	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		peer.Run()
	}()
	waitForIceResolveTestSignal(
		t,
		blockedResolver.entered,
		"Pion STUN resolver entry",
	)

	// Retirement is exactly what the manager does to a replaced connection:
	// cancel the generation's context. Pion resolves the STUN host in a
	// goroutine detached from SetLocalDescription, so a resolver that is not
	// retired with its generation leaves that lookup parked -- the exit and
	// run-done waits below time out.
	peer.Cancel()
	waitForIceResolveTestSignal(t, blockedResolver.exited, "STUN resolver exit")
	waitForIceResolveTestSignal(t, runDone, "peer run exit")
}
