// Exact signed liabilities outlive the API's historical artifact retention.
// Receipt recovery authenticates intent and event identity before disposal.
package main

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/urfoundation/sn/miner/onchain"
)

type claimArtifactRootMismatchError struct {
	epoch      int64
	advertised [32]byte
	finalized  [32]byte
}

func (self *claimArtifactRootMismatchError) Error() string {
	return fmt.Sprintf("finalized payout root does not match claim artifact for epoch %d: finalized 0x%x, advertised 0x%x", self.epoch, self.finalized, self.advertised)
}

// A failed/consumed signed attempt cannot authorize erasing its bytes. A later
// attempt needs authenticated new intent plus versioned attempt history first.
type claimSignedOutcomeError struct {
	reason string
	cause  error
}

func (self *claimSignedOutcomeError) Error() string {
	if self.cause != nil {
		return "signed claim outcome unresolved: " + self.reason + ": " + self.cause.Error()
	}
	return "signed claim outcome unresolved: " + self.reason
}
func (self *claimSignedOutcomeError) Unwrap() error { return self.cause }

func authenticateSignedClaim(cfg *ClaimDaemonConfig, entry *ClaimQueueEntry) (*types.Transaction, *onchain.ClaimIntent, common.Address, error) {
	if cfg == nil || entry == nil {
		return nil, nil, common.Address{}, errors.New("signed claim configuration is unavailable")
	}
	raw, err := hex.DecodeString(strings.TrimPrefix(entry.RawTxHex, "0x"))
	if err != nil || len(raw) == 0 {
		return nil, nil, common.Address{}, errors.New("signed claim has no valid exact transaction bytes")
	}
	var tx types.Transaction
	if err := tx.UnmarshalBinary(raw); err != nil || !strings.EqualFold(tx.Hash().Hex(), entry.TxHash) {
		return nil, nil, common.Address{}, errors.New("signed claim transaction bytes/hash mismatch")
	}
	if !tx.Protected() || tx.ChainId().Sign() <= 0 || tx.To() == nil || tx.Value().Sign() != 0 {
		return nil, nil, common.Address{}, errors.New("signed claim has an invalid chain, target or value")
	}
	intent, err := onchain.DecodeClaimCalldata(tx.Data())
	if err != nil {
		return nil, nil, common.Address{}, err
	}
	if !intent.E.IsInt64() || intent.E.Int64() != entry.Epoch || entry.Epoch < 0 || intent.NoID.Sign() <= 0 || intent.Coldkey == ([32]byte{}) || intent.ShareBps.Sign() <= 0 || !intent.ShareBps.IsUint64() || intent.ShareBps.Uint64() > 10_000 {
		return nil, nil, common.Address{}, errors.New("signed claim calldata identity differs from its queue entry")
	}
	from, err := types.Sender(types.LatestSignerForChainID(tx.ChainId()), &tx)
	if err != nil {
		return nil, nil, common.Address{}, err
	}
	key, err := onchain.LoadKeyFile(cfg.KeyFile)
	if err != nil || crypto.PubkeyToAddress(key.PublicKey) != from {
		return nil, nil, common.Address{}, errors.New("signed claim signer does not match configured relayer")
	}
	return &tx, intent, from, nil
}

func verifySignedClaimReceipt(tx *types.Transaction, intent *onchain.ClaimIntent, from common.Address, receipt *types.Receipt) error {
	if tx == nil || intent == nil || receipt == nil || receipt.BlockNumber == nil || !receipt.BlockNumber.IsUint64() || receipt.TxHash != tx.Hash() || receipt.Status != types.ReceiptStatusSuccessful {
		return errors.New("exact signed transaction has no successful receipt")
	}
	matches := 0
	for _, log := range receipt.Logs {
		if log == nil || log.Address != *tx.To() {
			continue
		}
		event, err := stSettlementVault.UnpackClaimedEvent(log)
		if err != nil {
			continue
		}
		if log.Removed || log.TxHash != receipt.TxHash || log.BlockHash != receipt.BlockHash || log.BlockNumber != receipt.BlockNumber.Uint64() || log.TxIndex != receipt.TransactionIndex || event.Epoch == nil || event.Epoch.Cmp(intent.E) != 0 || event.NoId == nil || event.NoId.Cmp(intent.NoID) != 0 || event.Coldkey != intent.Coldkey || event.ShareBps == nil || event.ShareBps.Cmp(intent.ShareBps) != 0 || event.Relayer != from || event.Amount == nil || event.Amount.Sign() < 0 {
			return errors.New("canonical Claimed event differs from exact signed intent")
		}
		matches++
	}
	if matches != 1 {
		return fmt.Errorf("exact signed claim has %d matching Claimed events, want one", matches)
	}
	return nil
}

func reconcileSignedClaim(ctx context.Context, cfg *ClaimDaemonConfig, entry *ClaimQueueEntry, store *claimQueueStore) (string, error) {
	if err := store.requireOwner(); err != nil {
		return "", err
	}
	tx, intent, from, err := authenticateSignedClaim(cfg, entry)
	if err != nil {
		return "", &claimSignedOutcomeError{reason: "signed identity cannot be authenticated", cause: err}
	}
	receipt, receiptErr := finalizedClaimReceipt(ctx, cfg, entry.TxHash, tx.ChainId())
	if receiptErr == nil {
		if err := verifySignedClaimReceipt(tx, intent, from, receipt); err != nil {
			return "", &claimSignedOutcomeError{reason: "canonical receipt does not prove this claim succeeded", cause: err}
		}
		if err := recordFinalizedClaimReceipt(entry, receipt); err != nil {
			return "", err
		}
		entry.PublicObservation = signedClaimObservation(tx, intent, from, receipt)
		return "finalized", nil
	}
	// The saved signature authorizes only the original transaction. If no
	// receipt is available, finalized nonce and exact eth_call authorize an
	// identical rebroadcast without requiring the API to retain old artifacts.
	consumed, err := rebroadcastSignedClaim(ctx, cfg, tx, from, store)
	if err != nil {
		return "", &claimSignedOutcomeError{reason: "exact rebroadcast did not reconcile outcome", cause: err}
	}
	if consumed {
		return "", &claimSignedOutcomeError{reason: "signed nonce is consumed without an authenticated successful receipt"}
	}
	return "", &claimSignedOutcomeError{reason: "exact transaction is not yet canonically finalized", cause: receiptErr}
}
