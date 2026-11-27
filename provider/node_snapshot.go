package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/connect"
)

// NodeSnapshot is the typed picture of the node served by the control socket
// "snapshot" command. Its JSON shape is the wire contract with urnet-tools,
// pinned by the fixtures in testdata/livestatus. Fields are only ever added.
// Optional fields are omitted when unknown, never zeroed.
type NodeSnapshot struct {
	V               int       `json:"v"`
	Version         string    `json:"version"`
	PreviousVersion string    `json:"previous_version,omitempty"`
	StartedAt       time.Time `json:"started_at"`
	Now             time.Time `json:"now"`
	UptimeSeconds   float64   `json:"uptime_seconds"`

	// State is one of starting, degraded, idle, flowing.
	State string `json:"state"`
	// StateReason says why the node is starting or degraded (a startup phase
	// that has not finished, dead proxies, pressure). Empty for idle and
	// flowing, and for an ordinary warmup.
	StateReason    string `json:"state_reason,omitempty"`
	Busy           bool   `json:"busy"`
	RestartPending bool   `json:"restart_pending"`

	Rate     SnapshotRate     `json:"rate"`
	Clients  int64            `json:"clients"`
	Sessions SnapshotSessions `json:"sessions"`
	Proxies  SnapshotProxies  `json:"proxies"`
	Pressure float64          `json:"pressure"`
	Restart  SnapshotRestart  `json:"restart"`

	Resources SnapshotResources `json:"resources"`

	// IdleHint says why the node is idle. Set only when State is idle.
	IdleHint string `json:"idle_hint,omitempty"`

	// Traffic carries the byte totals and the total-traffic rate that sit next
	// to the billable Rate. The provider always sets it; it is a pointer so a
	// snapshot from a provider that predates it (and the fixtures that stand
	// for one) round-trips without it.
	Traffic *SnapshotTraffic `json:"traffic,omitempty"`
}

// SnapshotTraffic is billable versus total traffic. Rate stays the billable
// rate; this adds what was actually moved. Session totals count from provider
// start and are built from the same deltas as the rates, so a proxy that is
// removed or respawns never adds or subtracts bytes.
type SnapshotTraffic struct {
	BillableBytes uint64 `json:"billable_bytes"`
	TotalBytes    uint64 `json:"total_bytes"`
	// LifetimeBillableBytes is the persisted lifetime total. Omitted when the
	// lifetime store is not running.
	LifetimeBillableBytes *uint64 `json:"lifetime_billable_bytes,omitempty"`

	// The total-traffic rate, sampled the same way and at the same interval as
	// Rate.HistoryBps.
	TotalNowBps     int64   `json:"total_now_bps"`
	TotalAvg1mBps   int64   `json:"total_avg_1m_bps"`
	TotalAvg5mBps   int64   `json:"total_avg_5m_bps"`
	TotalHistoryBps []int64 `json:"total_history_bps"`

	// The per-direction block, nil from a provider that predates the counters.
	// It carries the same rate and history figures split by rx and tx, which is
	// how the reader tells download from upload.
	Directions *SnapshotTrafficDirections `json:"directions,omitempty"`
	Packets    *SnapshotTrafficPackets    `json:"packets,omitempty"`
}

// SnapshotTrafficDirection is one direction's billable or total rate, sampled
// the same way and at the same interval as SnapshotRate, with the same per-second
// history.
type SnapshotTrafficDirection struct {
	NowBps     int64   `json:"now_bps"`
	Avg1mBps   int64   `json:"avg_1m_bps"`
	Avg5mBps   int64   `json:"avg_5m_bps"`
	HistoryBps []int64 `json:"history_bps"`
}

// SnapshotTrafficDirections is the billable versus total split by direction:
// rx (download, toward the client) and tx (upload, away from the client) per
// quantity. Nil at the provider means the running provider did not report the
// directional counters.
type SnapshotTrafficDirections struct {
	BillableRx SnapshotTrafficDirection `json:"billable_rx"`
	BillableTx SnapshotTrafficDirection `json:"billable_tx"`
	TotalRx    SnapshotTrafficDirection `json:"total_rx"`
	TotalTx    SnapshotTrafficDirection `json:"total_tx"`
	// Families is the per-family split, nil when the running provider has no
	// IPv6 counters to split on.
	Families *SnapshotTrafficFamilies `json:"families,omitempty"`
}

// SnapshotFamilySplit is one directional quantity split by IP family: V4
// derived, V6 counted directly.
type SnapshotFamilySplit struct {
	V4 SnapshotTrafficDirection `json:"v4"`
	V6 SnapshotTrafficDirection `json:"v6"`
}

// SnapshotTrafficFamilies is the IP family split of the directional quantities.
type SnapshotTrafficFamilies struct {
	BillableRx SnapshotFamilySplit `json:"billable_rx"`
	BillableTx SnapshotFamilySplit `json:"billable_tx"`
	TotalRx    SnapshotFamilySplit `json:"total_rx"`
	TotalTx    SnapshotFamilySplit `json:"total_tx"`
}

// SnapshotPacketRate reports billable IP packets per second.
type SnapshotPacketRate struct {
	NowPps     int64   `json:"now_pps"`
	Avg1mPps   int64   `json:"avg_1m_pps"`
	Avg5mPps   int64   `json:"avg_5m_pps"`
	HistoryPps []int64 `json:"history_pps"`
}

type SnapshotTrafficPackets struct {
	BillableRx SnapshotPacketRate `json:"billable_rx"`
	BillableTx SnapshotPacketRate `json:"billable_tx"`
}

