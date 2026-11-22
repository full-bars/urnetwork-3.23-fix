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
	"sync/atomic"
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
	// CheckedAt is when the check completed. The store sets it when it hands a
	// stored result to the dual stack gate (measurement), so the gate can tell
	// a new check from a repeated poll of the same one. A probe leaves it zero.
	CheckedAt time.Time
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
	result proxyUDPResult
	ipv6   proxyIPv6Result
	// ipv6Restored is true while ipv6 is a record this store loaded from disk
	// and no measurement of this process has replaced. The dual stack gate
	// baselines such a record instead of counting it as a new measurement. In
	// memory only: it is never written to the state file.
	ipv6Restored bool
	at           time.Time
	// completedAt is when a check last published a result here. at also moves
	// for flow evidence (noteFlowReply), so only this timestamp says a new
	// observation exists. It is not persisted: the gate that reads it does not
	// survive a restart either.
	completedAt time.Time
	// realStatus is the status of the last real check. A flow reply replaces
	// result with unknown, and unknown's TTL is the wrong one to expire the
	// controller's evidence on. In memory only: never persisted, and no
	// admission path reads it.
	realStatus proxyUDPStatus
	tcpExit    string // last known TCP exit, retained even when a relay fails
	// tcpExitAt is when a lookup last observed tcpExit. Only a lookup renews it.
	tcpExitAt time.Time
	flowReply bool
	// done is non-nil while a check for the identity is running
	done chan struct{}
	// ipv6Running is true while the IPv6 stage measures this identity, so a
	// second sweep call does not start another measurement. It is not persisted.
	ipv6Running bool
	// ipv6Token names the refresh that holds the reservation (ipv6Running). A
	// refresh clears the flag and publishes its result only while the entry
	// still carries its token, so a refresh whose entry was replaced meanwhile
	// cannot clear the flag of the one that followed it or write into it.
	ipv6Token uint64
	// ipv6RetryAt holds back the next IPv6 measurement after one stopped early
	// (its own timeout, not a cancelled sweep). Without it the identity stays
	// due and every sweep tick holds a probe slot for it again. It keeps the
	// previous result and is not persisted: a restart retries once.
	ipv6RetryAt time.Time
}

// proxyIPv6StopBackoff is how long an identity waits after a measurement that
// stopped early before the sweep measures it again.
const proxyIPv6StopBackoff = 15 * time.Minute

// ipv6CompletionTime returns the time the stage finished. A variable so a test
// can pin it; production reads the real clock, which also counts the time
// between the sweep picking its batch and this refresh running.
var ipv6CompletionTime = func(now time.Time) time.Time { return time.Now() }

// ipv6StageCompletedAt is the stamp base for a retry. It never lands before the
// sweep's own time, so a pinned synthetic clock stays consistent with it.
func ipv6StageCompletedAt(now time.Time) time.Time {
	completed := ipv6CompletionTime(now)
	if completed.Before(now) {
		return now
	}
	return completed
}

// proxyIPv6CutShortRetry is how long an identity waits when the sweep's tick
// deadline cut its stage short, so nothing was measured. It retries sooner than
// after a completed measurement, but not on the next tick: a stage that is cut
// short stays due, and the IPv6-due order would then keep it at the front of
// every batch and starve the other due identities.
const proxyIPv6CutShortRetry = 5 * time.Minute

// ipv6RecordDueAtLocked is when the stored record calls for its next
// measurement: the time of the last one plus its interval. A record restored
// with the state file carries the wall clock alone. Zero is no deadline at all,
// for a record that was never measured.
func (e *proxyUDPCheckEntry) ipv6RecordDueAtLocked() time.Time {
	if e == nil || e.ipv6.At.IsZero() || e.ipv6.Interval <= 0 {
		return time.Time{}
	}
	return e.ipv6.At.Add(e.ipv6.Interval)
}

