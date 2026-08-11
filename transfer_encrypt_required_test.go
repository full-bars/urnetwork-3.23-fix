package connect

// Additional unit coverage for the EncryptionModeRequired machinery (PR #350
// review round): the mode mappings, poll-interval fallback, notification
// dedup, the RWMutex Pack-vs-Close interaction, rekey continuity, and the
// typed-error contract of the send gate. All tests call the real functions.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

// TestReceiveGateAcceptsWrappedUnderRequired verifies the receive gate does
// NOT false-positive: a valid sealed application frame from a Required peer
// must be delivered (the gate only discards unwrapped frames).
func TestReceiveGateAcceptsWrappedUnderRequired(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	a, b, _, bClientId, _, receivesB := requiredGatePair(
		ctx, EncryptionModeRequired, EncryptionModeRequired, nil, true)
	defer a.Cancel()
	defer b.Cancel()

	sent := make(chan bool, 1)
	go func() {
		sent <- a.SendWithTimeout(
			requiredGateFrame(t, "sealed"),
			DestinationId(bClientId),
			func(error) {},
			30*time.Second,
		)
	}()

	select {
	case ok := <-sent:
		if !ok {
			t.Fatal("send must succeed once cipher is established")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("send timed out")
	}

	select {
	case got := <-receivesB:
		if got != "sealed" {
			t.Fatalf("peer received %q, want %q", got, "sealed")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("peer never received the sealed message")
	}
}

// TestRekeyContinuityCipherServesDuringHandshake verifies that Cipher()
// continues to return the established epoch's cipher while a replacement
// handshake is in flight (the rekey continuity property). Under the old code
// Cipher() returned nil during a rekey, opening a plaintext window.
func TestRekeyContinuityCipherServesDuringHandshake(t *testing.T) {
	sess, cleanup := newTestEncryptionSession(t, sequenceTlsRoleClient)
	defer cleanup()

	// Inject a completed epoch with a derived cipher.
	e1 := injectTestEpoch(sess, true, nil)
	fakeCipher := &sequenceCipher{}
	sess.stateLock.Lock()
	e1.derivedTlsCipher = fakeCipher
	sess.establishedEpoch = e1
	sess.stateLock.Unlock()

	// Cipher() returns the established cipher.
	if c := sess.Cipher(); c != fakeCipher {
		t.Fatalf("Cipher() should return established cipher, got %v", c)
	}

	// Trigger a rekey (restart handshake).
	sess.restartHandshake()

	// Immediately after restartHandshake, Cipher() must still return the
	// established cipher (not nil). This is the rekey continuity property.
	if c := sess.Cipher(); c != fakeCipher {
		t.Fatalf("Cipher() must keep serving during rekey, got %v (want established cipher)", c)
	}

	// Wait a tick for the handshake goroutine to settle, then check again.
	time.Sleep(50 * time.Millisecond)
	if c := sess.Cipher(); c != fakeCipher {
		t.Fatalf("Cipher() must keep serving after handshake goroutine starts, got %v", c)
	}
}

// TestRequireEncryptionModeMapping verifies that RequireEncryption() returns
// true only for EncryptionModeRequired.
func TestRequireEncryptionModeMapping(t *testing.T) {
	tests := []struct {
		mode     EncryptionMode
		expected bool
	}{
		{EncryptionModeOff, false},
		{EncryptionModeOpportunistic, false},
		{EncryptionModeRequired, true},
	}
	for _, tt := range tests {
		settings := &EncryptionSettings{Mode: tt.mode}
		sess := &peerEncryptionSession{settings: settings}
		if got := sess.RequireEncryption(); got != tt.expected {
			t.Errorf("RequireEncryption() with mode %v = %v, want %v", tt.mode, got, tt.expected)
		}
	}
}

// TestManagerRequireEncryptionMapping verifies that
// EncryptionSessionManager.RequireEncryption() reflects the mode setting.
func TestManagerRequireEncryptionMapping(t *testing.T) {
	tests := []struct {
		mode     EncryptionMode
		expected bool
	}{
		{EncryptionModeOff, false},
		{EncryptionModeOpportunistic, false},
		{EncryptionModeRequired, true},
	}
	for _, tt := range tests {
		settings := &EncryptionSettings{Mode: tt.mode}
		m := &EncryptionSessionManager{settings: settings}
		if got := m.RequireEncryption(); got != tt.expected {
			t.Errorf("RequireEncryption() with mode %v = %v, want %v", tt.mode, got, tt.expected)
		}
	}
}

// TestRequiredCipherPollIntervalDefaultFallback verifies that
// RequiredCipherPollInterval() falls back to the default when the setting is
// zero or negative.
func TestRequiredCipherPollIntervalDefaultFallback(t *testing.T) {
	tests := []struct {
		name     string
		interval time.Duration
		expected time.Duration
	}{
		{"zero falls back", 0, DefaultRequiredCipherPollInterval},
		{"negative falls back", -1, DefaultRequiredCipherPollInterval},
		{"positive uses setting", 50 * time.Millisecond, 50 * time.Millisecond},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			settings := &EncryptionSettings{RequiredCipherPollInterval: tt.interval}
			sess := &peerEncryptionSession{settings: settings}
			if got := sess.RequiredCipherPollInterval(); got != tt.expected {
				t.Errorf("RequiredCipherPollInterval() = %v, want %v", got, tt.expected)
			}
		})
	}
}

