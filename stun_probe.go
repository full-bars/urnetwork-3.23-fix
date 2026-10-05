package connect

import (
	"bytes"
	"context"
	"crypto/rand"
	"net"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// stun_probe.go adds an ACTIVE per-STUN-endpoint x per-IP-family reachability
// probe on top of the passive [stun] aggregate in stun_tally.go.
//
// Why it exists: the passive tally can only tell the operator "N successes /
// M failures happened somewhere." A successful srflx candidate is anonymous
// (pion does not log which STUN server answered), so passive counting can
// never attribute a success to a specific STUN host. This file answers the
// per-provider / per-family question directly by issuing a real STUN binding
// request to every configured endpoint, over both IPv4 and IPv6, and folding
// the resulting per-provider status into the emitted [stun] line.
//
// This is log-only: it (and stun_tally.go) observe reachability and never
// alter ICE, NAT, connection, or endpoint-selection behavior.
//
// STUN binding is transported as a single UDP datagram: a minimal request is
// the 20-byte header (message type 0x0001, length 0, magic cookie
// 0x2112A442, 12-byte transaction ID) with no attributes. A server reply whose
// magic cookie and transaction ID echo ours, with message type 0x0101 (Binding
// Success) or carrying a XOR-MAPPED-ADDRESS (0x0020) attribute, counts as a
// success. We hand-roll that client (instead of pulling in pion/transport's
// stun package) so the per-transaction timeout, retransmission, and result
// classification are fully under our control and there is no dependency churn.

const (
	// stunHeaderSize is the fixed STUN message header length in bytes.
	stunHeaderSize = 20
	// stunMsgBindingResp are the STUN Binding request and
	// success-response message types.
	stunMsgBinding     = 0x0001
	stunMsgBindingResp = 0x0101
	// stunAttrXorMapped is the XOR-MAPPED-ADDRESS attribute type.
	// NB: the STUN magic cookie (0x2112A442) already exists as the package-level
	// stunMagicCookie in ip_security_webstandard.go and is reused here.
	stunAttrXorMapped = 0x0020
	// stunDefaultPort is used when an endpoint omits its port.
	stunDefaultPort = "3478"
	// stunProbeTimeout bounds the write-to-ack round trip for one binding
	// transaction (set as the socket's read/write deadline).
	stunProbeTimeout = 2 * time.Second
	// stunProbeInterval is the background probe cadence. It matches
	// stunLowInterval so a quiet host produces at most one line per cycle.
	stunProbeInterval = stunLowInterval
)

type stunFamilyResult int

const (
	stunFamNA   stunFamilyResult = iota // no route / no A(AAA) record / v6 disabled
	stunFamOK                           // at least one binding success
	stunFamFail                         // probed and failed
)

func (r stunFamilyResult) String() string {
	switch r {
	case stunFamOK:
		return "ok"
	case stunFamFail:
		return "fail"
	default:
		return "n/a"
	}
}

// stunProbe holds the latest active-probe snapshot, shared with the emit path.
var stunProbe = &stunProber{}

// stunProber owns the background probe loop and the cached result snapshot.
// run() is called on a slow cadence; snapshot() renders the suffix appended to
// the emitted [stun] line.
type stunProber struct {
	mu     sync.Mutex
	result *stunProbeResult
}

type stunProbeResult struct {
	// At is when the snapshot was taken.
	At time.Time
	// Providers maps a provider key (google/meteredca/stunprotocol/...) to the
	// per-family reachability observed across the provider's endpoints.
	Providers map[string][2]stunFamilyResult // [0]=v4, [1]=v6
}

// providerOrder is the stable render order for the emitted suffix.
func (r *stunProbeResult) providerOrder() []string {
	keys := make([]string, 0, len(r.Providers))
	for k := range r.Providers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// suffix renders " | google: v4=ok v6=ok | meteredca: ..." compactly. Empty
// string when no probe has completed yet (e.g. in unit tests).
func (p *stunProber) suffix() string {
	p.mu.Lock()
	r := p.result
	p.mu.Unlock()
	if r == nil {
		return ""
	}
	parts := []string{}
	for _, prov := range r.providerOrder() {
		fam := r.Providers[prov]
		parts = append(parts, prov+": v4="+fam[0].String()+" v6="+fam[1].String())
	}
	return " | " + strings.Join(parts, " · ")
}

// startStunProbe begins the background loop on first real (non-test) use.
// Guarded so concurrent callers and repeated triggers are safe.
var (
	stunProbeOnce sync.Once
	stunProbeOn   bool
	// stunProbeStop ends the loop. Closed by StopStunProbe, once.
	stunProbeStop   = make(chan struct{})
	stunProbeStopOn sync.Once
)

func startStunProbe() {
	stunProbeOnce.Do(func() {
		stunProbeOn = true
		go func() {
			for {
				select {
				case <-stunProbeStop:
					return
				default:
				}
				stunProbe.run()
				// Wait out the interval, but wake immediately on a stop: a
				// shutdown must not sit behind a five-minute sleep.
				select {
				case <-stunProbeStop:
					return
				case <-time.After(stunProbeInterval):
				}
			}
		}()
	})
}

// StopStunProbe ends the background probe loop. Safe to call when it was never
// started, and safe to call more than once.
func StopStunProbe() {
	stunProbeStopOn.Do(func() {
		close(stunProbeStop)
	})
}

// run executes one probe cycle: it looks up each configured endpoint (plus the
// probe-only cloudflare host), resolves both address families, issues a STUN
// binding request to each host:family concurrently, aggregates results by
// provider, and stores the snapshot. It never panics; any single probe failure
// just records stunFamFail/stunFamNA for that endpoint.
func (p *stunProber) run() {
	defer func() { recover() }() // keep the loop alive no matter what

	settings := probeSettings()
	endpoints := stunProbeEndpoints(settings)

	v6Avail := ipv6Available() // reuse the existing gate: no route => n/a

	type res struct {
		prov string
		fam  int // 0=v4, 1=v6
		ok   stunFamilyResult
	}
	var (
		mu  sync.Mutex
		out []res
		wg  sync.WaitGroup
	)
	for _, url := range endpoints {
		host, port := stunEndpoint(url)
		if host == "" {
			continue
		}
		prov := stunProviderOf(host)
		for fam, familyName := range []string{"v4", "v6"} {
			wg.Add(1)
			go func(h, pr string, fi int, fn string) {
				defer wg.Done()
				// Probe OUTSIDE the lock: a probe is a socket round trip, and
				// evaluating it as an argument to the append below held mu for
				// the whole cycle, serializing every endpoint behind the first
				// slow one.
				ok := probeEndpoint(h, pr, fn, v6Avail)
				mu.Lock()
				out = append(out, res{prov, fi, ok})
				mu.Unlock()
			}(host, port, fam, familyName)
		}
	}
	wg.Wait()

	providers := map[string][2]stunFamilyResult{}
	for _, r := range out {
		cur := providers[r.prov]
		cur[r.fam] = aggregateFamily(cur[r.fam], r.ok)
		providers[r.prov] = cur
	}

	p.mu.Lock()
	p.result = &stunProbeResult{At: time.Now(), Providers: providers}
	p.mu.Unlock()
}

// activeWebRtcSettings is the settings ICE actually gathers from in this process.
// The probe reports reachability for the endpoints a client really uses, so it
// follows the active client's list rather than the compiled defaults — a client
// may be given different IceServerUrls, and the probe must not report endpoints
// ICE never gathers from.
var activeWebRtcSettings atomic.Pointer[WebRtcSettings]

// SetActiveWebRtcSettings records the settings a client's ICE uses. Called where
// the WebRTC manager is created.
func SetActiveWebRtcSettings(settings *WebRtcSettings) {
	if settings != nil {
		activeWebRtcSettings.Store(settings)
	}
}

// probeSettings returns the active ICE settings, or the defaults before any
// client exists (unit tests, and a probe that somehow starts first).
func probeSettings() *WebRtcSettings {
	if settings := activeWebRtcSettings.Load(); settings != nil {
		return settings
	}
	return DefaultWebRtcSettings()
}

// stunProbeEndpoints returns the endpoints one probe cycle covers: every
// configured ICE server, plus the probe-only cloudflare host (probed for
// visibility, deliberately kept OUT of IceServerUrls so it never affects ICE
// candidate gathering). Deduplicated by host:port, so an endpoint listed twice —
// or also configured in ICE — is probed once per cycle.
func stunProbeEndpoints(settings *WebRtcSettings) []string {
	all := append(append([]string{}, settings.IceServerUrls...), "stun:stun.cloudflare.com:3478")
	seen := map[string]bool{}
	out := make([]string, 0, len(all))
	for _, url := range all {
		host, port := stunEndpoint(url)
		if host == "" {
			continue
		}
		key := host + ":" + port
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, url)
	}
	return out
}

// aggregateFamily merges a new per-endpoint result into an existing per-provider
// family status. Precedence: ok > fail > n/a, so one reachable endpoint marks
// the whole provider-family reachable (we only need any working server).
func aggregateFamily(cur, next stunFamilyResult) stunFamilyResult {
	if next == stunFamOK || cur == stunFamOK {
		return stunFamOK
	}
	if next == stunFamFail || cur == stunFamFail {
		return stunFamFail
	}
	if next == stunFamNA || cur == stunFamNA {
		return stunFamNA
	}
	return cur
}

// stunProviderOf maps a STUN hostname to its operator/provider group.
func stunProviderOf(host string) string {
	switch {
	case strings.HasSuffix(host, "google.com"):
		return "google"
	case strings.HasSuffix(host, "metered.ca"):
		return "meteredca"
	case strings.HasSuffix(host, "stunprotocol.org"):
		return "stunprotocol"
	case strings.HasSuffix(host, "cloudflare.com"):
		return "cloudflare"
	}
	if i := strings.IndexByte(host, '.'); i > 0 {
		return host[:i]
	}
	return host
}

// stunEndpoint splits a "stun:host:port" config value into host and port,
// defaulting the port to 3478 when absent.
func stunEndpoint(raw string) (host, port string) {
	s := strings.TrimPrefix(raw, "stun://")
	s = strings.TrimPrefix(s, "stun:")
	if h, p, err := net.SplitHostPort(s); err == nil && p != "" {
		return h, p
	}
	if s == "" {
		return "", ""
	}
	return s, stunDefaultPort
}

// lookupHostBounded resolves host under the same deadline as the probe itself,
// so a resolver that never answers cannot hold the probe cycle open.
func lookupHostBounded(host string) ([]net.IP, error) {
	ctx, cancel := context.WithTimeout(context.Background(), stunProbeTimeout)
	defer cancel()
	return net.DefaultResolver.LookupIP(ctx, "ip", host)
}

// probeEndpoint resolves familyName on host and issues one STUN binding
// request, returning the per-endpoint family result.
func probeEndpoint(host, port, familyName string, v6Avail bool) stunFamilyResult {
	network := "udp4"
	if familyName == "v6" {
		network = "udp6"
		if !v6Avail {
			return stunFamNA // no IPv6 route: skip, not a failure
		}
	}
	ips, err := lookupHostBounded(host)
	if err != nil {
		return stunFamFail // host named by config but unresolvable
	}
	var target net.IP
	for _, ip := range ips {
		isV4 := ip.To4() != nil
		if (familyName == "v4") == isV4 {
			target = ip
			break
		}
	}
	if target == nil {
		return stunFamNA // no A record (or no AAAA record) for this family
	}
	conn, err := net.DialTimeout(network, net.JoinHostPort(target.String(), port), stunProbeTimeout)
	if err != nil {
		return stunFamFail
	}
	defer conn.Close()
	if !stunBindingRequest(conn) {
		return stunFamFail
	}
	return stunFamOK
}

// stunBindingRequest sends a minimal STUN Binding request on conn and reports
// whether the server answered with a matching success response. One
// transaction, no retransmission; the socket deadline bounds the round trip.
func stunBindingRequest(conn net.Conn) bool {
	_ = conn.SetDeadline(time.Now().Add(stunProbeTimeout))

	var txid [12]byte
	if _, err := rand.Read(txid[:]); err != nil {
		return false
	}

	req := make([]byte, stunHeaderSize)
	putU16(req[0:2], stunMsgBinding)
	putU16(req[2:4], 0) // no attributes: length 0
	putU32(req[4:8], stunMagicCookie)
	copy(req[8:20], txid[:])

	if _, err := conn.Write(req); err != nil {
		return false
	}

	resp := make([]byte, 512)
	n, err := conn.Read(resp)
	if err != nil || n < stunHeaderSize {
		return false
	}
	if !isStunSuccessResponse(resp[:n], txid[:]) {
		return false
	}
	return true
}

// isStunSuccessResponse reports whether a received datagram is a Binding Success
// Response to OUR transaction that carries the reflected address. Everything
// else is not a success: a mismatched cookie or transaction id, an error
// response (even one carrying bytes shaped like an attribute), or a success
// response with no XOR-MAPPED-ADDRESS.
func isStunSuccessResponse(msg, txid []byte) bool {
	if len(msg) < stunHeaderSize {
		return false
	}
	if putU32nil(msg[4:8]) != stunMagicCookie || !bytes.Equal(msg[8:20], txid) {
		return false // not a response to our transaction
	}
	if putU16nil(msg[0:2]) != stunMsgBindingResp {
		return false
	}
	// Attributes live inside the body the header declares, so the scan is bounded
	// by it: bytes past the declared length are not part of this message, and a
	// body the datagram does not carry is not a response at all.
	bodyByteCount := int(putU16nil(msg[2:4]))
	if len(msg) < stunHeaderSize+bodyByteCount {
		return false
	}
	return hasXorMappedAddress(msg[:stunHeaderSize+bodyByteCount])
}

// hasXorMappedAddress scans a STUN message's attribute section for a
// XOR-MAPPED-ADDRESS (0x0020) attribute — the presence of which is the canonical
// "the server reflected our address" success signal.
func hasXorMappedAddress(msg []byte) bool {
	for i := stunHeaderSize; i+4 <= len(msg); {
		attrType := putU16nil(msg[i : i+2])
		attrLen := int(putU16nil(msg[i+2 : i+4]))
		if attrType == stunAttrXorMapped {
			return true
		}
		i += 4 + attrLen
		i = (i + 3) &^ 3 // attributes are padded to 4-byte boundaries
	}
	return false
}

// putU16 / putU32 write big-endian network values into a byte slice.
func putU16(b []byte, v uint16) { b[0], b[1] = byte(v>>8), byte(v) }
func putU32(b []byte, v uint32) {
	b[0], b[1], b[2], b[3] = byte(v>>24), byte(v>>16), byte(v>>8), byte(v)
}

// putU16nil / putU32nil read big-endian network values from a byte slice
// without length checks (callers guard slice sizes beforehand).
func putU16nil(b []byte) uint16 { return uint16(b[0])<<8 | uint16(b[1]) }
func putU32nil(b []byte) uint32 {
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}
