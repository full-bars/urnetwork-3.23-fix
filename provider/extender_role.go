package main

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/extender"
)

// The provider-side extender serving role (EXTENDER.md A1-A4, G2).
//
// One provider process may serve one extender role, and only from its direct
// slot: the role binds this host's carrier ports and the host's address is
// what a record would publish, while a proxied slot egresses through an
// address the host does not hold. The role is opt-in with
// URNETWORK_EXTENDER=on, starts no listener otherwise, and never aborts the
// provider when a carrier port is taken: it disables that carrier, logs one
// line with the reason, and retries the tcp carrier no faster than every
// three minutes.
//
// This file owns only the wiring. The serving core is connect/extender; this
// lane does not implement activation, the directory, gossip or probes.

const (
	// extenderEnv turns the role on. Off unless the value is on/1/true.
	extenderEnv = "URNETWORK_EXTENDER"
	// extenderDns53Env also binds the dns carrier on 53, beside 4053. The
	// bind is additive and never required, so an unprivileged provider still
	// serves 4053.
	extenderDns53Env = "URNETWORK_EXTENDER_DNS53"
	// extenderKeyFileName is the identity seed of the serving role, alongside
	// the provider's other state. The public half names the extender in every
	// record it signs, so this file must survive restarts unchanged.
	extenderKeyFileName = ".provider.extender.key"
	// extenderTcpRetryInterval is the least time between attempts to take
	// tcp 443 after a failure (G2). A port that is held is usually held for
	// the life of another process; probing faster only fills the log.
	extenderTcpRetryInterval = 3 * time.Minute
)

// Extender role states, as published on the status line and in the
// urnet_extender_state metric. "serving" means every configured carrier is
// listening, "tcp_unavailable" means the udp carriers serve while tcp 443 is
// being retried, and "unavailable" means no carrier bound.
const (
	extenderStateOff            = "off"
	extenderStateStarting       = "starting"
	extenderStateServing        = "serving"
	extenderStateTcpUnavailable = "tcp_unavailable"
	extenderStateUnavailable    = "unavailable"
	extenderStateError          = "error"
	extenderStateClosed         = "closed"
)

var extenderRoleStates = []string{
	extenderStateOff,
	extenderStateStarting,
	extenderStateServing,
	extenderStateTcpUnavailable,
	extenderStateUnavailable,
	extenderStateError,
	extenderStateClosed,
}

// globalExtenderRole is the role of this process, or nil when it was never
// started. It stays set after close so /metrics keeps reporting a stable
// state instead of dropping the series.
var globalExtenderRole atomic.Pointer[extenderRole]

// extenderRoleRequested reports whether the operator asked for the role.
// Only an explicit affirmative starts it; a typo leaves it off rather than
// binding a public port nobody meant to offer.
func extenderRoleRequested(environ func(string) string) bool {
	switch strings.ToLower(strings.TrimSpace(environ(extenderEnv))) {
	case "on", "1", "true":
		return true
	}
	return false
}

// extenderDnsPrivilegedPortRequested reports whether the dns carrier should
// also try 53. It is meaningful only when the role itself is on.
func extenderDnsPrivilegedPortRequested(environ func(string) string) bool {
	switch strings.ToLower(strings.TrimSpace(environ(extenderDns53Env))) {
	case "on", "1", "true":
		return true
	}
	return false
}

// extenderRoleEligible reports whether a slot may hold the role. Only the
// direct slot qualifies: its egress is this host's own address, which is the
// address the role's carriers answer on. A proxied slot's traffic leaves
// through the proxy, so a listener here would advertise an address it cannot
// serve.
func extenderRoleEligible(proxySettings *connect.ProxySettings, directEnabled bool) bool {
	return directEnabled && proxySettings == nil
}

// extenderRoleConfig is the process-facing shape of the role. The defaults
// are the production ports; tests replace the listen seams and ports so they
// can bind loopback without a privileged port.
type extenderRoleConfig struct {
	keyPath           string
	ports             map[int][]connect.ExtenderConnectMode
	dnsPrivilegedPort bool
	retryInterval     time.Duration
	listen            func(network string, address string) (net.Listener, error)
	listenPacket      func(network string, address string) (net.PacketConn, error)
	forwardDialer     *net.Dialer
	// allowedHosts is the forward allow list. Empty refuses every forward
	// request, which is the server's default and upstream's network-space
	// behaviour; an open provider names its hosts here.
	allowedHosts []string
}

