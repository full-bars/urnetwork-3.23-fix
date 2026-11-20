package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/connect"
)

// UDP path check for a proxied identity.
//
// The proxy grade and the MiTM probes look at the TCP and TLS path only. QUIC
// takes a different path through the same proxy: a UDP relay that can be dead,
// can be silent, or can exit from a different address than the proxy's TCP
// side. The first two only cost the identity its H3 option (the engine falls
// back to TCP). The last one puts the identity at an address the grade never
// looked at, so H3 is not used for it.
//
// The check runs once per identity, for identities that actually reach an H3
// attempt (not for every candidate in the pool), and is remembered for a while:
//
//	ok        the relay works and its exit address equals the TCP exit address
//	no-relay  the proxy refuses UDP ASSOCIATE, or the relay never answers
//	mismatch  the relay works but exits from another address than TCP does
//	unknown   the relay works but the TCP exit could not be learned to compare
//
// H3 and the identity's client udp (proxy_udp_egress.go) are allowed for ok
// and unknown (the relay is the proxy either way) and refused for no-relay and
// mismatch. Unknown is re-checked sooner.

type proxyUDPStatus int

const (
	proxyUDPUnknown proxyUDPStatus = iota
	proxyUDPOK
	proxyUDPNoRelay
	proxyUDPExitMismatch
)

func (self proxyUDPStatus) String() string {
	switch self {
	case proxyUDPOK:
		return "ok"
	case proxyUDPNoRelay:
		return "no-relay"
	case proxyUDPExitMismatch:
		return "exit-mismatch"
	default:
		return "unknown"
	}
}

type proxyUDPResult struct {
	Status  proxyUDPStatus
	TCPExit string
	UDPExit string
	Err     error
	// TCPExitObserved is true when this check looked the TCP exit up itself
	// instead of reusing a remembered one.
	TCPExitObserved bool
}

type udpCheckTcpExitServer struct {
	address string
	host    string
}

var (
	// public services the check asks "what address do you see me as": STUN over
	// UDP through the relay, plain HTTP over TCP through the same proxy
	udpCheckStunServers    = []string{"stun.l.google.com:19302", "stun.cloudflare.com:3478"}
	udpCheckTcpExitServers = []udpCheckTcpExitServer{
		{address: "api.ipify.org:80", host: "api.ipify.org"},
		{address: "icanhazip.com:80", host: "icanhazip.com"},
	}
	udpCheckReplyTimeout = 2 * time.Second
)

const (
	udpCheckStunAttempts = 2
	// how many checks may hold sockets at once
	udpCheckMaxConcurrent = 8
	udpCheckOverallLimit  = 15 * time.Second
	proxyUDPOKTTL         = 24 * time.Hour
	proxyUDPNoRelayTTL    = 72 * time.Hour
	proxyUDPMismatchTTL   = 24 * time.Hour
	proxyUDPUnknownTTL    = time.Hour
	// A remembered TCP exit is reused by later checks for this long. It is
	// renewed only by a lookup that observed it, never by a UDP success, so a
	// proxy whose TCP exit changes cannot keep a stale match indefinitely.
	proxyTCPExitMaxAge = 6 * time.Hour
	// An active flow keeps the existing lazy-path recheck cadence. A sweep
	// and coverage use the longer state lifetimes; they do not delay a lazy
	// check when traffic returns after 30 minutes (5 minutes for unknown).
	proxyUDPLazyTTL        = 30 * time.Minute
	proxyUDPLazyUnknownTTL = 5 * time.Minute

	stunMagicCookie      = 0x2112A442
	stunBindingRequest   = 0x0001
	stunBindingSuccess   = 0x0101
	stunAttrMapped       = 0x0001
	stunAttrXorMapped    = 0x0020
	stunHeaderByteCount  = 20
	stunFamilyIpv4       = 1
	stunFamilyIpv6       = 2
	tcpExitReplyMaxBytes = 4096
)

func newStunBindingRequest() ([]byte, [12]byte) {
	var transaction [12]byte
	binaryRandomFill(transaction[:])
	request := make([]byte, 0, stunHeaderByteCount)
	request = binary.BigEndian.AppendUint16(request, stunBindingRequest)
	request = binary.BigEndian.AppendUint16(request, 0)
	request = binary.BigEndian.AppendUint32(request, stunMagicCookie)
	request = append(request, transaction[:]...)
	return request, transaction
}