// LiveTraffic is the light reply to the "traffic" command: live counter sums and
// the provider's clock at the moment they were read. urnet-tools top polls it
// at 100ms and derives rates from the deltas, so it never rebuilds a snapshot.
// The sums drop when a proxy is removed or respawns; readers skip a decrease.
//
// The four per-direction fields are always sent, so a zero is a real zero. A
// reader that does not find them at all is talking to a provider that predates
// them and has no direction split to draw.
type LiveTraffic struct {
	AtUnixNano    int64  `json:"at_unix_nano"`
	BillableBytes uint64 `json:"billable_bytes"`
	TotalBytes    uint64 `json:"total_bytes"`

	BillableRxBytes   uint64 `json:"billable_rx_bytes"`
	BillableTxBytes   uint64 `json:"billable_tx_bytes"`
	TotalRxBytes      uint64 `json:"total_rx_bytes"`
	TotalTxBytes      uint64 `json:"total_tx_bytes"`
	BillableRxPackets uint64 `json:"billable_rx_packets"`
	BillableTxPackets uint64 `json:"billable_tx_packets"`

	// The IPv6 share of each byte counter above. Always sent: a provider built
	// from this tree counts IPv6, and an older provider has no field at all,
	// which the client reads as nil.
	BillableRxBytesV6 uint64 `json:"billable_rx_bytes_v6"`
	BillableTxBytesV6 uint64 `json:"billable_tx_bytes_v6"`
	TotalRxBytesV6    uint64 `json:"total_rx_bytes_v6"`
	TotalTxBytesV6    uint64 `json:"total_tx_bytes_v6"`

	// Transports is the per-kind transport block. A provider that predates it
	// sends nothing here.
	Transports *LiveTransports `json:"transports,omitempty"`
}

// LiveTransport is one transport kind's cumulative payload counters and the
// number of its connections up now. The counters are cumulative like the byte
// totals, so a reader derives a window share from two samples.
type LiveTransport struct {
	BytesTx  uint64 `json:"bytes_tx"`
	BytesRx  uint64 `json:"bytes_rx"`
	FramesTx uint64 `json:"frames_tx"`
	FramesRx uint64 `json:"frames_rx"`
	Up       int64  `json:"up"`
}

// LiveTransports is the per-kind transport block of the traffic reply. The two
// H1 carriers are always present because they are the segments the bar splits;
// a mode the provider never ran is left out rather than sent as zeros.
type LiveTransports struct {
	H1WebSocket *LiveTransport `json:"h1_websocket,omitempty"`
	H1Plus      *LiveTransport `json:"h1_plus,omitempty"`
	H3          *LiveTransport `json:"h3,omitempty"`
	DNS         *LiveTransport `json:"dns,omitempty"`
	DNSPump     *LiveTransport `json:"dns_pump,omitempty"`
}

// liveTransportsSample copies the mode counters and the live H1 gauge into the
// transports block.
func liveTransportsSample(modes connect.TransportModeStatsSnapshot, conns connect.H1ConnectionStatsSnapshot) *LiveTransports {
	transports := &LiveTransports{
		H1WebSocket: &LiveTransport{
			BytesTx:  modes.H1WebSocketBytesTx,
			BytesRx:  modes.H1WebSocketBytesRx,
			FramesTx: modes.H1WebSocketFramesTx,
			FramesRx: modes.H1WebSocketFramesRx,
			Up:       conns.WebSocketConnectionCount,
		},
		H1Plus: &LiveTransport{
			BytesTx:  modes.H1PlusBytesTx,
			BytesRx:  modes.H1PlusBytesRx,
			FramesTx: modes.H1PlusFramesTx,
			FramesRx: modes.H1PlusFramesRx,
			Up:       conns.H1PlusConnectionCount,
		},
	}
	if carried(modes.H3FramesTx, modes.H3FramesRx, modes.H3BytesTx, modes.H3BytesRx, modes.H3Up) {
		transports.H3 = &LiveTransport{
			BytesTx:  modes.H3BytesTx,
			BytesRx:  modes.H3BytesRx,
			FramesTx: modes.H3FramesTx,
			FramesRx: modes.H3FramesRx,
			Up:       modes.H3Up,
		}
	}
	if carried(modes.Dns.FramesTx, modes.Dns.FramesRx, modes.Dns.BytesTx, modes.Dns.BytesRx, modes.Dns.Up) || modes.Dns.Drops > 0 {
		transports.DNS = &LiveTransport{
			BytesTx:  modes.Dns.BytesTx,
			BytesRx:  modes.Dns.BytesRx,
			FramesTx: modes.Dns.FramesTx,
			FramesRx: modes.Dns.FramesRx,
			Up:       modes.Dns.Up,
		}
	}
	if carried(modes.DnsPump.FramesTx, modes.DnsPump.FramesRx, modes.DnsPump.BytesTx, modes.DnsPump.BytesRx, modes.DnsPump.Up) || modes.DnsPump.Drops > 0 {
		transports.DNSPump = &LiveTransport{
			BytesTx:  modes.DnsPump.BytesTx,
			BytesRx:  modes.DnsPump.BytesRx,
			FramesTx: modes.DnsPump.FramesTx,
			FramesRx: modes.DnsPump.FramesRx,
			Up:       modes.DnsPump.Up,
		}
	}
	return transports
}

// carried reports whether a mode has ever carried a frame or has a connection
// up now. A mode that has never been used is left out of the block.
func carried(framesTx, framesRx, bytesTx, bytesRx uint64, up int64) bool {
	return framesTx|framesRx|bytesTx|bytesRx != 0 || up != 0
}

// liveTrafficSample reads the counter sums at now.
func liveTrafficSample(now time.Time, totals func() (billableRx, billableTx, totalRx, totalTx uint64), packets func() (uint64, uint64), families func() connect.ProxyBandwidthFamily) *LiveTraffic {
	billableRx, billableTx, totalRx, totalTx := totals()
	rxPackets, txPackets := packets()
	f := families()
	return &LiveTraffic{
		AtUnixNano:        now.UnixNano(),
		BillableBytes:     billableRx + billableTx,
		TotalBytes:        totalRx + totalTx,
		BillableRxBytes:   billableRx,
		BillableTxBytes:   billableTx,
		TotalRxBytes:      totalRx,
		TotalTxBytes:      totalTx,
		BillableRxPackets: rxPackets,
		BillableTxPackets: txPackets,
		Transports:        liveTransportsSample(connect.TransportModeStats(), h1ConnectionProcessStats.Snapshot()),
		BillableRxBytesV6: f.BillableRx,
		BillableTxBytesV6: f.BillableTx,
		TotalRxBytesV6:    f.TotalRx,
		TotalTxBytesV6:    f.TotalTx,
	}
}

