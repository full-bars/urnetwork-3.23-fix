// The v2 claim queue (§10.1) keeps one unit per (vault, noId, account index).
// Unlike a v1 epoch entry no unit is terminal: an idle unit reopens whenever
// a newer finalized root raises the account above its claimed watermark.
package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/holiman/uint256"

	"github.com/urnetwork/connect/internal/durablehead"
)

const ClaimQueueV2Schema = "urnetwork-provider-claim-queue-v2"

type ClaimQueueV2 struct {
	Schema string                  `json:"schema"`
	Units  map[string]*ClaimUnitV2 `json:"units"`
}

// Status is idle (nothing due), open (due for a verified claim), submitting
// (the durable boundary before signing) or uncertain (its one signed claim is
// in flight). Cumulative and Claimed are the last verified finalized values.
type ClaimUnitV2 struct {
	Vault       string `json:"vault"`
	NoID        uint64 `json:"no_id"`
	Index       uint64 `json:"index"`
	Coldkey     string `json:"coldkey"`
	Status      string `json:"status"`
	Demand      bool   `json:"demand,omitempty"`
	Attempts    int    `json:"attempts"`
	RootIndex   uint64 `json:"root_index"`
	Cumulative  string `json:"cumulative"`
	Claimed     string `json:"claimed"`
	UpdatedAt   string `json:"updated_at"`
	NextRetryAt string `json:"next_retry_at,omitempty"`
	TxHash      string `json:"tx_hash,omitempty"`
	RawTxHex    string `json:"raw_tx_hex,omitempty"`
	LastError   string `json:"last_error,omitempty"`
}

func claimUnitV2Key(vault string, noID, index uint64) string {
	return fmt.Sprintf("%s/%d/%d", vault, noID, index)
}

func (self *ClaimUnitV2) signed() bool { return self.TxHash != "" || self.RawTxHex != "" }

// A unit is due once claimable (finalized cumulative − claimed) reaches the
// threshold, or on demand once anything is claimable.
func claimV2Due(cumulative, claimed, threshold *uint256.Int, demand bool) bool {
	if !claimed.Lt(cumulative) {
		return false
	}
	return demand || !new(uint256.Int).Sub(cumulative, claimed).Lt(threshold)
}

// Discovery only opens a unit from the endpoint's figures; only a verified
// finalized read closes one, so a stale or false figure cannot skip a claim.
// A demand marks every unit with anything claimable until it is idle again.
// Reports whether the queue changed.
func (self *ClaimQueueV2) merge(accounts map[string]*claimAccountV2, threshold *uint256.Int, demand bool, now string) bool {
	changed := false
	for key, account := range accounts {
		unit := self.Units[key]
		if unit == nil {
			unit = &ClaimUnitV2{Vault: account.Vault, NoID: account.NoID, Index: account.Index, Coldkey: "0x" + hex.EncodeToString(account.Coldkey[:]), Status: "idle", Cumulative: "0", Claimed: "0", UpdatedAt: now}
			self.Units[key], changed = unit, true
		}
		if demand && !unit.Demand && claimV2Due(&account.Cumulative, &account.Claimed, threshold, true) {
			unit.Demand, changed = true, true
		}
		if unit.Status == "idle" && claimV2Due(&account.Cumulative, &account.Claimed, threshold, unit.Demand) {
			unit.Status, unit.UpdatedAt, changed = "open", now, true
		}
	}
	return changed
}

// Units past their retry deadline, oldest deadline first so that a unit that
// keeps failing cannot hide another, as in v1. An unsigned unit waits for
// fresh account data; a signed one reconciles its exact bytes alone.
func (self *ClaimQueueV2) due(now time.Time, accounts map[string]*claimAccountV2) []string {
	keys, deadlines := []string{}, map[string]time.Time{}
	for key, unit := range self.Units {
		when, _ := time.Parse(time.RFC3339Nano, unit.NextRetryAt)
		if unit.Status == "idle" || !unit.signed() && accounts[key] == nil || now.Before(when) {
			continue
		}
		keys, deadlines[key] = append(keys, key), when
	}
	sort.Slice(keys, func(i, j int) bool {
		left, right := deadlines[keys[i]], deadlines[keys[j]]
		return left.Before(right) || left.Equal(right) && keys[i] < keys[j]
	})
	return keys
}

