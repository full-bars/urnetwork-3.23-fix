// Pending nonce observations can lag an earlier prepared/send timeout. The
// shared admission therefore remembers every durably signed nonce, including
// all member queues loaded before startup, until this process ends.
package main

import (
	"errors"
	"fmt"
	"math"
	"math/big"

	"github.com/ethereum/go-ethereum/core/types"
)

type claimNonceSafetyError struct{ cause error }

func (self *claimNonceSafetyError) Error() string {
	return "claim nonce custody is unavailable: " + self.cause.Error()
}
func (self *claimNonceSafetyError) Unwrap() error { return self.cause }

func (self *claimAdmission) checkChain(chainId *big.Int) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.nonceError != nil {
		return self.nonceError
	}
	if chainId == nil || chainId.Sign() <= 0 {
		self.nonceError = &claimNonceSafetyError{cause: errors.New("invalid chain identity")}
	} else if self.chainId != nil && self.chainId.Cmp(chainId) != 0 {
		self.nonceError = &claimNonceSafetyError{cause: errors.New("shared relayer has signed intents on a different chain")}
	} else if self.chainId == nil {
		self.chainId = new(big.Int).Set(chainId)
	}
	return self.nonceError
}

// An authenticated nonce-custody failure stops new signatures immediately,
// before the failing member unwinds and the swarm cancels its siblings.
func (self *claimAdmission) failNonce(err error) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.nonceError == nil {
		self.nonceError = &claimNonceSafetyError{cause: err}
	}
	return self.nonceError
}

func (self *claimAdmission) nonceMinimum() uint64 {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.nonceFloor
}

// Call after an acknowledged or uncertain exact raw/hash checkpoint, or while
// seeding retained files before workers start. Uncertainty conservatively keeps
// the signed nonce reserved until that owner's actual bytes are reconciled.
// No receipt/finality is inferred.
func (self *claimAdmission) rememberSigned(cfg *ClaimDaemonConfig, entry *ClaimQueueEntry) error {
	tx, _, _, err := authenticateSignedClaim(cfg, entry)
	if err != nil {
		return self.failNonce(err)
	}
	return self.rememberNonce(tx)
}

// The v1 and v2 daemons share this floor for one authenticated relayer signature.
func (self *claimAdmission) rememberNonce(tx *types.Transaction) error {
	if tx.Nonce() == math.MaxUint64 {
		return self.failNonce(errors.New("signed nonce has no successor"))
	}
	if err := self.checkChain(tx.ChainId()); err != nil {
		return err
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.nonceFloor = max(self.nonceFloor, tx.Nonce()+1)
	return nil
}

// The caller must retain ownership through startup and every worker's joined
// shutdown; an unlocked path cannot supply a stable nonce-custody snapshot.
func (self *claimAdmission) seedMember(cfg *ClaimDaemonConfig, store *claimQueueStore) error {
	queue, err := store.load()
	if err != nil {
		return err
	}
	self.observeEpoch(queue.LastDiscovered)
	for _, entry := range queue.Entries {
		if entry == nil {
			return errors.New("claim queue contains a nil entry")
		}
		if entry.TxHash == "" && entry.RawTxHex == "" {
			continue
		}
		if err := self.rememberSigned(cfg, entry); err != nil {
			return fmt.Errorf("claim epoch %d: %w", entry.Epoch, err)
		}
	}
	return nil
}
