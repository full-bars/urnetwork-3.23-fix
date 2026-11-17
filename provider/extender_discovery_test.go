package main

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/extenderrole"
)

type fakeDiscoveryRole struct {
	gossip bool
	closed atomic.Bool
	feed   atomic.Int32
	status extenderrole.Status
}

func (self *fakeDiscoveryRole) FeedConnHandler(conn net.Conn)   { self.feed.Add(1) }
func (self *fakeDiscoveryRole) GossipConnHandler(conn net.Conn) {}
func (self *fakeDiscoveryRole) GossipRunning() bool             { return self.gossip }
func (self *fakeDiscoveryRole) Status() extenderrole.Status     { return self.status }
func (self *fakeDiscoveryRole) Close()                          { self.closed.Store(true) }

type fakeDiscoveryServer struct {
	mu          sync.Mutex
	gossip      func(net.Conn)
	feed        func(net.Conn)
	waits       int
	setGossipOn int
}

func (self *fakeDiscoveryServer) SetGossipConnHandler(handler func(net.Conn)) {
	self.mu.Lock()
	defer self.mu.Unlock()
	self.gossip = handler
	self.setGossipOn++
}

func (self *fakeDiscoveryServer) SetFeedConnHandler(handler func(net.Conn)) {
	self.mu.Lock()
	defer self.mu.Unlock()
	self.feed = handler
}

func (self *fakeDiscoveryServer) WaitServiceStreams(ctx context.Context) error {
	self.mu.Lock()
	defer self.mu.Unlock()
	self.waits++
	return nil
}

func (self *fakeDiscoveryServer) handlers() (gossip bool, feed bool) {
	self.mu.Lock()
	defer self.mu.Unlock()
	return self.gossip != nil, self.feed != nil
}

// discoveryTestHarness replaces the role factory with a recorder.
type discoveryTestHarness struct {
	mu       sync.Mutex
	roles    []*fakeDiscoveryRole
	settings []*extenderrole.Settings
	fail     error
}

func newDiscoveryTestHarness(t *testing.T) *discoveryTestHarness {
	h := &discoveryTestHarness{}
	original := newExtenderDiscoveryRole
	newExtenderDiscoveryRole = func(ctx context.Context, settings *extenderrole.Settings) (extenderDiscoveryRole, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.fail != nil {
			return nil, h.fail
		}
		role := &fakeDiscoveryRole{gossip: !settings.DisableGossip}
		h.roles = append(h.roles, role)
		h.settings = append(h.settings, settings)
		return role, nil
	}
	t.Cleanup(func() { newExtenderDiscoveryRole = original })
	return h
}

func (h *discoveryTestHarness) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.roles)
}

func discoveryTestInputs(t *testing.T, servers ...*fakeDiscoveryServer) extenderDiscoveryInputs {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	list := make([]extenderDiscoveryServer, 0, len(servers))
	for _, server := range servers {
		list = append(list, server)
	}
	return extenderDiscoveryInputs{
		servers:     list,
		directory:   connect.NewExtenderDirectory(ctx, connect.DefaultExtenderDirectorySettings()),
		networkHost: "bringyour.com",
		keySeed:     make([]byte, 32),
	}
}

func TestExtenderDiscoveryModeValidation(t *testing.T) {
	for _, mode := range []string{"off", "on", "feed-only", " ON ", "Feed-Only"} {
		if !validExtenderDiscoveryMode(mode) {
			t.Fatalf("%q should be valid", mode)
		}
	}
	for _, mode := range []string{"", "yes", "feed", "gossip"} {
		if validExtenderDiscoveryMode(mode) {
			t.Fatalf("%q should be invalid", mode)
		}
	}
	d := newExtenderDiscovery(context.Background(), nil)
	if err := d.setMode("bogus"); err == nil {
		t.Fatal("setMode accepted a bad value")
	}
	if d.mode() != extenderDiscoveryOff {
		t.Fatalf("the default mode = %q, want off", d.mode())
	}
}

func TestExtenderDiscoveryOffBuildsNothing(t *testing.T) {
	h := newDiscoveryTestHarness(t)
	server := &fakeDiscoveryServer{}
	d := newExtenderDiscovery(context.Background(), nil)
	d.reconcile(discoveryTestInputs(t, server))
	if h.count() != 0 {
		t.Fatal("off built a role")
	}
	if gossip, feed := server.handlers(); gossip || feed {
		t.Fatal("off attached a handler")
	}
	if state := d.snapshot().State; state != extenderDiscoveryStateOff {
		t.Fatalf("state = %q", state)
	}
}

