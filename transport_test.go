package connect

import (
	"sync"
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

// TestPlatformTransportGetAuthZeroValue pins the zero-value behavior of the
// atomic.Pointer[ClientAuth] auth field: before any SetAuth call, getAuth
// must return nil rather than panicking on an uninitialized pointer.
func TestPlatformTransportGetAuthZeroValue(t *testing.T) {
	transport := &PlatformTransport{}

	if got := transport.getAuth(); got != nil {
		t.Fatalf("getAuth on a zero-value transport = %v, want nil", got)
	}
}

// TestPlatformTransportSetAuthGetAuthRoundTrip verifies SetAuth/getAuth carry
// the exact pointer through, and that a later SetAuth call fully replaces the
// previous credential (the renewal watcher hot-swaps the whole *ClientAuth on
// every JWT rotation).
func TestPlatformTransportSetAuthGetAuthRoundTrip(t *testing.T) {
	transport := &PlatformTransport{}

	auth1 := &ClientAuth{ByJwt: "jwt-1", AppVersion: "1.0.0", InstanceId: NewId()}
	transport.SetAuth(auth1)
	if got := transport.getAuth(); got != auth1 {
		t.Fatalf("getAuth = %v, want the exact pointer set by SetAuth (%v)", got, auth1)
	}

	auth2 := &ClientAuth{ByJwt: "jwt-2", AppVersion: "1.0.1", InstanceId: NewId()}
	transport.SetAuth(auth2)
	if got := transport.getAuth(); got != auth2 {
		t.Fatalf("getAuth after second SetAuth = %v, want %v", got, auth2)
	}
	if got := transport.getAuth(); got == auth1 {
		t.Fatal("getAuth still returned the stale auth pointer after a second SetAuth call")
	}
}

// TestPlatformTransportSetAuthConcurrentWithGetAuth exercises SetAuth (the
// renewal watcher rotating the bearer token) racing with getAuth (the dial
// paths reading it) — this is the scenario atomic.Pointer replaced the old
// stateLock-guarded field for. Run with -race to catch any regression back to
// an unsynchronized read/write.
func TestPlatformTransportSetAuthConcurrentWithGetAuth(t *testing.T) {
	transport := &PlatformTransport{}
	transport.SetAuth(&ClientAuth{ByJwt: "initial"})

	var wg sync.WaitGroup
	stop := make(chan struct{})

	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					transport.SetAuth(&ClientAuth{ByJwt: "rotated"})
				}
			}
		}(i)
	}

	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					if auth := transport.getAuth(); auth == nil {
						t.Errorf("getAuth returned nil after SetAuth had already been called")
						return
					}
				}
			}
		}()
	}

	time.Sleep(100 * time.Millisecond)
	close(stop)
	wg.Wait()
}