// SnapshotRate is billable throughput in bytes per second. HistoryBps holds
// per-second samples, oldest first, at most snapshotRingSize of them.
type SnapshotRate struct {
	NowBps          int64 `json:"now_bps"`
	Avg1mBps        int64 `json:"avg_1m_bps"`
	Avg5mBps        int64 `json:"avg_5m_bps"`
	HistoryInterval int   `json:"history_interval_seconds"`
	// HistorySeq is the absolute index of the newest sample in HistoryBps: the
	// number of samples ever recorded. A reader that draws the history against
	// this, not against a clock, sees each sample keep its place from one
	// snapshot to the next however the poll and the sampler tick line up.
	HistorySeq uint64  `json:"history_seq,omitempty"`
	HistoryBps []int64 `json:"history_bps"`
}

type SnapshotSessions struct {
	PQE       int64 `json:"pqe"`
	Classical int64 `json:"classical"`
}

type SnapshotProxies struct {
	UDP        *SnapshotProxyUDP  `json:"udp,omitempty"`
	IPv6       *SnapshotProxyIPv6 `json:"ipv6,omitempty"`
	Up         int                `json:"up"`
	Degraded   int                `json:"degraded"`
	Connecting int                `json:"connecting"`
	Dead       int                `json:"dead"`
	Parked     int                `json:"parked,omitempty"`
	// Configured is the pool size before the trim cap; TrimCap is the
	// effective cap on running proxies (0 = none) and TrimCapSource says who
	// set it (operator, oomcap, thrashcap). Omitted before the first trim
	// decision, so a reader cannot mistake "not measured yet" for "no cap".
	TrimReported  bool   `json:"trim_reported,omitempty"`
	Configured    int    `json:"configured,omitempty"`
	TrimCap       int    `json:"trim_cap,omitempty"`
	TrimCapSource string `json:"trim_cap_source,omitempty"`
}

// SnapshotProxyUDP counts online proxies (up or degraded) by non-expired UDP
// result. Unchecked has no usable result. The five counts sum to online.
// Checked is ok + no_relay + mismatch + unknown; percentages are ok/checked
// and ok/online, and are not defined when their denominator is zero.
type SnapshotProxyUDP struct {
	OK        int `json:"ok"`
	NoRelay   int `json:"no_relay"`
	Mismatch  int `json:"mismatch"`
	Unknown   int `json:"unknown"`
	Unchecked int `json:"unchecked"`
}

// SnapshotProxyIPv6 counts online proxies by cached IPv6 capability and UDP relay result.
type SnapshotProxyIPv6 struct {
	Yes        int   `json:"yes"`
	No         int   `json:"no"`
	Unknown    int   `json:"unknown"`
	NotProbed  int   `json:"not_probed"`
	Probes     int64 `json:"probes"`
	UDPYes     int   `json:"udp_yes"`
	UDPNo      int   `json:"udp_no"`
	UDPUnknown int   `json:"udp_unknown"`
}

type SnapshotRestart struct {
	Reason        string `json:"reason"`
	CleanShutdown bool   `json:"clean_shutdown"`
}

// SnapshotResources fields the platform cannot supply are left zero and
// omitted from the JSON.
type SnapshotResources struct {
	HeapInuseBytes uint64  `json:"heap_inuse_bytes"`
	MemLimitBytes  uint64  `json:"mem_limit_bytes,omitempty"`
	RSSBytes       uint64  `json:"rss_bytes,omitempty"`
	Goroutines     int     `json:"goroutines"`
	OpenFDs        uint64  `json:"open_fds,omitempty"`
	FDLimit        uint64  `json:"fd_limit,omitempty"`
	Threads        uint64  `json:"threads,omitempty"`
	TasksMax       uint64  `json:"tasks_max,omitempty"`
	SwapUsedBytes  *uint64 `json:"swap_used_bytes,omitempty"`
	SwapTotalBytes uint64  `json:"swap_total_bytes,omitempty"`
}

const (
	snapshotSchemaVersion = 1

	// snapshotRingSize is 10 minutes of one-second rate samples.
	snapshotRingSize = 600

	// snapshotCacheTTL is how long Get() reuses a built snapshot.
	snapshotCacheTTL = time.Second

	// snapshotStartingWindow is how long after start the state reads
	// "starting" regardless of traffic.
	snapshotStartingWindow = 120 * time.Second

	// snapshotIdleBps is the 1 minute rate below which the node is idle. It
	// is the same 5 KiB/s default `urnet-tools update --idle` uses.
	snapshotIdleBps = 5120

	// snapshotHistoryInterval and snapshotHistorySize keep 10 minutes of
	// cumulative counter samples for the "in the last 10 min" idle hints.
	snapshotHistoryInterval = 10 * time.Second
	snapshotHistorySize     = 60

	// The idle hint blames auth only when failures are a wave or the pool is
	// mostly down, never for a trickle. A large paid pool always has a few
	// percent of its proxies failing and retrying every minute, so "any
	// failure at all" read as an outage on a healthy node. A wave is at least
	// 1/snapshotAuthWaveDivisor of the pool failing within
	// snapshotAuthWaveWindow, with a floor so a tiny pool is not a hair trigger.
	// snapshotStartupStuckAfter is how long a node may stay "starting" because
	// proxy startup has not finished before it reads degraded instead. A big
	// pool legitimately takes a couple of minutes; this is a stall.
	snapshotStartupStuckAfter = 5 * time.Minute

	snapshotAuthWaveWindow  = time.Minute
	snapshotAuthWaveDivisor = 4
	snapshotAuthWaveFloor   = 3
)

// rateSampler turns cumulative per-proxy billable byte counters into a
// per-second rate ring. It uses the same delta method as runBillableRateWriter:
// keys present in both samples contribute their delta, new and removed keys
// are ignored, and a counter that went down (a proxy respawn hands back a
// zeroed ProxyBandwidth) contributes nothing for that tick.
type rateSampler struct {
	mu       sync.Mutex
	prev     map[string]uint64
	prevTime time.Time
	ring     [snapshotRingSize]int64
	next     int // slot the next sample is written to
	count    int // valid samples, capped at snapshotRingSize
	// sessionBytes is every delta counted so far, so the total since the
	// provider started survives proxies being removed and respawned.
	sessionBytes uint64
	// recorded counts every sample ever written to the ring, so the newest
	// sample has an absolute index that never depends on a clock.
	recorded uint64
}

func newRateSampler() *rateSampler {
	return &rateSampler{}
}