// parseStunMappedAddress returns the address a STUN server saw the request
// from, for the response to the given transaction.
func parseStunMappedAddress(response []byte, transaction [12]byte) (net.IP, bool) {
	if len(response) < stunHeaderByteCount ||
		binary.BigEndian.Uint16(response[0:2]) != stunBindingSuccess ||
		binary.BigEndian.Uint32(response[4:8]) != stunMagicCookie ||
		!bytes.Equal(response[8:20], transaction[:]) {
		return nil, false
	}
	length := int(binary.BigEndian.Uint16(response[2:4]))
	if len(response) < stunHeaderByteCount+length {
		return nil, false
	}
	body := response[stunHeaderByteCount : stunHeaderByteCount+length]
	cookie := binary.BigEndian.AppendUint32(nil, stunMagicCookie)
	for len(body) >= 4 {
		attributeType := binary.BigEndian.Uint16(body[0:2])
		attributeLength := int(binary.BigEndian.Uint16(body[2:4]))
		if len(body) < 4+attributeLength {
			return nil, false
		}
		value := body[4 : 4+attributeLength]
		if (attributeType == stunAttrXorMapped || attributeType == stunAttrMapped) && len(value) >= 4 {
			xor := attributeType == stunAttrXorMapped
			switch value[1] {
			case stunFamilyIpv4:
				if len(value) >= 8 {
					ip := make(net.IP, 4)
					for i := range ip {
						ip[i] = value[4+i]
						if xor {
							ip[i] ^= cookie[i]
						}
					}
					return ip, true
				}
			case stunFamilyIpv6:
				if len(value) >= 20 {
					mask := append(append([]byte(nil), cookie...), transaction[:]...)
					ip := make(net.IP, 16)
					for i := range ip {
						ip[i] = value[4+i]
						if xor {
							ip[i] ^= mask[i]
						}
					}
					return ip, true
				}
			}
		}
		padded := attributeLength + (4-attributeLength%4)%4
		if len(body) < 4+padded {
			break
		}
		body = body[4+padded:]
	}
	return nil, false
}

// probeProxyUDPRelay performs only the SOCKS5 handshake and UDP ASSOCIATE.
// It sends no UDP packets and does not contact an external service.
func probeProxyUDPRelay(ctx context.Context, settings *connect.ProxySettings) (net.PacketConn, error) {
	return dialSocks5UDP(ctx, settings.Network, settings.Address, settings.Auth)
}

// udpExitViaProxy asks a STUN server, through the already opened relay, which
// address it sees the datagram from.
func udpExitViaProxy(ctx context.Context, pc net.PacketConn) (string, error) {
	buffer := make([]byte, 2048)
	for _, server := range udpCheckStunServers {
		destination, err := net.ResolveUDPAddr("udp4", server)
		if err != nil {
			continue
		}
		request, transaction := newStunBindingRequest()
		for attempt := 0; attempt < udpCheckStunAttempts; attempt++ {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			if _, err := pc.WriteTo(request, destination); err != nil {
				return "", err
			}
			pc.SetReadDeadline(time.Now().Add(udpCheckReplyTimeout))
			for {
				n, _, err := pc.ReadFrom(buffer)
				if err != nil {
					break
				}
				if ip, ok := parseStunMappedAddress(buffer[:n], transaction); ok {
					return ip.String(), nil
				}
			}
		}
	}
	return "", errors.New("the relay never answered a STUN request")
}

// tcpExitViaProxy asks a plain-HTTP address service, through the same proxy
// over TCP, which address it sees the connection from.
func tcpExitViaProxy(ctx context.Context, settings *connect.ProxySettings) (string, error) {
	var lastErr error
	for _, server := range udpCheckTcpExitServers {
		ip, err := tcpExitFrom(ctx, settings, server)
		if err == nil {
			return ip, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("no address service configured")
	}
	return "", lastErr
}

func tcpExitFrom(ctx context.Context, settings *connect.ProxySettings, server udpCheckTcpExitServer) (string, error) {
	network := settings.Network
	if network == "" {
		network = "tcp"
	}
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, network, settings.Address)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	deadline := time.Now().Add(8 * time.Second)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	conn.SetDeadline(deadline)

	user, password := "", ""
	if settings.Auth != nil {
		user, password = settings.Auth.User, settings.Auth.Password
	}
	if !socks5Greet(conn, user, password) {
		return "", errors.New("socks5 greeting failed")
	}
	host, portText, err := net.SplitHostPort(server.address)
	if err != nil {
		return "", err
	}
	port, err := net.LookupPort("tcp", portText)
	if err != nil {
		return "", err
	}
	request := []byte{socks5Version, 1, 0, 3, byte(len(host))}
	request = append(request, host...)
	request = binary.BigEndian.AppendUint16(request, uint16(port))
	if _, err := conn.Write(request); err != nil {
		return "", err
	}
	if !readSocks5ConnectReply(conn) {
		return "", errors.New("socks5 CONNECT refused")
	}
	if _, err := fmt.Fprintf(conn, "GET / HTTP/1.0\r\nHost: %s\r\nUser-Agent: curl/8\r\nConnection: close\r\n\r\n", server.host); err != nil {
		return "", err
	}
	response, err := io.ReadAll(io.LimitReader(conn, tcpExitReplyMaxBytes))
	if err != nil && len(response) == 0 {
		return "", err
	}
	_, body, found := bytes.Cut(response, []byte("\r\n\r\n"))
	if !found {
		return "", errors.New("malformed reply from the address service")
	}
	ip := net.ParseIP(strings.TrimSpace(string(body)))
	if ip == nil {
		return "", errors.New("the address service did not answer with an address")
	}
	return ip.String(), nil
}

