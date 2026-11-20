package main

import (
	"math/bits"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/connect"
)

// proxyCellStore serves compact per-proxy grid cells from the provider control
// socket. It holds the computed wire bytes for one second so repeated polls
// (top draws every frame) reuse them instead of re-scanning the proxy registry
// and recomputing throughput buckets on every tick.
//
// Packed wire format, 4 bytes per proxy:
//
//	[indexLo, indexHi, state<<5|bucket, clients]
//
// state is one of connect.ProxyCellUp..ProxyCellParked; bucket is 0..7; clients
// is capped at 255. The whole slice is a base64 string on the JSON wire.
type proxyCellStore struct {
	mu               sync.Mutex
	at               time.Time
	raw              []connect.ProxyCellRaw // the sampled pool, shared by Cells and Detail
	deltas           map[int]uint64         // index -> bytes in the last interval
	cells            []byte                 // packed wire bytes, valid while cached
	prev             map[int]uint64         // index -> Total bytes at last sample, for interval deltas
	prevRegistration map[int]connect.ProxyCellRegistration
	prevAt           time.Time
}

// proxyCellCacheTTL is how long the computed grid cells stay valid. A second
// matches top's snapshot cadence and keeps throughput buckets meaningful.
const proxyCellCacheTTL = time.Second

// Allow three times top's slowest built-in polling step (10s), while still
// discarding traffic accumulated during a long-closed top session.
const proxyCellMaxBaselineAge = 30 * time.Second

var (
	proxyCellStoreMu sync.Mutex
	proxyCellStore_  *proxyCellStore
)

func getProxyCellStore() *proxyCellStore {
	proxyCellStoreMu.Lock()
	defer proxyCellStoreMu.Unlock()
	if proxyCellStore_ == nil {
		proxyCellStore_ = &proxyCellStore{prev: map[int]uint64{}}
	}
	return proxyCellStore_
}

// proxyCellDetail is the hover/popup record for one proxy. Built only on
// demand; the grid poll never pays for it.
type proxyCellTraffic struct {
	Rx uint64 `json:"rx"`
	Tx uint64 `json:"tx"`
}

type proxyCellDetail struct {
	LifetimeBillable *proxyCellTraffic `json:"lifetime_billable,omitempty"`
	LifetimeTotal    *proxyCellTraffic `json:"lifetime_total,omitempty"`
	Index            int               `json:"index"`
	Addr             string            `json:"addr"`
	State            uint8             `json:"state"`
	Clients          int64             `json:"clients"`
	IntervalBytes    uint64            `json:"interval_bytes"`
	AgeSeconds       int64             `json:"age_seconds"`
	// UDP is the cached relay check result: ok, no_relay, mismatch, unknown or
	// unchecked, and direct for the direct slot, which has no proxy to check.
	UDP string `json:"udp"`
}

// sample refreshes the cache if expired and returns the raw pool plus the
// per-index interval-byte deltas. Callers hold c.mu.
func (c *proxyCellStore) sample(now time.Time) ([]connect.ProxyCellRaw, map[int]uint64) {
	if len(c.raw) > 0 && !now.Before(c.at) && now.Sub(c.at) < proxyCellCacheTTL {
		return c.raw, c.deltas
	}
	raw := connect.ProxyCells(now)
	deltas := make(map[int]uint64, len(raw))
	nextPrev := make(map[int]uint64, len(raw))
	nextRegistration := make(map[int]connect.ProxyCellRegistration, len(raw))
	buf := make([]byte, 0, 4*len(raw))
	elapsed := now.Sub(c.at)
	validBaseline := !c.at.IsZero() && elapsed > 0 && elapsed <= proxyCellMaxBaselineAge
	for _, p := range raw {
		delta := uint64(0)
		if last, ok := c.prev[p.Index]; validBaseline && ok && c.prevRegistration[p.Index] == p.Registration && p.Total >= last {
			delta = p.Total - last
		}
		deltas[p.Index] = delta
		bucket := proxyThroughputBucket(proxyBytesPerSecond(delta, elapsed))
		state := p.State
		if state > 7 {
			state = 7 // guard: state codes fit in 3 bits
		}
		clients := p.Clients
		if clients > 255 {
			clients = 255
		}
		index := p.Index
		if index < 0 {
			index = 0
		} else if index > 0xFFFF {
			index = 0xFFFF
		}
		buf = append(buf,
			byte(index), byte(index>>8),
			state<<5|bucket,
			byte(clients),
		)
		nextPrev[p.Index] = p.Total
		nextRegistration[p.Index] = p.Registration
	}
	c.prev, c.prevRegistration = nextPrev, nextRegistration
	c.prevAt = c.at
	c.at = now
	c.raw = raw
	c.deltas = deltas
	c.cells = buf
	return raw, deltas
}

