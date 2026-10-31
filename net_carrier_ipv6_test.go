package connect

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeSocksExit is a minimal SOCKS5 server standing in for an exit. A CONNECT to
// an IPv6 address is answered with "host unreachable" when v6Fails, otherwise
// with success; IPv4 always succeeds. It counts the requests it saw per family.
type fakeSocksExit struct {
	listener net.Listener
	v6Fails  atomic.Bool
	v6Seen   atomic.Int64
	v4Seen   atomic.Int64
}

func newFakeSocksExit(t *testing.T, v6Fails bool) *fakeSocksExit {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	exit := &fakeSocksExit{listener: listener}
	exit.v6Fails.Store(v6Fails)
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go exit.serve(conn)
		}
	}()
	return exit
}

func (self *fakeSocksExit) serve(conn net.Conn) {
	defer conn.Close()
	head := make([]byte, 2)
	if _, err := io.ReadFull(conn, head); err != nil {
		return
	}
	if _, err := io.ReadFull(conn, make([]byte, int(head[1]))); err != nil {
		return
	}
	conn.Write([]byte{5, 0})
	request := make([]byte, 4)
	if _, err := io.ReadFull(conn, request); err != nil {
		return
	}
	addrLen := 4
	if request[3] == 4 {
		addrLen = 16
	}
	if _, err := io.ReadFull(conn, make([]byte, addrLen+2)); err != nil {
		return
	}
	isV6 := request[3] == 4
	if isV6 {
		self.v6Seen.Add(1)
		if self.v6Fails.Load() {
			conn.Write([]byte{5, 4, 0, 1, 0, 0, 0, 0, 0, 0})
			return
		}
	} else {
		self.v4Seen.Add(1)
	}
	conn.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
	io.Copy(io.Discard, conn)
}

func (self *fakeSocksExit) settings(index int) *ProxySettings {
	return &ProxySettings{Index: index, Network: "tcp", Address: self.listener.Addr().String()}
}

// withCarrierClock swaps the process-wide table for a fresh one on a manual
// clock and restores it after the test.
type carrierClock struct {
	mu  sync.Mutex
	now time.Time
}

func (self *carrierClock) Now() time.Time {
	self.mu.Lock()
	defer self.mu.Unlock()
	return self.now
}

func (self *carrierClock) Advance(d time.Duration) {
	self.mu.Lock()
	defer self.mu.Unlock()
	self.now = self.now.Add(d)
}

func withCarrierClock(t *testing.T) (*carrierClock, *[]string) {
	t.Helper()
	clock := &carrierClock{now: time.Unix(1_700_000_000, 0)}
	lines := &[]string{}
	var linesMu sync.Mutex
	saved := carrierIpv6
	table := newCarrierIpv6Table()
	table.now = clock.Now
	table.logf = func(format string, args ...any) {
		linesMu.Lock()
		defer linesMu.Unlock()
		*lines = append(*lines, format)
	}
	carrierIpv6 = table
	t.Cleanup(func() { carrierIpv6 = saved })
	return clock, lines
}

var testV6Target = netip.MustParseAddr("2001:db8::1")
var testV4Target = netip.MustParseAddr("192.0.2.1")

func dialThrough(settings *ProxySettings, addr netip.Addr) (net.Conn, error) {
	dial := settings.NewDialContext(context.Background(), &net.Dialer{Timeout: 2 * time.Second})
	return dial(context.Background(), familyDialNetwork("tcp", addr), net.JoinHostPort(addr.String(), "443"))
}

func closeConn(conn net.Conn) {
	if conn != nil {
		conn.Close()
	}
}

