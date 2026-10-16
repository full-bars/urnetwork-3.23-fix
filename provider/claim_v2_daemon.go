// The v2 claim daemon (§10.1) claims settlement-v2 accounts beside the
// unchanged v1 daemon. It reuses the v1 directory lock, durable head, nonce
// admission, finality helpers and relayer key as they are.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/docopt/docopt-go"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/holiman/uint256"
	"github.com/urnetwork/connect"
	"gopkg.in/yaml.v3"

	"github.com/urfoundation/sn/miner/onchain"
	"github.com/urfoundation/sn/ss58"
	"github.com/urfoundation/sn/stabi"
	"github.com/urnetwork/connect/internal/durablehead"
	"github.com/urnetwork/connect/internal/durablepath"
)

const (
	// §3 planning figures: a claim that uses 250k gas at a 5 gwei base fee and
	// 0.006587 TAO/α costs about 0.19 α, so a 1.9 α threshold keeps gas at most
	// 10% of a claim. The cost is gas used; the much higher limit a claim is
	// signed with, onchain.ClaimBatchGasPerClaim or its estimate, never sets
	// the threshold. R-6's measured gas replaces the 250k figure.
	ClaimV2DefaultThresholdRao uint64 = 1_900_000_000
	// The runtime refuses a transfer below 100,000 rao of TAO, about 0.0152 α
	// at the planning price (§3). A lower threshold only sends doomed claims.
	ClaimV2TransferFloorRao uint64 = 15_200_000
)

type ClaimDaemonV2Config struct {
	SchemaVersion int      `yaml:"schema_version" json:"schema_version"`
	APIURL        string   `yaml:"api_url" json:"api_url"`
	RPC           []string `yaml:"rpc" json:"rpc"`
	ChainID       uint64   `yaml:"chain_id" json:"chain_id"`
	Vault         string   `yaml:"vault" json:"vault"`
	// Batcher, optional, is the claim batcher (STClaimBatcher, §10.2) that
	// claims several due accounts of one pass in one transaction.
	Batcher     string   `yaml:"batcher,omitempty" json:"batcher,omitempty"`
	Coldkeys    []string `yaml:"coldkeys" json:"coldkeys"`
	KeyFile     string   `yaml:"key_file" json:"key_file"`
	StateDir    string   `yaml:"state_dir" json:"state_dir"`
	PollSeconds int      `yaml:"poll_seconds" json:"poll_seconds"`
	coldkeys    [][32]byte
}

// One strict YAML document. The vault is pinned here, never taken from the
// operator's endpoint; relative paths are anchored at the config.
func LoadClaimDaemonV2Config(path string) (*ClaimDaemonV2Config, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(abs)
	if err != nil {
		return nil, err
	}
	var cfg ClaimDaemonV2Config
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return nil, err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, errors.New("v2 claim daemon config must be one YAML document")
	}
	if cfg.PollSeconds == 0 {
		cfg.PollSeconds = 30
	}
	if cfg.SchemaVersion != 2 || cfg.APIURL == "" || len(cfg.RPC) == 0 || cfg.ChainID == 0 || cfg.PollSeconds < 5 || cfg.PollSeconds > 3600 ||
		!strings.HasPrefix(cfg.Vault, "0x") || !common.IsHexAddress(cfg.Vault) || cfg.Vault != strings.ToLower(cfg.Vault) || len(cfg.Coldkeys) == 0 || cfg.KeyFile == "" || cfg.StateDir == "" ||
		cfg.Batcher != "" && (!strings.HasPrefix(cfg.Batcher, "0x") || !common.IsHexAddress(cfg.Batcher) || cfg.Batcher != strings.ToLower(cfg.Batcher) || cfg.Batcher == cfg.Vault) {
		return nil, errors.New("invalid v2 claim daemon configuration")
	}
	for _, value := range []*string{&cfg.KeyFile, &cfg.StateDir} {
		if !filepath.IsAbs(*value) {
			*value = filepath.Join(filepath.Dir(abs), *value)
		}
		*value = filepath.Clean(*value)
	}
	for index, text := range cfg.Coldkeys {
		coldkey, err := ss58.DecodeWithPrefix(text, ss58.BittensorPrefix)
		if err != nil {
			return nil, fmt.Errorf("claim coldkey %d: %w", index, err)
		}
		cfg.coldkeys = append(cfg.coldkeys, coldkey)
	}
	if _, err := os.Stat(cfg.KeyFile); err != nil {
		return nil, fmt.Errorf("claim relayer key: %w", err)
	}
	return &cfg, nil
}

