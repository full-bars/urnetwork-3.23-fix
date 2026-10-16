// Public facts originate at the actual claim reconciler. A successful Claimed
// event accepts liability; only a separate ClaimPaid event transfers aggregate
// coldkey credit. These observations never authorize a new transaction.
package main

import (
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/urfoundation/sn/miner/onchain"
	"github.com/urfoundation/sn/protocol"
	"github.com/urfoundation/sn/stabi"
	"github.com/urnetwork/connect"
)

func claimObservationAmount(value *big.Int) string {
	if value == nil || value.Sign() < 0 || value.BitLen() > 256 {
		return ""
	}
	return value.String()
}

func claimObservationLogIdentity(log *types.Log, receipt *types.Receipt) bool {
	return log != nil && !log.Removed && log.TxHash == receipt.TxHash && log.BlockHash == receipt.BlockHash && log.BlockNumber == receipt.BlockNumber.Uint64() && log.TxIndex == receipt.TransactionIndex
}

// Pure projection follows the already verified exact receipt. Malformed or
// ambiguous payment events degrade payment reporting, not signed reconciliation.
func signedClaimObservation(tx *types.Transaction, intent *onchain.ClaimIntent, from common.Address, receipt *types.Receipt) *protocol.ClaimObservation {
	if verifySignedClaimReceipt(tx, intent, from, receipt) != nil || !tx.ChainId().IsUint64() {
		return nil
	}
	observation := &protocol.ClaimObservation{Schema: protocol.ClaimObservationSchema, EvidenceKind: "signed-receipt", Epoch: intent.E.Int64(), ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), Pool: protocol.ClaimProgressPool{ChainId: tx.ChainId().Uint64(), Vault: strings.ToLower(tx.To().Hex()), NoId: intent.NoID.String(), Coldkey: common.Hash(intent.Coldkey).Hex()}, ShareBps: intent.ShareBps.Uint64(), ProofStatus: "contract-accepted", BlockNumber: receipt.BlockNumber.Uint64(), BlockHash: strings.ToLower(receipt.BlockHash.Hex()), TransactionHash: strings.ToLower(tx.Hash().Hex()), Relayer: strings.ToLower(from.Hex()), PaymentStatus: "unknown"}
	contractAbi, err := stabi.STSettlementVaultMetaData.ParseABI()
	if err != nil {
		return nil
	}
	paidTopic := contractAbi.Events["ClaimPaid"].ID
	deferredTopic := contractAbi.Events["ClaimPaymentDeferred"].ID
	if paidTopic == (common.Hash{}) || deferredTopic == (common.Hash{}) {
		return nil
	}
	payments := 0
	invalid := false
	observation.Authority = "configured-rpc-assertion"
	observation.GenesisStatus = "unverified"
	for _, log := range receipt.Logs {
		if log == nil || log.Address != *tx.To() {
			continue
		}
		if event, err := stSettlementVault.UnpackClaimedEvent(log); err == nil {
			observation.AcceptedAmountRao = claimObservationAmount(event.Amount)
		}
		if len(log.Topics) == 0 || (log.Topics[0] != paidTopic && log.Topics[0] != deferredTopic) {
			continue
		}
		if len(log.Topics) < 2 {
			invalid = true
			continue
		}
		if log.Topics[1] != common.Hash(intent.Coldkey) {
			continue
		}
		payments++
		if !claimObservationLogIdentity(log, receipt) {
			invalid = true
			continue
		}
		if log.Topics[0] == paidTopic {
			event, err := stSettlementVault.UnpackClaimPaidEvent(log)
			if err != nil || event.Coldkey != intent.Coldkey || event.Relayer != from || claimObservationAmount(event.Amount) == "" {
				invalid = true
				continue
			}
			observation.PaymentStatus, observation.AggregatePaidRao = "aggregate-paid", event.Amount.String()
		} else {
			event, err := stSettlementVault.UnpackClaimPaymentDeferredEvent(log)
			if err != nil || event.Coldkey != intent.Coldkey || claimObservationAmount(event.CreditAlphaRao) == "" {
				invalid = true
				continue
			}
			observation.PaymentStatus, observation.UnpaidCreditRao = "deferred", event.CreditAlphaRao.String()
		}
	}
	if invalid || payments > 1 {
		observation.PaymentStatus = "invalid"
		observation.UnpaidCreditRao, observation.AggregatePaidRao = "", ""
	}
	// The contract adds this accepted liability before settling its aggregate
	// credit. A smaller reported settlement cannot describe that transition.
	accepted, _ := new(big.Int).SetString(observation.AcceptedAmountRao, 10)
	for _, amount := range []string{observation.UnpaidCreditRao, observation.AggregatePaidRao} {
		if amount != "" {
			value, ok := new(big.Int).SetString(amount, 10)
			if !ok || accepted == nil || value.Cmp(accepted) < 0 {
				observation.PaymentStatus = "invalid"
				observation.UnpaidCreditRao, observation.AggregatePaidRao = "", ""
			}
		}
	}
	if observation.Validate() != nil {
		return nil
	}
	return observation
}

// The finalized entitlement and leaf were read at one closed block witness.
// A missing/invalid advertised Merkle proof remains explicitly unknown.
func finalizedClaimObservation(claim *connect.SnPoolClaimResult, entitlement stabi.STSettlementVaultEntitlement, block onchain.EVMBlockIdentity, claimed bool) *protocol.ClaimObservation {
	// The v33 claim result carries the contract address only.
	vault := claim.ContractAddress
	observation := &protocol.ClaimObservation{Schema: protocol.ClaimObservationSchema, EvidenceKind: "finalized-leaf", Epoch: int64(claim.Epoch), ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), Pool: protocol.ClaimProgressPool{ChainId: claim.ChainId, Vault: strings.ToLower(vault), NoId: new(big.Int).SetBytes(claim.NoId).String(), Coldkey: common.BytesToHash(claim.Coldkey).Hex()}, ShareBps: uint64(claim.ShareBps), ProofStatus: "unknown", PayoutRoot: common.Hash(entitlement.PayoutRoot).Hex(), ArtifactHash: common.Hash(entitlement.ArtifactHash).Hex(), LeafClaimed: &claimed, BlockNumber: block.Number, BlockHash: strings.ToLower(block.Hash.Hex()), PaymentStatus: "unknown"}
	if len(claim.Proof) <= 64 {
		if _, _, err := claimCalldata(claim); err == nil {
			observation.ProofStatus = "merkle-verified"
		}
	}
	observation.Authority = "configured-rpc-assertion"
	observation.GenesisStatus = "unverified"
	if claim.ChainId == 0 || observation.Validate() != nil {
		return nil
	}
	return observation
}

func absentClaimObservation(epoch int64) *protocol.ClaimObservation {
	return &protocol.ClaimObservation{Schema: protocol.ClaimObservationSchema, EvidenceKind: "api-no-claim", Authority: "api-assertion", GenesisStatus: "unverified", Epoch: epoch, ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), ProofStatus: "unknown", PaymentStatus: "unknown"}
}
