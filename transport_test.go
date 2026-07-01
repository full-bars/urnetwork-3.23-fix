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
		modeMonitor:    NewMonitor(),
		availableModes: map[TransportMode]bool{},
	}

	available, notify := transport.modeAvailable(TransportModeH1)
	assert.Equal(t, false, available)

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
