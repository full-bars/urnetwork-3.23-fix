package main

import (
	"sync"

	"github.com/urnetwork/connect"
)

func startupHeld(settings []*connect.ProxySettings) {
	for _, s := range settings {
		setTrimHeld(s.Key(), true)
	}
}

// trimHeld records actual decisions, not the difference between a cap and a
// configured count. Keys are the same identities used by the health registry.
var trimHeld = struct {
	sync.RWMutex
	keys map[string]bool
}{keys: make(map[string]bool)}

func setTrimHeld(key string, held bool) {
	trimHeld.Lock()
	defer trimHeld.Unlock()
	if held {
		trimHeld.keys[key] = true
	} else {
		delete(trimHeld.keys, key)
	}
}

func trimHeldSnapshot() map[string]bool {
	trimHeld.RLock()
	defer trimHeld.RUnlock()
	keys := make(map[string]bool, len(trimHeld.keys))
	for key := range trimHeld.keys {
		keys[key] = true
	}
	return keys
}

func resetTrimHeld() {
	trimHeld.Lock()
	defer trimHeld.Unlock()
	clear(trimHeld.keys)
}
