package connect

import (
	"sync"
)

var (
	pulseMu      sync.RWMutex
	pulseChannel chan struct{}
)

func init() {
	pulseChannel = make(chan struct{})
}

// Pulse returns a channel that is closed when a global wakeup event fires.
// Callers should call Pulse() again immediately after receiving to re-arm.
func Pulse() <-chan struct{} {
	pulseMu.RLock()
	defer pulseMu.RUnlock()
	return pulseChannel
}

// TriggerPulse wakes all goroutines currently listening on Pulse().
// Used to give stalled transports and proxies a fresh connection attempt.
func TriggerPulse() {
	pulseMu.Lock()
	defer pulseMu.Unlock()
	close(pulseChannel)
	pulseChannel = make(chan struct{})
}
