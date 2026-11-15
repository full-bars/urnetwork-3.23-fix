package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/extenderrole"
)

// Extender discovery serves the platform's extender directory to clients (the
// feed, service 2) and takes part in the gossip mesh (service 1). The code of
// both lives in connect (the extenderrole package). This file owns the
// provider side: it builds one role from the extender's own identity and
// directory, attaches its handlers to every running extender server, and
// detaches them again. With nothing attached the server keeps its polite
// "service N is not available" refusal.

const (
	extenderDiscoveryOff      = "off"
	extenderDiscoveryOn       = "on"
	extenderDiscoveryFeedOnly = "feed-only"
)

const (
	extenderDiscoveryStateOff      = "off"
	extenderDiscoveryStateWaiting  = "waiting"
	extenderDiscoveryStateRunning  = "running"
	extenderDiscoveryStateFeedOnly = "feed-only"
	extenderDiscoveryStateFailed   = "failed"
)

// Variables so tests can shorten them.
var (
	// how long a detach waits for streams the old handler admitted
	extenderDiscoveryDrainTimeout = 5 * time.Second
	extenderDiscoveryRetryMin     = 30 * time.Second
	extenderDiscoveryRetryMax     = 10 * time.Minute
)

// validExtenderDiscoveryMode reports whether mode is a value of the control key.
func validExtenderDiscoveryMode(mode string) bool {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case extenderDiscoveryOff, extenderDiscoveryOn, extenderDiscoveryFeedOnly:
		return true
	}
	return false
}

func normalizeExtenderDiscoveryMode(mode string) string {
	return strings.ToLower(strings.TrimSpace(mode))
}

// extenderDiscoveryRole is the part of extenderrole.Role the provider uses, so
// a test can stand in for the mesh.
type extenderDiscoveryRole interface {
	FeedConnHandler(conn net.Conn)
	GossipConnHandler(conn net.Conn)
	GossipRunning() bool
	Status() extenderrole.Status
	Close()
}

// newExtenderDiscoveryRole builds the role. It is a variable for tests.
var newExtenderDiscoveryRole = func(ctx context.Context, settings *extenderrole.Settings) (extenderDiscoveryRole, error) {
	return extenderrole.New(ctx, settings)
}

// extenderDiscoveryServer is the part of the extender server the provider
// drives: the swappable service handlers and the wait for admitted streams.
type extenderDiscoveryServer interface {
	SetGossipConnHandler(handler func(net.Conn))
	SetFeedConnHandler(handler func(net.Conn))
	WaitServiceStreams(ctx context.Context) error
}

// extenderDiscoveryInputs is what the extender role knows when it asks the
// controller to reconcile.
type extenderDiscoveryInputs struct {
	servers     []extenderDiscoveryServer
	directory   *connect.ExtenderDirectory
	networkHost string
	keySeed     []byte
}

// extenderDiscoverySnapshot is the discovery state as one value for the status.
type extenderDiscoverySnapshot struct {
	Requested       string
	State           string
	Failure         string
	PeerId          string
	Peers           int
	FeedSubscribers int
}

type extenderDiscovery struct {
	ctx context.Context
	// retry runs when a failed start is due again. The extender role sets it
	// to a reconcile with fresh inputs.
	retry func()

	mu         sync.Mutex
	requested  string
	role       extenderDiscoveryRole
	roleGossip bool
	attached   []extenderDiscoveryServer
	waiting    bool
	failure    string
	failures   int
	retryTimer *time.Timer
}

func newExtenderDiscovery(ctx context.Context, retry func()) *extenderDiscovery {
	return &extenderDiscovery{ctx: ctx, retry: retry, requested: extenderDiscoveryOff}
}

// setMode records the requested mode. The caller reconciles afterwards.
func (d *extenderDiscovery) setMode(mode string) error {
	if !validExtenderDiscoveryMode(mode) {
		return fmt.Errorf("extender discovery mode %q is not off, on or feed-only", mode)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.requested = normalizeExtenderDiscoveryMode(mode)
	return nil
}

func (d *extenderDiscovery) mode() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.requested
}