// ipv6DueLocked is whether the sweep should measure the identity now. The two
// deadlines are independent and both must have passed: the record's own
// deadline, and the retry hold. Combining them into one stamp would compare a
// restored wall-clock deadline against a live retry stamp that carries a
// monotonic reading, so a backward wall-clock step could let the retry stamp
// stand in for the record's deadline. The caller holds the store lock (or owns
// the entry).
func (e *proxyUDPCheckEntry) ipv6DueLocked(now time.Time) bool {
	if e == nil {
		return true
	}
	if next := e.ipv6RecordDueAtLocked(); !next.IsZero() && now.Before(next) {
		return false
	}
	return !now.Before(e.ipv6RetryAt)
}

// ipv6DueAtLocked orders the sweep: the later of the record's deadline and the
// retry hold, on the wall clock alone. The sweep sorts candidates by it, beside
// wall-clock times restored from the state file, so t.Round(0) drops the
// monotonic reading a live retry stamp carries and the sort never mixes the two
// clocks. The due decision does not use this value; see ipv6DueLocked.
func (e *proxyUDPCheckEntry) ipv6DueAtLocked() time.Time {
	if e == nil {
		return time.Time{}
	}
	due := e.ipv6RetryAt.Round(0)
	if next := e.ipv6RecordDueAtLocked(); next.After(due) {
		due = next
	}
	return due
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
	// ipv6Probe runs the three-stage IPv6 probe for an identity when its
	// previous IPv6 result is due. It is nil by default; the store used by
	// the sweep sets it to runProxyIPv6Stage.
	ipv6Probe     func(context.Context, *connect.ProxySettings, proxyIPv6Result, time.Time, proxyIPv6Dials) (proxyIPv6Result, bool)
	ipv6ProbesRun atomic.Int64
	// ipv6Seq numbers the IPv6 reservations (proxyUDPCheckEntry.ipv6Token)
	ipv6Seq uint64
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
			if entry.done == nil && !entry.ipv6Running && now.Sub(entry.at) > 2*proxyUDPTTL(entry.result.Status) && !entry.ipv6.retain(now) {
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
		// an entry with an IPv6 reservation is not evicted: its refresh would
		// publish into a replacement
		if entry.done == nil && !entry.ipv6Running {
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

func init() {
	globalProxyUDPCheck.ipv6Probe = runProxyIPv6Stage
}

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
		freshUDP := entry != nil && !entry.at.IsZero() && now.Sub(entry.at) < proxyUDPLazyRecheckTTL(entry.result.Status)
		// the IPv6 stage runs only in checks the background sweep starts, so a
		// UDP flow or an H3 dial never waits for it
		ipv6Due := proxyIPv6ProbeEnabled(globalControlState) && self.ipv6Probe != nil && ipv6SweepFromContext(ctx) && entry.ipv6DueLocked(now)
		if freshUDP {
			// the completed UDP result stays in place while the IPv6 stage runs, so
			// no caller waits for it and ipv6UDPAllowed keeps reading it
			result := entry.result
			self.mu.Unlock()
			if ipv6Due {
				self.refreshIPv6(ctx, settings, key, now)
			}
			return result
		}
		var previous *proxyUDPResult
		var previousAt time.Time
		var previousCompletedAt time.Time
		var previousRealStatus proxyUDPStatus
		var ipv6 proxyIPv6Result
		ipv6Restored := false
		ipv6Running := false
		var ipv6RetryAt time.Time
		var ipv6Token uint64
		var tcpExit string
		var tcpExitAt time.Time
		if entry != nil {
			previousAt = entry.at
			previousCompletedAt = entry.completedAt
			previousRealStatus = entry.realStatus
			ipv6 = entry.ipv6
			ipv6Restored = entry.ipv6Restored
			ipv6Running = entry.ipv6Running
			ipv6RetryAt = entry.ipv6RetryAt
			ipv6Token = entry.ipv6Token
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
		running := &proxyUDPCheckEntry{done: make(chan struct{}), tcpExit: tcpExit, tcpExitAt: tcpExitAt, ipv6: ipv6, ipv6Restored: ipv6Restored, ipv6Running: ipv6Running, ipv6Token: ipv6Token, ipv6RetryAt: ipv6RetryAt}
		if previous != nil {
			// the last completed result stays readable while the recheck runs:
			// a dial decision taken now must read it, and its own TTL still
			// decides whether it counts (measurement reports this entry in
			// flight, so no reader treats it as a fresh check)
			running.result = *previous
			running.at = previousAt
			running.completedAt = previousCompletedAt
			running.realStatus = previousRealStatus
		}
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
				// the IPv6 fields come from the entry this check installed: an IPv6
				// refresh that ran meanwhile updated it and cleared its flag, and a
				// restore from the snapshot taken before the probe would bring back
				// the old measurement and a flag nothing will clear
				restored := &proxyUDPCheckEntry{result: *previous, tcpExit: tcpExit, tcpExitAt: tcpExitAt, at: previousAt, completedAt: previousCompletedAt, realStatus: previousRealStatus, ipv6: running.ipv6, ipv6Restored: running.ipv6Restored, ipv6Running: running.ipv6Running, ipv6Token: running.ipv6Token, ipv6RetryAt: running.ipv6RetryAt}
				self.entries[key] = restored
				if err := self.persistLocked(key, restored, now); err != nil {
					tlog("[proxy][udp] could not persist probe state\n")
				}
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
		running.completedAt = now
		running.realStatus = running.result.Status
		close(running.done)
		running.done = nil
		self.prune(now)
		if err := self.persistLocked(key, running, now); err != nil {
			tlog("[proxy][udp] could not persist probe state\n")
		}
		self.mu.Unlock()

		logProxyUDPResult(settings, previous, result)
		if ipv6Due {
			// the stage is independent of the UDP result: a proxy without a UDP
			// relay can still have IPv6, so it runs after a failed check too, and
			// only after the UDP result is published
			self.refreshIPv6(ctx, settings, key, now)
		}
		return result
	}
}

// refreshIPv6 measures the IPv6 capability of one identity apart from the UDP
// check. The completed UDP entry stays in place the whole time (no done channel
// is installed), so nobody waits for the measurement and the cached UDP result
// stays readable. A flag keeps a second sweep call from measuring the same
// identity at once. A cancelled measurement leaves the previous result.
func (self *proxyUDPCheckStore) refreshIPv6(ctx context.Context, settings *connect.ProxySettings, key string, now time.Time) {
	self.mu.Lock()
	entry := self.cachedEntryLocked(key)
	if entry == nil || entry.ipv6Running || !entry.ipv6DueLocked(now) || self.ipv6Probe == nil || !proxyIPv6ProbeEnabled(globalControlState) {
		self.mu.Unlock()
		return
	}
	previous := entry.ipv6
	self.ipv6Seq++
	token := self.ipv6Seq
	entry.ipv6Running = true
	entry.ipv6Token = token
	self.mu.Unlock()

	var measured proxyIPv6Result
	measuredOK := false
	skipped := false
	select {
	case self.slots <- struct{}{}:
		if ctx.Err() != nil || !proxyIPv6ProbeEnabled(globalControlState) {
			// the operator switched the probe off while this waited for a slot:
			// no measurement was made and none is held against the identity
			<-self.slots
			skipped = true
			break
		}
		stageCtx, cancel := context.WithTimeout(ctx, udpCheckOverallLimit)
		measured, measuredOK = self.ipv6Probe(stageCtx, settings, previous, now, defaultProxyIPv6Dials())
		cancel()
		<-self.slots
	case <-ctx.Done():
	}

	self.mu.Lock()
	defer self.mu.Unlock()
	current := self.cachedEntryLocked(key)
	if current == nil || current.ipv6Token != token {
		// the entry was replaced since this refresh took its reservation: the
		// flag and the result now belong to the refresh that holds the new one
		return
	}
	current.ipv6Running = false
	// the backoff counts from the moment the stage finished, so neither the wait
	// for a worker slot nor a long probe shortens it
	completedAt := ipv6StageCompletedAt(now)
	if !measuredOK && !skipped && ctx.Err() == nil {
		// the stage stopped on its own: keep the previous result and wait before
		// the next tick measures this identity again
		current.ipv6RetryAt = completedAt.Add(proxyIPv6StopBackoff)
	}
	if !measuredOK && ctx.Err() != nil {
		// the tick deadline cut the stage short, so nothing was measured. Without
		// a stamp the identity stays due and the IPv6-due order keeps it at the
		// front of every batch, starving the other due identities
		current.ipv6RetryAt = completedAt.Add(proxyIPv6CutShortRetry)
	}
	if measuredOK {
		// a measurement that was made is kept even when the parent context ends
		// right after it: throwing it away would also skip the retry clear and
		// send the identity into a backoff for a result it already has
		current.ipv6 = measured
		// the record is this process's now: a gate that reads it counts it
		current.ipv6Restored = false
		current.ipv6RetryAt = time.Time{}
		self.ipv6ProbesRun.Add(1)
		// while a UDP recheck is in flight the entry is a placeholder with no
		// UDP result, and persisting it would replace the last completed UDP
		// result on disk. The recheck persists the entry when it publishes, or
		// when it is cancelled and restores the previous result.
		if current.done == nil {
			if err := self.persistLocked(key, current, now); err != nil {
				tlog("[proxy][udp] could not persist probe state\n")
			}
		}
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
	var realCompletedAt time.Time
	var realStatus proxyUDPStatus
	var tcpExit string
	var tcpExitAt time.Time
	var ipv6 proxyIPv6Result
	var ipv6Restored bool
	if entry != nil {
		ipv6 = entry.ipv6
		ipv6Restored = entry.ipv6Restored
		tcpExit = entry.tcpExit
		tcpExitAt = entry.tcpExitAt
		// the real check behind this evidence keeps its time and status, so the
		// controller can still see it expire while admission reads the unknown
		realCompletedAt = entry.completedAt
		realStatus = entry.realStatus
	}
	self.prune(now)
	entry = &proxyUDPCheckEntry{result: proxyUDPResult{Status: proxyUDPUnknown}, tcpExit: tcpExit, tcpExitAt: tcpExitAt, at: now, flowReply: true, completedAt: realCompletedAt, realStatus: realStatus, ipv6: ipv6, ipv6Restored: ipv6Restored}
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

// proxyUDPDetail is one identity's udp row for the hover detail: the result
// verdict, both exits and the ipv6 pair, all from one entry state.
type proxyUDPDetail struct {
	Status   string
	TCPExit  string
	UDPExit  string
	IPv6     string
	IPv6Exit string
}

// detailFor reads every field of the detail row under one hold of the store
// lock, so a check that completes between two reads cannot pair one check's
// verdict with another check's exits. The row describes the last completed
// check, which stays on the entry while a recheck runs (see Check).
func (self *proxyUDPCheckStore) detailFor(key string, now time.Time) proxyUDPDetail {
	self.mu.Lock()
	defer self.mu.Unlock()
	detail := proxyUDPDetail{Status: "unchecked", IPv6: "unknown"}
	entry := self.cachedEntryLocked(key)
	if entry == nil {
		return detail
	}
	if !entry.at.IsZero() && now.Sub(entry.at) < proxyUDPTTL(entry.result.Status) {
		switch entry.result.Status {
		case proxyUDPOK:
			detail.Status = "ok"
		case proxyUDPNoRelay:
			detail.Status = "no_relay"
		case proxyUDPExitMismatch:
			detail.Status = "mismatch"
		case proxyUDPUnknown:
			detail.Status = "unknown"
		}
		safe := func(raw string) string {
			if ip := net.ParseIP(raw); ip != nil {
				return ip.String()
			}
			return ""
		}
		detail.TCPExit, detail.UDPExit = safe(entry.result.TCPExit), safe(entry.result.UDPExit)
	}
	if !entry.ipv6.At.IsZero() && entry.ipv6.retain(now) {
		exit := entry.ipv6.TCPExit
		if exit == "" {
			exit = entry.ipv6.UDPExit
		}
		if ip := net.ParseIP(exit); ip != nil && ip.To4() == nil {
			detail.IPv6Exit = ip.String()
		}
		if status := string(entry.ipv6.Status); status != "" {
			detail.IPv6 = status
		}
	}
	return detail
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

// ipv6Coverage reads cached IPv6 results for online identities.
func (self *proxyUDPCheckStore) ipv6Coverage(online []string, now time.Time) *SnapshotProxyIPv6 {
	self.mu.Lock()
	defer self.mu.Unlock()
	out := &SnapshotProxyIPv6{
		Probes: self.ipv6ProbesRun.Load(),
	}
	for _, key := range online {
		entry := self.cachedEntryLocked(key)
		if entry == nil || entry.ipv6.At.IsZero() || !entry.ipv6.retain(now) {
			out.NotProbed++
			continue
		}
		switch entry.ipv6.Status {
		case proxyIPv6Yes:
			out.Yes++
			switch entry.ipv6.UDP {
			case "yes":
				out.UDPYes++
			case "no", "mismatch":
				out.UDPNo++
			default:
				out.UDPUnknown++
			}
		case proxyIPv6No:
			out.No++
		case proxyIPv6Unknown:
			out.Unknown++
		default:
			out.NotProbed++
		}
	}
	return out
}

// ipv6For returns the cached IPv6 status and safe IPv6 exit address.
func (self *proxyUDPCheckStore) ipv6For(key string, now time.Time) (string, string) {
	self.mu.Lock()
	defer self.mu.Unlock()
	entry := self.cachedEntryLocked(key)
	if entry == nil || entry.ipv6.At.IsZero() || !entry.ipv6.retain(now) {
		return "unknown", ""
	}
	exit := entry.ipv6.TCPExit
	if exit == "" {
		exit = entry.ipv6.UDPExit
	}
	safeExit := ""
	if ip := net.ParseIP(exit); ip != nil && ip.To4() == nil {
		safeExit = ip.String()
	}
	status := string(entry.ipv6.Status)
	if status == "" {
		status = "unknown"
	}
	return status, safeExit
}

// size is how many identities the store currently remembers.
func (self *proxyUDPCheckStore) size() int {
	self.mu.Lock()
	defer self.mu.Unlock()
	return len(self.entries)
}

// proxyUDPIPv6EnabledIn reports the proxy_udp_ipv6 setting. It is on by
// default, and off restores the refusal of every ipv6 destination.
func proxyUDPIPv6EnabledIn(state *controlState) bool {
	if state == nil {
		return true
	}
	value, found := state.get("proxy_udp_ipv6")
	return !found || strings.EqualFold(value, "on")
}

func proxyUDPIPv6Enabled() bool {
	return proxyUDPIPv6EnabledIn(globalControlState)
}

// ipv6UDPAllowed reports whether a client udp flow to an ipv6 destination may
// use this identity's relay. It reads the cached measurement and never probes.
// All of these must hold: the setting is on, the udp check of the relay is ok,
// the measured ipv6 result is a fresh yes for both tcp and udp (the udp exit
// matched the tcp exit), and the result is younger than twice its interval, so
// a proxy that stopped being measured stops being trusted.
func (self *proxyUDPCheckStore) ipv6UDPAllowed(key string, now time.Time) bool {
	if !proxyUDPIPv6Enabled() {
		return false
	}
	self.mu.Lock()
	defer self.mu.Unlock()
	entry := self.cachedEntryLocked(key)
	if entry == nil || entry.result.Status != proxyUDPOK {
		return false
	}
	// the same age rule measurement applies: a result past its TTL is not
	// evidence, whatever the cached status says
	if !entry.at.IsZero() && now.Sub(entry.at) >= proxyUDPTTL(entry.result.Status) {
		return false
	}
	r := entry.ipv6
	if r.Status != proxyIPv6Yes || r.UDP != "yes" || !r.valid(now) {
		return false
	}
	return now.Before(r.At.Add(2 * r.Interval))
}