// The shared v1 relayer helpers read only these fields.
func (self *ClaimDaemonV2Config) relayer() *ClaimDaemonConfig {
	return &ClaimDaemonConfig{RPC: self.RPC, KeyFile: self.KeyFile}
}

// The shared admission keeps one floor for v1 and v2 signatures of one key.
func (self *claimAdmission) rememberClaimV2(cfg *ClaimDaemonV2Config, unit *ClaimUnitV2) error {
	tx, _, _, err := authenticateSignedClaimV2(cfg, unit)
	if err != nil {
		return self.failNonce(err)
	}
	return self.rememberNonce(tx)
}

func (self *claimAdmission) seedClaimV2(cfg *ClaimDaemonV2Config, queue *ClaimQueueV2) error {
	for key, unit := range queue.Units {
		if !unit.signed() {
			continue
		}
		if err := self.rememberClaimV2(cfg, unit); err != nil {
			return fmt.Errorf("v2 claim unit %s: %w", key, err)
		}
	}
	return nil
}

// Only the daemon loop owns the queue. The accounts are the latest snapshot.
type claimDaemonV2 struct {
	cfg       *ClaimDaemonV2Config
	store     *claimQueueV2Store
	queue     *ClaimQueueV2
	admission *claimAdmission
	threshold *uint256.Int
	accounts  map[string]*claimAccountV2
	now       func() time.Time
}

func (self *claimDaemonV2) stamp() string { return self.now().UTC().Format(time.RFC3339Nano) }

func (self *claimDaemonV2) observe(snapshot map[string]*claimAccountV2, demand bool) error {
	self.accounts = snapshot
	if !self.queue.merge(snapshot, self.threshold, demand, self.stamp()) {
		return nil
	}
	return self.store.save(self.queue)
}

// One admitted operation per wakeup, as v1 yields after each. v2 units have
// no epoch, so each joins the shared admission's current class. With a
// claim batcher, the operation claims every due unsigned unit of the pass
// that one transaction holds, when there are two or more.
func (self *claimDaemonV2) poll(ctx context.Context) error {
	keys := self.queue.due(self.now(), self.accounts)
	candidates := []claimPollCandidate{}
	if len(keys) != 0 {
		candidates = append(candidates, claimPollCandidate{entry: &ClaimQueueEntry{Epoch: math.MaxInt64}})
	}
	_, operationCtx, done, err := beginClaimOperation(ctx, self.admission, self.cfg.StateDir, candidates)
	if err != nil || operationCtx == nil {
		if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
			return nil
		}
		return err
	}
	defer done()
	if self.cfg.Batcher != "" && !self.queue.Units[keys[0]].signed() {
		var unsigned []string
		for _, key := range keys {
			if !self.queue.Units[key].signed() {
				unsigned = append(unsigned, key)
			}
		}
		if len(unsigned) > 1 {
			return self.processBatch(operationCtx, unsigned)
		}
	}
	return self.process(operationCtx, self.queue.Units[keys[0]], self.accounts[keys[0]])
}

