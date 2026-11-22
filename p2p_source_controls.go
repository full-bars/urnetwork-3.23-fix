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