func TestExtenderDiscoveryWaitsForTheDirectory(t *testing.T) {
	h := newDiscoveryTestHarness(t)
	server := &fakeDiscoveryServer{}
	d := newExtenderDiscovery(context.Background(), nil)
	_ = d.setMode("on")
	in := discoveryTestInputs(t, server)
	in.directory = nil
	d.reconcile(in)
	if h.count() != 0 {
		t.Fatal("a role was built without a directory")
	}
	if gossip, feed := server.handlers(); gossip || feed {
		t.Fatal("handlers attached without a directory, the refusal must stay")
	}
	if state := d.snapshot().State; state != extenderDiscoveryStateWaiting {
		t.Fatalf("state = %q, want waiting", state)
	}
	// the directory arrives
	d.reconcile(discoveryTestInputs(t, server))
	if state := d.snapshot().State; state != extenderDiscoveryStateRunning {
		t.Fatalf("state = %q, want running", state)
	}
}

func TestExtenderDiscoveryOnAttachesBothAndFeedOnlyAttachesFeed(t *testing.T) {
	h := newDiscoveryTestHarness(t)
	a, b := &fakeDiscoveryServer{}, &fakeDiscoveryServer{}
	d := newExtenderDiscovery(context.Background(), nil)
	_ = d.setMode("on")
	d.reconcile(discoveryTestInputs(t, a, b))
	for _, server := range []*fakeDiscoveryServer{a, b} {
		if gossip, feed := server.handlers(); !gossip || !feed {
			t.Fatalf("on: gossip=%v feed=%v, want both", gossip, feed)
		}
	}
	if h.settings[0].DisableGossip {
		t.Fatal("on built a feed-only role")
	}
	if got := h.settings[0]; got.NetworkHost != "bringyour.com" || len(got.IdentityKeySeed) != 32 || got.Directory == nil {
		t.Fatalf("role settings = %+v", got)
	}

	// switching to feed-only rebuilds the role and takes gossip away
	_ = d.setMode("feed-only")
	d.reconcile(discoveryTestInputs(t, a, b))
	if h.count() != 2 || !h.roles[0].closed.Load() {
		t.Fatalf("roles built = %d, first closed = %v", h.count(), h.roles[0].closed.Load())
	}
	if !h.settings[1].DisableGossip {
		t.Fatal("feed-only built a gossip role")
	}
	for _, server := range []*fakeDiscoveryServer{a, b} {
		if gossip, feed := server.handlers(); gossip || !feed {
			t.Fatalf("feed-only: gossip=%v feed=%v, want feed only", gossip, feed)
		}
	}
	if state := d.snapshot().State; state != extenderDiscoveryStateFeedOnly {
		t.Fatalf("state = %q", state)
	}
}

func TestExtenderDiscoveryOffDetachesDrainsAndCloses(t *testing.T) {
	h := newDiscoveryTestHarness(t)
	server := &fakeDiscoveryServer{}
	d := newExtenderDiscovery(context.Background(), nil)
	_ = d.setMode("on")
	d.reconcile(discoveryTestInputs(t, server))
	_ = d.setMode("off")
	d.reconcile(discoveryTestInputs(t, server))
	if gossip, feed := server.handlers(); gossip || feed {
		t.Fatal("off left a handler attached")
	}
	server.mu.Lock()
	waits := server.waits
	server.mu.Unlock()
	if waits != 1 {
		t.Fatalf("waited for admitted streams %d times, want 1", waits)
	}
	if !h.roles[0].closed.Load() {
		t.Fatal("off did not close the role")
	}
	if state := d.snapshot().State; state != extenderDiscoveryStateOff {
		t.Fatalf("state = %q", state)
	}
}

func TestExtenderDiscoveryNewServerGetsTheHandlers(t *testing.T) {
	newDiscoveryTestHarness(t)
	first := &fakeDiscoveryServer{}
	d := newExtenderDiscovery(context.Background(), nil)
	_ = d.setMode("on")
	d.reconcile(discoveryTestInputs(t, first))
	second := &fakeDiscoveryServer{}
	d.reconcile(discoveryTestInputs(t, first, second))
	if gossip, feed := second.handlers(); !gossip || !feed {
		t.Fatalf("a server that started later has gossip=%v feed=%v", gossip, feed)
	}
}

