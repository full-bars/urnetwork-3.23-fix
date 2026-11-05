package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
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
	// how long a check is remembered, and how soon an unverifiable one is
	// repeated
	proxyUDPResultTTL  = 30 * time.Minute
	proxyUDPUnknownTTL = 5 * time.Minute

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

// udpExitViaProxy asks a STUN server, through the proxy's UDP relay, which
// address it sees the datagram from.
func udpExitViaProxy(ctx context.Context, settings *connect.ProxySettings) (string, error) {
	pc, err := dialSocks5UDP(ctx, settings.Network, settings.Address, settings.Auth)
	if err != nil {
		return "", err
	}
	defer pc.Close()
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

// checkProxyUDP runs the whole check for one identity.
func checkProxyUDP(ctx context.Context, settings *connect.ProxySettings) proxyUDPResult {
	ctx, cancel := context.WithTimeout(ctx, udpCheckOverallLimit)
	defer cancel()
	udpExit, err := udpExitViaProxy(ctx, settings)
	if err != nil {
		return proxyUDPResult{Status: proxyUDPNoRelay, Err: err}
	}
	tcpExit, err := tcpExitViaProxy(ctx, settings)
	if err != nil {
		return proxyUDPResult{Status: proxyUDPUnknown, UDPExit: udpExit, Err: err}
	}
	if tcpExit != udpExit {
		return proxyUDPResult{Status: proxyUDPExitMismatch, TCPExit: tcpExit, UDPExit: udpExit}
	}
	return proxyUDPResult{Status: proxyUDPOK, TCPExit: tcpExit, UDPExit: udpExit}
}

type proxyUDPCheckEntry struct {
	result proxyUDPResult
	at     time.Time
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

// prune drops identities nobody has asked about for two lifetimes of a result,
// at most once per lifetime, so the store follows the proxy lists instead of
// growing with every proxy ever seen. Called with the lock held.
func (self *proxyUDPCheckStore) prune(now time.Time) {
	if now.Sub(self.lastPrune) < proxyUDPResultTTL {
		return
	}
	self.lastPrune = now
	for key, entry := range self.entries {
		if entry.done == nil && now.Sub(entry.at) > 2*proxyUDPResultTTL {
			delete(self.entries, key)
		}
	}
}

var globalProxyUDPCheck = newProxyUDPCheckStore(checkProxyUDP)

func proxyUDPTTL(status proxyUDPStatus) time.Duration {
	if status == proxyUDPUnknown {
		return proxyUDPUnknownTTL
	}
	return proxyUDPResultTTL
}

func (self *proxyUDPCheckStore) Check(ctx context.Context, settings *connect.ProxySettings, now time.Time) proxyUDPResult {
	key := settings.Key()
	for {
		self.mu.Lock()
		entry := self.entries[key]
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
		if entry != nil && now.Sub(entry.at) < proxyUDPTTL(entry.result.Status) {
			result := entry.result
			self.mu.Unlock()
			return result
		}
		var previous *proxyUDPResult
		if entry != nil {
			copyOf := entry.result
			previous = &copyOf
		}
		self.prune(now)
		running := &proxyUDPCheckEntry{done: make(chan struct{})}
		self.entries[key] = running
		self.mu.Unlock()

		var result proxyUDPResult
		select {
		case self.slots <- struct{}{}:
			result = self.probe(ctx, settings)
			<-self.slots
		case <-ctx.Done():
			result = proxyUDPResult{Status: proxyUDPUnknown, Err: ctx.Err()}
		}

		self.mu.Lock()
		if ctx.Err() != nil {
			// an interrupted check says nothing: forget it
			delete(self.entries, key)
			if previous != nil {
				self.entries[key] = &proxyUDPCheckEntry{result: *previous, at: now.Add(-proxyUDPTTL(previous.Status))}
			}
			close(running.done)
			self.mu.Unlock()
			return proxyUDPResult{Status: proxyUDPUnknown, Err: ctx.Err()}
		}
		running.result = result
		running.at = now
		close(running.done)
		running.done = nil
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
	if entry := self.entries[key]; entry != nil && entry.done == nil && now.Sub(entry.at) < proxyUDPTTL(entry.result.Status) {
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
	index := settings.Index
	switch result.Status {
	case proxyUDPOK:
		tlog("[proxy][udp] proxy[%d] (%s) udp relay ok, exit %s matches tcp; h3 and client udp allowed\n", index, redactedProxyAddress(settings.Address), result.UDPExit)
	case proxyUDPExitMismatch:
		tlog("[proxy][udp] proxy[%d] (%s) udp exits from %s but tcp from %s; h3 and client udp not used for this proxy\n", index, redactedProxyAddress(settings.Address), result.UDPExit, result.TCPExit)
	case proxyUDPNoRelay:
		tlog("[proxy][udp] proxy[%d] (%s) no udp relay (%v); h3 and client udp not used for this proxy, tcp only\n", index, redactedProxyAddress(settings.Address), result.Err)
	default:
		tlog("[proxy][udp] proxy[%d] (%s) udp relay works (exit %s) but the tcp exit could not be learned (%v); h3 and client udp allowed, checking again soon\n", index, redactedProxyAddress(settings.Address), result.UDPExit, result.Err)
	}
}

// size is how many identities the store currently remembers.
func (self *proxyUDPCheckStore) size() int {
	self.mu.Lock()
	defer self.mu.Unlock()
	return len(self.entries)
}