// extenderAllowedHostsFromEnv reads the comma-separated forward allow list.
func extenderAllowedHostsFromEnv(getenv func(string) string) []string {
	var allowedHosts []string
	for _, host := range strings.Split(getenv("URNETWORK_EXTENDER_ALLOWED_HOSTS"), ",") {
		host = strings.TrimSpace(strings.ToLower(host))
		if host != "" {
			allowedHosts = append(allowedHosts, host)
		}
	}
	return allowedHosts
}

func defaultExtenderRoleConfig() extenderRoleConfig {
	return extenderRoleConfig{
		ports: map[int][]connect.ExtenderConnectMode{
			connect.ExtenderTcpPort: {
				connect.ExtenderConnectModeTcpTls,
				connect.ExtenderConnectModeQuic,
			},
			connect.ExtenderDnsPort: {
				connect.ExtenderConnectModeDns,
			},
		},
		dnsPrivilegedPort: extenderDnsPrivilegedPortRequested(os.Getenv),
		allowedHosts:      extenderAllowedHostsFromEnv(os.Getenv),
		retryInterval:     extenderTcpRetryInterval,
		listen:            net.Listen,
		listenPacket:      net.ListenPacket,
		// The forward egress is the direct slot's: a dialer with a bounded
		// connect and a keepalive, not the proxied client strategy.
		forwardDialer: &net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		},
	}
}

// loadOrCreateExtenderKeySeed loads the ed25519 seed from path, creating it
// on first use. A file that exists but does not hold exactly one seed is an
// error, never a reason to replace it: the public key is already the identity
// an activation and every signed record name, and silently rotating it would
// strand them.
func loadOrCreateExtenderKeySeed(path string) (seed []byte, created bool, returnErr error) {
	b, err := os.ReadFile(path)
	if err == nil {
		if len(b) != ed25519.SeedSize {
			return nil, false, fmt.Errorf(
				"%s holds %d bytes, want a %d-byte extender seed; refusing to replace it",
				path, len(b), ed25519.SeedSize,
			)
		}
		return b, false, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, false, err
	}
	seed, err = connect.NewExtenderKeySeed()
	if err != nil {
		return nil, false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, false, err
	}
	if err := atomicWriteFile(path, seed, 0600); err != nil {
		return nil, false, err
	}
	// CreateTemp already creates 0600; the chmod makes the guarantee explicit
	// for a state directory whose umask or ACLs are unusual.
	if err := os.Chmod(path, 0600); err != nil {
		return nil, false, err
	}
	return seed, true, nil
}

// extenderKeyIdString is the short stable id of the identity public key, for
// a status line. It is a hint, not a secret.
func extenderKeyIdString(seed []byte) string {
	publicKey, err := connect.ExtenderPublicKeyFromSeed(seed)
	if err != nil {
		return ""
	}
	return hex.EncodeToString(connect.ExtenderKeyId(publicKey))
}

// extenderRole is one running serving role. Its goroutine owns the servers;
// every field read from /metrics or the status path is behind mu.
type extenderRole struct {
	ctx    context.Context
	cancel context.CancelFunc
	config extenderRoleConfig
	keyId  string

	mu             sync.Mutex
	lastStatusLine string
	lastStatusNote string
	readyLogged    bool
	keySeed        []byte
	state          string
	note           string
	carriers       map[string]bool
	listenErrors   map[string]string
	dnsPorts       []int
	servers        []*extender.ExtenderServer
	baseStats      extender.ExtenderStats
	// counts folded from retired servers, so the cumulative metrics do not
	// drop when a server leaves r.servers
	baseServiceCounts map[string]uint64
	baseRefusalCounts map[string]uint64
	tcpRetries        int64

	ready     chan struct{}
	readyOnce sync.Once
	done      chan struct{}
}

