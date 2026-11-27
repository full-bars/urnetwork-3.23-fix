package connect

import (
	"fmt"
	"strings"
	"sync/atomic"
)

// Bound lane source policy modes. The values index the mismatch counters and
// the metric labels, so their order is fixed. Drop is the default, set in init,
// so the zero value of the setting is not the default.
const (
	p2pSourceModeCount = iota
	p2pSourceModeOff
	p2pSourceModeDrop
)

// What happened to a frame whose source did not match its lane's bound peer.
// A transit hop relays a stream on its own route managers, so its traffic
// never reaches this check and reports no outcome.
const (
	p2pSourceOutcomeDecoded = iota
	p2pSourceOutcomeRejected
	p2pSourceOutcomeDropped
)

var p2pSourceBinding atomic.Uint32
var p2pForgedSourceDropDisabled atomic.Bool

// P2PSourceBindingDefault is the mode a process starts in. Drop matches
// upstream: production streams are direct, so the bound peer is the stream's
// other end. A multihop stream ending here over a P2P leg is dropped until the
// platform authenticates the stream origin to the endpoint.
const P2PSourceBindingDefault = "drop"

func init() {
	p2pSourceBinding.Store(p2pSourceModeDrop)
}

func SetP2PSourceBinding(value string) error {
	switch strings.ToLower(value) {
	case "count":
		p2pSourceBinding.Store(p2pSourceModeCount)
	case "off":
		p2pSourceBinding.Store(p2pSourceModeOff)
	case "drop":
		p2pSourceBinding.Store(p2pSourceModeDrop)
	default:
		return fmt.Errorf("p2p source binding must be off, count or drop")
	}
	return nil
}

func P2PSourceBinding() string {
	switch p2pSourceBinding.Load() {
	case p2pSourceModeCount:
		return "count"
	case p2pSourceModeOff:
		return "off"
	default:
		return "drop"
	}
}

func p2pSourceBindingMode() uint32 { return p2pSourceBinding.Load() }

func SetP2PForgedSourceDrop(enabled bool) { p2pForgedSourceDropDisabled.Store(!enabled) }
func P2PForgedSourceDropEnabled() bool    { return !p2pForgedSourceDropDisabled.Load() }

// forgedSourceKind reports the enforced rule a frame on a direct peer-to-peer
// lane breaks: it names the platform, which only the platform may do, or it
// names this client, which only this client's own traffic does. A relayed
// frame can never legitimately name either. Every other frame returns "".
func forgedSourceKind(disposition transferReceiveDisposition, localId Id, sourceId Id) string {
	if disposition.transportType != TransportTypeP2p {
		return ""
	}
	switch sourceId {
	case ControlId:
		return "platform"
	case localId:
		return "self"
	}
	return ""
}

// boundSourceMismatch reports whether a frame named a source other than the
// peer its receive lane is bound to. Only a direct peer-to-peer lane is bound:
// the binding is the source the adjacent peer's signalling authenticated, and
// no other transport type carries one. An unbound lane accepts any source,
// because a multihop endpoint only knows its adjacent hop.
func boundSourceMismatch(disposition transferReceiveDisposition, sourceId Id) bool {
	return disposition.transportType == TransportTypeP2p &&
		disposition.boundSourceId != (Id{}) &&
		sourceId != disposition.boundSourceId
}

// p2pSourceCounters holds the counters behind the two source policy metrics.
// Forged drops are counted by the rule the frame broke. Bound lane mismatches
// are counted by the policy mode in force when the frame arrived and by what
// happened to it, which is what separates legitimate multihop traffic from
// spoofing. Every index is a fixed constant, so the exposed label values stay
// bounded.
type p2pSourceCounters struct {
	forgedPlatform atomic.Uint64
	forgedSelf     atomic.Uint64
	mismatch       [3][3]atomic.Uint64
}

// p2pSourceMetrics aggregates every client in the process for /metrics.
var p2pSourceMetrics p2pSourceCounters

func (self *p2pSourceCounters) addForged(kind string) {
	switch kind {
	case "platform":
		self.forgedPlatform.Add(1)
	case "self":
		self.forgedSelf.Add(1)
	}
}

func (self *p2pSourceCounters) forged() (platform uint64, self_ uint64) {
	return self.forgedPlatform.Load(), self.forgedSelf.Load()
}

func (self *p2pSourceCounters) addMismatch(mode uint32, outcome int) {
	if p2pSourceModeDrop < mode || p2pSourceOutcomeDropped < outcome {
		return
	}
	self.mismatch[mode][outcome].Add(1)
}

func (self *p2pSourceCounters) mismatchAt(mode uint32, outcome int) uint64 {
	if p2pSourceModeDrop < mode || p2pSourceOutcomeDropped < outcome {
		return 0
	}
	return self.mismatch[mode][outcome].Load()
}

func (self *p2pSourceCounters) mismatchTotal() (total uint64) {
	for mode := range self.mismatch {
		for outcome := range self.mismatch[mode] {
			total += self.mismatch[mode][outcome].Load()
		}
	}
	return total
}

func (self *Client) recordForgedSourceDrop(kind string) {
	self.p2pSources.addForged(kind)
	p2pSourceMetrics.addForged(kind)
}

func (self *Client) recordSourceMismatch(mode uint32, outcome int) {
	self.p2pSources.addMismatch(mode, outcome)
	p2pSourceMetrics.addMismatch(mode, outcome)
}

// p2pSourceMismatch records the outcome of one frame whose source did not
// match the peer its lane is bound to. The record travels with the frame: the
// receive loop holds it until the frame is handed to a receive sequence, and
// the sequence decides it where the frame is delivered or discarded. Only the
// first outcome counts, so a frame cannot be counted twice. A nil record
// belongs to a frame with no mismatch and records nothing, which keeps an
// accepted frame allocation free and silent.
type p2pSourceMismatch struct {
	client *Client
	mode   uint32
	active atomic.Bool
}

func newP2PSourceMismatch(client *Client, mode uint32) *p2pSourceMismatch {
	mismatch := &p2pSourceMismatch{client: client, mode: mode}
	mismatch.active.Store(true)
	return mismatch
}

func (self *p2pSourceMismatch) record(outcome int) {
	if self != nil && self.active.CompareAndSwap(true, false) {
		self.client.recordSourceMismatch(self.mode, outcome)
	}
}

// decoded means the frame was accepted: it passed the contract check and its
// frames were delivered, or consumed by the encryption session.
func (self *p2pSourceMismatch) decoded() { self.record(p2pSourceOutcomeDecoded) }

// rejected means the frame failed to decode or to decrypt, was discarded by the
// receive buffer or sequence (such as an older sequence, a resend, or no
// contract), or was thrown away by the encryption policy.
func (self *p2pSourceMismatch) rejected() { self.record(p2pSourceOutcomeRejected) }

// dropped means the frame was not decoded here: binding drop mode, or a
// routing guard that discards a frame it cannot place.
func (self *p2pSourceMismatch) dropped() { self.record(p2pSourceOutcomeDropped) }