type cachedProxyTCPExitKey struct{}

// cachedProxyTCPExitReuseKey carries how long the cached TCP exit may still be
// reused. The probe can wait for a slot after the store decided to reuse the
// exit, so the probe checks the budget again when it is about to rely on it.
type cachedProxyTCPExitReuseKey struct{}

type tcpExitReuse struct {
	start  time.Time
	budget time.Duration
}

// reusableCachedTCPExit returns the cached TCP exit the store offered, or an
// empty string when it is missing or its reuse budget ran out while the probe
// waited.
func reusableCachedTCPExit(ctx context.Context) string {
	tcpExit, _ := ctx.Value(cachedProxyTCPExitKey{}).(string)
	if reuse, ok := ctx.Value(cachedProxyTCPExitReuseKey{}).(tcpExitReuse); ok && time.Since(reuse.start) >= reuse.budget {
		return ""
	}
	return tcpExit
}

// checkProxyUDP runs the whole check for one identity.
func checkProxyUDP(ctx context.Context, settings *connect.ProxySettings) proxyUDPResult {
	ctx, cancel := context.WithTimeout(ctx, udpCheckOverallLimit)
	defer cancel()
	pc, err := probeProxyUDPRelay(ctx, settings)
	if err != nil {
		return proxyUDPResult{Status: proxyUDPNoRelay, Err: err}
	}
	defer pc.Close()
	udpExit, err := udpExitViaProxy(ctx, pc)
	if err != nil {
		return proxyUDPResult{Status: proxyUDPNoRelay, Err: err}
	}
	tcpExit := reusableCachedTCPExit(ctx)
	observed := false
	if tcpExit == "" || tcpExit != udpExit {
		tcpExit, err = tcpExitViaProxy(ctx, settings)
		if err != nil {
			return proxyUDPResult{Status: proxyUDPUnknown, UDPExit: udpExit, Err: err}
		}
		observed = true
	}
	if tcpExit != udpExit {
		return proxyUDPResult{Status: proxyUDPExitMismatch, TCPExit: tcpExit, UDPExit: udpExit, TCPExitObserved: observed}
	}
	return proxyUDPResult{Status: proxyUDPOK, TCPExit: tcpExit, UDPExit: udpExit, TCPExitObserved: observed}
}

type proxyUDPCheckEntry struct {
	result  proxyUDPResult
	ipv6    proxyIPv6Result
	at      time.Time
	tcpExit string // last known TCP exit, retained even when a relay fails
	// tcpExitAt is when a lookup last observed tcpExit. Only a lookup renews it.
	tcpExitAt time.Time
	flowReply bool
	// done is non-nil while a check for the identity is running
	done chan struct{}
}

// proxyUDPCheckStore remembers the check per proxy IDENTITY (address plus
// user), runs at most one check per identity at a time, and repeats it when
// the remembered result is old.
type proxyUDPCheckStore struct {
	mu      sync.Mutex
	probe   func(context.Context, *connect.ProxySettings) proxyUDPResult
	entries map[string]*proxyUDPCheckEntry
	// slots bounds how many checks hold sockets at once: a check holds a
	// control connection and a UDP socket, and a pool of hundreds of proxies
	// reaches its first H3 attempt within seconds of starting
	slots     chan struct{}
	lastPrune time.Time
	// detachedRuns holds the one detached check in flight per identity
	// (checkDetached)
	detachedRuns map[string]*proxyUDPCheckRun
	persistPath  string
	persisted    map[string]udpPersistEntry
	// background state writer (proxy_udp_persist.go)
	persistDirty   bool
	persistWriting bool
	persistNow     time.Time
	persistKick    chan struct{}
	persistDone    chan struct{}
	persistFailed  bool
}

