package connect

import (
	"testing"
	"time"

	"github.com/go-playground/assert/v2"
)

func TestConnectHost(t *testing.T) {

	host, err := connectHost("http://connect.foo.bar")
	assert.Equal(t, err, nil)
	assert.Equal(t, host, "connect.foo.bar")

	host, err = connectHost("https://other-connect.bar.com")
	assert.Equal(t, err, nil)
	assert.Equal(t, host, "other-connect.bar.com")
}

func TestConnectPumpHost(t *testing.T) {
	host, err := pumpHost("http://connect.foo.bar", []byte("foo.com"))
	assert.Equal(t, err, nil)
	assert.Equal(t, host, "zone-foo-com.foo.bar")

	host, err = pumpHost("http://127.0.0.1", []byte("foo.com"))
	assert.Equal(t, err, nil)
	assert.Equal(t, host, "127.0.0.1")
}

func TestSetModeAvailableWakesWaiter(t *testing.T) {
	transport := &PlatformTransport{
		availableModeMonitor: NewMonitor(),
		availableModes:       map[TransportMode]bool{},
	}

	available, notify := transport.modesAvailable()
	assert.Equal(t, false, available[TransportModeH1])

	woke := make(chan struct{})
	go func() {
		<-notify
		close(woke)
	}()

	transport.setModeAvailable(TransportModeH1, true)

	select {
	case <-woke:
	case <-time.After(1 * time.Second):
		t.Fatal("waiter was not notified after setModeAvailable")
	}
}

func TestSetModeAvailableNoSpuriousWake(t *testing.T) {
	transport := &PlatformTransport{
		availableModeMonitor: NewMonitor(),
		availableModes:       map[TransportMode]bool{},
	}

	// Set mode to true — should notify.
	transport.setModeAvailable(TransportModeH1, true)

	// Capture watcher on a channel returned BEFORE the second set.
	_, notify := transport.modesAvailable()

	// Set the mode to the same value — should NOT notify.
	transport.setModeAvailable(TransportModeH1, true)

	select {
	case <-notify:
		t.Fatal("NotifyAll fired when mode availability did not change")
	case <-time.After(200 * time.Millisecond):
		// Expected: channel not closed.
	}
}

func TestSetActiveModeNoSpuriousWake(t *testing.T) {
	transport := &PlatformTransport{
		availableModeMonitor: NewMonitor(),
		availableModes:       map[TransportMode]bool{},
		mode:                 NewMonitorValue[TransportMode](TransportModeNone),
	}

	// Set active mode to H1 — should notify.
	transport.setActiveMode(TransportModeH1)

	// Capture watcher on a channel returned BEFORE the second set.
	_, notify := transport.activeMode()

	// Set the active mode to the same value — should NOT notify (MonitorValue
	// handles this internally).
	transport.setActiveMode(TransportModeH1)

	select {
	case <-notify:
		t.Fatal("NotifyAll fired when active mode did not change")
	case <-time.After(200 * time.Millisecond):
		// Expected: channel not closed.
	}

	// Changing the mode to a different value SHOULD still notify.
	transport.setActiveMode(TransportModeNone)

	select {
	case <-notify:
		// Expected.
	case <-time.After(200 * time.Millisecond):
		t.Fatal("NotifyAll did not fire on real active mode change")
	}
}