func TestCarrierIpv6SuppressedAfterThresholdAndRaceUsesV4(t *testing.T) {
	_, lines := withCarrierClock(t)
	exitA := newFakeSocksExit(t, true)
	a := exitA.settings(1)

	for i := 0; i < carrierIpv6FailureThreshold; i++ {
		conn, err := dialThrough(a, testV6Target)
		closeConn(conn)
		if err == nil || errors.Is(err, errCarrierIpv6Suppressed) {
			t.Fatalf("dial %d: want a real v6 failure, got %v", i, err)
		}
	}
	if got := exitA.v6Seen.Load(); got != int64(carrierIpv6FailureThreshold) {
		t.Fatalf("exit saw %d v6 dials, want %d", got, carrierIpv6FailureThreshold)
	}

	// marked: v6 is no longer initiated through A
	_, err := dialThrough(a, testV6Target)
	if !errors.Is(err, errCarrierIpv6Suppressed) {
		t.Fatalf("want errCarrierIpv6Suppressed, got %v", err)
	}
	if got := exitA.v6Seen.Load(); got != int64(carrierIpv6FailureThreshold) {
		t.Fatalf("a suppressed dial reached the exit: %d v6 dials", got)
	}

	// the dial race (v6 first) lands on v4 without touching v6 again
	dial := a.NewDialContext(context.Background(), &net.Dialer{Timeout: 2 * time.Second})
	conn, err := dialHostPortRace(context.Background(), "tcp", "443",
		[]netip.Addr{testV6Target, testV4Target}, time.Minute, dial)
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if exitA.v4Seen.Load() != 1 || exitA.v6Seen.Load() != int64(carrierIpv6FailureThreshold) {
		t.Fatalf("race: v4=%d v6=%d", exitA.v4Seen.Load(), exitA.v6Seen.Load())
	}

	// the verdict is visible in the snapshot and was logged
	snapshot := CarrierIpv6Snapshot()
	if len(snapshot) != 1 || snapshot[0].Carrier != a.Address || snapshot[0].ConsecutiveFailed != carrierIpv6FailureThreshold {
		t.Fatalf("snapshot: %v", snapshot)
	}
	if len(*lines) != 1 {
		t.Fatalf("want one transition line, got %v", *lines)
	}
}

func TestCarrierIpv6VerdictIsPerCarrier(t *testing.T) {
	withCarrierClock(t)
	exitA := newFakeSocksExit(t, true)
	exitB := newFakeSocksExit(t, false)
	a, b := exitA.settings(1), exitB.settings(2)

	for i := 0; i < carrierIpv6FailureThreshold; i++ {
		conn, _ := dialThrough(a, testV6Target)
		closeConn(conn)
	}
	if _, err := dialThrough(a, testV6Target); !errors.Is(err, errCarrierIpv6Suppressed) {
		t.Fatalf("A should be suppressed, got %v", err)
	}
	for i := 0; i < 2*carrierIpv6FailureThreshold; i++ {
		conn, err := dialThrough(b, testV6Target)
		if err != nil {
			t.Fatalf("B dial %d: %v", i, err)
		}
		conn.Close()
	}
	if exitB.v6Seen.Load() != int64(2*carrierIpv6FailureThreshold) {
		t.Fatalf("B was not offered v6 every time: %d", exitB.v6Seen.Load())
	}
	if len(CarrierIpv6Snapshot()) != 1 {
		t.Fatalf("only A carries a verdict: %v", CarrierIpv6Snapshot())
	}
}

func TestCarrierIpv6SameGatewayDifferentUserIsADifferentCarrier(t *testing.T) {
	withCarrierClock(t)
	table := carrierIpv6
	for i := 0; i < carrierIpv6FailureThreshold; i++ {
		_, probe := table.allow("gw:1\x1falice")
		table.done("gw:1\x1falice", probe, errors.New("x"), false)
	}
	if ok, _ := table.allow("gw:1\x1falice"); ok {
		t.Fatal("alice should be suppressed")
	}
	if ok, _ := table.allow("gw:1\x1fbob"); !ok {
		t.Fatal("bob shares the gateway but not the verdict")
	}
}