// sample records one tick. cur maps proxy key to cumulative billable bytes
// (rx+tx). The first call only establishes the baseline and records nothing.
func (s *rateSampler) sample(cur map[string]uint64, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	first := s.prev == nil
	var delta uint64
	for k, v := range cur {
		if p, ok := s.prev[k]; ok && v >= p {
			delta += v - p
		}
	}
	next := make(map[string]uint64, len(cur))
	for k, v := range cur {
		next[k] = v
	}
	elapsed := now.Sub(s.prevTime).Seconds()
	s.prev = next
	s.prevTime = now
	if first {
		return
	}
	s.sessionBytes += delta
	if elapsed < 1 {
		elapsed = 1
	}
	s.ring[s.next] = int64(float64(delta) / elapsed)
	s.recorded++
	s.next = (s.next + 1) % snapshotRingSize
	if s.count < snapshotRingSize {
		s.count++
	}
}

// seq returns how many samples have ever been recorded: the absolute index of
// the newest sample in history().
func (s *rateSampler) seq() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recorded
}

// session returns the bytes counted since the provider started.
func (s *rateSampler) session() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessionBytes
}

// history returns the samples oldest first. Never nil.
func (s *rateSampler) history() []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]int64, s.count)
	start := (s.next - s.count + snapshotRingSize) % snapshotRingSize
	for i := 0; i < s.count; i++ {
		out[i] = s.ring[(start+i)%snapshotRingSize]
	}
	return out
}

// rates returns the newest sample and the averages over the last 60 and 300
// samples, or over however many exist yet.
func (s *rateSampler) rates() (now, avg1m, avg5m int64) {
	h := s.history()
	if len(h) == 0 {
		return 0, 0, 0
	}
	return h[len(h)-1], avgTail(h, 60), avgTail(h, 300)
}

func avgTail(h []int64, n int) int64 {
	if len(h) < n {
		n = len(h)
	}
	if n == 0 {
		return 0
	}
	var sum int64
	for _, v := range h[len(h)-n:] {
		sum += v
	}
	return sum / int64(n)
}

// directionLanes is one proxy's cumulative byte counters, one field per
// quantity and per direction.
type directionLanes struct {
	billableRx, billableTx uint64
	totalRx, totalTx       uint64
	packetRx, packetTx     uint64

	billableRxV6, billableTxV6 uint64
	totalRxV6, totalTxV6       uint64
}

// The directional lanes, in sampler order: billable rx, billable tx, total rx,
// total tx, plus their IPv6 shares.
const (
	dirBillableRx = iota
	dirBillableTx
	dirTotalRx
	dirTotalTx
	dirPacketRx
	dirPacketTx
	dirBillableRxV6
	dirBillableTxV6
	dirTotalRxV6
	dirTotalTxV6
	directionLaneCount
)

// directionSampler turns the per-proxy directional counters into one rate ring
// per quantity and direction. It uses the same delta method as rateSampler: a key
// present in both samples contributes its delta, new and removed keys are
// ignored, and a counter that went down (a proxy respawn hands back a zeroed
// ProxyBandwidth) contributes nothing for that tick.
type directionSampler struct {
	mu       sync.Mutex
	prev     map[string]directionLanes
	prevTime time.Time
	rings    [directionLaneCount][snapshotRingSize]int64
	next     int // slot the next sample is written to
	count    int // valid samples, capped at snapshotRingSize
	// session keeps every delta counted since the provider started, per lane.
	session [directionLaneCount]uint64
	// recorded counts every sample ever written to the rings, so the newest
	// sample keeps an absolute index.
	recorded uint64
}

func newDirectionSampler() *directionSampler {
	return &directionSampler{}
}

// sample records one tick. cur maps proxy key to the four directional counters
// (billableRx, billableTx, totalRx, totalTx). The first call only establishes
// the baseline and records nothing.
func (s *directionSampler) sample(cur map[string]directionLanes, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	first := s.prev == nil
	var deltas [directionLaneCount]uint64
	for k, v := range cur {
		p, ok := s.prev[k]
		if !ok {
			continue
		}
		// A counter that went down (a respawned proxy starts from zero) adds
		// nothing for this tick; uint64 subtraction would wrap instead.
		if v.billableRx >= p.billableRx {
			deltas[dirBillableRx] += v.billableRx - p.billableRx
		}
		if v.billableTx >= p.billableTx {
			deltas[dirBillableTx] += v.billableTx - p.billableTx
		}
		if v.totalRx >= p.totalRx {
			deltas[dirTotalRx] += v.totalRx - p.totalRx
		}
		if v.totalTx >= p.totalTx {
			deltas[dirTotalTx] += v.totalTx - p.totalTx
		}
		if v.packetRx >= p.packetRx {
			deltas[dirPacketRx] += v.packetRx - p.packetRx
		}
		if v.packetTx >= p.packetTx {
			deltas[dirPacketTx] += v.packetTx - p.packetTx
		}
		if v.billableRxV6 >= p.billableRxV6 {
			deltas[dirBillableRxV6] += v.billableRxV6 - p.billableRxV6
		}
		if v.billableTxV6 >= p.billableTxV6 {
			deltas[dirBillableTxV6] += v.billableTxV6 - p.billableTxV6
		}
		if v.totalRxV6 >= p.totalRxV6 {
			deltas[dirTotalRxV6] += v.totalRxV6 - p.totalRxV6
		}
		if v.totalTxV6 >= p.totalTxV6 {
			deltas[dirTotalTxV6] += v.totalTxV6 - p.totalTxV6
		}
	}
	next := make(map[string]directionLanes, len(cur))
	for k, v := range cur {
		next[k] = v
	}
	elapsed := now.Sub(s.prevTime).Seconds()
	s.prev = next
	s.prevTime = now
	if first {
		return
	}
	for i := range deltas {
		s.session[i] += deltas[i]
	}
	if elapsed < 1 {
		elapsed = 1
	}
	for i := range deltas {
		s.rings[i][s.next] = int64(float64(deltas[i]) / elapsed)
	}
	s.recorded++
	s.next = (s.next + 1) % snapshotRingSize
	if s.count < snapshotRingSize {
		s.count++
	}
}

// history returns the samples of lane oldest first. Never nil.
func (s *directionSampler) history(lane int) []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]int64, s.count)
	start := (s.next - s.count + snapshotRingSize) % snapshotRingSize
	for i := 0; i < s.count; i++ {
		out[i] = s.rings[lane][(start+i)%snapshotRingSize]
	}
	return out
}