func TestExtenderDiscoveryFailureKeepsTheRefusalAndRetries(t *testing.T) {
	h := newDiscoveryTestHarness(t)
	h.fail = errors.New("no mesh today")
	oldMin, oldMax := extenderDiscoveryRetryMin, extenderDiscoveryRetryMax
	extenderDiscoveryRetryMin, extenderDiscoveryRetryMax = 10*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { extenderDiscoveryRetryMin, extenderDiscoveryRetryMax = oldMin, oldMax })

	server := &fakeDiscoveryServer{}
	retried := make(chan struct{}, 8)
	var d *extenderDiscovery
	d = newExtenderDiscovery(context.Background(), func() {
		retried <- struct{}{}
		d.reconcile(discoveryTestInputs(t, server))
	})
	_ = d.setMode("on")
	d.reconcile(discoveryTestInputs(t, server))
	if gossip, feed := server.handlers(); gossip || feed {
		t.Fatal("a failed start attached a handler")
	}
	snapshot := d.snapshot()
	if snapshot.State != extenderDiscoveryStateFailed || snapshot.Failure != "no mesh today" {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	select {
	case <-retried:
	case <-time.After(5 * time.Second):
		t.Fatal("a failed start was not retried")
	}
	// the cause goes away and the next retry starts discovery
	h.mu.Lock()
	h.fail = nil
	h.mu.Unlock()
	deadline := time.Now().Add(5 * time.Second)
	for d.snapshot().State != extenderDiscoveryStateRunning {
		if time.Now().After(deadline) {
			t.Fatalf("state = %q, want running after the retry", d.snapshot().State)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestExtenderDiscoverySnapshotReportsPeersAndSubscribers(t *testing.T) {
	h := newDiscoveryTestHarness(t)
	server := &fakeDiscoveryServer{}
	d := newExtenderDiscovery(context.Background(), nil)
	_ = d.setMode("on")
	d.reconcile(discoveryTestInputs(t, server))
	h.roles[0].status = extenderrole.Status{PeerId: "peer-1", GossipRunning: true, FeedSubscriberCount: 3}
	h.roles[0].status.Node.MeshPeerCount = 7
	h.roles[0].status.Node.PeerCount = 9
	h.roles[0].status.ActiveRecords = 8
	h.roles[0].status.MessagesAccepted = 11
	h.roles[0].status.ApplyErrors = 2
	h.roles[0].status.LastAcceptedUnixMilli = 123456
	snapshot := d.snapshot()
	if snapshot.PeerId != "peer-1" || snapshot.Peers != 7 || snapshot.FeedSubscribers != 3 || snapshot.ActiveRecords == nil || *snapshot.ActiveRecords != 8 || snapshot.ConnectedPeers == nil || *snapshot.ConnectedPeers != 9 || snapshot.MessagesAccepted == nil || *snapshot.MessagesAccepted != 11 || snapshot.ApplyErrors == nil || *snapshot.ApplyErrors != 2 || snapshot.LastAcceptedUnixMilli != 123456 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
}

func TestExtenderDiscoveryConcurrentToggleIsRaceFree(t *testing.T) {
	newDiscoveryTestHarness(t)
	server := &fakeDiscoveryServer{}
	d := newExtenderDiscovery(context.Background(), nil)
	in := discoveryTestInputs(t, server)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 50 {
				modes := []string{"on", "feed-only", "off"}
				_ = d.setMode(modes[(i+j)%3])
				d.reconcile(in)
				_ = d.snapshot()
			}
		}()
	}
	wg.Wait()
	d.stop()
	if gossip, feed := server.handlers(); gossip || feed {
		t.Fatal("stop left a handler attached")
	}
}

func TestApplyExtenderDiscoveryModeWithoutARole(t *testing.T) {
	previous := globalControlState
	globalControlState = newControlState()
	t.Cleanup(func() { globalControlState = previous })
	globalExtenderRole.Store(nil)
	if err := applyExtenderDiscoveryMode("on"); err == nil {
		t.Fatal("applying a mode with no extender role should fail so the caller can queue it")
	}
	if err := applyExtenderDiscoveryMode("bogus"); err == nil {
		t.Fatal("a bad mode was accepted")
	}
	if got := currentExtenderDiscoverySnapshot(); got.State != extenderDiscoveryStateOff || got.Requested != extenderDiscoveryOn {
		t.Fatalf("snapshot with no role = %+v", got)
	}
	if err := globalControlState.set("extender_discovery", "off"); err != nil {
		t.Fatal(err)
	}
	if got := currentExtenderDiscoverySnapshot(); got.Requested != "off" || got.State != extenderDiscoveryStateOff {
		t.Fatalf("saved off with no role: %+v", got)
	}
}