func TestCarrierIpv6CooldownAllowsExactlyOneProbe(t *testing.T) {
	clock, _ := withCarrierClock(t)
	exit := newFakeSocksExit(t, true)
	a := exit.settings(1)
	for i := 0; i < carrierIpv6FailureThreshold; i++ {
		conn, _ := dialThrough(a, testV6Target)
		closeConn(conn)
	}
	clock.Advance(carrierIpv6Cooldown - time.Second)
	if _, err := dialThrough(a, testV6Target); !errors.Is(err, errCarrierIpv6Suppressed) {
		t.Fatalf("still cooling down, got %v", err)
	}

	clock.Advance(2 * time.Second)
	table := carrierIpv6
	ok, probe := table.allow(a.Key())
	if !ok || !probe {
		t.Fatal("first dial after the cooldown must be the probe")
	}
	// a second concurrent dial while the probe is in flight is refused
	if ok2, _ := table.allow(a.Key()); ok2 {
		t.Fatal("only one probe may be in flight")
	}
	// the probe fails: re-armed for a full cooldown
	table.done(a.Key(), true, errors.New("unreachable"), false)
	if ok3, _ := table.allow(a.Key()); ok3 {
		t.Fatal("failed probe must re-arm the cooldown")
	}
	clock.Advance(carrierIpv6Cooldown + time.Second)

	// this time the exit has v6: the probe succeeds and the verdict clears
	exit.v6Fails.Store(false)
	conn, err := dialThrough(a, testV6Target)
	if err != nil {
		t.Fatalf("probe dial: %v", err)
	}
	conn.Close()
	if len(CarrierIpv6Snapshot()) != 0 {
		t.Fatalf("verdict should be cleared: %v", CarrierIpv6Snapshot())
	}
	for i := 0; i < 3; i++ {
		conn, err := dialThrough(a, testV6Target)
		if err != nil {
			t.Fatalf("v6 should flow again: %v", err)
		}
		conn.Close()
	}
}

func TestCarrierIpv6CancelledProbeReleasesTheSlot(t *testing.T) {
	clock, _ := withCarrierClock(t)
	table := carrierIpv6
	key := "gw:2"
	for i := 0; i < carrierIpv6FailureThreshold; i++ {
		table.done(key, false, errors.New("x"), false)
	}
	clock.Advance(carrierIpv6Cooldown + time.Second)
	ok, probe := table.allow(key)
	if !ok || !probe {
		t.Fatal("want probe")
	}
	table.done(key, true, context.Canceled, true)
	if ok, probe = table.allow(key); !ok || !probe {
		t.Fatal("a cancelled probe proves nothing; the next dial probes")
	}
}

func TestCarrierIpv6SuccessAndV4FailureResetTheStreak(t *testing.T) {
	withCarrierClock(t)
	table := carrierIpv6
	key := "gw:3"
	for round := 0; round < 3; round++ {
		table.done(key, false, errors.New("x"), false)
		table.done(key, false, errors.New("x"), false)
		table.done(key, false, nil, false) // success resets
	}
	if ok, _ := table.allow(key); !ok || len(table.Snapshot()) != 0 {
		t.Fatal("interleaved successes must never mark the carrier")
	}
	table.done(key, false, errors.New("x"), false)
	table.done(key, false, errors.New("x"), false)
	table.noteV4Failure(key) // a dead proxy is not a v6 verdict
	table.done(key, false, errors.New("x"), false)
	if len(table.Snapshot()) != 0 {
		t.Fatal("a v4 failure in between must reset the streak")
	}
}

func TestCarrierIpv6DialNamesFamily(t *testing.T) {
	cases := []struct {
		network, addr string
		want          int
	}{
		{"tcp6", "example.com:443", 6},
		{"tcp4", "[2001:db8::1]:443", 4},
		{"tcp", "[2001:db8::1]:443", 6},
		{"tcp", "192.0.2.1:443", 4},
		{"tcp", "example.com:443", 0},
	}
	for _, c := range cases {
		if got := dialNamesFamily(c.network, c.addr); got != c.want {
			t.Errorf("%s %s: got %d want %d", c.network, c.addr, got, c.want)
		}
	}
}

func TestCarrierIpv6ConcurrentDialsAreRaceFree(t *testing.T) {
	withCarrierClock(t)
	table := carrierIpv6
	keys := []string{"gw:a", "gw:b", "gw:c"}
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				key := keys[(g+i)%len(keys)]
				ok, probe := table.allow(key)
				if !ok {
					table.noteV4Failure(key)
					table.Snapshot()
					continue
				}
				var err error
				if (g+i)%3 != 0 {
					err = errors.New("x")
				}
				table.done(key, probe, err, i%97 == 0)
			}
		}(g)
	}
	wg.Wait()
}