// The v2 queue reuses the v1 directory lock and durable head unchanged in its
// own state_dir. Only its bytes differ, so v1 refuses a v2 queue and back.
type claimQueueV2Store struct{ owner *claimQueueStore }

func newClaimQueueV2Store(ctx context.Context, stateDir string) (*claimQueueV2Store, error) {
	owner, err := newClaimQueueStore(stateDir, ctx)
	if err != nil {
		return nil, err
	}
	return &claimQueueV2Store{owner: owner}, nil
}

func (self *claimQueueV2Store) close() error { return self.owner.close() }

// As in v1, a restart before the prepared checkpoint retries; after it the
// exact signed bytes are reconciled and never replaced.
func (self *claimQueueV2Store) load() (*ClaimQueueV2, error) {
	if err := self.owner.requireOwner(); err != nil {
		return nil, err
	}
	raw, present, err := self.owner.head.Read()
	if err != nil {
		return nil, err
	}
	if !present {
		return &ClaimQueueV2{Schema: ClaimQueueV2Schema, Units: map[string]*ClaimUnitV2{}}, nil
	}
	var queue ClaimQueueV2
	if err := json.Unmarshal(raw, &queue); err != nil {
		return nil, err
	}
	if queue.Schema != ClaimQueueV2Schema || queue.Units == nil {
		return nil, errors.New("invalid v2 claim queue schema")
	}
	for key, unit := range queue.Units {
		if unit != nil && unit.Status == "submitting" {
			unit.Status, unit.LastError = "open", "process restarted before the durable broadcast boundary"
			if unit.signed() {
				unit.Status, unit.LastError = "uncertain", "process restarted after exact transaction preparation"
			}
		}
		if unit == nil || key != claimUnitV2Key(unit.Vault, unit.NoID, unit.Index) || unit.Status != "idle" && unit.Status != "open" && unit.Status != "uncertain" || (unit.Status == "uncertain") != unit.signed() {
			return nil, fmt.Errorf("v2 claim queue unit %s is malformed", key)
		}
		if _, err := time.Parse(time.RFC3339Nano, unit.NextRetryAt); unit.NextRetryAt != "" && err != nil {
			return nil, fmt.Errorf("v2 claim queue unit %s has an invalid retry deadline: %w", key, err)
		}
	}
	return &queue, nil
}

// Write admission gates every publication, as in v1. An uncertain one refuses
// later saves until a new owner reconciles it.
func (self *claimQueueV2Store) save(queue *ClaimQueueV2) error {
	owner := self.owner
	if err := owner.requireWrite(owner.ctx); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(queue, "", "  ")
	if err != nil {
		return err
	}
	err = owner.head.Publish(append(raw, '\n'), owner.afterSync)
	owner.uncertain = errors.Is(err, durablehead.ErrUncertain)
	return err
}

// A physical writer alone cannot authorize unrelated bytes: the exact signed
// RLP must still be in the acknowledged v2 queue.
func (self *claimQueueV2Store) requireSigned(ctx context.Context, tx *types.Transaction) error {
	if err := self.owner.requireWrite(ctx); err != nil {
		return err
	}
	raw, err := tx.MarshalBinary()
	if err != nil {
		return err
	}
	queue, err := self.load()
	if err != nil {
		return err
	}
	for _, unit := range queue.Units {
		if strings.EqualFold(unit.TxHash, tx.Hash().Hex()) && unit.RawTxHex == "0x"+hex.EncodeToString(raw) {
			return self.owner.requireWrite(ctx)
		}
	}
	return errors.New("v2 claim exact signed transaction is absent from acknowledged custody")
}
