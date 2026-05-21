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

// Pulse returns a channel that is closed when a global wakeup event occurs.
// Goroutines should immediately call Pulse() again after receiving a signal.
func Pulse() <-chan struct{} {
	pulseMu.RLock()
	defer pulseMu.RUnlock()
	return pulseChannel
}

// TriggerPulse notifies all goroutines listening to Pulse() to wake up and reset.
func TriggerPulse() {
	pulseMu.Lock()
	defer pulseMu.Unlock()

	// Broadcast by closing the existing channel
	close(pulseChannel)

	// Create a new channel for the next pulse
	pulseChannel = make(chan struct{})
}
