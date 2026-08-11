package connect

import (
	"context"
	"testing"
	"time"
)

// TestRequiredContractOpenRideAlongDelivers exercises the contract-open
// ride-along path (transfer.go updateContract ForceUnwrapped pinning) under
// EncryptionModeRequired on both peers: the first send, which carries the
// contract-open, must deliver once the cipher establishes, and a subsequent
// app-data frame must deliver sealed. This is the end-to-end Required-mode
// contract-open path no other test covers; the plaintext-before-cipher pin
// itself has no deterministic seam (documented known gap) — this test pins
// that the ride-along does not wedge or drop the session.
func TestRequiredContractOpenRideAlongDelivers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Required×Required: both sides need the cipher. Attach b's transports
	// immediately so the handshake completes and the cipher establishes.
	a, b, _, bClientId, _, receivesB := requiredGatePair(
		ctx, EncryptionModeRequired, EncryptionModeRequired, nil, true)
	defer a.Cancel()
	defer b.Cancel()

	// First send: the contract-open rides along. Since both sides are
	// Required and b's transports are attached, the handshake completes and
	// the send delivers.
	sent1 := make(chan bool, 1)
	go func() {
		sent1 <- a.SendWithTimeout(
			requiredGateFrame(t, "contract-open-ride"),
			DestinationId(bClientId),
			func(error) {},
			30*time.Second,
		)
	}()
	select {
	case ok := <-sent1:
		if !ok {
			t.Fatal("first send (with contract-open) must succeed once cipher establishes")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("first send timed out")
	}

	select {
	case got := <-receivesB:
		if got != "contract-open-ride" {
			t.Fatalf("peer received %q, want %q", got, "contract-open-ride")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("peer never received the contract-open ride-along message")
	}

	// Second send: cipher is already established, so this is wrapped normally.
	sent2 := make(chan bool, 1)
	go func() {
		sent2 <- a.SendWithTimeout(
			requiredGateFrame(t, "sealed-app-data"),
			DestinationId(bClientId),
			func(error) {},
			30*time.Second,
		)
	}()
	select {
	case ok := <-sent2:
		if !ok {
			t.Fatal("second send (sealed) must succeed")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("second send timed out")
	}

	select {
	case got := <-receivesB:
		if got != "sealed-app-data" {
			t.Fatalf("peer received %q, want %q", got, "sealed-app-data")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("peer never received the sealed app-data message")
	}
}