// proxyUDPCheckRun is one check whose result is ready once done is closed.
type proxyUDPCheckRun struct {
	done   chan struct{}
	result proxyUDPResult
}

func newProxyUDPCheckStore(probe func(context.Context, *connect.ProxySettings) proxyUDPResult) *proxyUDPCheckStore {
	return &proxyUDPCheckStore{
		probe:        probe,
		entries:      map[string]*proxyUDPCheckEntry{},
		slots:        make(chan struct{}, udpCheckMaxConcurrent),
		detachedRuns: map[string]*proxyUDPCheckRun{},
	}
}

// prune drops identities nobody has asked about for two state lifetimes,
// at most once per lazy lifetime. Called with the lock held.
func (self *proxyUDPCheckStore) prune(now time.Time) {
	if self.lastPrune.IsZero() || now.Sub(self.lastPrune) >= proxyUDPLazyTTL {
		self.lastPrune = now
		for key, entry := range self.entries {
			if entry.done == nil && now.Sub(entry.at) > 2*proxyUDPTTL(entry.result.Status) && !entry.ipv6.retain(now) {
				delete(self.entries, key)
			}
		}
	}
	const limit = 4 * udpStateMaxEntries
	if len(self.entries) <= limit {
		return
	}
	type completed struct {
		key string
		at  time.Time
	}
	var oldest []completed
	for key, entry := range self.entries {
		if entry.done == nil {
			oldest = append(oldest, completed{key, entry.at})
		}
	}
	sort.Slice(oldest, func(i, j int) bool {
		if oldest[i].at.Equal(oldest[j].at) {
			return oldest[i].key < oldest[j].key
		}
		return oldest[i].at.Before(oldest[j].at)
	})
	for _, entry := range oldest {
		if len(self.entries) <= limit {
			break
		}
		delete(self.entries, entry.key)
	}
}

var globalProxyUDPCheck = newProxyUDPCheckStore(checkProxyUDP)

func proxyUDPTTL(status proxyUDPStatus) time.Duration {
	switch status {
	case proxyUDPOK:
		return proxyUDPOKTTL
	case proxyUDPNoRelay:
		return proxyUDPNoRelayTTL
	case proxyUDPExitMismatch:
		return proxyUDPMismatchTTL
	default:
		return proxyUDPUnknownTTL
	}
}

func proxyUDPLazyRecheckTTL(status proxyUDPStatus) time.Duration {
	if status == proxyUDPUnknown {
		return proxyUDPLazyUnknownTTL
	}
	return proxyUDPLazyTTL
}