// startExtenderRole loads the identity, starts the supervisor and returns
// once the role owns its state. Bind failures are reported asynchronously;
// only an identity or configuration error is returned here.
func startExtenderRole(ctx context.Context, config extenderRoleConfig) (*extenderRole, error) {
	keyPath := config.keyPath
	if keyPath == "" {
		p, err := providerStatePath(extenderKeyFileName)
		if err != nil {
			return nil, err
		}
		keyPath = p
	}
	seed, created, err := loadOrCreateExtenderKeySeed(keyPath)
	if err != nil {
		return nil, err
	}
	if config.retryInterval <= 0 {
		config.retryInterval = extenderTcpRetryInterval
	}
	if config.listen == nil {
		config.listen = net.Listen
	}
	if config.listenPacket == nil {
		config.listenPacket = net.ListenPacket
	}
	if len(config.allowedHosts) == 0 {
		tlog("[extender] no forward hosts configured (URNETWORK_EXTENDER_ALLOWED_HOSTS); every forward request will be refused\n")
	}
	if config.forwardDialer == nil {
		config.forwardDialer = &net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}
	}
	roleCtx, cancel := context.WithCancel(ctx)
	r := &extenderRole{
		ctx:               roleCtx,
		cancel:            cancel,
		config:            config,
		keyId:             extenderKeyIdString(seed),
		state:             extenderStateStarting,
		carriers:          map[string]bool{},
		listenErrors:      map[string]string{},
		baseServiceCounts: map[string]uint64{},
		baseRefusalCounts: map[string]uint64{},
		ready:             make(chan struct{}),
		done:              make(chan struct{}),
	}
	r.setKeySeed(seed)
	if created {
		tlog("[extender] created identity key %s (0600)\n", keyPath)
	}
	globalExtenderRole.Store(r)
	go connect.HandleError(r.run, r.cancel)
	return r, nil
}

// setKeySeed keeps the identity out of the config struct, which the status
// and metrics paths must never be able to print.
func (r *extenderRole) setKeySeed(seed []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.keySeed = seed
}

// Ready closes once the first bind attempt has finished, whether it succeeded
// or not.
func (r *extenderRole) Ready() <-chan struct{} {
	return r.ready
}

// Done closes when the supervisor has stopped and every server is closed.
func (r *extenderRole) Done() <-chan struct{} {
	return r.done
}

// CloseAndWait cancels the role and joins its supervisor. Safe to call more
// than once and from the provider's shutdown path.
func (r *extenderRole) CloseAndWait() {
	r.cancel()
	<-r.done
}

func (r *extenderRole) run() {
	defer close(r.done)
	defer r.shutdown()

	for {
		if r.ctx.Err() != nil {
			return
		}
		primary, primaryErr, err := r.startPrimary()
		if err != nil {
			r.setError(err)
			r.markReady()
			tlog("[extender] role failed: %v; retrying no faster than %s\n", err, r.config.retryInterval)
			if !r.waitRetry() {
				return
			}
			continue
		}
		if primary == nil {
			// No carrier bound. Nothing is left to watch, so wait out the
			// retry interval and try the whole role again.
			r.markReady()
			if !r.waitRetry() {
				return
			}
			continue
		}

		r.mu.Lock()
		r.servers = append(r.servers, primary)
		r.refreshLocked()
		r.mu.Unlock()
		r.markReady()
		r.publishStatus("listening")

		// tcp 443 may have failed while udp is serving. Retry it on its own
		// server so a late tcp bind never interrupts the udp carriers.
		tcpTimer := time.NewTimer(r.config.retryInterval)
		tcpRetryC := tcpTimer.C
		if r.tcpBound() {
			if !tcpTimer.Stop() {
				<-tcpTimer.C
			}
			tcpRetryC = nil
		}
		var tcpServer *extender.ExtenderServer
		var tcpErr chan error

	inner:
		for {
			select {
			case <-r.ctx.Done():
				tcpTimer.Stop()
				return
			case <-tcpRetryC:
				if !r.tcpBound() {
					var errCh chan error
					tcpServer, errCh = r.startTcpRetry()
					if tcpServer != nil {
						tcpErr = errCh
						r.mu.Lock()
						r.servers = append(r.servers, tcpServer)
						r.refreshLocked()
						r.mu.Unlock()
						r.publishStatus("tcp carrier recovered")
					}
				}
				tcpTimer.Reset(r.config.retryInterval)
			case err := <-tcpErr:
				if tcpServer != nil {
					r.retireServer(tcpServer)
					tcpServer = nil
				}
				tcpErr = nil
				r.mu.Lock()
				r.note = fmt.Sprintf("tcp carrier ended: %v", err)
				r.mu.Unlock()
				r.publishStatus("tcp carrier ended")
				tcpTimer.Reset(r.config.retryInterval)
			case err := <-primaryErr:
				tcpTimer.Stop()
				r.retireServer(primary)
				// the recovered tcp carrier holds 443 too: retiring only the
				// primary would leave it orphaned and the restart in EADDRINUSE
				if tcpServer != nil {
					r.retireServer(tcpServer)
					tcpServer = nil
					tcpErr = nil
				}
				r.mu.Lock()
				r.note = fmt.Sprintf("primary serve ended: %v", err)
				r.mu.Unlock()
				r.publishStatus("primary ended")
				if !r.waitRetry() {
					return
				}
				break inner
			}
		}
	}
}