// reconcile makes the attached handlers match the requested mode and the
// inputs. It is safe to call at any time and as often as needed.
func (d *extenderDiscovery) reconcile(in extenderDiscoveryInputs) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.stopRetryLocked()

	if d.ctx.Err() != nil || d.requested == extenderDiscoveryOff {
		if d.role != nil {
			importantLogf("🌉 [extender] discovery stopped: the feed and gossip services are refused again\n")
		}
		d.teardownLocked()
		d.waiting = false
		d.failure = ""
		d.failures = 0
		return
	}
	if in.directory == nil || in.networkHost == "" || len(in.keySeed) == 0 {
		// The directory is built once the direct slot reads the platform's
		// root keys. Until then nothing can be served, so nothing is attached.
		d.teardownLocked()
		d.waiting = true
		return
	}
	d.waiting = false

	wantGossip := d.requested == extenderDiscoveryOn
	if d.role != nil && d.roleGossip != wantGossip {
		d.teardownLocked()
	}
	if d.role == nil {
		role, err := newExtenderDiscoveryRole(d.ctx, &extenderrole.Settings{
			NetworkHost:     in.networkHost,
			Directory:       in.directory,
			IdentityKeySeed: in.keySeed,
			DisableGossip:   !wantGossip,
		})
		if err != nil {
			d.failLocked(err)
			return
		}
		d.role = role
		d.roleGossip = role.GossipRunning()
		d.failure = ""
		d.failures = 0
		if d.roleGossip {
			importantLogf("🌉✅ [extender] discovery started: this host serves the directory feed and takes part in the gossip mesh.\n")
		} else {
			importantLogf("🌉✅ [extender] discovery started in feed-only mode: this host serves the directory feed and refuses gossip.\n")
		}
	}
	d.attachLocked(in.servers)
}

// attachLocked installs the role's handlers on every server. A server that was
// replaced while discovery ran gets them here, and a mode change takes the
// gossip handler away from a feed-only role.
func (d *extenderDiscovery) attachLocked(servers []extenderDiscoveryServer) {
	for _, server := range servers {
		server.SetFeedConnHandler(d.role.FeedConnHandler)
		if d.roleGossip {
			server.SetGossipConnHandler(d.role.GossipConnHandler)
		} else {
			server.SetGossipConnHandler(nil)
		}
	}
	d.attached = servers
}

// teardownLocked detaches the handlers first, so no new stream is admitted,
// then waits a bounded time for the streams already admitted, then closes the
// role. A server that was never attached is left alone.
func (d *extenderDiscovery) teardownLocked() {
	for _, server := range d.attached {
		server.SetGossipConnHandler(nil)
		server.SetFeedConnHandler(nil)
	}
	if 0 < len(d.attached) {
		ctx, cancel := context.WithTimeout(context.Background(), extenderDiscoveryDrainTimeout)
		for _, server := range d.attached {
			_ = server.WaitServiceStreams(ctx)
		}
		cancel()
	}
	d.attached = nil
	if d.role != nil {
		d.role.Close()
		d.role = nil
		d.roleGossip = false
	}
}

func (d *extenderDiscovery) failLocked(err error) {
	d.teardownLocked()
	d.failure = err.Error()
	d.failures++
	wait := extenderDiscoveryRetryMin
	for i := 1; i < d.failures && wait < extenderDiscoveryRetryMax; i++ {
		wait *= 2
	}
	if extenderDiscoveryRetryMax < wait {
		wait = extenderDiscoveryRetryMax
	}
	if d.failures == 1 {
		importantLogf("🌉⚠️ [extender] discovery could not start (%s): the feed and gossip services stay refused. Trying again in %s.\n", d.failure, wait)
	}
	if d.retry != nil && d.ctx.Err() == nil {
		d.retryTimer = time.AfterFunc(wait, d.retry)
	}
}

func (d *extenderDiscovery) stopRetryLocked() {
	if d.retryTimer != nil {
		d.retryTimer.Stop()
		d.retryTimer = nil
	}
}