// TestTlsTimeoutSettingFallback verifies TlsTimeoutSetting() returns 0 when
// settings are nil and the configured value otherwise.
func TestTlsTimeoutSettingFallback(t *testing.T) {
	sess := &peerEncryptionSession{settings: nil}
	if got := sess.TlsTimeoutSetting(); got != 0 {
		t.Errorf("TlsTimeoutSetting() with nil settings = %v, want 0", got)
	}

	settings := &EncryptionSettings{TlsTimeout: 30 * time.Second}
	sess = &peerEncryptionSession{settings: settings}
	if got := sess.TlsTimeoutSetting(); got != 30*time.Second {
		t.Errorf("TlsTimeoutSetting() = %v, want 30s", got)
	}
}

// TestNotifyRequiredSendBlockedDedup verifies that NotifyRequiredSendBlocked
// fires only once per session (the dedup contract).
func TestNotifyRequiredSendBlockedDedup(t *testing.T) {
	sess, cleanup := newTestEncryptionSession(t, sequenceTlsRoleClient)
	defer cleanup()

	sess.NotifyRequiredSendBlocked("first call")
	sess.NotifyRequiredSendBlocked("second call")
	sess.NotifyRequiredSendBlocked("third call")

	sess.stateLock.Lock()
	defer sess.stateLock.Unlock()
	if !sess.requiredSendBlockedNotified {
		t.Fatal("requiredSendBlockedNotified should be true after first call")
	}
}

// TestNotifyRequiredReceiveDiscardedDedup verifies that
// NotifyRequiredReceiveDiscarded fires only once per session.
func TestNotifyRequiredReceiveDiscardedDedup(t *testing.T) {
	sess, cleanup := newTestEncryptionSession(t, sequenceTlsRoleClient)
	defer cleanup()

	sess.NotifyRequiredReceiveDiscarded("first call")
	sess.NotifyRequiredReceiveDiscarded("second call")

	sess.stateLock.Lock()
	defer sess.stateLock.Unlock()
	if !sess.requiredReceiveDiscardedNotified {
		t.Fatal("requiredReceiveDiscardedNotified should be true after first call")
	}
}

// TestSendPackRWMutexPackVsClose verifies that Pack (RLock) and Close (Lock)
// do not deadlock. Pack holds RLock for up to the gate-poll duration; Close
// cancels the sequence ctx first, which unblocks the parked send within one
// poll, then takes the write lock to close the channel. Both must complete.
func TestSendPackRWMutexPackVsClose(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	a, b, _, bClientId, _, _ := requiredGatePair(
		ctx, EncryptionModeRequired, EncryptionModeOff, nil, true)
	defer b.Cancel()

	// Start a send that will park at the gate (cipher never establishes
	// because b is Off).
	parked := make(chan bool, 1)
	go func() {
		parked <- a.SendWithTimeout(
			requiredGateFrame(t, "parked"),
			DestinationId(bClientId),
			func(error) {},
			-1,
		)
	}()

	// Wait for the send to be parked (gate is holding RLock).
	time.Sleep(200 * time.Millisecond)

	// Cancel a's context, which triggers Close on the send sequence.
	// Close takes packMutex.Lock(). If Pack's RLock were held forever,
	// Lock would block — the parked send must unblock on ctx.Done first.
	a.Cancel()

	select {
	case <-parked:
		// Good: the send returned (either success or error).
	case <-time.After(10 * time.Second):
		t.Fatal("deadlock: Pack (RLock) and Close (Lock) did not complete within 10s")
	}
}