// startPrimary constructs and starts the server that owns every carrier. It
// tries tcp 443 first, by binding it here and handing the open listener to
// the core through its Listen seam, so the operator-visible result of the
// gating carrier is known before the udp carriers are attempted. A tcp
// failure starts the udp-only server instead and does not fail the role.
func (r *extenderRole) startPrimary() (*extender.ExtenderServer, chan error, error) {
	held, tcpErr := r.config.listen("tcp", fmt.Sprintf(":%d", connect.ExtenderTcpPort))
	if tcpErr != nil {
		if held != nil {
			held.Close()
		}
		r.recordListenError(connect.ExtenderCarrierTcp, tcpErr)
		held = nil
	}
	ports := r.ports(held != nil)
	if len(ports) == 0 {
		return nil, nil, fmt.Errorf("no extender carrier is configured")
	}
	server, err := r.newServer(ports, held)
	if err != nil {
		if held != nil {
			held.Close()
		}
		return nil, nil, err
	}
	// Wait until every carrier bind has been attempted, so the role state and
	// the status line describe a complete carrier set.
	errCh := r.serve(server)
	select {
	case <-server.Listening():
		return server, errCh, nil
	case err := <-errCh:
		server.CloseAndWait()
		if err == nil {
			err = errors.New("extender server stopped before binding")
		}
		return nil, nil, err
	case <-r.ctx.Done():
		server.CloseAndWait()
		return nil, nil, nil
	}
}

// startTcpRetry binds tcp 443 again and serves it on its own server. Called
// only from the retry timer, so attempts are at least retryInterval apart.
func (r *extenderRole) startTcpRetry() (*extender.ExtenderServer, chan error) {
	r.mu.Lock()
	r.tcpRetries += 1
	r.mu.Unlock()

	held, err := r.config.listen("tcp", fmt.Sprintf(":%d", connect.ExtenderTcpPort))
	if err != nil {
		if held != nil {
			held.Close()
		}
		r.recordListenError(connect.ExtenderCarrierTcp, err)
		return nil, nil
	}
	server, err := r.newServer(map[int][]connect.ExtenderConnectMode{
		connect.ExtenderTcpPort: {connect.ExtenderConnectModeTcpTls},
	}, held)
	if err != nil {
		held.Close()
		r.mu.Lock()
		r.note = fmt.Sprintf("tcp retry server: %v", err)
		r.mu.Unlock()
		return nil, nil
	}
	errCh := make(chan error, 1)
	go connect.HandleError(func() { errCh <- server.ListenAndServe() }, r.cancel)
	select {
	case <-server.Listening():
		return server, errCh
	case err := <-errCh:
		server.CloseAndWait()
		if err != nil {
			r.mu.Lock()
			r.note = fmt.Sprintf("tcp retry server ended: %v", err)
			r.mu.Unlock()
		}
		return nil, nil
	case <-r.ctx.Done():
		server.CloseAndWait()
		return nil, nil
	}
}

// ports returns the carrier map for one server. Without a held tcp listener
// the tcp mode is removed and udp 443 plus dns 4053 remain.
func (r *extenderRole) ports(includeTcp bool) map[int][]connect.ExtenderConnectMode {
	ports := map[int][]connect.ExtenderConnectMode{}
	for port, modes := range r.config.ports {
		kept := make([]connect.ExtenderConnectMode, 0, len(modes))
		for _, mode := range modes {
			if !includeTcp && mode == connect.ExtenderConnectModeTcpTls {
				continue
			}
			kept = append(kept, mode)
		}
		if 0 < len(kept) {
			ports[port] = kept
		}
	}
	return ports
}

