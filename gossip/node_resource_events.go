package gossip

import (
	"time"

	rcmgr "github.com/libp2p/go-libp2p/p2p/host/resource-manager"
)

type nodeResourceEvents struct {
	events *EventRing
}

func (r nodeResourceEvents) ConsumeEvent(event rcmgr.TraceEvt) {
	switch event.Type {
	case rcmgr.TraceBlockReserveMemoryEvt, rcmgr.TraceBlockAddStreamEvt, rcmgr.TraceBlockAddConnEvt:
		r.events.RecordCap(Event{Time: time.Now(), Kind: "cap", Reason: "resource_limit"})
	}
}