// rates returns the newest sample and the averages over the last 60 and 300
// samples of lane, or over however many exist yet.
func (s *directionSampler) rates(lane int) (now, avg1m, avg5m int64) {
	h := s.history(lane)
	if len(h) == 0 {
		return 0, 0, 0
	}
	return h[len(h)-1], avgTail(h, 60), avgTail(h, 300)
}

// direction builds one lane's figures from its history.
func (s *directionSampler) direction(lane int, h []int64) SnapshotTrafficDirection {
	if len(h) == 0 {
		return SnapshotTrafficDirection{}
	}
	return SnapshotTrafficDirection{
		NowBps:     h[len(h)-1],
		Avg1mBps:   avgTail(h, 60),
		Avg5mBps:   avgTail(h, 300),
		HistoryBps: append([]int64(nil), h...),
	}
}

// block builds the whole per-direction block from the current samples. It is
// read under the caller's sample lock, same as the rate block.
func (s *directionSampler) block() SnapshotTrafficDirections {
	return SnapshotTrafficDirections{
		BillableRx: s.direction(dirBillableRx, s.history(dirBillableRx)),
		BillableTx: s.direction(dirBillableTx, s.history(dirBillableTx)),
		TotalRx:    s.direction(dirTotalRx, s.history(dirTotalRx)),
		TotalTx:    s.direction(dirTotalTx, s.history(dirTotalTx)),
	}
}

func (s *directionSampler) packetRate(lane int) SnapshotPacketRate {
	h := s.history(lane)
	if len(h) == 0 {
		return SnapshotPacketRate{}
	}
	return SnapshotPacketRate{NowPps: h[len(h)-1], Avg1mPps: avgTail(h, 60), Avg5mPps: avgTail(h, 300), HistoryPps: h}
}

// familySplit builds one quantity's V4 and V6 rates. V6 is its own lane. V4 is
// the base lane minus the V6 lane sample by sample. The base and
// V6 counter are separate atomics, so a tick can read the V6 delta one
// sample ahead of the base it belongs to. The negative difference is carried
// into the next sample instead of being discarded, so the base catching up
// does not show up as IPv4 traffic.
func (s *directionSampler) familySplit(lane, laneV6 int) SnapshotFamilySplit {
	base := s.history(lane)
	v6 := s.history(laneV6)
	v4 := make([]int64, len(base))
	var carry int64
	for i := range base {
		d := base[i] - carry
		if i < len(v6) {
			d -= v6[i]
		}
		if d < 0 {
			carry = -d
			d = 0
		} else {
			carry = 0
		}
		v4[i] = d
	}
	return SnapshotFamilySplit{
		V4: s.direction(lane, v4),
		V6: s.direction(laneV6, v6),
	}
}

// familyBlock builds the IP family split of the four directional quantities.
func (s *directionSampler) familyBlock() SnapshotTrafficFamilies {
	return SnapshotTrafficFamilies{
		BillableRx: s.familySplit(dirBillableRx, dirBillableRxV6),
		BillableTx: s.familySplit(dirBillableTx, dirBillableTxV6),
		TotalRx:    s.familySplit(dirTotalRx, dirTotalRxV6),
		TotalTx:    s.familySplit(dirTotalTx, dirTotalTxV6),
	}
}

// cumulativeSample is one reading of the monotonic counters behind the idle
// hints.
type cumulativeSample struct {
	at        time.Time
	auth      int64 // sum of per-proxy auth failures (urnet_proxy_auth_failures)
	contracts int64 // contracts acquired (urnet_contracts_total{result="acquired"})
}

// cumulativeHistory keeps a small ring of counter samples, one per
// snapshotHistoryInterval, so "in the last 10 minutes" questions have data.
type cumulativeHistory struct {
	mu      sync.Mutex
	samples []cumulativeSample
}

// due reports whether enough time passed since the last sample to take
// another. Callers check this before paying for the reads.
func (h *cumulativeHistory) due(now time.Time) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.samples) == 0 {
		return true
	}
	return now.Sub(h.samples[len(h.samples)-1].at) >= snapshotHistoryInterval
}

func (h *cumulativeHistory) add(s cumulativeSample) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.samples = append(h.samples, s)
	if len(h.samples) > snapshotHistorySize {
		h.samples = append(h.samples[:0], h.samples[len(h.samples)-snapshotHistorySize:]...)
	}
}

func (h *cumulativeHistory) copy() []cumulativeSample {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]cumulativeSample(nil), h.samples...)
}

// authFailuresSince counts auth failures recorded after cutoff. A decrease (a
// proxy was removed) is not a failure, so only consecutive pairs that went up
// count. A zero cutoff counts the whole history window.
func authFailuresSince(hist []cumulativeSample, cutoff time.Time) int64 {
	var n int64
	for i := 1; i < len(hist); i++ {
		if hist[i].at.After(cutoff) && hist[i].auth > hist[i-1].auth {
			n += hist[i].auth - hist[i-1].auth
		}
	}
	return n
}

// contractsAcquiredInWindow reports whether any contract was acquired across
// the history window.
func contractsAcquiredInWindow(hist []cumulativeSample) bool {
	for i := 1; i < len(hist); i++ {
		if hist[i].contracts > hist[i-1].contracts {
			return true
		}
	}
	return false
}

// stateInputs is everything deriveSnapshotState needs.
type stateInputs struct {
	uptime   time.Duration
	proxies  SnapshotProxies
	pressure float64
	avg1m    int64
	// startup is the proxy startup phase still pending, or "" once settled.
	startup string
}

func (p SnapshotProxies) total() int {
	return p.Up + p.Degraded + p.Connecting + p.Dead
}