func (self *proxyUDPCheckStore) Check(ctx context.Context, settings *connect.ProxySettings, now time.Time) proxyUDPResult {
	key := settings.Key()
	for {
		self.mu.Lock()
		entry := self.cachedEntryLocked(key)
		if entry != nil && entry.done != nil {
			done := entry.done
			self.mu.Unlock()
			select {
			case <-done:
				continue
			case <-ctx.Done():
				return proxyUDPResult{Status: proxyUDPUnknown, Err: ctx.Err()}
			}
		}
		if entry != nil && !entry.at.IsZero() && now.Sub(entry.at) < proxyUDPLazyRecheckTTL(entry.result.Status) {
			result := entry.result
			self.mu.Unlock()
			return result
		}
		var previous *proxyUDPResult
		var previousAt time.Time
		var ipv6 proxyIPv6Result
		var tcpExit string
		var tcpExitAt time.Time
		if entry != nil {
			previousAt = entry.at
			ipv6 = entry.ipv6
			copyOf := entry.result
			previous = &copyOf
			tcpExit = entry.tcpExit
			if tcpExit == "" {
				tcpExit = entry.result.TCPExit
			}
			tcpExitAt = entry.tcpExitAt
		}
		// the probe may reuse the remembered TCP exit only while it is fresh
		reusableTcpExit := ""
		var reuseBudget time.Duration
		// The reuse clock starts here, before the wait for a probe slot, so the
		// queue time counts against the six hour limit.
		reuseStart := time.Now()
		if tcpExit != "" && !tcpExitAt.IsZero() && now.Sub(tcpExitAt) < proxyTCPExitMaxAge {
			reusableTcpExit = tcpExit
			reuseBudget = proxyTCPExitMaxAge - now.Sub(tcpExitAt)
		}
		self.prune(now)
		running := &proxyUDPCheckEntry{done: make(chan struct{}), tcpExit: tcpExit, tcpExitAt: tcpExitAt, ipv6: ipv6}
		self.entries[key] = running
		self.mu.Unlock()

		var result proxyUDPResult
		select {
		case self.slots <- struct{}{}:
			probeCtx := context.WithValue(ctx, cachedProxyTCPExitKey{}, reusableTcpExit)
			probeCtx = context.WithValue(probeCtx, cachedProxyTCPExitReuseKey{}, tcpExitReuse{start: reuseStart, budget: reuseBudget})
			result = self.probe(probeCtx, settings)
			<-self.slots
		case <-ctx.Done():
			result = proxyUDPResult{Status: proxyUDPUnknown, Err: ctx.Err()}
		}

		self.mu.Lock()
		if ctx.Err() != nil {
			// an interrupted check says nothing: forget it
			delete(self.entries, key)
			if previous != nil {
				self.entries[key] = &proxyUDPCheckEntry{result: *previous, tcpExit: tcpExit, tcpExitAt: tcpExitAt, at: previousAt, ipv6: ipv6}
			}
			close(running.done)
			self.mu.Unlock()
			return proxyUDPResult{Status: proxyUDPUnknown, Err: ctx.Err()}
		}
		running.result = result
		if result.TCPExit != "" {
			running.tcpExit = result.TCPExit
		}
		if result.TCPExitObserved {
			running.tcpExitAt = now
		}
		running.at = now
		close(running.done)
		running.done = nil
		self.prune(now)
		if err := self.persistLocked(key, running, now); err != nil {
			tlog("[proxy][udp] could not persist probe state\n")
		}
		self.mu.Unlock()

		logProxyUDPResult(settings, previous, result)
		return result
	}
}

// checkDetached returns the identity's check, run apart from the caller's
// context so its result is remembered even when the caller stops waiting. A
// remembered result returns at once; otherwise at most one detached check per
// identity runs, and every caller shares it, so a burst of flows on an
// identity whose check is queued costs one goroutine, not one each.
func (self *proxyUDPCheckStore) checkDetached(settings *connect.ProxySettings, now time.Time) *proxyUDPCheckRun {
	key := settings.Key()
	self.mu.Lock()
	defer self.mu.Unlock()
	if entry := self.cachedEntryLocked(key); entry != nil && entry.done == nil && !entry.at.IsZero() && now.Sub(entry.at) < proxyUDPLazyRecheckTTL(entry.result.Status) {
		run := &proxyUDPCheckRun{done: make(chan struct{}), result: entry.result}
		close(run.done)
		return run
	}
	if run := self.detachedRuns[key]; run != nil {
		return run
	}
	run := &proxyUDPCheckRun{done: make(chan struct{})}
	self.detachedRuns[key] = run
	go connect.HandleError(func() {
		defer func() {
			self.mu.Lock()
			delete(self.detachedRuns, key)
			self.mu.Unlock()
			close(run.done)
		}()
		run.result = self.Check(context.Background(), settings, now)
	})
	return run
}

// logProxyUDPResult says so once per change of answer for an identity, so a
// fleet of proxies produces one line each, not one per attempt.
func logProxyUDPResult(settings *connect.ProxySettings, previous *proxyUDPResult, result proxyUDPResult) {
	if previous != nil && previous.Status == result.Status {
		return
	}
	identity := udpProxyIdentity(settings)
	switch result.Status {
	case proxyUDPOK:
		tlog("[proxy][udp] %s udp relay ok, exit matches tcp; h3 and client udp allowed\n", identity)
	case proxyUDPExitMismatch:
		tlog("[proxy][udp] %s udp and tcp exits differ; h3 and client udp not used for this proxy\n", identity)
	case proxyUDPNoRelay:
		tlog("[proxy][udp] %s no udp relay (%s); h3 and client udp not used for this proxy, tcp only\n", identity, proxyUDPFailureClass(result.Err))
	default:
		tlog("[proxy][udp] %s udp relay works but the tcp exit could not be learned (%s); h3 and client udp allowed, checking again soon\n", identity, proxyUDPFailureClass(result.Err))
	}
}