// Cells returns the packed grid-cell wire bytes for now, recomputing at most
// once per proxyCellCacheTTL. The registry is read under its single lock; the
// throughput bucket comes from the Total-byte delta divided by elapsed time
// (bytes per second), bucketed logarithmically 0..7. Detail keeps the actual
// interval-byte delta for compatibility. New and stale baselines bucket 0.
func (c *proxyCellStore) Cells(now time.Time) []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sample(now)
	return c.cells
}

// Detail returns the full per-proxy records for the hover/click popup. It
// shares the cached sample with Cells so the popup describes the same pool the
// grid shows. Not polled; called only when the popup opens.
func (c *proxyCellStore) Detail(now time.Time) []proxyCellDetail {
	c.mu.Lock()
	defer c.mu.Unlock()
	raw, deltas := c.sample(now)
	out := make([]proxyCellDetail, 0, len(raw))
	for _, p := range raw {
		clients := p.Clients
		if clients < 0 {
			clients = 0
		}
		detail := proxyCellDetail{
			Index:         p.Index,
			Addr:          proxyCellSafeAddress(p.Index, p.Addr),
			State:         p.State,
			Clients:       clients,
			IntervalBytes: deltas[p.Index],
			AgeSeconds:    int64(p.Age / time.Second),
			UDP:           proxyCellUDPStatus(p, now),
		}
		if p.HasBandwidth {
			detail.LifetimeBillable = &proxyCellTraffic{Rx: p.BillableRx, Tx: p.BillableTx}
			detail.LifetimeTotal = &proxyCellTraffic{Rx: p.TotalRx, Tx: p.TotalTx}
		}
		out = append(out, detail)
	}
	return out
}

// proxyCellSafeAddress limits the detail response to a dial host and port.
func proxyCellSafeAddress(index int, address string) string {
	if index == 0 {
		return "direct (this host)"
	}
	raw := strings.TrimSpace(address)
	if !strings.Contains(raw, "://") {
		raw = "//" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.Port() == "" {
		return "?"
	}
	host := u.Hostname()
	if !connect.ProxyHostAllowed(host) || !connect.ProxyPortAllowed(u.Port()) {
		return "?"
	}
	return net.JoinHostPort(host, u.Port())
}

// proxyBytesPerSecond scales without overflowing even for very large counters.
func proxyBytesPerSecond(delta uint64, elapsed time.Duration) uint64 {
	if elapsed <= 0 {
		return 0
	}
	hi, lo := bits.Mul64(delta, uint64(time.Second))
	denom := uint64(elapsed)
	if hi >= denom {
		return ^uint64(0)
	}
	rate, _ := bits.Div64(hi, lo, denom)
	return rate
}

// proxyThroughputBucket buckets bytes per second into
// eight logarithmic levels so the heatmap can shade cells by activity:
//
//	0 idle, 1 <1KiB, 2 <8KiB, 3 <64KiB, 4 <512KiB,
//	5 <4MiB, 6 <32MiB, 7 >=32MiB
func proxyThroughputBucket(delta uint64) uint8 {
	switch {
	case delta == 0:
		return 0
	case delta < 1<<10:
		return 1
	case delta < 8<<10:
		return 2
	case delta < 64<<10:
		return 3
	case delta < 512<<10:
		return 4
	case delta < 4<<20:
		return 5
	case delta < 32<<20:
		return 6
	default:
		return 7
	}
}

// proxyCellUDPStatus is the UDP check state shown in the hover detail.
func proxyCellUDPStatus(p connect.ProxyCellRaw, now time.Time) string {
	if p.Index == 0 {
		return "direct"
	}
	return globalProxyUDPCheck.statusFor(p.Key, now)
}