// A signed unit only reconciles its exact bytes. An unsigned one verifies the
// finalized account, then claims if it is due. Only nonce custody and storage
// failures stop the daemon; every other failure retries the unit.
func (self *claimDaemonV2) process(ctx context.Context, unit *ClaimUnitV2, account *claimAccountV2) error {
	if !unit.signed() {
		claimed, err := readClaimedV2(ctx, self.cfg, account)
		if err != nil {
			return self.retry(unit, time.Minute, "claim is not ready for finalized verification: "+err.Error())
		}
		unit.RootIndex, unit.Cumulative, unit.Claimed = account.RootIndex, account.Cumulative.Dec(), claimed.Dec()
		if !claimV2Due(&account.Cumulative, claimed, self.threshold, unit.Demand) {
			return self.idle(unit)
		}
		unit.Status, unit.Attempts, unit.NextRetryAt, unit.LastError, unit.UpdatedAt = "submitting", unit.Attempts+1, "", "", self.stamp()
		if err := self.store.save(self.queue); err != nil {
			return err
		}
		if err := self.submit(ctx, []*ClaimUnitV2{unit}, common.HexToAddress(self.cfg.Vault), claimV2Calldata(account), 0); err != nil {
			// Signed bytes are settled by their receipt, never by a watermark.
			var saveErr error
			if unit.signed() {
				saveErr = self.retry(unit, time.Minute, "exact transaction was prepared but finality was not confirmed: "+err.Error())
			} else {
				saveErr = self.refused(ctx, unit, account, err.Error())
			}
			var custody *claimNonceSafetyError
			if saveErr != nil || !errors.As(err, &custody) {
				return saveErr
			}
			return custody
		}
	}
	if tx, intent, from, err := authenticateSignedClaimV2(self.cfg, unit); err == nil && intent.Items != 0 {
		return self.settleBatch(ctx, unit, tx, from, intent.Items)
	}
	intent, event, reverted, err := self.settle(ctx, unit)
	if err != nil {
		return self.retry(unit, time.Minute, "uncertain transaction reconciliation: "+err.Error())
	}
	if reverted != nil {
		message := fmt.Sprintf("claim %s reverted in finalized block %s", unit.TxHash, reverted.BlockNumber)
		unit.TxHash, unit.RawTxHex = "", ""
		return self.refused(ctx, unit, intent.account(self.cfg.Vault), message)
	}
	unit.RootIndex, unit.Cumulative, unit.Claimed = event.RootIndex.Uint64(), event.Cumulative.String(), event.Cumulative.String()
	return self.idle(unit)
}

// A refused claim, at preflight or after inclusion, is done for its root when
// the re-read finalized watermark covers its cumulative: a third party claimed
// first (A6, NothingToClaim). Any other refusal (transfer floor, staking-hotkey
// cap) consumed at most a nonce, and the unit stays open under the backoff.
func (self *claimDaemonV2) refused(ctx context.Context, unit *ClaimUnitV2, account *claimAccountV2, message string) error {
	if claimed, err := readClaimedV2(ctx, self.cfg, account); err == nil && !claimed.Lt(&account.Cumulative) {
		unit.RootIndex, unit.Cumulative, unit.Claimed = account.RootIndex, account.Cumulative.Dec(), claimed.Dec()
		return self.idle(unit)
	}
	return self.retry(unit, claimRetry(unit.Attempts), message)
}

func (self *claimDaemonV2) retry(unit *ClaimUnitV2, delay time.Duration, message string) error {
	unit.Status = "open"
	if unit.signed() {
		unit.Status = "uncertain"
	}
	unit.LastError, unit.NextRetryAt, unit.UpdatedAt = message, self.now().Add(delay).UTC().Format(time.RFC3339Nano), self.stamp()
	return self.store.save(self.queue)
}

// A paid claim, or nothing due, spends any demand and resets the backoff.
func (self *claimDaemonV2) idle(unit *ClaimUnitV2) error {
	unit.Status, unit.Demand, unit.Attempts, unit.NextRetryAt, unit.LastError, unit.UpdatedAt = "idle", false, 0, "", "", self.stamp()
	unit.TxHash, unit.RawTxHex = "", ""
	return self.store.save(self.queue)
}