// proxyUDPFailureClass keeps underlying network errors out of operator logs.
func proxyUDPFailureClass(err error) string {
	class := dialFailureClass(err)
	if class == "dial error" {
		return "check failed"
	}
	return class
}

// noteFlowReply records relay use without claiming that UDP and TCP exits match.
// In-flight checks and fresh definitive checks carry stronger evidence.
//
// Only a matching reply is evidence. A timeout, a refused association or a
// read error says nothing about the relay, so flows never record a negative
// result: coverage changes only through checks and through this method.
func (self *proxyUDPCheckStore) noteFlowReply(key string, now time.Time) {
	self.mu.Lock()
	defer self.mu.Unlock()
	entry := self.cachedEntryLocked(key)
	if entry != nil {
		if entry.done != nil || now.Before(entry.at) {
			return
		}
		if !entry.at.IsZero() && now.Sub(entry.at) < proxyUDPTTL(entry.result.Status) {
			if entry.result.Status != proxyUDPUnknown || entry.flowReply {
				return
			}
			entry.flowReply = true
			entry.at = now
			if err := self.persistLocked(key, entry, now); err != nil {
				tlog("[proxy][udp] could not persist probe state\n")
			}
			return
		}
	}
	var tcpExit string
	var tcpExitAt time.Time
	var ipv6 proxyIPv6Result
	if entry != nil {
		ipv6 = entry.ipv6
		tcpExit = entry.tcpExit
		tcpExitAt = entry.tcpExitAt
	}
	self.prune(now)
	entry = &proxyUDPCheckEntry{result: proxyUDPResult{Status: proxyUDPUnknown}, tcpExit: tcpExit, tcpExitAt: tcpExitAt, at: now, flowReply: true, ipv6: ipv6}
	self.entries[key] = entry
	self.prune(now)
	if err := self.persistLocked(key, entry, now); err != nil {
		tlog("[proxy][udp] could not persist probe state\n")
	}
}

// coverage reads cached results without starting checks. A missing, in-flight,
// or expired result is unchecked. The caller supplies only online identities.
func (self *proxyUDPCheckStore) coverage(online []string, now time.Time) *SnapshotProxyUDP {
	self.mu.Lock()
	defer self.mu.Unlock()
	out := &SnapshotProxyUDP{}
	for _, key := range online {
		entry := self.cachedEntryLocked(key)
		if entry == nil || entry.done != nil || entry.at.IsZero() || now.Sub(entry.at) >= proxyUDPTTL(entry.result.Status) {
			out.Unchecked++
			continue
		}
		switch entry.result.Status {
		case proxyUDPOK:
			out.OK++
		case proxyUDPNoRelay:
			out.NoRelay++
		case proxyUDPExitMismatch:
			out.Mismatch++
		case proxyUDPUnknown:
			out.Unknown++
		default:
			out.Unchecked++
		}
	}
	return out
}

// statusFor reads the cached UDP result of one identity without starting a
// check, for the hover detail. It uses the same rules as coverage: a missing,
// in-flight or expired result is unchecked.
func (self *proxyUDPCheckStore) statusFor(key string, now time.Time) string {
	self.mu.Lock()
	defer self.mu.Unlock()
	entry := self.cachedEntryLocked(key)
	if entry == nil || entry.done != nil || entry.at.IsZero() || now.Sub(entry.at) >= proxyUDPTTL(entry.result.Status) {
		return "unchecked"
	}
	switch entry.result.Status {
	case proxyUDPOK:
		return "ok"
	case proxyUDPNoRelay:
		return "no_relay"
	case proxyUDPExitMismatch:
		return "mismatch"
	case proxyUDPUnknown:
		return "unknown"
	}
	return "unchecked"
}

// exitsFor returns only valid IP addresses from a fresh cached check.
func (self *proxyUDPCheckStore) exitsFor(key string, now time.Time) (string, string) {
	self.mu.Lock()
	defer self.mu.Unlock()
	entry := self.cachedEntryLocked(key)
	if entry == nil || entry.done != nil || entry.at.IsZero() || now.Sub(entry.at) >= proxyUDPTTL(entry.result.Status) {
		return "", ""
	}
	safe := func(raw string) string {
		if ip := net.ParseIP(raw); ip != nil {
			return ip.String()
		}
		return ""
	}
	return safe(entry.result.TCPExit), safe(entry.result.UDPExit)
}

// size is how many identities the store currently remembers.
func (self *proxyUDPCheckStore) size() int {
	self.mu.Lock()
	defer self.mu.Unlock()
	return len(self.entries)
}
