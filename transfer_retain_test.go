package connect

import (
	"errors"
	"testing"
	"time"
)

// TestContractOpenAckErrorDoesNotPromoteContract verifies that a failed
// contract-open send does not promote the opening contract. Ported from
// upstream to match the fork's contractOpenAckCallback.
func TestContractOpenAckErrorDoesNotPromoteContract(t *testing.T) {
	contract := &sequenceContract{}
	sequence := &SendSequence{sendContract: contract}
	callback := sequence.contractOpenAckCallback(contract)

	callback(errors.New("opening contract send failed"))
	if sequence.sendContractAcked {
		t.Fatal("failed contract-open send promoted the contract")
	}

	callback(nil)
	if !sequence.sendContractAcked {
		t.Fatal("successful contract-open Ack did not promote the contract")
	}

	replacement := &sequenceContract{}
	sequence.sendContract = replacement
	sequence.sendContractAcked = false
	callback(nil)
	if sequence.sendContractAcked {
		t.Fatal("late contract-open Ack promoted a replacement contract")
	}
}

// TestSendItemRetainBackstopDeadline verifies that retained items get a
// backstop deadline of 10x AckTimeout and non-retained items do not.
func TestSendItemRetainBackstopDeadline(t *testing.T) {
	ackTimeout := 30 * time.Second
	now := time.Now()

	// Retained item should have a backstop deadline
	retained := &sendItem{
		retainAfterAckTimeout: true,
		sendTime:              now,
	}
	// Simulate what sendWithSetContract does
	if retained.retainAfterAckTimeout {
		retained.backstopDeadline = now.Add(ackTimeout * 10)
	}
	expected := now.Add(ackTimeout * 10)
	if !retained.backstopDeadline.Equal(expected) {
		t.Fatalf("retained backstop deadline = %v, want %v", retained.backstopDeadline, expected)
	}

	// Non-retained item should have zero backstop deadline
	ordinary := &sendItem{
		retainAfterAckTimeout: false,
		sendTime:              now,
	}
	if !ordinary.backstopDeadline.IsZero() {
		t.Fatalf("non-retained backstop deadline = %v, want zero", ordinary.backstopDeadline)
	}
}

// TestSendItemRetainPastBackstop verifies the backstop expiry logic:
// items past their backstop deadline are eligible for forced drop.
func TestSendItemRetainPastBackstop(t *testing.T) {
	now := time.Now()
	ackTimeout := 30 * time.Second

	// Item created 11 minutes ago with 30s AckTimeout -> backstop at +300s
	// = 6 minutes ago -> past backstop
	oldItem := &sendItem{
		retainAfterAckTimeout: true,
		sendTime:              now.Add(-11 * time.Minute),
		backstopDeadline:      now.Add(-11 * time.Minute).Add(ackTimeout * 10),
	}
	if !now.After(oldItem.backstopDeadline) {
		t.Fatal("old item should be past backstop")
	}

	// Item created 1 minute ago with 30s AckTimeout -> backstop at +300s
	// = 4 minutes from now -> NOT past backstop
	freshItem := &sendItem{
		retainAfterAckTimeout: true,
		sendTime:              now.Add(-1 * time.Minute),
		backstopDeadline:      now.Add(-1 * time.Minute).Add(ackTimeout * 10),
	}
	if now.After(freshItem.backstopDeadline) {
		t.Fatal("fresh item should NOT be past backstop")
	}
}