// Signs one transaction, a claim to vault2 or a batch to the claim batcher,
// for its units through the shared submitter; a zero gas limit takes the
// estimate, and never less than onchain.ClaimBatchGasPerClaim, which covers
// the claim whatever vault2's staking-hotkey list has grown to by its
// inclusion or a later rebroadcast. Its exact bytes are durable in every unit
// and raise the shared nonce floor before any broadcast.
func (self *claimDaemonV2) submit(ctx context.Context, units []*ClaimUnitV2, to common.Address, calldata []byte, gasLimit uint64) error {
	key, err := onchain.LoadKeyFile(self.cfg.KeyFile)
	if err != nil {
		return err
	}
	chainID := new(big.Int).SetUint64(self.cfg.ChainID)
	if err := self.admission.checkChain(chainID); err != nil {
		return err
	}
	owner := self.store.owner
	_, err = onchain.SubmitWithHooks(ctx, onchain.SubmitParams{
		Contract: to, Rpcs: self.cfg.RPC, Key: key, Calldata: calldata,
		GasLimit: gasLimit, GasFloor: onchain.ClaimBatchGasPerClaim,
		NonceFloor: self.admission.nonceMinimum(), ChainID: chainID,
		RuntimeAdmission: func(request context.Context, _ *ethclient.Client, number *big.Int) error {
			if number != nil {
				return errors.Join(request.Err(), owner.requireOwner())
			}
			return owner.requireWrite(request)
		},
	}, onchain.SubmitHooks{
		Prepared: func(hash common.Hash, raw []byte) error {
			for _, unit := range units {
				unit.TxHash, unit.RawTxHex, unit.UpdatedAt = strings.ToLower(hash.Hex()), hexutil.Encode(raw), self.stamp()
			}
			if err := self.store.save(self.queue); err != nil {
				// An uncertain checkpoint may already hold these bytes.
				if errors.Is(err, durablehead.ErrUncertain) {
					err = errors.Join(err, self.admission.rememberClaimV2(self.cfg, units[0]))
				}
				for _, unit := range units {
					unit.TxHash, unit.RawTxHex = "", ""
				}
				return err
			}
			return self.admission.rememberClaimV2(self.cfg, units[0])
		},
		BeforeBroadcast: func(common.Hash) error {
			tx, _, _, err := authenticateSignedClaimV2(self.cfg, units[0])
			if err != nil {
				return err
			}
			return self.store.requireSigned(ctx, tx)
		},
	})
	return err
}

// A batch claims two or more due accounts in one transaction through the
// claim batcher (§10.2), at most onchain.ClaimBatchMaxClaims, since every
// unit keeps the batch's signed bytes. Each unit is verified alone against
// its finalized root, as for a single claim, then preflighted alone; a
// refused preflight is a refused claim (A6). The units the batch holds are
// signed together, the rest of the due units wait for the next pass, and a
// single passing unit is claimed alone.
func (self *claimDaemonV2) processBatch(ctx context.Context, keys []string) error {
	var units []*ClaimUnitV2
	var accounts []*claimAccountV2
	for _, key := range keys {
		if len(units) == onchain.ClaimBatchMaxClaims {
			break
		}
		unit, account := self.queue.Units[key], self.accounts[key]
		claimed, err := readClaimedV2(ctx, self.cfg, account)
		if err != nil {
			if err := self.retry(unit, time.Minute, "claim is not ready for finalized verification: "+err.Error()); err != nil {
				return err
			}
			continue
		}
		unit.RootIndex, unit.Cumulative, unit.Claimed = account.RootIndex, account.Cumulative.Dec(), claimed.Dec()
		if !claimV2Due(&account.Cumulative, claimed, self.threshold, unit.Demand) {
			if err := self.idle(unit); err != nil {
				return err
			}
			continue
		}
		units, accounts = append(units, unit), append(accounts, account)
	}
	if len(units) < 2 {
		if len(units) == 1 {
			return self.process(ctx, units[0], accounts[0])
		}
		return nil
	}
	batch, err := self.prepareBatch(ctx, units, accounts)
	if batch == nil {
		return err
	}
	signed := make([]*ClaimUnitV2, len(batch.Items))
	for i, index := range batch.Items {
		unit := units[index]
		unit.Status, unit.NextRetryAt, unit.LastError, unit.UpdatedAt = "submitting", "", "", self.stamp()
		signed[i] = unit
	}
	if err := self.store.save(self.queue); err != nil {
		return err
	}
	if err := self.submit(ctx, signed, common.HexToAddress(self.cfg.Batcher), batch.Calldata, batch.GasLimit); err != nil {
		var saveErr error
		for i, unit := range signed {
			if unit.signed() {
				saveErr = errors.Join(saveErr, self.retry(unit, time.Minute, "exact transaction was prepared but finality was not confirmed: "+err.Error()))
			} else {
				saveErr = errors.Join(saveErr, self.refused(ctx, unit, accounts[batch.Items[i]], err.Error()))
			}
		}
		var custody *claimNonceSafetyError
		if saveErr != nil || !errors.As(err, &custody) {
			return saveErr
		}
		return custody
	}
	return self.process(ctx, signed[0], accounts[batch.Items[0]])
}