// deriveSnapshotState returns starting, degraded, idle or flowing, checked in
// that order. A node whose proxy startup has not finished stays "starting" past
// the warmup window, and reads degraded once that has taken too long or the
// proxy source failed, so the status agrees with the systemd STATUS= line.
func deriveSnapshotState(in stateInputs) string {
	// A proxy source that failed or came back empty is degraded at once, like
	// the systemd line, even inside the warmup window: warmup covers a pool still
	// connecting, not a source that has already given its answer.
	if in.startup == startupSourceUnreachable || in.startup == startupSourceEmpty || in.startup == startupNoSource {
		return "degraded"
	}
	if in.uptime < snapshotStartingWindow {
		return "starting"
	}
	if in.startup != "" {
		if in.uptime < snapshotStartupStuckAfter {
			return "starting"
		}
		return "degraded"
	}
	total := in.proxies.total()
	if total > 0 && 2*(in.proxies.Dead+in.proxies.Degraded) > total {
		return "degraded"
	}
	if in.pressure >= 0.8 {
		return "degraded"
	}
	if in.avg1m < snapshotIdleBps {
		return "idle"
	}
	return "flowing"
}

// deriveStateReason says why the state is starting or degraded, or "" when
// there is nothing informative to add (idle and flowing have their own hint, and
// an ordinary warmup needs no explanation).
func deriveStateReason(in stateInputs) string {
	mins := int(in.uptime / time.Minute)
	switch deriveSnapshotState(in) {
	case "starting":
		if in.startup == startupResolving {
			return fmt.Sprintf("%s (%d min)", startupResolving, mins)
		}
	case "degraded":
		if in.startup == startupResolving {
			return fmt.Sprintf("startup stuck: %s for %d min", startupResolving, mins)
		}
		if in.startup != "" {
			// The no-source phase is a deliberate configuration, not something
			// being retried: the operator set up no proxy source and turned the
			// direct transport off, so there is nothing to retry. systemdStatusLine
			// describes the same case without "retrying"; the two must agree.
			if in.startup == startupNoSource {
				return in.startup
			}
			return in.startup + ", retrying"
		}
		var parts []string
		if total := in.proxies.total(); total > 0 && 2*(in.proxies.Dead+in.proxies.Degraded) > total {
			parts = append(parts, fmt.Sprintf("%d of %d proxies down", in.proxies.Dead+in.proxies.Degraded, total))
		}
		if in.pressure >= 0.8 {
			parts = append(parts, fmt.Sprintf("resource pressure %.2f", in.pressure))
		}
		return strings.Join(parts, "; ")
	}
	return ""
}

// deriveIdleHint picks the first matching reason a node is idle. It blames
// auth only when that plausibly explains the idleness (a failure wave, or most
// of the pool not connected while failures are happening), and otherwise says
// what is true and shows the numbers so the reader can judge it.
func deriveIdleHint(proxies SnapshotProxies, hist []cumulativeSample, now time.Time) string {
	total := proxies.total()
	switch {
	case total == 0:
		return "no proxies configured"
	case proxies.Up == 0 && total == 1:
		// A node with no proxies still runs its native direct connection as
		// proxy[0], so a lone entry is that connection, not a proxy.
		return "the only connection is dead or connecting"
	case proxies.Up == 0:
		return fmt.Sprintf("all %d proxies down or connecting", total)
	}

	recent := authFailuresSince(hist, now.Add(-snapshotAuthWaveWindow))
	waveAt := int64((total + snapshotAuthWaveDivisor - 1) / snapshotAuthWaveDivisor)
	if waveAt < snapshotAuthWaveFloor {
		waveAt = snapshotAuthWaveFloor
	}
	if recent >= waveAt {
		return fmt.Sprintf("auth failing: %d failures in the last minute across %d proxies", recent, total)
	}
	if 2*proxies.Up < total {
		if authFailuresSince(hist, now.Add(-snapshotAuthWaveWindow)) > 0 {
			return fmt.Sprintf("auth failing: only %d of %d proxies authenticated", proxies.Up, total)
		}
		return fmt.Sprintf("only %d of %d proxies connected", proxies.Up, total)
	}

	evidence := idleEvidence(proxies, recent)
	if !contractsAcquiredInWindow(hist) {
		return "no contracts acquired in the last 10 min" + evidence
	}
	return "no traffic offered" + evidence
}

// idleEvidence is the parenthetical that backs up a hint with the numbers it
// was derived from. The retry rate is left out when there are no retries.
func idleEvidence(proxies SnapshotProxies, retriesPerMin int64) string {
	if retriesPerMin > 0 {
		return fmt.Sprintf(" (%d/%d proxies up, ~%d auth retries/min)", proxies.Up, proxies.total(), retriesPerMin)
	}
	return fmt.Sprintf(" (%d/%d proxies up)", proxies.Up, proxies.total())
}

// snapshotSources are the live inputs behind a snapshot, injectable so the
// collector can be tested without a running provider.
type snapshotSources struct {
	now        func() time.Time
	startedAt  time.Time
	billable   func() map[string]uint64 // proxy key to cumulative billable bytes
	pool       func() (SnapshotProxies, int64)
	cumulative func() (auth, contracts int64)
	sessions   func() (pqe, classical int64)
	pressure   func() float64
	version    func() string
	prevVer    func() string
	restart    func() SnapshotRestart
	resources  func() SnapshotResources

	restartPending func() bool

	// startup reports the proxy startup phase still pending, or "" once it has
	// settled. Nil means always settled.
	startup func() string

	// traffic maps proxy key to cumulative TOTAL bytes (rx+tx), like billable
	// does for billable bytes. Nil means no total-traffic figures.
	traffic func() map[string]uint64
	// directions maps proxy key to its four cumulative directional counters. Nil
	// means no per-direction figures.
	directions     func() map[string]directionLanes
	packetCounters bool
	// familyCounters reports whether this source counts IPv6 traffic, which
	// decides whether the snapshot carries the IP family split.
	familyCounters bool
	// lifetimeBillable reports the persisted lifetime billable total, false
	// when the lifetime store is not running. Nil means not available.
	lifetimeBillable func() (uint64, bool)
}

// nodeSnapshotCollector samples the node once a second and builds snapshots.
type nodeSnapshotCollector struct {
	src     snapshotSources
	rate    *rateSampler // billable bytes
	traffic *rateSampler // total bytes
	hist    *cumulativeHistory

	mu       sync.Mutex
	cached   *NodeSnapshot
	cachedAt time.Time

	// sampleMu makes one tick's samples, and one snapshot's capture of them,
	// atomic against each other. The graph anchors both series on HistorySeq; a
	// tick landing between reading it and reading the history it labels made the
	// anchor one sample stale and shifted every completed column. It is separate
	// from mu on purpose: a slow snapshot build must not stall the sampler, and
	// this lock is held only for the samplers' own short reads and writes.
	sampleMu sync.Mutex
	dir      *directionSampler
}