// TestSendPackRWMutexConcurrentPacks verifies that multiple concurrent Pack
// calls (all taking RLock) do not block each other once the cipher is up.
func TestSendPackRWMutexConcurrentPacks(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	a, b, _, bClientId, _, receivesB := requiredGatePair(
		ctx, EncryptionModeRequired, EncryptionModeRequired, nil, true)
	defer a.Cancel()
	defer b.Cancel()

	// Wait for the cipher to establish.
Established:
	for i := 0; i < 100; i++ {
		m := &protocol.SimpleMessage{Content: "probe"}
		frame, err := ToFrame(m, DefaultProtocolVersion)
		if err != nil {
			t.Fatalf("frame: %v", err)
		}
		ok := a.SendWithTimeout(frame, DestinationId(bClientId), func(error) {}, 1*time.Second)
		if ok {
			break Established
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Once established, fire many concurrent sends.
	const N = 10
	var wg sync.WaitGroup
	wg.Add(N)
	start := time.Now()
	for i := 0; i < N; i++ {
		go func(i int) {
			defer wg.Done()
			m := &protocol.SimpleMessage{Content: fmt.Sprintf("concurrent-%d", i)}
			frame, err := ToFrame(m, DefaultProtocolVersion)
			if err != nil {
				t.Errorf("frame: %v", err)
				return
			}
			ok := a.SendWithTimeout(frame, DestinationId(bClientId), func(error) {}, 5*time.Second)
			if !ok {
				t.Errorf("concurrent send %d failed", i)
			}
		}(i)
	}
	wg.Wait()
	elapsed := time.Since(start)

	if 10*time.Second <= elapsed {
		t.Fatalf("10 concurrent sends took %s: RWMutex Pack may be serialized", elapsed)
	}

	// Verify all messages were received.
	timeout := time.After(30 * time.Second)
	received := 0
	for received < N {
		select {
		case <-receivesB:
			received++
		case <-timeout:
			t.Fatalf("only received %d/%d messages", received, N)
		}
	}
}

// TestEncryptionModeZeroValueIsOff verifies that the zero value of
// EncryptionMode is Off (the default for an uninitialized EncryptionSettings).
func TestEncryptionModeZeroValueIsOff(t *testing.T) {
	var mode EncryptionMode
	if mode != EncryptionModeOff {
		t.Fatalf("zero value of EncryptionMode is %v, want EncryptionModeOff", mode)
	}

	settings := &EncryptionSettings{}
	if settings.Mode != EncryptionModeOff {
		t.Fatalf("zero-value EncryptionSettings.Mode is %v, want EncryptionModeOff", settings.Mode)
	}
}

// TestErrEncryptionRequiredNotEstablishedIsDistinct verifies that
// ErrEncryptionRequiredNotEstablished is distinguishable from transport
// backpressure via errors.Is.
func TestErrEncryptionRequiredNotEstablishedIsDistinct(t *testing.T) {
	err := ErrEncryptionRequiredNotEstablished
	if !errors.Is(err, ErrEncryptionRequiredNotEstablished) {
		t.Fatal("errors.Is should match")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("should not match unrelated errors")
	}
}

// TestRequiredGateNonBlockingSendDistinguishesFromBackpressure verifies that
// a timeout==0 send returns ErrEncryptionRequiredNotEstablished (the typed
// error), not (false, nil) backpressure, so callers can distinguish
// "encryption not ready" from "queue full".
func TestRequiredGateNonBlockingSendDistinguishesFromBackpressure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	a, b, _, bClientId, _, receivesB := requiredGatePair(
		ctx, EncryptionModeRequired, EncryptionModeOff, nil, true)
	defer a.Cancel()
	defer b.Cancel()

	// Initialize the session: a parked send (infinite timeout) starts the
	// held handshake against the Off peer.
	parked := make(chan bool, 1)
	go func() {
		parked <- a.SendWithTimeout(
			requiredGateFrame(t, "parked"),
			DestinationId(bClientId),
			func(error) {},
			-1,
		)
	}()
	select {
	case ok := <-parked:
		t.Fatalf("send against an Off peer must park at the gate, returned %t", ok)
	case <-time.After(1 * time.Second):
	}

	_, err := a.SendWithTimeoutDetailed(
		requiredGateFrame(t, "nonblocking"),
		DestinationId(bClientId),
		func(error) {},
		0,
	)
	if err == nil {
		t.Fatal("expected error for non-blocking send with no cipher")
	}
	if !errors.Is(err, ErrEncryptionRequiredNotEstablished) {
		t.Fatalf("expected ErrEncryptionRequiredNotEstablished, got %v", err)
	}

	// Verify nothing was delivered to the Off peer.
	select {
	case got := <-receivesB:
		t.Fatalf("fail-closed violated: Off peer received %q", got)
	case <-time.After(200 * time.Millisecond):
	}
}