// newServer builds one core server. held, when not nil, is the already-bound
// tcp 443 listener the server takes ownership of on its first Listen call.
func (r *extenderRole) newServer(ports map[int][]connect.ExtenderConnectMode, held net.Listener) (*extender.ExtenderServer, error) {
	r.mu.Lock()
	seed := slices.Clone(r.keySeed)
	r.mu.Unlock()
	if len(seed) == 0 {
		return nil, errors.New("extender identity seed is missing")
	}

	settings := extender.DefaultExtenderSettings()
	settings.IdentityKeySeed = seed
	settings.DnsPrivilegedPort = r.config.dnsPrivilegedPort
	settings.ListenErrorHandler = r.recordListenError
	settings.ListenPacket = r.config.listenPacket
	settings.Listen = func(network string, address string) (net.Listener, error) {
		if held == nil {
			return nil, fmt.Errorf("extender: no held listener for %s %s", network, address)
		}
		listener := held
		held = nil
		return listener, nil
	}
	return extender.NewExtenderServer(
		r.ctx,
		nil,
		r.config.allowedHosts,
		ports,
		r.config.forwardDialer,
		settings,
	), nil
}

// serve starts the server goroutine and returns the channel its error arrives
// on. The caller registers the server once Listening has closed.
func (r *extenderRole) serve(server *extender.ExtenderServer) chan error {
	errCh := make(chan error, 1)
	go connect.HandleError(func() { errCh <- server.ListenAndServe() }, r.cancel)
	return errCh
}

func (r *extenderRole) recordListenError(carrier string, err error) {
	message := err.Error()
	r.mu.Lock()
	previous := r.listenErrors[carrier]
	r.listenErrors[carrier] = message
	r.mu.Unlock()
	if previous == message {
		return
	}
	// a bind below 1024 needs a capability: say so once, because the bare
	// permission error reads like a bug rather than a deployment choice
	hint := ""
	if errors.Is(err, fs.ErrPermission) {
		hint = "; port 443 needs CAP_NET_BIND_SERVICE (setcap cap_net_bind_service=+ep <binary>) or root"
	}
	if carrier == connect.ExtenderCarrierTcp {
		tlog("[extender] tcp 443 unavailable: %v; retrying no faster than %s%s\n", err, r.config.retryInterval, hint)
		return
	}
	if carrier == connect.ExtenderCarrierDns && strings.Contains(message, ":53") {
		// 53 is the optional second dns port. Its failure never removes the
		// carrier: 4053 is bound and serves.
		tlog("[extender] dns 53 unavailable: %v; keeping %d\n", err, connect.ExtenderDnsPort)
		return
	}
	tlog("[extender] %s carrier unavailable: %v%s\n", carrier, err, hint)
}

func (r *extenderRole) retireServer(server *extender.ExtenderServer) {
	// the counters are atomic and survive close, so sample after the drain:
	// requests that finish while CloseAndWait runs must count
	server.CloseAndWait()
	stats := server.Stats()
	serviceCounts := server.ServiceCounts()
	refusalCounts := server.RefusalCounts()
	r.mu.Lock()
	defer r.mu.Unlock()
	index := slices.Index(r.servers, server)
	if index < 0 {
		// already retired: folding the counts again would double count
		return
	}
	r.servers = slices.Delete(r.servers, index, index+1)
	r.baseStats.IngressByteCount += stats.IngressByteCount
	r.baseStats.IngressReadCount += stats.IngressReadCount
	r.baseStats.EgressByteCount += stats.EgressByteCount
	r.baseStats.EgressReadCount += stats.EgressReadCount
	for service, count := range serviceCounts {
		r.baseServiceCounts[service] += count
	}
	for stage, count := range refusalCounts {
		r.baseRefusalCounts[stage] += count
	}
	r.refreshLocked()
}