func newNodeSnapshotCollector(src snapshotSources) *nodeSnapshotCollector {
	return &nodeSnapshotCollector{src: src, rate: newRateSampler(), traffic: newRateSampler(), dir: newDirectionSampler(), hist: &cumulativeHistory{}}
}

// tick takes one rate sample and, when due, one counter sample.
func (c *nodeSnapshotCollector) tick() {
	now := c.src.now()
	// The counter reads walk the proxy pool; do them before taking the lock.
	billable := c.src.billable()
	var total map[string]uint64
	if c.src.traffic != nil {
		total = c.src.traffic()
	}
	var dirs map[string]directionLanes
	if c.src.directions != nil {
		dirs = c.src.directions()
	}
	c.sampleMu.Lock()
	c.rate.sample(billable, now)
	if c.src.traffic != nil {
		c.traffic.sample(total, now)
	}
	if c.src.directions != nil {
		c.dir.sample(dirs, now)
	}
	c.sampleMu.Unlock()
	if c.hist.due(now) {
		auth, contracts := c.src.cumulative()
		c.hist.add(cumulativeSample{at: now, auth: auth, contracts: contracts})
	}
}

// run ticks once a second until ctx is done.
func (c *nodeSnapshotCollector) run(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	c.tick()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.tick()
		}
	}
}

// Get returns the current snapshot, reusing one built within the last second.
// The result is shared: callers must not modify it.
func (c *nodeSnapshotCollector) Get() *NodeSnapshot {
	now := c.src.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cached != nil && now.Sub(c.cachedAt) < snapshotCacheTTL && !now.Before(c.cachedAt) {
		return c.cached
	}
	c.cached = c.build(now)
	c.cachedAt = now
	return c.cached
}

func (c *nodeSnapshotCollector) build(now time.Time) *NodeSnapshot {
	// Everything read from the samplers is captured together, so the anchor, the
	// billable history and the total history all describe the same sample.
	c.sampleMu.Lock()
	nowBps, avg1m, avg5m := c.rate.rates()
	histSeq, histBps := c.rate.seq(), c.rate.history()
	traffic := c.trafficSnapshot()
	c.sampleMu.Unlock()
	c.withLifetime(traffic)

	proxies, clients := c.src.pool()
	pqe, classical := c.src.sessions()
	uptime := now.Sub(c.src.startedAt)
	if uptime < 0 {
		uptime = 0
	}
	pressure := c.src.pressure()

	snap := &NodeSnapshot{
		V:               snapshotSchemaVersion,
		Version:         c.src.version(),
		PreviousVersion: c.src.prevVer(),
		StartedAt:       c.src.startedAt.UTC(),
		Now:             now.UTC(),
		UptimeSeconds:   uptime.Seconds(),
		Busy:            avg1m >= snapshotIdleBps,
		RestartPending:  c.src.restartPending(),
		Rate: SnapshotRate{
			NowBps:          nowBps,
			Avg1mBps:        avg1m,
			Avg5mBps:        avg5m,
			HistoryInterval: 1,
			HistorySeq:      histSeq,
			HistoryBps:      histBps,
		},
		Clients:   clients,
		Sessions:  SnapshotSessions{PQE: pqe, Classical: classical},
		Proxies:   proxies,
		Pressure:  pressure,
		Restart:   c.src.restart(),
		Resources: c.src.resources(),
	}
	in := stateInputs{uptime: uptime, proxies: proxies, pressure: pressure, avg1m: avg1m}
	if c.src.startup != nil {
		in.startup = c.src.startup()
	}
	snap.Traffic = traffic
	snap.State = deriveSnapshotState(in)
	snap.StateReason = deriveStateReason(in)
	if snap.State == "idle" {
		snap.IdleHint = deriveIdleHint(proxies, c.hist.copy(), now)
	}
	return snap
}

// trafficSnapshot builds the billable-versus-total block.
func (c *nodeSnapshotCollector) trafficSnapshot() *SnapshotTraffic {
	nowBps, avg1m, avg5m := c.traffic.rates()
	tr := &SnapshotTraffic{
		BillableBytes:   c.rate.session(),
		TotalBytes:      c.traffic.session(),
		TotalNowBps:     nowBps,
		TotalAvg1mBps:   avg1m,
		TotalAvg5mBps:   avg5m,
		TotalHistoryBps: c.traffic.history(),
	}
	if c.src.directions != nil {
		d := c.dir.block()
		if c.src.familyCounters {
			f := c.dir.familyBlock()
			d.Families = &f
		}
		tr.Directions = &d
	}
	if c.src.packetCounters && c.src.directions != nil {
		tr.Packets = &SnapshotTrafficPackets{BillableRx: c.dir.packetRate(dirPacketRx), BillableTx: c.dir.packetRate(dirPacketTx)}
	}
	return tr
}

// withLifetime adds the persisted lifetime total to tr. It is read separately,
// after the sampling lock is released: the lifetime store holds its own lock
// across a file write, and reading it under the sampling lock would let a flush
// stall the per-second sampler.
func (c *nodeSnapshotCollector) withLifetime(tr *SnapshotTraffic) {
	if c.src.lifetimeBillable == nil {
		return
	}
	if v, ok := c.src.lifetimeBillable(); ok {
		tr.LifetimeBillableBytes = &v
	}
}

// restartPendingFor reports whether any restart-required control key now
// holds a value different from the one the process started with. Keys are
// the ones startupValues reports. A key with no current value is not counted:
// the same absence is what an env-provided setting looks like.
func restartPendingFor(current func(key string) (string, bool), startup map[string]string) bool {
	for key, started := range startup {
		cur, ok := current(key)
		if !ok {
			continue
		}
		if key == "ramlogs" {
			if snapshotTruthy(cur) != snapshotTruthy(started) {
				return true
			}
			continue
		}
		if cur != started {
			return true
		}
	}
	return false
}

func snapshotTruthy(v string) bool {
	switch v {
	case "1", "on", "true", "yes", "ON", "TRUE", "YES", "True", "On", "Yes":
		return true
	}
	return false
}

// --- production wiring ---

var nodeSnapshots = newNodeSnapshotCollector(productionSnapshotSources())