// Preflights each unit's claim alone and sizes the batch from the first RPC
// endpoint that serves the configured chain. Each unit counts an attempt, as
// a lone claim does before its preflight, so failures back off alike. A
// refused preflight settles its unit as a refused claim; without a batch, a
// single passing unit is claimed alone. Every unit retries when nothing
// could be preflighted.
func (self *claimDaemonV2) prepareBatch(ctx context.Context, units []*ClaimUnitV2, accounts []*claimAccountV2) (*onchain.ClaimBatch, error) {
	for _, unit := range units {
		unit.Attempts++
	}
	retryAll := func(message string) (*onchain.ClaimBatch, error) {
		var err error
		for _, unit := range units {
			err = errors.Join(err, self.retry(unit, claimRetry(unit.Attempts), message))
		}
		return nil, err
	}
	key, err := onchain.LoadKeyFile(self.cfg.KeyFile)
	if err != nil {
		return retryAll("claim batch is not ready: " + err.Error())
	}
	client, err := dialClaimV2(ctx, self.cfg)
	if err != nil {
		return retryAll("claim batch is not ready: " + err.Error())
	}
	defer client.Close()
	items := make([]stabi.STClaimBatcherClaim, len(accounts))
	for i, account := range accounts {
		items[i] = claimV2Item(account)
	}
	refusals, batch, err := onchain.PrepareClaimBatch(ctx, client, crypto.PubkeyToAddress(key.PublicKey), common.HexToAddress(self.cfg.Batcher), common.HexToAddress(self.cfg.Vault), items)
	var passed []int
	for i, refusal := range refusals {
		if refusal == nil {
			passed = append(passed, i)
			continue
		}
		if err := self.refused(ctx, units[i], accounts[i], "claim preflight: "+refusal.Error()); err != nil {
			return nil, err
		}
	}
	if err != nil {
		for _, i := range passed {
			if saveErr := self.retry(units[i], claimRetry(units[i].Attempts), "claim batch is not ready: "+err.Error()); saveErr != nil {
				return nil, saveErr
			}
		}
		return nil, nil
	}
	if batch == nil && len(passed) == 1 {
		// Claimed alone, the unit counts its attempt there.
		units[passed[0]].Attempts--
		return nil, self.process(ctx, units[passed[0]], accounts[passed[0]])
	}
	return batch, nil
}

// A configured batcher must be the release claim batcher: any other address
// would refuse or misreport every batch. The daemon checks it once at start.
func verifyClaimV2Batcher(ctx context.Context, cfg *ClaimDaemonV2Config) error {
	client, err := dialClaimV2(ctx, cfg)
	if err != nil {
		return fmt.Errorf("claim batcher %s: %w", cfg.Batcher, err)
	}
	defer client.Close()
	return onchain.VerifyClaimBatcher(ctx, client, common.HexToAddress(cfg.Batcher))
}