// stop detaches everything and closes the role. The extender role calls it
// before it closes its servers.
func (d *extenderDiscovery) stop() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.stopRetryLocked()
	d.teardownLocked()
}

func (d *extenderDiscovery) snapshot() extenderDiscoverySnapshot {
	d.mu.Lock()
	defer d.mu.Unlock()
	snapshot := extenderDiscoverySnapshot{Requested: d.requested, Failure: d.failure}
	switch {
	case d.requested == extenderDiscoveryOff:
		snapshot.State = extenderDiscoveryStateOff
	case d.failure != "":
		snapshot.State = extenderDiscoveryStateFailed
	case d.role == nil && d.waiting:
		snapshot.State = extenderDiscoveryStateWaiting
	case d.role == nil:
		snapshot.State = extenderDiscoveryStateWaiting
	case d.roleGossip:
		snapshot.State = extenderDiscoveryStateRunning
	default:
		snapshot.State = extenderDiscoveryStateFeedOnly
	}
	if d.role != nil {
		status := d.role.Status()
		snapshot.PeerId = status.PeerId
		snapshot.Peers = status.Node.MeshPeerCount
		snapshot.FeedSubscribers = status.FeedSubscriberCount
	}
	return snapshot
}

// The extender role side.

// discoveryInputs gathers what the controller needs under the role's lock.
func (r *extenderRole) discoveryInputs() extenderDiscoveryInputs {
	r.mu.Lock()
	defer r.mu.Unlock()
	servers := make([]extenderDiscoveryServer, 0, len(r.servers))
	for _, server := range r.servers {
		servers = append(servers, server)
	}
	return extenderDiscoveryInputs{
		servers:     servers,
		directory:   r.discoveryDirectory,
		networkHost: r.discoveryNetworkHost,
		keySeed:     append([]byte(nil), r.keySeed...),
	}
}

// reconcileDiscovery brings the handlers in line with the requested mode. The
// role calls it when the directory appears, when a server starts and when the
// mode changes.
func (r *extenderRole) reconcileDiscovery() {
	if r.discovery == nil {
		return
	}
	r.discovery.reconcile(r.discoveryInputs())
}

// setDiscoveryDirectory is called once the platform's directory exists.
func (r *extenderRole) setDiscoveryDirectory(directory *connect.ExtenderDirectory, networkHost string) {
	r.mu.Lock()
	r.discoveryDirectory = directory
	r.discoveryNetworkHost = networkHost
	r.mu.Unlock()
	go connect.HandleError(r.reconcileDiscovery, r.cancel)
}

// setDiscoveryMode applies a value of the control key `extender_discovery`.
func (r *extenderRole) setDiscoveryMode(mode string) error {
	if r.discovery == nil {
		return errors.New("the extender role is not running")
	}
	if err := r.discovery.setMode(mode); err != nil {
		return err
	}
	r.reconcileDiscovery()
	return nil
}

func (r *extenderRole) discoverySnapshot() extenderDiscoverySnapshot {
	if r.discovery == nil {
		return extenderDiscoverySnapshot{Requested: extenderDiscoveryOff, State: extenderDiscoveryStateOff}
	}
	return r.discovery.snapshot()
}

// applyExtenderDiscoveryMode is what the control socket calls for the key
// `extender_discovery`. It returns an error when no extender role runs in this
// process, so the caller can queue the value for the next start.
func applyExtenderDiscoveryMode(mode string) error {
	if !validExtenderDiscoveryMode(mode) {
		return fmt.Errorf("extender discovery mode %q is not off, on or feed-only", mode)
	}
	role := globalExtenderRole.Load()
	if role == nil {
		return errors.New("the extender role is not running")
	}
	return role.setDiscoveryMode(mode)
}

// currentExtenderDiscoverySnapshot is the discovery state of this process for
// the status, the control socket and the metrics.
func currentExtenderDiscoverySnapshot() extenderDiscoverySnapshot {
	role := globalExtenderRole.Load()
	if role == nil {
		return extenderDiscoverySnapshot{Requested: extenderDiscoveryOff, State: extenderDiscoveryStateOff}
	}
	return role.discoverySnapshot()
}
