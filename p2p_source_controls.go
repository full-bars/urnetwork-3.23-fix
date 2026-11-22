package connect

import (
	"fmt"
	"strings"
	"sync/atomic"
)

// Zero values select the default policy.
var p2pSourceBinding atomic.Uint32
var p2pForgedSourceDropDisabled atomic.Bool

func SetP2PSourceBinding(value string) error {
	switch strings.ToLower(value) {
	case "count":
		p2pSourceBinding.Store(0)
	case "off":
		p2pSourceBinding.Store(1)
	case "drop":
		p2pSourceBinding.Store(2)
	default:
		return fmt.Errorf("p2p source binding must be off, count or drop")
	}
	return nil
}

func P2PSourceBinding() string {
	switch p2pSourceBinding.Load() {
	case 1:
		return "off"
	case 2:
		return "drop"
	default:
		return "count"
	}
}

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
// peer its receive lane is bound to. An unbound lane accepts any source,
// because a multihop endpoint only knows its adjacent hop.
func boundSourceMismatch(disposition transferReceiveDisposition, sourceId Id) bool {
	return disposition.boundSourceId != (Id{}) && sourceId != disposition.boundSourceId
}
