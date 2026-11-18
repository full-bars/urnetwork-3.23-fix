package gossip

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"syscall"
)

// DialStats counts peering calls, not the individual addresses a call may try.
// A failure is assigned to exactly one class, including cancellation.
type DialStats struct {
	Attempts uint64 `json:"attempts"`
	Timeout  uint64 `json:"timeout"`
	Refused  uint64 `json:"refused"`
	Canceled uint64 `json:"canceled"`
	Other    uint64 `json:"other"`
}

type dialCounters struct {
	attempts atomic.Uint64
	timeout  atomic.Uint64
	refused  atomic.Uint64
	canceled atomic.Uint64
	other    atomic.Uint64
}

func (c *dialCounters) recordResult(err error) {
	if err == nil {
		return
	}
	var netErr net.Error
	switch {
	case errors.Is(err, context.Canceled):
		c.canceled.Add(1)
	case errors.Is(err, context.DeadlineExceeded):
		c.timeout.Add(1)
	case errors.Is(err, syscall.ECONNREFUSED):
		c.refused.Add(1)
	case errors.As(err, &netErr) && netErr.Timeout():
		c.timeout.Add(1)
	default:
		c.other.Add(1)
	}
}

func (c *dialCounters) snapshot() DialStats {
	return DialStats{
		Attempts: c.attempts.Load(),
		Timeout:  c.timeout.Load(),
		Refused:  c.refused.Load(),
		Canceled: c.canceled.Load(),
		Other:    c.other.Load(),
	}
}