// The units of one batch settle together from its finalized receipt: a paid
// unit by its account's Claimed event from vault2, with the batcher as caller
// and the batcher's amount; a refused unit as a refused claim (A6); and a
// unit the batch did not attempt reopens at once. A reverted batch refuses
// each unit. Without a finalized receipt the exact bytes are rebroadcast
// under the rules of a single claim, and every unit stays in flight.
func (self *claimDaemonV2) settleBatch(ctx context.Context, unit *ClaimUnitV2, tx *types.Transaction, from common.Address, items int) error {
	var keys []string
	for key, other := range self.queue.Units {
		if other.TxHash == unit.TxHash && other.RawTxHex == unit.RawTxHex {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	retryAll := func(message string) error {
		var err error
		for _, key := range keys {
			err = errors.Join(err, self.retry(self.queue.Units[key], time.Minute, "uncertain transaction reconciliation: "+message))
		}
		return err
	}
	receipt, receiptErr := finalizedClaimReceipt(ctx, self.cfg.relayer(), unit.TxHash, tx.ChainId())
	if receiptErr != nil {
		consumed, err := rebroadcastSignedClaim(ctx, self.cfg.relayer(), tx, from, self.store)
		switch {
		case err != nil:
			return retryAll((&claimSignedOutcomeError{reason: "exact rebroadcast did not reconcile outcome", cause: err}).Error())
		case consumed:
			return retryAll((&claimSignedOutcomeError{reason: "signed nonce is consumed without an authenticated receipt"}).Error())
		}
		return retryAll((&claimSignedOutcomeError{reason: "exact transaction is not yet canonically finalized", cause: receiptErr}).Error())
	}
	var outcomes []onchain.ClaimBatchOutcome
	if receipt.Status == types.ReceiptStatusSuccessful {
		var err error
		if outcomes, err = onchain.ClaimBatchOutcomes(receipt, *tx.To(), items); err != nil {
			return retryAll((&claimSignedOutcomeError{reason: "canonical receipt does not prove the batch's outcome", cause: err}).Error())
		}
	}
	vault := common.HexToAddress(self.cfg.Vault)
	for _, key := range keys {
		unit := self.queue.Units[key]
		_, intent, _, err := authenticateSignedClaimV2(self.cfg, unit)
		if err != nil {
			if err := self.retry(unit, time.Minute, "uncertain transaction reconciliation: "+err.Error()); err != nil {
				return err
			}
			continue
		}
		hash := unit.TxHash
		switch {
		case outcomes == nil:
			unit.TxHash, unit.RawTxHex = "", ""
			err = self.refused(ctx, unit, intent.account(self.cfg.Vault), fmt.Sprintf("claim batch %s reverted in finalized block %s", hash, receipt.BlockNumber))
		case outcomes[intent.Item].Amount != nil:
			event, verifyErr := verifyBatchedClaimV2Receipt(vault, tx, intent, outcomes[intent.Item].Amount, receipt)
			if verifyErr != nil {
				err = self.retry(unit, time.Minute, "uncertain transaction reconciliation: "+(&claimSignedOutcomeError{reason: "canonical receipt does not prove this claim succeeded", cause: verifyErr}).Error())
				break
			}
			unit.RootIndex, unit.Cumulative, unit.Claimed = event.RootIndex.Uint64(), event.Cumulative.String(), event.Cumulative.String()
			err = self.idle(unit)
		case outcomes[intent.Item].Attempted:
			unit.TxHash, unit.RawTxHex = "", ""
			err = self.refused(ctx, unit, intent.account(self.cfg.Vault), fmt.Sprintf("claim batch %s refused the claim with 0x%x", hash, outcomes[intent.Item].Selector))
		default:
			unit.TxHash, unit.RawTxHex = "", ""
			unit.Status, unit.NextRetryAt, unit.LastError, unit.UpdatedAt = "open", "", "claim batch "+hash+" did not attempt the claim", self.stamp()
			err = self.store.save(self.queue)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// The finalized outcome of the exact signed bytes: the matching Claimed event,
// or the failed receipt of a reverted claim, with the signed intent. Without a
// finalized receipt the same bytes are rebroadcast under the v1 rules, except
// that a claim already covered is sent without the preflight, and stay in
// flight.
func (self *claimDaemonV2) settle(ctx context.Context, unit *ClaimUnitV2) (*claimIntentV2, *stabi.STSettlementVault2Claimed, *types.Receipt, error) {
	tx, intent, from, err := authenticateSignedClaimV2(self.cfg, unit)
	if err != nil {
		return nil, nil, nil, &claimSignedOutcomeError{reason: "signed identity cannot be authenticated", cause: err}
	}
	receipt, receiptErr := finalizedClaimReceipt(ctx, self.cfg.relayer(), unit.TxHash, tx.ChainId())
	if receiptErr == nil {
		if receipt.Status != types.ReceiptStatusSuccessful {
			return intent, nil, receipt, nil
		}
		event, err := verifySignedClaimV2Receipt(tx, intent, from, receipt)
		if err != nil {
			return nil, nil, nil, &claimSignedOutcomeError{reason: "canonical receipt does not prove this claim succeeded", cause: err}
		}
		return intent, event, nil, nil
	}
	consumed, err := rebroadcastSignedClaim(ctx, self.cfg.relayer(), tx, from, self.store)
	if err != nil {
		// Anyone may claim for anyone in v2. Bytes whose claim the finalized
		// watermark already covers can only revert NothingToClaim inside the
		// vault, before any precompile call, so the same bytes are sent without
		// the preflight: their nonce is consumed and later claims are not stuck
		// behind it. Their reverted receipt then settles the unit.
		account := intent.account(self.cfg.Vault)
		if claimed, readErr := readClaimedV2(ctx, self.cfg, account); readErr == nil && !claimed.Lt(&account.Cumulative) {
			consumed, err = replaySignedClaim(ctx, self.cfg.relayer(), tx, from, self.store, false)
		}
	}
	if err != nil {
		return nil, nil, nil, &claimSignedOutcomeError{reason: "exact rebroadcast did not reconcile outcome", cause: err}
	}
	if consumed {
		return nil, nil, nil, &claimSignedOutcomeError{reason: "signed nonce is consumed without an authenticated receipt"}
	}
	return nil, nil, nil, &claimSignedOutcomeError{reason: "exact transaction is not yet canonically finalized", cause: receiptErr}
}

// The demand applies to the first snapshot only: --claim-now claims every
// account with anything claimable once, then the threshold rules again.
func (self *claimDaemonV2) run(ctx context.Context, snapshots <-chan map[string]*claimAccountV2, polls <-chan time.Time, demand bool) error {
	for {
		// A completed read outranks an immediate ready ticket, as in v1, so no
		// claim is built on a snapshot that a newer one already replaced.
		select {
		case snapshot := <-snapshots:
			if err := self.observe(snapshot, demand); err != nil {
				return err
			}
			demand = false
		default:
		}
		if err := self.poll(ctx); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case snapshot := <-snapshots:
			if err := self.observe(snapshot, demand); err != nil {
				return err
			}
			demand = false
		case <-polls:
		case <-self.admission.ready(self.cfg.StateDir):
		}
	}
}

// The caller retains the store until this returns; the reader is joined first.
func runClaimDaemonV2WithStore(ctx context.Context, cfg *ClaimDaemonV2Config, store *claimQueueV2Store, admission *claimAdmission, threshold uint64, demand bool) error {
	defer admission.forget(cfg.StateDir)
	if cfg.Batcher != "" {
		if err := verifyClaimV2Batcher(ctx, cfg); err != nil && ctx.Err() == nil {
			return err
		} else if err != nil {
			return nil
		}
	}
	queue, err := store.load()
	if err != nil {
		return err
	}
	if err := admission.seedClaimV2(cfg, queue); err != nil {
		return err
	}
	if err := store.save(queue); err != nil {
		return err
	}
	strategy := connect.NewClientStrategy(ctx, defaultClaimReadStrategySettings())
	defer strategy.Close()
	period := time.Duration(cfg.PollSeconds) * time.Second
	readCtx, readCancel := context.WithCancel(ctx)
	snapshots, readDone := startClaimEpochReader(readCtx, period, func(ctx context.Context) (map[string]*claimAccountV2, error) {
		return readClaimSnapshotV2(ctx, strategy, cfg)
	})
	defer func() { readCancel(); <-readDone }()
	ticker := time.NewTicker(period)
	defer ticker.Stop()
	daemon := &claimDaemonV2{cfg: cfg, store: store, queue: queue, admission: admission, threshold: uint256.NewInt(threshold), accounts: map[string]*claimAccountV2{}, now: time.Now}
	// As in v1, a failure after shutdown began is the shutdown; the next owner
	// reconciles any publication it left uncertain.
	if err := daemon.run(ctx, snapshots, ticker.C, demand); ctx.Err() == nil {
		return err
	}
	return nil
}

// RunClaimDaemonV2 runs the v2 claim daemon and, given a v1 config, the
// unchanged v1 daemon beside it. One process shares one relayer's nonce
// admission, so both must sign with the same key; separate processes need
// separate relayer keys, as for v1.
func RunClaimDaemonV2(ctx context.Context, configPath, v1ConfigPath string, threshold uint64, demand bool) (runErr error) {
	if threshold < ClaimV2TransferFloorRao {
		return fmt.Errorf("claim threshold %d rao is below the %d rao transfer floor", threshold, ClaimV2TransferFloorRao)
	}
	if err := durablepath.Require(ctx); err != nil {
		return err
	}
	cfg, err := LoadClaimDaemonV2Config(configPath)
	if err != nil {
		return err
	}
	var v1 *ClaimDaemonConfig
	if v1ConfigPath != "" {
		if v1, err = LoadClaimDaemonConfig(v1ConfigPath); err != nil {
			return err
		}
		if v1.KeyFile != cfg.KeyFile {
			return errors.New("v1 and v2 claim daemons in one process must share one relayer key")
		}
	}
	store, err := newClaimQueueV2Store(ctx, cfg.StateDir)
	if err != nil {
		return err
	}
	defer func() { runErr = errors.Join(runErr, store.close()) }()
	admission := &claimAdmission{}
	if v1 == nil {
		return runClaimDaemonV2WithStore(ctx, cfg, store, admission, threshold, demand)
	}
	// Seed both queues' signed nonces before either daemon can sign, as the
	// swarm does for its members.
	queue, err := store.load()
	if err != nil {
		return err
	}
	if err := admission.seedClaimV2(cfg, queue); err != nil {
		return err
	}
	v1Store, err := newClaimQueueStore(v1.StateDir, ctx)
	if err != nil {
		return err
	}
	if err := admission.seedMember(v1, v1Store); err != nil {
		return errors.Join(err, v1Store.close())
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	v1Done := make(chan error, 1)
	go func() {
		// A stopped v1 daemon stops its v2 sibling, so neither fails unseen.
		defer cancel()
		v1Done <- runClaimOwner(runCtx, v1, v1Store, claimOwnerHooks{run: func(owner *claimQueueStore) error {
			return runClaimDaemonWithStore(runCtx, v1, owner, admission, 0, nil)
		}})
	}()
	v2Err := runClaimDaemonV2WithStore(runCtx, cfg, store, admission, threshold, demand)
	cancel()
	return errors.Join(v2Err, <-v1Done)
}

// "provider claim-daemon-v2". The threshold is in rao.
func runClaimDaemonV2Command(ctx context.Context, opts docopt.Opts) error {
	threshold := ClaimV2DefaultThresholdRao
	if text := fleetOpt(opts, "--threshold-rao"); text != "" {
		value, err := strconv.ParseUint(text, 10, 64)
		if err != nil {
			return fmt.Errorf("--threshold-rao: %w", err)
		}
		threshold = value
	}
	demand, _ := opts.Bool("--claim-now")
	event := connect.NewEventWithContext(ctx)
	defer event.SetOnSignals(syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM)()
	return RunClaimDaemonV2(event.Ctx(), fleetOpt(opts, "--config"), fleetOpt(opts, "--v1-config"), threshold, demand)
}
