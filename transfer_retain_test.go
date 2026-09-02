package connect

import (
	"context"
	"errors"
	"testing"
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

// TestDecodeTransferOptionsRetainAfterAckTimeout is the R-1 regression test.
// It drives the REAL option decoding path (decodeTransferOptions, extracted
// from sendWithTimeoutDetailed) through the public RetainAfterAckTimeout()
// option. If the option switch were to ever drop this case again (as it did
// initially — the retain flag never reached the send path), this test fails.
func TestDecodeTransferOptionsRetainAfterAckTimeout(t *testing.T) {
	ctx := context.Background()
	base := DefaultTransferOpts()
	if base.RetainAfterAckTimeout {
		t.Fatal("default TransferOptions must not retain")
	}

	got := decodeTransferOptions(base, []any{RetainAfterAckTimeout()}, &ctx)
	if !got.RetainAfterAckTimeout {
		t.Fatal("decodeTransferOptions(RetainAfterAckTimeout()) lost the retain flag — R-1 regression (option case missing)")
	}
}

// TestDecodeTransferOptionsDefaults checks the decode doesn't clobber other
// options when only the retain flag is passed.
func TestDecodeTransferOptionsDefaults(t *testing.T) {
	ctx := context.Background()
	base := TransferOptions{Ack: false, ForceStream: true, CompanionContract: true, RetainAfterAckTimeout: false}

	got := decodeTransferOptions(base, []any{RetainAfterAckTimeout()}, &ctx)
	if !got.RetainAfterAckTimeout {
		t.Fatal("retain flag not set")
	}
	if got.Ack != false || got.ForceStream != true || got.CompanionContract != true {
		t.Fatalf("decode clobbered unrelated options: %+v", got)
	}
}

// TestDecodeTransferOptionsRetainOrderDependent verifies that passing a full
// TransferOptions AFTER RetainAfterAckTimeout() clobbers the retain flag.
// This order-dependent behavior was previously untested.
// The case "TransferOptions: base = v" overwrites base entirely.
func TestDecodeTransferOptionsRetainOrderDependent(t *testing.T) {
	ctx := context.Background()
	// RetainAfterAckTimeout() sets the flag, then a full TransferOptions
	// with RetainAfterAckTimeout=false overwrites it.
	got := decodeTransferOptions(
		DefaultTransferOpts(),
		[]any{RetainAfterAckTimeout(), TransferOptions{RetainAfterAckTimeout: false}},
		&ctx,
	)
	if got.RetainAfterAckTimeout {
		t.Fatal("full TransferOptions after RetainAfterAckTimeout() should clobber the retain flag")
	}
}

// TestDecodeTransferOptionsRetainThroughFullOptions verifies that embedding
// the retain flag in a full TransferOptions also works (the non-clobber case).
func TestDecodeTransferOptionsRetainThroughFullOptions(t *testing.T) {
	ctx := context.Background()
	got := decodeTransferOptions(
		DefaultTransferOpts(),
		[]any{TransferOptions{RetainAfterAckTimeout: true}},
		&ctx,
	)
	if !got.RetainAfterAckTimeout {
		t.Fatal("full TransferOptions with RetainAfterAckTimeout=true should set the flag")
	}
}