func (r *extenderRole) refreshLocked() {
	carriers := map[string]bool{}
	dnsPorts := map[int]bool{}
	// The pre-bound tcp failure never reaches the core's own error map, so
	// keep the role-level entries and merge the servers' over them.
	listenErrors := maps.Clone(r.listenErrors)
	for _, server := range r.servers {
		for _, carrier := range server.Carriers() {
			carriers[carrier] = true
		}
		for _, port := range server.DnsPorts() {
			dnsPorts[port] = true
		}
		for carrier, err := range server.ListenErrors() {
			listenErrors[carrier] = err.Error()
		}
	}
	for carrier := range carriers {
		delete(listenErrors, carrier)
	}
	r.carriers = carriers
	r.listenErrors = listenErrors
	r.dnsPorts = slices.Sorted(maps.Keys(dnsPorts))
	switch {
	case len(r.servers) == 0 || len(carriers) == 0:
		if r.state != extenderStateError {
			r.state = extenderStateUnavailable
		}
	case !carriers[connect.ExtenderCarrierTcp]:
		r.state = extenderStateTcpUnavailable
	default:
		r.state = extenderStateServing
	}
}

func (r *extenderRole) tcpBound() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.carriers[connect.ExtenderCarrierTcp]
}

func (r *extenderRole) setError(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.state = extenderStateError
	r.note = err.Error()
}

func (r *extenderRole) markReady() {
	r.readyOnce.Do(func() { close(r.ready) })
}

