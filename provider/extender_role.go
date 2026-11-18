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
	// extenderEnv is the optional environment default for the role: off/0/false
	// turns it off. The control setting (urnet-tools extender) beats it.
	extenderEnv = "URNETWORK_EXTENDER"
	// extenderDns53Env controls the second dns port. Like the miner, the role
	// also tries 53 beside 4053 by default; off/0/false keeps it to 4053. The
	// bind is additive and never required, so a provider that cannot bind 53
	// (no capability, or another resolver holds it) still serves 4053.
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
// listening, "tcp_unavailable" means tcp 443 cannot be bound, so nothing is open and it is
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

// extenderDnsPrivilegedPortRequested reports whether the dns carrier should
// also try 53: yes unless it is switched off. It is meaningful only when the
// role itself is on.
func extenderDnsPrivilegedPortRequested(environ func(string) string) bool {
	switch strings.ToLower(strings.TrimSpace(environ(extenderDns53Env))) {
	case "off", "0", "false", "no":
		return false
	}
	return true
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
	// disabledCarriers records settings captured for this role's lifetime.
	disabledCarriers map[string]bool
	retryInterval    time.Duration
	listen           func(network string, address string) (net.Listener, error)
	listenPacket     func(network string, address string) (net.PacketConn, error)
	forwardDialer    *net.Dialer
	// allowedHosts is the forward allow list. Empty refuses every forward
	// request, which is the server's default and upstream's network-space
	// behaviour; an open provider names its hosts here.
	allowedHosts []string
	// configureActivator, when set, adjusts the activation settings before the
	// loop starts. Tests use it to pin the address families and shorten the
	// backoff; production leaves it nil.
	configureActivator func(*connect.ExtenderActivatorSettings)
	// fetchHello, when set, replaces the GET of the platform's /hello (tests)
	fetchHello func(ctx context.Context, inputs *extenderActivationInputs) ([]byte, error)
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
	config := extenderRoleConfig{
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
	config.disabledCarriers = make(map[string]bool)
	for _, item := range []struct{ key, name string }{
		{"extender_udp443", "udp 443"}, {"extender_udp4053", "dns 4053"}, {"extender_udp53", "dns 53"},
	} {
		if value, ok := globalControlState.get(item.key); ok && strings.EqualFold(value, "off") {
			config.disabledCarriers[item.name] = true
		}
	}
	if config.disabledCarriers["udp 443"] {
		config.ports[connect.ExtenderTcpPort] = []connect.ExtenderConnectMode{connect.ExtenderConnectModeTcpTls}
	}
	if config.disabledCarriers["dns 4053"] {
		delete(config.ports, connect.ExtenderDnsPort)
	}
	if config.disabledCarriers["dns 53"] {
		config.dnsPrivilegedPort = false
	}
	// An explicit 53 mode is needed when there is no 4053 mode to widen.
	if config.dnsPrivilegedPort && config.disabledCarriers["dns 4053"] {
		config.ports[connect.DefaultDnsPort] = []connect.ExtenderConnectMode{connect.ExtenderConnectModeDns}
	}
	return config
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
	baseServedCounts  map[string]uint64
	// tcpTried is set once tcp 443 has been attempted, so later failed attempts count as retries
	tcpTried bool
	// servers taken out of r.servers by shutdown whose counters are not yet folded
	closing                  []*extender.ExtenderServer
	baseRefusalCounts        map[string]uint64
	baseCarrierServedCounts  map[string]uint64
	baseCarrierRefusalCounts map[string]map[string]uint64
	// the cumulative per-carrier relayed bytes of retired servers, so the byte
	// totals survive when a server leaves the live set
	baseCarrierByteCounts map[string]extender.ExtenderCarrierByteStats
	tcpRetries            int64

	// the activation loop, attached late by the direct slot (extender_activation.go)
	activator       *connect.ExtenderActivator
	activatorInputs *extenderActivationInputs
	// serializes attach, detach and replacement of the activation (extender_activation.go)
	activationMu sync.Mutex
	// directory holds the platform's root keys, which verify the signed record each
	// activation returns so renewals follow its lifetime. One per role, shared by
	// every slot's activation.
	directoryOnce sync.Once
	directory     *connect.ExtenderDirectory
	// discovery serves the directory feed and takes part in the gossip mesh
	// (extender_discovery.go). The directory and network host it needs are
	// set once the platform's keys are read, under mu.
	discovery            *extenderDiscovery
	discoveryDirectory   *connect.ExtenderDirectory
	discoveryNetworkHost string
	// the carrier set last reported to the activation, so a change is told to it once
	carrierSigKnown bool
	carrierSig      string
	// when the "tcp 443 is still held" reminder was last written
	lastTcpReminder time.Time

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
		tlog("🌉⚠️ [extender] no forward hosts configured (URNETWORK_EXTENDER_ALLOWED_HOSTS); every forward request will be refused\n")
	}
	if config.forwardDialer == nil {
		config.forwardDialer = &net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}
	}
	roleCtx, cancel := context.WithCancel(ctx)
	r := &extenderRole{
		ctx:                      roleCtx,
		cancel:                   cancel,
		config:                   config,
		keyId:                    extenderKeyIdString(seed),
		state:                    extenderStateStarting,
		carriers:                 map[string]bool{},
		listenErrors:             map[string]string{},
		baseServiceCounts:        map[string]uint64{},
		baseServedCounts:         map[string]uint64{},
		baseRefusalCounts:        map[string]uint64{},
		baseCarrierServedCounts:  map[string]uint64{},
		baseCarrierRefusalCounts: map[string]map[string]uint64{},
		baseCarrierByteCounts:    map[string]extender.ExtenderCarrierByteStats{},
		ready:                    make(chan struct{}),
		done:                     make(chan struct{}),
	}
	r.setKeySeed(seed)
	r.discovery = newExtenderDiscovery(roleCtx, func() { r.reconcileDiscovery() })
	if created {
		tlog("🌉 [extender] created identity key %s (0600)\n", keyPath)
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
	defer globalExtenderRole.CompareAndSwap(r, nil)
	defer r.shutdown()

	for {
		if r.ctx.Err() != nil {
			return
		}
		primary, primaryErr, err := r.startPrimary()
		if err != nil {
			r.setError(err)
			r.markReady()
			tlog("🌉❌ [extender] role failed: %v; retrying no faster than %s\n", err, r.config.retryInterval)
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
		r.reconcileDiscovery()
		r.markReady()
		r.publishStatus("listening")

		// The primary server only exists once tcp 443 was bound. If that carrier is
		// lost later while the role runs, retry it on its own server so rebinding it
		// never interrupts the udp carriers.
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
						r.reconcileDiscovery()
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
// binds tcp 443 first, the one port the platform requires (as upstream does),
// and hands the open listener to the core through its Listen seam. While that
// bind fails nothing else is bound: the host cannot be listed, so a udp or dns
// port would only be exposed for no client. It returns no server then, and the
// run loop tries again after the retry interval.
func (r *extenderRole) startPrimary() (*extender.ExtenderServer, chan error, error) {
	held, tcpErr := r.config.listen("tcp", fmt.Sprintf(":%d", connect.ExtenderTcpPort))
	if tcpErr != nil {
		if held != nil {
			held.Close()
		}
		r.recordListenError(connect.ExtenderCarrierTcp, tcpErr)
		r.mu.Lock()
		retried := r.tcpTried
		r.tcpTried = true
		if retried {
			r.tcpRetries += 1
		}
		r.carriers = map[string]bool{}
		r.dnsPorts = nil
		if r.state != extenderStateError {
			r.state = extenderStateTcpUnavailable
		}
		r.mu.Unlock()
		if retried {
			r.remindTcpStillHeld(tcpErr)
		}
		return nil, nil, nil
	}
	r.mu.Lock()
	r.tcpTried = true
	r.mu.Unlock()
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
		r.remindTcpStillHeld(err)
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

// extenderTcpReminderInterval is how often a still-blocked tcp 443 is repeated
// in the log. The first failure is explained in full and the bind is retried
// every few minutes; repeating every retry would bury the log.
const extenderTcpReminderInterval = 30 * time.Minute

// remindTcpStillHeld writes a short reminder, at most every
// extenderTcpReminderInterval, that tcp 443 is still blocked, how many tries
// have failed and when the next one is.
func (r *extenderRole) remindTcpStillHeld(err error) {
	r.mu.Lock()
	if !r.lastTcpReminder.IsZero() && time.Since(r.lastTcpReminder) < extenderTcpReminderInterval {
		r.mu.Unlock()
		return
	}
	r.lastTcpReminder = time.Now()
	tries := r.tcpRetries
	r.mu.Unlock()
	who := ""
	if holder := tcpPortHolder(fmt.Sprint(connect.ExtenderTcpPort)); holder != "" {
		who = " (held by " + holder + ")"
	}
	importantLogf("🌉❌ [extender] STILL BLOCKED: tcp 443 is unavailable%s after %d retries (%v). This host is not listed and gets no clients until it is freed. Next attempt at %s.\n",
		who, tries, err, clockLabel(time.Now().Add(r.config.retryInterval)))
}

// notifyActivatorOfCarriers tells the activation loop when the set of bound
// carriers changed: the return of tcp 443 wakes the families that were held for
// it, any other change makes the next pass re-offer the carriers. Called after
// the role refreshed its carriers, outside r.mu.
func (r *extenderRole) notifyActivatorOfCarriers(carriers []string) {
	signature := strings.Join(carriers, ",")
	r.mu.Lock()
	known := r.carrierSigKnown
	previous := r.carrierSig
	r.carrierSigKnown = true
	r.carrierSig = signature
	activator := r.activator
	r.mu.Unlock()
	if activator == nil || !known || previous == signature {
		return
	}
	hadTcp := strings.Contains(previous, connect.ExtenderCarrierTcp)
	hasTcp := slices.Contains(carriers, connect.ExtenderCarrierTcp)
	if hasTcp && !hadTcp {
		importantLogf("🌉✅ [extender] tcp 443 is bound again: asking the platform to list this host now.\n")
		activator.TcpCarrierReturned()
		return
	}
	activator.CarriersChanged()
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
		who := ""
		if holder := tcpPortHolder(fmt.Sprint(connect.ExtenderTcpPort)); holder != "" {
			who = " Port 443 is held by " + holder + "."
		}
		importantLogf("🌉❌ [extender] CANNOT USE tcp 443: %v.%s%s "+
			"The platform proves an extender by dialing back to it on tcp 443, so while that port is taken this host will NOT be listed "+
			"and no clients will be sent to it, so every other extender port stays closed until tcp 443 can be bound. "+
			"Free the port (stop or move whatever holds it). The bind is retried every %s; next attempt at %s.\n",
			err, who, hint, r.config.retryInterval, clockLabel(time.Now().Add(r.config.retryInterval)))
		r.mu.Lock()
		r.lastTcpReminder = time.Now()
		r.mu.Unlock()
		return
	}
	if carrier == connect.ExtenderCarrierDns && strings.Contains(message, ":53") {
		// 53 is the optional second dns port. Its failure never removes the
		// carrier: 4053 is bound and serves.
		tlog("🌉⚠️ [extender] dns 53 unavailable: %v; keeping %d\n", err, connect.ExtenderDnsPort)
		return
	}
	tlog("🌉❌ [extender] %s carrier unavailable: %v%s\n", carrier, err, hint)
}

// foldCountsLocked adds a retired or closed server's counters to the role's base
// so the cumulative totals do not drop when it leaves r.servers. One helper for
// every way a server leaves, so none of them can forget a counter.
func (r *extenderRole) foldCountsLocked(
	stats extender.ExtenderStats,
	serviceCounts, servedCounts, refusalCounts map[string]uint64,
	carrierServedCounts map[string]uint64,
	carrierRefusalCounts map[string]map[string]uint64,
	carrierByteCounts map[string]extender.ExtenderCarrierByteStats,
) {
	r.baseStats.IngressByteCount += stats.IngressByteCount
	r.baseStats.IngressReadCount += stats.IngressReadCount
	r.baseStats.EgressByteCount += stats.EgressByteCount
	r.baseStats.EgressReadCount += stats.EgressReadCount
	for service, count := range serviceCounts {
		r.baseServiceCounts[service] += count
	}
	for service, count := range servedCounts {
		r.baseServedCounts[service] += count
	}
	for stage, count := range refusalCounts {
		r.baseRefusalCounts[stage] += count
	}
	if r.baseCarrierServedCounts == nil {
		r.baseCarrierServedCounts = map[string]uint64{}
	}
	for carrier, count := range carrierServedCounts {
		r.baseCarrierServedCounts[carrier] += count
	}
	if r.baseCarrierRefusalCounts == nil {
		r.baseCarrierRefusalCounts = map[string]map[string]uint64{}
	}
	for carrier, stages := range carrierRefusalCounts {
		dst := r.baseCarrierRefusalCounts[carrier]
		if dst == nil {
			dst = map[string]uint64{}
			r.baseCarrierRefusalCounts[carrier] = dst
		}
		for stage, count := range stages {
			dst[stage] += count
		}
	}
	if r.baseCarrierByteCounts == nil {
		r.baseCarrierByteCounts = map[string]extender.ExtenderCarrierByteStats{}
	}
	for carrier, byteStats := range carrierByteCounts {
		base := r.baseCarrierByteCounts[carrier]
		base.Carrier = carrier
		base.IngressBytes += byteStats.IngressBytes
		base.EgressBytes += byteStats.EgressBytes
		base.ConnsTotal += byteStats.ConnsTotal
		r.baseCarrierByteCounts[carrier] = base
	}
}

func (r *extenderRole) retireServer(server *extender.ExtenderServer) {
	// the counters are atomic and survive close, so sample after the drain:
	// requests that finish while CloseAndWait runs must count
	server.CloseAndWait()
	stats := server.Stats()
	serviceCounts := server.ServiceCounts()
	servedCounts := server.ServedCounts()
	refusalCounts := server.RefusalCounts()
	carrierServedCounts := server.CarrierServedCounts()
	carrierRefusalCounts := server.CarrierRefusalCounts()
	carrierByteCounts := server.CarrierByteStats(time.Now())
	r.mu.Lock()
	defer r.mu.Unlock()
	index := slices.Index(r.servers, server)
	if index < 0 {
		// already retired: folding the counts again would double count
		return
	}
	r.servers = slices.Delete(r.servers, index, index+1)
	r.foldCountsLocked(stats, serviceCounts, servedCounts, refusalCounts, carrierServedCounts, carrierRefusalCounts, carrierByteCounts)
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
			if listenErrors[connect.ExtenderCarrierTcp] != "" {
				r.state = extenderStateTcpUnavailable
			} else {
				r.state = extenderStateUnavailable
			}
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
	// detach the discovery handlers and drain their streams before the
	// servers close
	if r.discovery != nil {
		r.discovery.stop()
	}
	r.mu.Lock()
	servers := slices.Clone(r.servers)
	r.servers = nil
	// still visible to snapshots until folded, so the totals never dip
	r.closing = append(r.closing, servers...)
	activator := r.activator
	r.activator = nil
	r.activatorInputs = nil
	r.mu.Unlock()
	// closing joins the activation loop, whose carrier closures take r.mu
	if activator != nil {
		activator.Close()
	}
	// Close every server first, then read its counters: they are atomic and
	// survive close, and requests that finish while CloseAndWait drains must
	// count. Reading before the close lost them, and the service and refusal
	// counts were never folded at all, so a stopped role forgot its history.
	for _, server := range servers {
		server.CloseAndWait()
	}
	r.mu.Lock()
	for _, server := range servers {
		r.foldCountsLocked(
			server.Stats(),
			server.ServiceCounts(),
			server.ServedCounts(),
			server.RefusalCounts(),
			server.CarrierServedCounts(),
			server.CarrierRefusalCounts(),
			server.CarrierByteStats(time.Now()),
		)
	}
	r.closing = slices.DeleteFunc(r.closing, func(c *extender.ExtenderServer) bool { return slices.Contains(servers, c) })
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
	// requests accepted and handed a stream (extender.ExtenderServer.ServedCounts)
	ServedCounts         map[string]uint64
	RefusalCounts        map[string]uint64
	CarrierServedCounts  map[string]uint64
	CarrierRefusalCounts map[string]map[string]uint64
	// per-carrier relayed bytes, rates and minute history, summed over every
	// server the role owns plus the cumulative bytes of retired ones
	CarrierBytes map[string]extender.ExtenderCarrierByteStats
}

func (r *extenderRole) snapshot() extenderRoleSnapshot {
	r.mu.Lock()
	servers := append(slices.Clone(r.servers), r.closing...)
	baseServiceCounts := maps.Clone(r.baseServiceCounts)
	baseServedCounts := maps.Clone(r.baseServedCounts)
	baseRefusalCounts := maps.Clone(r.baseRefusalCounts)
	baseCarrierServedCounts := maps.Clone(r.baseCarrierServedCounts)
	baseCarrierRefusalCounts := map[string]map[string]uint64{}
	for carrier, stages := range r.baseCarrierRefusalCounts {
		baseCarrierRefusalCounts[carrier] = maps.Clone(stages)
	}
	baseCarrierByteCounts := map[string]extender.ExtenderCarrierByteStats{}
	for carrier, byteStats := range r.baseCarrierByteCounts {
		baseCarrierByteCounts[carrier] = byteStats
	}
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
	snapshot.ServedCounts = baseServedCounts
	snapshot.RefusalCounts = baseRefusalCounts
	snapshot.CarrierServedCounts = baseCarrierServedCounts
	snapshot.CarrierRefusalCounts = baseCarrierRefusalCounts
	if snapshot.ServiceCounts == nil {
		snapshot.ServiceCounts = map[string]uint64{}
	}
	if snapshot.ServedCounts == nil {
		snapshot.ServedCounts = map[string]uint64{}
	}
	if snapshot.RefusalCounts == nil {
		snapshot.RefusalCounts = map[string]uint64{}
	}
	if snapshot.CarrierServedCounts == nil {
		snapshot.CarrierServedCounts = map[string]uint64{}
	}
	if snapshot.CarrierRefusalCounts == nil {
		snapshot.CarrierRefusalCounts = map[string]map[string]uint64{}
	}
	snapshot.CarrierBytes = mergeCarrierByteStats(baseCarrierByteCounts, servers, time.Now())
	for _, server := range servers {
		for service, count := range server.ServiceCounts() {
			snapshot.ServiceCounts[service] += count
		}
		for service, count := range server.ServedCounts() {
			snapshot.ServedCounts[service] += count
		}
		for stage, count := range server.RefusalCounts() {
			snapshot.RefusalCounts[stage] += count
		}
		for carrier, count := range server.CarrierServedCounts() {
			snapshot.CarrierServedCounts[carrier] += count
		}
		for carrier, stages := range server.CarrierRefusalCounts() {
			dst := snapshot.CarrierRefusalCounts[carrier]
			if dst == nil {
				dst = map[string]uint64{}
				snapshot.CarrierRefusalCounts[carrier] = dst
			}
			for stage, count := range stages {
				dst[stage] += count
			}
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

// mergeCarrierByteStats builds the role's per-carrier byte picture from the
// cumulative bytes of retired servers plus every live server's reading.
// Cumulative counters sum; rates sum; the minute histories add positionally,
// which is exact because every server's minutes are computed against the same
// now and so name the same absolute minutes.
func mergeCarrierByteStats(
	base map[string]extender.ExtenderCarrierByteStats,
	servers []*extender.ExtenderServer,
	now time.Time,
) map[string]extender.ExtenderCarrierByteStats {
	out := map[string]extender.ExtenderCarrierByteStats{}
	for carrier, byteStats := range base {
		out[carrier] = byteStats
	}
	for _, server := range servers {
		for carrier, serverStats := range server.CarrierByteStats(now) {
			merged := out[carrier]
			merged.Carrier = carrier
			merged.IngressBytes += serverStats.IngressBytes
			merged.EgressBytes += serverStats.EgressBytes
			merged.LiveConns += serverStats.LiveConns
			merged.ConnsTotal += serverStats.ConnsTotal
			merged.RateInBps += serverStats.RateInBps
			merged.RateOutBps += serverStats.RateOutBps
			if len(serverStats.Minutes) > 0 {
				if len(merged.Minutes) == 0 {
					merged.Minutes = slices.Clone(serverStats.Minutes)
				} else {
					for i := 0; i < len(serverStats.Minutes) && i < len(merged.Minutes); i++ {
						merged.Minutes[i] += serverStats.Minutes[i]
					}
				}
			}
			out[carrier] = merged
		}
	}
	return out
}

// sampleCarrierRates folds every server's cumulative per-carrier counters into
// its rate and minute history, on the periodic tick.
func (r *extenderRole) sampleCarrierRates(now time.Time) {
	r.mu.Lock()
	servers := slices.Clone(r.servers)
	r.mu.Unlock()
	for _, server := range servers {
		server.SampleCarrierRates(now)
	}
}

func (r *extenderRole) publishStatus(note string) {
	snapshot := r.snapshot()
	r.notifyActivatorOfCarriers(snapshot.Carriers)
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
		tlog("🌉✅ [extender] ready — %s; %s\n", summary, policy)
		return
	}
	if !changed {
		return
	}
	if snapshot.State == extenderStateServing {
		tlog("🌉✅ [extender] %s\n", summary)
		return
	}
	if note == "" {
		note = summary
	}
	mark := "🌉"
	switch snapshot.State {
	case extenderStateTcpUnavailable, extenderStateUnavailable, extenderStateError:
		mark = "🌉❌"
	}
	tlog("%s [extender] %s — %s\n", mark, snapshot.State, note)
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
	fmt.Fprintf(&b, "# HELP urnet_extender_carrier_requests_total Extender requests by carrier and outcome, cumulative.\n")
	fmt.Fprintf(&b, "# TYPE urnet_extender_carrier_requests_total counter\n")
	for _, carrier := range extender.ExtenderCarrierPorts() {
		served := snapshot.CarrierServedCounts[carrier]
		var refused uint64
		for _, count := range snapshot.CarrierRefusalCounts[carrier] {
			refused += count
		}
		fmt.Fprintf(&b, "urnet_extender_carrier_requests_total{carrier=%s,outcome=\"served\"} %d\n",
			connect.PrometheusLabelValue(carrier), served)
		fmt.Fprintf(&b, "urnet_extender_carrier_requests_total{carrier=%s,outcome=\"refused\"} %d\n",
			connect.PrometheusLabelValue(carrier), refused)
	}
	fmt.Fprintf(&b, "# HELP urnet_extender_carrier_refused_total Extender requests refused by carrier and stage, cumulative.\n")
	fmt.Fprintf(&b, "# TYPE urnet_extender_carrier_refused_total counter\n")
	for _, carrier := range extender.ExtenderCarrierPorts() {
		stages := snapshot.CarrierRefusalCounts[carrier]
		if len(stages) == 0 {
			continue
		}
		stageNames := make([]string, 0, len(stages))
		for stage := range stages {
			stageNames = append(stageNames, stage)
		}
		slices.Sort(stageNames)
		for _, stage := range stageNames {
			fmt.Fprintf(&b, "urnet_extender_carrier_refused_total{carrier=%s,stage=%s} %d\n",
				connect.PrometheusLabelValue(carrier), connect.PrometheusLabelValue(stage), stages[stage])
		}
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
	fmt.Fprintf(&b, "# HELP urnet_extender_carrier_bytes_total Bytes the extender relayed per carrier and direction.\n")
	fmt.Fprintf(&b, "# TYPE urnet_extender_carrier_bytes_total counter\n")
	for _, carrier := range extender.ExtenderCarrierPorts() {
		byteStats := snapshot.CarrierBytes[carrier]
		fmt.Fprintf(&b, "urnet_extender_carrier_bytes_total{carrier=%s,direction=\"ingress\"} %d\n",
			connect.PrometheusLabelValue(carrier), byteStats.IngressBytes)
		fmt.Fprintf(&b, "urnet_extender_carrier_bytes_total{carrier=%s,direction=\"egress\"} %d\n",
			connect.PrometheusLabelValue(carrier), byteStats.EgressBytes)
	}
	fmt.Fprintf(&b, "# HELP urnet_extender_carrier_connections Relayed connections open now over each extender carrier.\n")
	fmt.Fprintf(&b, "# TYPE urnet_extender_carrier_connections gauge\n")
	for _, carrier := range extender.ExtenderCarrierPorts() {
		fmt.Fprintf(&b, "urnet_extender_carrier_connections{carrier=%s} %d\n",
			connect.PrometheusLabelValue(carrier), snapshot.CarrierBytes[carrier].LiveConns)
	}
	fmt.Fprintf(&b, "# HELP urnet_extender_tcp_retry_total Attempts to take tcp 443 after a bind failure.\n")
	fmt.Fprintf(&b, "# TYPE urnet_extender_tcp_retry_total counter\n")
	fmt.Fprintf(&b, "urnet_extender_tcp_retry_total %d\n", snapshot.TcpRetries)
	fmt.Fprintf(&b, "# HELP urnet_extender_discovery_state Extender discovery state.\n# TYPE urnet_extender_discovery_state gauge\n")
	discovery := currentExtenderDiscoverySnapshot()
	for _, candidate := range []string{extenderDiscoveryStateOff, extenderDiscoveryStateWaiting, extenderDiscoveryStateRunning, extenderDiscoveryStateFeedOnly, extenderDiscoveryStateFailed} {
		value := 0
		if discovery.State == candidate {
			value = 1
		}
		fmt.Fprintf(&b, "urnet_extender_discovery_state{state=%s} %d\n", connect.PrometheusLabelValue(candidate), value)
	}
	fmt.Fprintf(&b, "# HELP urnet_extender_discovery_peers Discovery mesh peers.\n# TYPE urnet_extender_discovery_peers gauge\nurnet_extender_discovery_peers %d\n", discovery.Peers)
	fmt.Fprintf(&b, "# HELP urnet_extender_discovery_feed_subscribers Discovery feed subscribers.\n# TYPE urnet_extender_discovery_feed_subscribers gauge\nurnet_extender_discovery_feed_subscribers %d\n", discovery.FeedSubscribers)
	return b.String()
}