// bandwidthShare hands the billable and the total per-proxy counters from one
// read, and reuses that read for ttl. Building the proxy snapshot is the costly
// part (about 2ms and 35k allocations at 5000 proxies), and the billable and
// total samplers tick together, so without this every tick would build it twice.
// The directional counters come off the same read, so all three samplers pay
// for the pool walk once. The maps are only read by the samplers, which copy
// what they keep.
type bandwidthShare struct {
	mu      sync.Mutex
	now     func() time.Time
	ttl     time.Duration
	read    func() proxyBandwidthRead
	at      time.Time
	got     proxyBandwidthRead
	fetched bool
}

// proxyBandwidthRead is one proxy snapshot: billable and total per key, and the
// four directional counters per key, all from the same read.
type proxyBandwidthRead struct {
	billable   map[string]uint64
	total      map[string]uint64
	directions map[string]directionLanes
}

func newBandwidthShare(now func() time.Time, ttl time.Duration, read func() proxyBandwidthRead) *bandwidthShare {
	return &bandwidthShare{now: now, ttl: ttl, read: read}
}

func (s *bandwidthShare) get() (billable, total map[string]uint64, directions map[string]directionLanes) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if at := s.now(); !s.fetched || at.Sub(s.at) >= s.ttl {
		s.got = s.read()
		s.at, s.fetched = at, true
	}
	return s.got.billable, s.got.total, s.got.directions
}

// readProxyBandwidth reads all per-proxy counters from one proxy snapshot: billable,
// total and the four directional lanes. The two sums are what ProxyBandwidthTotals
// reports; the lanes are the per-direction split behind the live snapshot.
func readProxyBandwidth() (billable, total map[string]uint64, directions map[string]directionLanes) {
	_, _, _, bw, _ := connect.ProxyHealthSnapshot()
	if len(bw) == 0 {
		return map[string]uint64{}, map[string]uint64{}, map[string]directionLanes{}
	}
	billable = make(map[string]uint64, len(bw))
	total = make(map[string]uint64, len(bw))
	directions = make(map[string]directionLanes, len(bw))
	for k, p := range bw {
		billable[k] = p.BillableRx.Load() + p.BillableTx.Load()
		total[k] = p.TotalRx.Load() + p.TotalTx.Load()
		directions[k] = directionLanes{
			billableRx: p.BillableRx.Load(),
			billableTx: p.BillableTx.Load(),
			totalRx:    p.TotalRx.Load(),
			totalTx:    p.TotalTx.Load(),
			packetRx:   p.BillableRxPackets.Load(),
			packetTx:   p.BillableTxPackets.Load(),

			billableRxV6: p.BillableRxV6.Load(),
			billableTxV6: p.BillableTxV6.Load(),
			totalRxV6:    p.TotalRxV6.Load(),
			totalTxV6:    p.TotalTxV6.Load(),
		}
	}
	return billable, total, directions
}

func productionSnapshotSources() snapshotSources {
	bandwidth := newBandwidthShare(time.Now, 500*time.Millisecond, func() proxyBandwidthRead {
		b, t, d := readProxyBandwidth()
		return proxyBandwidthRead{billable: b, total: t, directions: d}
	})
	return snapshotSources{
		packetCounters: true,
		familyCounters: true,
		now:            time.Now,
		startedAt:      providerStartTime,
		billable: func() map[string]uint64 {
			b, _, _ := bandwidth.get()
			return b
		},
		directions: func() map[string]directionLanes {
			_, _, d := bandwidth.get()
			return d
		},
		pool: func() (SnapshotProxies, int64) {
			held := trimHeldSnapshot()
			up, dead, degraded, bw, connecting, online, proxied := connect.ProxyHealthSnapshotWithIdentities(held)
			var clients int64
			for _, p := range bw {
				clients += p.Clients.Load()
			}
			sp := SnapshotProxies{Up: up, Degraded: len(degraded), Connecting: len(connecting), Dead: len(dead), Parked: len(held)}
			if proxied {
				sp.UDP = globalProxyUDPCheck.coverage(online, time.Now())
				sp.IPv6 = globalProxyUDPCheck.ipv6Coverage(online, time.Now())
			}
			if desired, cap, source, ok := currentTrimSelection(); ok {
				sp.TrimReported = true
				sp.Configured = desired
				sp.TrimCap = cap
				sp.TrimCapSource = source
			}
			return sp, clients
		},
		cumulative: func() (int64, int64) {
			return totalProxyAuthFailures(), connect.ContractsAcquiredTotal()
		},
		sessions: func() (int64, int64) {
			pq := pqeTotalCounts()
			return int64(pq.ActivePQE), int64(pq.ActiveClas)
		},
		pressure: currentPressure,
		version: func() string {
			// A build without -ldflags has no version; say "dev" as the
			// control socket "version" command does.
			if v := RequireVersion(); v != "" {
				return v
			}
			return "dev"
		},
		prevVer: func() string {
			startupDiag.mu.Lock()
			defer startupDiag.mu.Unlock()
			return startupDiag.previousVersion
		},
		restart: func() SnapshotRestart {
			detectStartup()
			startupDiag.mu.Lock()
			defer startupDiag.mu.Unlock()
			return SnapshotRestart{Reason: startupDiag.restartReason, CleanShutdown: startupDiag.cleanShutdown}
		},
		resources: collectResources,
		restartPending: func() bool {
			return restartPendingFor(globalControlState.get, startupValues())
		},
		startup: proxyStartupPhase,
		traffic: func() map[string]uint64 {
			_, t, _ := bandwidth.get()
			return t
		},
		lifetimeBillable: func() (uint64, bool) {
			if lifetimeStore == nil {
				return 0, false
			}
			_, _, _, _, _, _, bill := lifetimeStore.Snapshot()
			return bill, true
		},
	}
}

// totalProxyAuthFailures is the sum behind urnet_proxy_auth_failures.
func totalProxyAuthFailures() int64 {
	state, err := readProxyState()
	if err != nil || state == nil {
		return 0
	}
	var total int64
	for _, entry := range state.Proxies {
		total += entry.AuthFailures
	}
	return total
}

// runNodeSnapshotSampler feeds the global collector until ctx is done.
func runNodeSnapshotSampler(ctx context.Context) {
	nodeSnapshots.run(ctx)
}