// waitRetry waits one retry interval, returning false when the role is
// shutting down.
func (r *extenderRole) waitRetry() bool {
	timer := time.NewTimer(r.config.retryInterval)
	defer timer.Stop()
	select {
	case <-r.ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (r *extenderRole) shutdown() {
	r.mu.Lock()
	servers := slices.Clone(r.servers)
	r.servers = nil
	r.mu.Unlock()
	// Capture the last counters before closing, so a scrape after shutdown
	// still reports the traffic the role relayed rather than dropping to the
	// pre-shutdown base.
	stats := extender.ExtenderStats{}
	for _, server := range servers {
		serverStats := server.Stats()
		stats.IngressByteCount += serverStats.IngressByteCount
		stats.IngressReadCount += serverStats.IngressReadCount
		stats.EgressByteCount += serverStats.EgressByteCount
		stats.EgressReadCount += serverStats.EgressReadCount
		server.CloseAndWait()
	}
	r.mu.Lock()
	r.baseStats.IngressByteCount += stats.IngressByteCount
	r.baseStats.IngressReadCount += stats.IngressReadCount
	r.baseStats.EgressByteCount += stats.EgressByteCount
	r.baseStats.EgressReadCount += stats.EgressReadCount
	r.carriers = map[string]bool{}
	r.dnsPorts = nil
	r.state = extenderStateClosed
	r.mu.Unlock()
	r.markReady()
	r.publishStatus("stopped")
}

// extenderRoleSnapshot is the lock-free view of the role for the status line
// and /metrics. Relay counters are summed over every server still live plus
// the totals of servers already retired.
type extenderRoleSnapshot struct {
	Enabled      bool
	State        string
	Note         string
	Carriers     []string
	ListenErrors map[string]string
	DnsPorts     []int
	Connections  int
	Stats        extender.ExtenderStats
	TcpRetries   int64
	// per-service requests and per-stage refusals, summed over every server
	// the role currently owns
	ServiceCounts map[string]uint64
	RefusalCounts map[string]uint64
}

func (r *extenderRole) snapshot() extenderRoleSnapshot {
	r.mu.Lock()
	servers := slices.Clone(r.servers)
	baseServiceCounts := maps.Clone(r.baseServiceCounts)
	baseRefusalCounts := maps.Clone(r.baseRefusalCounts)
	snapshot := extenderRoleSnapshot{
		Enabled:      true,
		State:        r.state,
		Note:         r.note,
		ListenErrors: maps.Clone(r.listenErrors),
		DnsPorts:     slices.Clone(r.dnsPorts),
		Stats:        r.baseStats,
		TcpRetries:   r.tcpRetries,
	}
	for carrier := range r.carriers {
		snapshot.Carriers = append(snapshot.Carriers, carrier)
	}
	slices.Sort(snapshot.Carriers)
	r.mu.Unlock()

	// retired servers' counts are folded into the base maps, so the cumulative
	// counters keep their totals when a server leaves
	snapshot.ServiceCounts = baseServiceCounts
	snapshot.RefusalCounts = baseRefusalCounts
	if snapshot.ServiceCounts == nil {
		snapshot.ServiceCounts = map[string]uint64{}
	}
	if snapshot.RefusalCounts == nil {
		snapshot.RefusalCounts = map[string]uint64{}
	}
	for _, server := range servers {
		for service, count := range server.ServiceCounts() {
			snapshot.ServiceCounts[service] += count
		}
		for stage, count := range server.RefusalCounts() {
			snapshot.RefusalCounts[stage] += count
		}
		stats := server.Stats()
		snapshot.Stats.IngressByteCount += stats.IngressByteCount
		snapshot.Stats.IngressReadCount += stats.IngressReadCount
		snapshot.Stats.EgressByteCount += stats.EgressByteCount
		snapshot.Stats.EgressReadCount += stats.EgressReadCount
		snapshot.Connections += server.ConnectionCount()
	}
	return snapshot
}

func (r *extenderRole) publishStatus(note string) {
	snapshot := r.snapshot()
	summary := extenderRoleSummary(snapshot)
	policy := r.forwardPolicySummary()

	r.mu.Lock()
	changed := summary != r.lastStatusLine || note != r.lastStatusNote
	firstServing := snapshot.State == extenderStateServing && !r.readyLogged
	if changed {
		r.lastStatusLine = summary
		r.lastStatusNote = note
	}
	if firstServing {
		r.readyLogged = true
	}
	r.mu.Unlock()

	// one line a human can read at 3am, then only when something changes:
	// the counters live in /metrics, not in a repeated dump
	if firstServing {
		tlog("[extender] ready — %s; %s\n", summary, policy)
		return
	}
	if !changed {
		return
	}
	if snapshot.State == extenderStateServing {
		tlog("[extender] %s\n", summary)
		return
	}
	if note == "" {
		note = summary
	}
	tlog("[extender] %s — %s\n", snapshot.State, note)
}

// extenderRoleSummary says what the role is serving in words, not keys.
func extenderRoleSummary(snapshot extenderRoleSnapshot) string {
	up := make([]string, 0, 3)
	down := make([]string, 0, 3)
	for _, carrier := range []string{
		connect.ExtenderCarrierTcp,
		connect.ExtenderCarrierQuic,
		connect.ExtenderCarrierDns,
	} {
		if slices.Contains(snapshot.Carriers, carrier) {
			up = append(up, carrier)
			continue
		}
		if _, failed := snapshot.ListenErrors[carrier]; failed {
			down = append(down, carrier)
		}
	}
	switch {
	case len(up) == 0:
		return "not serving: no carrier is listening"
	case len(down) == 0:
		return "serving on " + englishJoin(up)
	default:
		return "serving on " + englishJoin(up) + "; " + englishJoin(down) + " unavailable"
	}
}

func (r *extenderRole) forwardPolicySummary() string {
	if len(r.config.allowedHosts) == 0 {
		return "forwards are disabled: set URNETWORK_EXTENDER_ALLOWED_HOSTS to the hosts clients may ask for"
	}
	if len(r.config.allowedHosts) == 1 {
		return "forwards allowed for 1 host"
	}
	return fmt.Sprintf("forwards allowed for %d hosts", len(r.config.allowedHosts))
}

// englishJoin renders ["a","b","c"] as "a, b and c".
func englishJoin(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	case 2:
		return items[0] + " and " + items[1]
	default:
		return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
	}
}

// extenderRoleMetricsText renders the role for connect.PrometheusHandler.
// Every series is emitted even before the role starts, so a dashboard sees a
// stable zero rather than a gap.
func extenderRoleMetricsText() string {
	snapshot := extenderRoleSnapshot{State: extenderStateOff}
	if role := globalExtenderRole.Load(); role != nil {
		snapshot = role.snapshot()
	}
	return renderExtenderRoleMetrics(snapshot)
}

func renderExtenderRoleMetrics(snapshot extenderRoleSnapshot) string {
	enabled := 0
	if snapshot.Enabled {
		enabled = 1
	}
	state := snapshot.State
	if state == "" {
		state = extenderStateOff
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# HELP urnet_extender_enabled 1 when this provider runs the extender serving role.\n")
	fmt.Fprintf(&b, "# TYPE urnet_extender_enabled gauge\n")
	fmt.Fprintf(&b, "urnet_extender_enabled %d\n", enabled)
	fmt.Fprintf(&b, "# HELP urnet_extender_state Extender role state, one series per state.\n")
	fmt.Fprintf(&b, "# TYPE urnet_extender_state gauge\n")
	for _, candidate := range extenderRoleStates {
		value := 0
		if candidate == state {
			value = 1
		}
		fmt.Fprintf(&b, "urnet_extender_state{state=%s} %d\n", connect.PrometheusLabelValue(candidate), value)
	}
	fmt.Fprintf(&b, "# HELP urnet_extender_carrier_listening 1 when the carrier is listening.\n")
	fmt.Fprintf(&b, "# TYPE urnet_extender_carrier_listening gauge\n")
	for _, carrier := range []string{
		connect.ExtenderCarrierTcp,
		connect.ExtenderCarrierQuic,
		connect.ExtenderCarrierDns,
	} {
		value := 0
		if slices.Contains(snapshot.Carriers, carrier) {
			value = 1
		}
		fmt.Fprintf(&b, "urnet_extender_carrier_listening{carrier=%s} %d\n", connect.PrometheusLabelValue(carrier), value)
	}
	fmt.Fprintf(&b, "# HELP urnet_extender_requests_total Extender requests by service, cumulative.\n")
	fmt.Fprintf(&b, "# TYPE urnet_extender_requests_total counter\n")
	for _, service := range []string{"proxy", "forward", "gossip", "feed", "probe", "other"} {
		fmt.Fprintf(&b, "urnet_extender_requests_total{service=%s} %d\n",
			connect.PrometheusLabelValue(service), snapshot.ServiceCounts[service])
	}
	fmt.Fprintf(&b, "# HELP urnet_extender_refused_total Extender requests refused, by stage, cumulative.\n")
	fmt.Fprintf(&b, "# TYPE urnet_extender_refused_total counter\n")
	refusalStages := make([]string, 0, len(snapshot.RefusalCounts))
	for stage := range snapshot.RefusalCounts {
		refusalStages = append(refusalStages, stage)
	}
	slices.Sort(refusalStages)
	for _, stage := range refusalStages {
		fmt.Fprintf(&b, "urnet_extender_refused_total{stage=%s} %d\n",
			connect.PrometheusLabelValue(stage), snapshot.RefusalCounts[stage])
	}
	fmt.Fprintf(&b, "# HELP urnet_extender_carrier_listen_error 1 when the carrier's last bind failed.\n")
	fmt.Fprintf(&b, "# TYPE urnet_extender_carrier_listen_error gauge\n")
	for _, carrier := range []string{
		connect.ExtenderCarrierTcp,
		connect.ExtenderCarrierQuic,
		connect.ExtenderCarrierDns,
	} {
		value := 0
		if 0 < len(snapshot.ListenErrors[carrier]) {
			value = 1
		}
		fmt.Fprintf(&b, "urnet_extender_carrier_listen_error{carrier=%s} %d\n", connect.PrometheusLabelValue(carrier), value)
	}
	fmt.Fprintf(&b, "# HELP urnet_extender_connections_active Connections open over every extender carrier.\n")
	fmt.Fprintf(&b, "# TYPE urnet_extender_connections_active gauge\n")
	fmt.Fprintf(&b, "urnet_extender_connections_active %d\n", snapshot.Connections)
	fmt.Fprintf(&b, "# HELP urnet_extender_relay_bytes_total Bytes the extender relayed, by direction.\n")
	fmt.Fprintf(&b, "# TYPE urnet_extender_relay_bytes_total counter\n")
	fmt.Fprintf(&b, "urnet_extender_relay_bytes_total{dir=\"ingress\"} %d\n", snapshot.Stats.IngressByteCount)
	fmt.Fprintf(&b, "urnet_extender_relay_bytes_total{dir=\"egress\"} %d\n", snapshot.Stats.EgressByteCount)
	fmt.Fprintf(&b, "# HELP urnet_extender_tcp_retry_total Attempts to take tcp 443 after a bind failure.\n")
	fmt.Fprintf(&b, "# TYPE urnet_extender_tcp_retry_total counter\n")
	fmt.Fprintf(&b, "urnet_extender_tcp_retry_total %d\n", snapshot.TcpRetries)
	return b.String()
}
