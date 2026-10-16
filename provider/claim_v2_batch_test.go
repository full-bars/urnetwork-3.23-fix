//go:build linux || darwin

// E-5, CL-5: the v2 claim daemon with a claim batcher (§10.2) over the
// synthetic vault2 pool, whose fixture gains a model of STClaimBatcher.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"

	"github.com/urfoundation/sn/merkle"
	"github.com/urfoundation/sn/miner/onchain"
	"github.com/urfoundation/sn/stabi"
	"github.com/urnetwork/connect/internal/durablehead"
)

const (
	claimV2TestBatcher = "0x00000000000000000000000000000000000b47c4"
	// The batcher model spends claimV2TestBatchBase outside its items,
	// claimV2TestItemGas on a paid item and claimV2TestRevertGas on one the
	// vault refuses; one the runtime refuses consumes its whole cap.
	claimV2TestBatchBase  = 60_000
	claimV2TestItemGas    = 240_000
	claimV2TestRevertGas  = 30_000
	claimV2TestBatchCap   = onchain.ClaimBatchGasPerClaim
	claimV2TestBatchNeeds = claimV2TestBatchCap + claimV2TestBatchCap/63 + 20_000
)

var (
	claimV2TestThird      = [32]byte{0xc0, 0x1d, 0x04}
	claimV2TestBatcherABI = func() *abi.ABI {
		parsed, err := stabi.STClaimBatcherMetaData.ParseABI()
		if err != nil {
			panic(err)
		}
		return parsed
	}()
)

// claimV2TestBatcherModel is the claim batcher at address over the fixture's
// vault. itemGas, when set, replaces the gas a paid item uses; refuseCalls
// refuses every eth_call of it; dropVault drops vault2's events from a mined
// batch, and caller, when set, replaces the batcher as their caller.
type claimV2TestBatcherModel struct {
	address     common.Address
	itemGas     uint64
	refuseCalls bool
	dropVault   bool
	caller      *common.Address
	// code, when set, is the batcher address's code in place of the
	// release runtime.
	code []byte
}

// answerCode answers eth_getCode: the release claim batcher's runtime at the
// model's address, or its code knob, and no code elsewhere.
func (self *claimV2TestChain) answerCode(params []json.RawMessage) (any, *claimV2TestRPCError) {
	var address common.Address
	if len(params) != 2 || json.Unmarshal(params[0], &address) != nil {
		self.t.Errorf("eth_getCode request: %s", params)
	}
	self.calls = append(self.calls, "code")
	if self.batch == nil || address != self.batch.address {
		return "0x", nil
	}
	if self.batch.code != nil {
		return hexutil.Encode(self.batch.code), nil
	}
	return onchain.ClaimBatcherRuntime, nil
}

// answerBatch answers eth_call and eth_estimateGas of claimMany: the items
// the model attempts with the call's gas, or the gas of a batch attempting
// none, as the batcher succeeds at any gas.
func (self *claimV2TestChain) answerBatch(method string, gas *hexutil.Uint64, data []byte) (any, *claimV2TestRPCError) {
	self.calls = append(self.calls, "batch "+method)
	if method == "eth_estimateGas" {
		return hexutil.EncodeUint64(claimV2TestBatchBase), nil
	}
	if self.batch.refuseCalls {
		return nil, &claimV2TestRPCError{Code: -32000, Message: "synthetic batcher refusal"}
	}
	limit := uint64(75_000_000)
	if gas != nil {
		limit = uint64(*gas)
	}
	attempted, _ := self.runBatch(data, limit, nil)
	out, err := claimV2TestBatcherABI.Methods["claimMany"].Outputs.Pack(big.NewInt(int64(attempted)))
	if err != nil {
		self.t.Error(err)
	}
	return hexutil.Encode(out), nil
}

// mineBatch runs a sent batch into its receipt, which a reverted batch
// leaves failed and without logs.
func (self *claimV2TestChain) mineBatch(tx *types.Transaction, receipt *types.Receipt) {
	if self.revertMined {
		return
	}
	_, logs := self.runBatch(tx.Data(), tx.Gas(), receipt)
	receipt.Status = types.ReceiptStatusSuccessful
	if !self.dropLogs {
		receipt.Logs = logs
	}
}

// runBatch runs claimMany on the vault's checks: an item is attempted while
// the gas left covers its cap as the batcher requires. A receipt commits the
// paid claims and receives the events.
func (self *claimV2TestChain) runBatch(data []byte, gas uint64, receipt *types.Receipt) (int, []*types.Log) {
	method := claimV2TestBatcherABI.Methods["claimMany"]
	if len(data) < 4 || !slices.Equal(data[:4], method.ID) {
		self.t.Errorf("batch 0x%x is not claimMany", data)
		return 0, nil
	}
	values, err := method.Inputs.Unpack(data[4:])
	if err != nil || values[0].(common.Address) != self.vault {
		self.t.Errorf("batch 0x%x is not claimMany of the vault: %v", data, err)
		return 0, nil
	}
	items := *abi.ConvertType(values[1], new([]stabi.STClaimBatcherClaim)).(*[]stabi.STClaimBatcherClaim)
	perClaim := values[2].(*big.Int).Uint64()
	itemGas := uint64(claimV2TestItemGas)
	if self.batch.itemGas != 0 {
		itemGas = self.batch.itemGas
	}
	left := gas - min(gas, claimV2TestBatchBase)
	var logs []*types.Log
	event := func(name string, args ...any) {
		packed, err := claimV2TestBatcherABI.Events[name].Inputs.Pack(args...)
		if err != nil {
			self.t.Error(err)
		}
		logs = append(logs, &types.Log{Address: self.batch.address, Topics: []common.Hash{claimV2TestBatcherABI.Events[name].ID}, Data: packed})
	}
	for attempted, item := range items {
		if left < perClaim+perClaim/63+20_000 {
			event("ClaimsNotAttempted", big.NewInt(int64(attempted)))
			return attempted, logs
		}
		intent, amount, refusal := self.execute(stSettlementVault2.PackClaim(item.NoId, item.RootIndex, item.Account, item.Proof))
		if refusal != nil {
			var selector [4]byte
			if custom, ok := claimV2TestABI.Errors[strings.TrimPrefix(refusal.Message, "execution reverted: ")]; ok {
				copy(selector[:], custom.ID[:4])
				left -= min(left, claimV2TestRevertGas)
			} else {
				left -= perClaim
			}
			event("ClaimRefused", big.NewInt(int64(attempted)), selector)
			continue
		}
		left -= min(left, itemGas)
		if receipt != nil {
			self.claimed[intent.Account.Index.Uint64()] = *uint256.MustFromBig(intent.Account.Cumulative)
			caller := self.batch.address
			if self.batch.caller != nil {
				caller = *self.batch.caller
			}
			if !self.batch.dropVault {
				logs = append(logs, claimV2TestClaimedLog(self.vault, intent, amount, caller, receipt))
			}
		}
		event("ClaimPaid", big.NewInt(int64(attempted)), amount.ToBig())
	}
	return len(items), logs
}

// A pool whose accounts 1, 2 and 3 belong to three configured coldkeys, each
// with 5, 6 and 7 α to claim, and a daemon claiming through the batcher.
func newClaimV2BatchTest(t *testing.T) (*claimV2TestChain, *claimV2TestDaemon) {
	t.Helper()
	chain := newClaimV2TestChain(t)
	chain.batch = &claimV2TestBatcherModel{address: common.HexToAddress(claimV2TestBatcher)}
	chain.finalize(claimV2TestAccount(claimV2TestOther, 1), claimV2TestAccount(claimV2TestColdkey, 5), claimV2TestAccount(claimV2TestSecond, 6), claimV2TestAccount(claimV2TestThird, 7))
	daemon := newClaimV2TestDaemon(t, chain, ClaimV2DefaultThresholdRao, claimV2TestColdkey, claimV2TestSecond, claimV2TestThird)
	daemon.cfg.Batcher = claimV2TestBatcher
	return chain, daemon
}

// The batch the daemon signs for accounts, each capped at the default.
func (self *claimV2TestDaemon) batchCalldata(indexes ...uint64) []byte {
	self.t.Helper()
	items := make([]stabi.STClaimBatcherClaim, len(indexes))
	for i, index := range indexes {
		items[i] = claimV2Item(self.account(index))
	}
	return stClaimBatcher.PackClaimMany(common.HexToAddress(self.cfg.Vault), items, new(big.Int).SetUint64(claimV2TestBatchCap))
}

// claimV2TestSent decodes a signed transaction the fixture received.
func claimV2TestSent(t *testing.T, raw string) *types.Transaction {
	t.Helper()
	encoded, err := hexutil.Decode(raw)
	var tx types.Transaction
	if err != nil || tx.UnmarshalBinary(encoded) != nil {
		t.Fatalf("not a signed transaction: %s", raw)
	}
	return &tx
}

// The least gas limit at which the model attempts every one of n items.
func claimV2TestBatchLeast(n uint64) uint64 {
	return claimV2TestBatchBase + (n-1)*claimV2TestItemGas + claimV2TestBatchNeeds
}

// requireBatch requires raw to be the relayer's batch of data to the
// batcher, with a gas limit the least limit of n items plus 20% gives.
func (self *claimV2TestDaemon) requireBatch(raw string, n uint64, data []byte) {
	self.t.Helper()
	tx := claimV2TestSent(self.t, raw)
	least := claimV2TestBatchLeast(n)
	from, err := types.Sender(types.LatestSignerForChainID(tx.ChainId()), tx)
	if err != nil || from != crypto.PubkeyToAddress(self.key.PublicKey) || tx.To() == nil || *tx.To() != common.HexToAddress(claimV2TestBatcher) || tx.Value().Sign() != 0 ||
		!slices.Equal(tx.Data(), data) || tx.Gas() < least+least/5 || tx.Gas() > least+10_000+(least+10_000)/5 {
		self.t.Fatalf("batch: from %s (%v) to %v gas %d data 0x%x, want gas from %d and data 0x%x", from, err, tx.To(), tx.Gas(), tx.Data(), least+least/5, data)
	}
}

// requireIdle requires unit index paid up to alpha and idle.
func (self *claimV2TestDaemon) requireIdle(index uint64, alpha float64) {
	self.t.Helper()
	unit, claimed := self.durable(index), self.chain.claimedOf(index)
	if unit.Status != "idle" || unit.Claimed != claimV2TestDec(alpha) || claimed.Dec() != claimV2TestDec(alpha) || unit.TxHash != "" || unit.RawTxHex != "" || unit.Attempts != 0 || unit.LastError != "" {
		self.t.Fatalf("unit %d: %+v, claimed %s", index, unit, claimed.Dec())
	}
}

// requireOpen requires unit index open, unsigned, after attempts, with a
// last error starting prefix and retrying at the daemon's clock plus delay.
func (self *claimV2TestDaemon) requireOpen(index uint64, attempts int, delay time.Duration, prefix string) {
	self.t.Helper()
	unit := self.durable(index)
	if unit.Status != "open" || unit.TxHash != "" || unit.RawTxHex != "" || unit.Attempts != attempts || unit.NextRetryAt != self.clock.Add(delay).UTC().Format(time.RFC3339Nano) || !strings.HasPrefix(unit.LastError, prefix) {
		self.t.Fatalf("unit %d: %+v, want open after %d attempts retrying in %s with %q", index, unit, attempts, delay, prefix)
	}
}

// The fixture's calls named call.
func (self *claimV2TestChain) count(call string) int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	count := 0
	for _, made := range self.calls {
		if made == call {
			count++
		}
	}
	return count
}

// E-5, CL-5: the due accounts of one pass go in one batch to the batcher,
// each verified and preflighted alone, with the least gas limit attempting
// all plus 20%; each settles from its vault2 Claimed with the batcher as
// caller.
func TestClaimV2BatchesDueAccounts(t *testing.T) {
	chain, daemon := newClaimV2BatchTest(t)
	daemon.step(false)
	sends := chain.sent()
	if len(sends) != 1 {
		t.Fatalf("batch sent %d transactions", len(sends))
	}
	daemon.requireBatch(sends[0], 3, daemon.batchCalldata(1, 2, 3))
	for index, alpha := range map[uint64]float64{1: 5, 2: 6, 3: 7} {
		daemon.requireIdle(index, alpha)
	}
	// Each claim's estimate is its preflight; the batch is offered at the
	// most gas a transaction may name, sized in 13 more calls, and the send
	// preflights it once more.
	if chain.count("estimate") != 3 || chain.count("batch eth_call") != 15 || chain.count("batch eth_estimateGas") != 1 || chain.count("preflight") != 0 {
		t.Fatalf("calls: %v", chain.calls)
	}
}

// E-5, CL-5: with one due account the daemon claims it alone, to vault2.
func TestClaimV2BatcherClaimsOneDueAccountAlone(t *testing.T) {
	chain, daemon := newClaimV2BatchTest(t)
	chain.with(func() {
		chain.claimed[2], chain.claimed[3] = claimV2TestRao(6), claimV2TestRao(7)
	})
	daemon.step(false)
	sends := chain.sent()
	if len(sends) != 1 || *claimV2TestSent(t, sends[0]).To() != common.HexToAddress(daemon.cfg.Vault) || chain.count("batch eth_call") != 0 {
		t.Fatalf("one due account: %d sends, calls %v", len(sends), chain.calls)
	}
	daemon.requireIdle(1, 5)
}

// E-5, CL-5: a unit whose finalized verification fails retries, one whose
// claimed watermark already covers it goes idle, and the rest are claimed.
func TestClaimV2BatchVerifiesEachUnit(t *testing.T) {
	chain, daemon := newClaimV2BatchTest(t)
	chain.with(func() {
		chain.claimed[2] = claimV2TestRao(6)
		chain.account = func(entry map[string]any) {
			switch entry["index"] {
			case 2:
				entry["claimed"] = "0"
			case 3:
				entry["cumulative"] = claimV2TestDec(8)
			}
		}
	})
	daemon.step(false)
	sends := chain.sent()
	if len(sends) != 1 || *claimV2TestSent(t, sends[0]).To() != common.HexToAddress(daemon.cfg.Vault) {
		t.Fatalf("verification: %d sends", len(sends))
	}
	daemon.requireIdle(1, 5)
	if unit := daemon.durable(2); unit.Status != "idle" || unit.Claimed != claimV2TestDec(6) {
		t.Fatalf("covered unit: %+v", unit)
	}
	daemon.requireOpen(3, 0, time.Minute, "claim is not ready for finalized verification: "+chain.server.URL+": "+errClaimV2Proof.Error())
}

// E-5, CL-5, SPEC A6: in a batch, a claim a third party made first is done;
// one the runtime refused stays open under the backoff, and is claimed once
// the runtime accepts it; the other pays.
func TestClaimV2BatchSettlesEachOutcome(t *testing.T) {
	chain, daemon := newClaimV2BatchTest(t)
	chain.with(func() {
		chain.beforeMine = func() {
			chain.claimed[2] = claimV2TestRao(6)
			chain.refuseIndex[3] = true
		}
	})
	daemon.step(false)
	daemon.requireIdle(1, 5)
	if unit := daemon.durable(2); unit.Status != "idle" || unit.Claimed != claimV2TestDec(6) || unit.TxHash != "" {
		t.Fatalf("third party's claim: %+v", unit)
	}
	daemon.requireOpen(3, 1, time.Minute, "claim batch "+strings.ToLower(claimV2TestSent(t, chain.sent()[0]).Hash().Hex())+" refused the claim with 0x00000000")
	chain.with(func() { chain.beforeMine, chain.refuseIndex[3] = nil, false })
	daemon.step(false)
	if len(chain.sent()) != 1 {
		t.Fatal("the refused claim retried before its backoff")
	}
	daemon.advance(time.Minute)
	daemon.step(false)
	if sends := chain.sent(); len(sends) != 2 || *claimV2TestSent(t, sends[1]).To() != common.HexToAddress(daemon.cfg.Vault) {
		t.Fatalf("retry after the backoff: %d sends", len(chain.sent()))
	}
	daemon.requireIdle(3, 7)
}

// E-5, CL-5: claims a batch did not attempt, because they used more gas than
// its preflight, reopen at once and go in the next pass's batch.
func TestClaimV2BatchReopensItemsNotAttempted(t *testing.T) {
	chain, daemon := newClaimV2BatchTest(t)
	chain.with(func() { chain.beforeMine = func() { chain.batch.itemGas = 1_200_000 } })
	daemon.step(false)
	daemon.requireIdle(1, 5)
	hash := strings.ToLower(claimV2TestSent(t, chain.sent()[0]).Hash().Hex())
	for _, index := range []uint64{2, 3} {
		if unit := daemon.durable(index); unit.Status != "open" || unit.TxHash != "" || unit.NextRetryAt != "" || unit.LastError != "claim batch "+hash+" did not attempt the claim" {
			t.Fatalf("unit %d not attempted: %+v", index, unit)
		}
	}
	chain.with(func() { chain.beforeMine, chain.batch.itemGas = nil, 0 })
	daemon.step(false)
	sends := chain.sent()
	if len(sends) != 2 || claimV2TestNonce(t, sends[1]) != claimV2TestNonce(t, sends[0])+1 {
		t.Fatalf("next batch: %d sends", len(sends))
	}
	daemon.requireBatch(sends[1], 2, daemon.batchCalldata(2, 3))
	daemon.requireIdle(2, 6)
	daemon.requireIdle(3, 7)
}

// E-5, CL-5: a batch whose broadcast is lost keeps every unit in flight with
// the same exact bytes, across a restart, and settles them all once those
// bytes are rebroadcast and final; nothing is signed again.
func TestClaimV2BatchInFlightKeepsItsExactBytes(t *testing.T) {
	chain, daemon := newClaimV2BatchTest(t)
	chain.with(func() { chain.sendFailures = 1 })
	daemon.step(false)
	first := daemon.durable(1)
	for _, index := range []uint64{1, 2, 3} {
		if unit := daemon.durable(index); unit.Status != "uncertain" || unit.TxHash != first.TxHash || unit.RawTxHex != first.RawTxHex || unit.RawTxHex == "" ||
			!strings.HasPrefix(unit.LastError, "exact transaction was prepared but finality was not confirmed: ") {
			t.Fatalf("unit %d of a lost batch: %+v", index, unit)
		}
	}
	daemon.restart()
	if daemon.admission.nonceMinimum() != claimV2TestNonce(t, first.RawTxHex)+1 {
		t.Fatalf("restart seeded nonce floor %d", daemon.admission.nonceMinimum())
	}
	daemon.advance(time.Minute)
	daemon.step(false)
	for _, index := range []uint64{1, 2, 3} {
		if unit := daemon.durable(index); unit.Status != "uncertain" || unit.RawTxHex != first.RawTxHex || !strings.Contains(unit.LastError, "exact transaction is not yet canonically finalized") {
			t.Fatalf("unit %d after the rebroadcast: %+v", index, unit)
		}
	}
	daemon.advance(time.Minute)
	daemon.step(false)
	if sends := chain.sent(); len(sends) != 2 || sends[0] != first.RawTxHex || sends[1] != first.RawTxHex {
		t.Fatalf("rebroadcast: %v", sends)
	}
	for index, alpha := range map[uint64]float64{1: 5, 2: 6, 3: 7} {
		daemon.requireIdle(index, alpha)
	}
}

// E-5, CL-5: an exact batch whose rebroadcast fails, or whose nonce another
// transaction consumed, stays in flight.
func TestClaimV2BatchRebroadcastOutcomes(t *testing.T) {
	for _, test := range []struct {
		name string
		act  func(*claimV2TestChain)
		want string
	}{
		{"rebroadcast refused", func(chain *claimV2TestChain) { chain.sendFailures = 2 }, "exact rebroadcast did not reconcile outcome"},
		{"nonce consumed", func(chain *claimV2TestChain) { chain.nonce++ }, "signed nonce is consumed without an authenticated receipt"},
	} {
		t.Run(test.name, func(t *testing.T) {
			chain, daemon := newClaimV2BatchTest(t)
			chain.with(func() { chain.sendFailures = 1 })
			daemon.step(false)
			chain.with(func() { test.act(chain) })
			daemon.advance(time.Minute)
			daemon.step(false)
			for _, index := range []uint64{1, 2, 3} {
				if unit := daemon.durable(index); unit.Status != "uncertain" || !strings.Contains(unit.LastError, test.want) {
					t.Fatalf("unit %d: %+v", index, unit)
				}
			}
		})
	}
}

// E-5, CL-5, SPEC A6: each claim's preflight decides its place in the batch:
// a refused one stays open under the backoff while the others are batched; a
// claim a third party made first is done, and a last passing claim goes alone.
func TestClaimV2BatchPreflightsEachClaim(t *testing.T) {
	chain, daemon := newClaimV2BatchTest(t)
	chain.with(func() { chain.refuseIndex[3] = true })
	daemon.step(false)
	sends := chain.sent()
	if len(sends) != 1 {
		t.Fatalf("preflights: %d sends", len(sends))
	}
	daemon.requireBatch(sends[0], 2, daemon.batchCalldata(1, 2))
	daemon.requireOpen(3, 1, time.Minute, "claim preflight: execution reverted: runtime transfer refusal")

	chain, daemon = newClaimV2BatchTest(t)
	chain.with(func() {
		chain.refuseIndex[3] = true
		chain.beforeEstimate = func() { chain.claimed[2] = claimV2TestRao(6) }
	})
	daemon.step(false)
	if sends := chain.sent(); len(sends) != 1 || *claimV2TestSent(t, sends[0]).To() != common.HexToAddress(daemon.cfg.Vault) {
		t.Fatalf("a single passing claim: %d sends", len(chain.sent()))
	}
	daemon.requireIdle(1, 5)
	if unit := daemon.durable(2); unit.Status != "idle" || unit.Claimed != claimV2TestDec(6) {
		t.Fatalf("third party's claim: %+v", unit)
	}

	// No passing claim sends nothing.
	chain, daemon = newClaimV2BatchTest(t)
	chain.with(func() { chain.refuseClaims = true })
	daemon.step(false)
	if len(chain.sent()) != 0 {
		t.Fatal("refused preflights sent a batch")
	}
	for _, index := range []uint64{1, 2, 3} {
		daemon.requireOpen(index, 1, time.Minute, "claim preflight: ")
	}
}

// E-5, CL-5: a reverted batch stays in flight until its finalized receipt
// refuses each of its claims, which stay open under the backoff; the
// watermark re-read closes one a third party made.
func TestClaimV2BatchRevertedRefusesEachClaim(t *testing.T) {
	chain, daemon := newClaimV2BatchTest(t)
	chain.with(func() {
		chain.revertMined = true
		chain.beforeMine = func() { chain.claimed[2] = claimV2TestRao(6) }
	})
	daemon.step(false)
	hash := strings.ToLower(claimV2TestSent(t, chain.sent()[0]).Hash().Hex())
	if unit := daemon.durable(1); unit.Status != "uncertain" || !strings.Contains(unit.LastError, " reverted on-chain ") {
		t.Fatalf("reverted batch before its settlement: %+v", unit)
	}
	daemon.advance(time.Minute)
	daemon.step(false)
	for _, index := range []uint64{1, 3} {
		daemon.requireOpen(index, 1, time.Minute, "claim batch "+hash+" reverted in finalized block ")
	}
	if unit := daemon.durable(2); unit.Status != "idle" || unit.Claimed != claimV2TestDec(6) {
		t.Fatalf("third party's claim in a reverted batch: %+v", unit)
	}
}

// E-5, CL-5: a batch receipt that does not prove an outcome keeps the units
// it concerns in flight: the batcher's events missing, or a paid claim without
// its vault2 Claimed, or with another caller.
func TestClaimV2BatchUnprovenReceiptStaysInFlight(t *testing.T) {
	other := common.HexToAddress("0x00000000000000000000000000000000000000aa")
	for _, test := range []struct {
		name   string
		mutate func(*claimV2TestChain)
		want   string
	}{
		{"no events", func(chain *claimV2TestChain) { chain.dropLogs = true }, "canonical receipt does not prove the batch's outcome"},
		{"no Claimed", func(chain *claimV2TestChain) { chain.batch.dropVault = true }, "exact batched v2 claim has no Claimed event"},
		{"another caller", func(chain *claimV2TestChain) { chain.batch.caller = &other }, "canonical v2 Claimed event differs from the exact batched intent"},
	} {
		t.Run(test.name, func(t *testing.T) {
			chain, daemon := newClaimV2BatchTest(t)
			chain.with(func() { test.mutate(chain) })
			daemon.step(false)
			for _, index := range []uint64{1, 2, 3} {
				if unit := daemon.durable(index); unit.Status != "uncertain" || unit.RawTxHex == "" || !strings.Contains(unit.LastError, test.want) {
					t.Fatalf("unit %d: %+v", index, unit)
				}
			}
		})
	}
}

// CL-5: a batch that cannot be prepared retries its units: every unit when
// the relayer key or every endpoint fails it, and the units that passed their
// preflight when the batch cannot be sized.
func TestClaimV2BatchNotReadyRetries(t *testing.T) {
	chain, daemon := newClaimV2BatchTest(t)
	if err := os.Remove(daemon.cfg.KeyFile); err != nil {
		t.Fatal(err)
	}
	daemon.step(false)
	for _, index := range []uint64{1, 2, 3} {
		daemon.requireOpen(index, 1, time.Minute, "claim batch is not ready: key_file: ")
	}

	chain, daemon = newClaimV2BatchTest(t)
	views := 0
	chain.with(func() {
		chain.afterView = func() {
			if views++; views == 9 {
				chain.failMethod = "eth_chainId"
			}
		}
	})
	daemon.step(false)
	for _, index := range []uint64{1, 2, 3} {
		daemon.requireOpen(index, 1, time.Minute, "claim batch is not ready: "+chain.server.URL+": chain id ")
	}

	chain, daemon = newClaimV2BatchTest(t)
	chain.with(func() { chain.refuseIndex[3], chain.batch.refuseCalls = true, true })
	daemon.step(false)
	if len(chain.sent()) != 0 {
		t.Fatal("an unsized batch was sent")
	}
	daemon.requireOpen(1, 1, time.Minute, "claim batch is not ready: batch preflight at ")
	daemon.requireOpen(2, 1, time.Minute, "claim batch is not ready: batch preflight at ")
	daemon.requireOpen(3, 1, time.Minute, "claim preflight: ")
	_ = chain
}

// E-5, CL-5: a batch refused before it is signed refuses each of its claims,
// and a nonce custody failure stops the daemon with the units unsigned.
func TestClaimV2BatchRefusedBeforeSigning(t *testing.T) {
	chain, daemon := newClaimV2BatchTest(t)
	chain.with(func() { chain.failMethod = "eth_gasPrice" })
	daemon.step(false)
	if len(chain.sent()) != 0 {
		t.Fatal("a batch without a gas price was sent")
	}
	for _, index := range []uint64{1, 2, 3} {
		daemon.requireOpen(index, 1, time.Minute, "gas price: ")
	}

	chain, daemon = newClaimV2BatchTest(t)
	daemon.admission.chainId = big.NewInt(946)
	snapshot, err := readClaimSnapshotV2(daemon.ctx, daemon.strategy, daemon.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := daemon.observe(snapshot, false); err != nil {
		t.Fatal(err)
	}
	err = daemon.poll(daemon.ctx)
	var custody *claimNonceSafetyError
	if !errors.As(err, &custody) || len(chain.sent()) != 0 {
		t.Fatalf("custody failure did not stop the daemon: %v", err)
	}
	for _, index := range []uint64{1, 2, 3} {
		if unit := daemon.durable(index); unit.Status != "open" || unit.signed() || !strings.Contains(unit.LastError, "different chain") {
			t.Fatalf("unit %d: %+v", index, unit)
		}
	}
}

// E-5, CL-5: an uncertain publication of the batch's prepared bytes raises
// the nonce floor and leaves every unit unsigned, as a single claim's does.
func TestClaimV2BatchLostPreparedAcknowledgement(t *testing.T) {
	chain, daemon := newClaimV2BatchTest(t)
	snapshot, err := readClaimSnapshotV2(daemon.ctx, daemon.strategy, daemon.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := daemon.observe(snapshot, false); err != nil {
		t.Fatal(err)
	}
	units := []*ClaimUnitV2{daemon.unit(1), daemon.unit(2)}
	daemon.store.owner.afterSync = func(*os.File) error { return syscall.EIO }
	err = daemon.submit(daemon.ctx, units, common.HexToAddress(claimV2TestBatcher), daemon.batchCalldata(1, 2), 4_000_000)
	if !errors.Is(err, durablehead.ErrUncertain) || len(chain.sent()) != 0 || daemon.admission.nonceMinimum() != 1 || units[0].signed() || units[1].signed() {
		t.Fatalf("uncertain prepared batch lost nonce custody: err=%v floor=%d", err, daemon.admission.nonceMinimum())
	}
}

// CL-5: a signed batch authenticates for each unit it claims, with its place,
// and is refused when it is not the configured batcher's exact claim batch of
// the configured vault naming the unit once.
func TestClaimV2BatchAuthentication(t *testing.T) {
	chain, daemon := newClaimV2BatchTest(t)
	_ = chain
	items := []stabi.STClaimBatcherClaim{claimV2Item(daemon.account(1)), claimV2Item(daemon.account(2))}
	sign := func(to string, data []byte) (string, string) {
		return claimV2TestSign(t, daemon.key, claimV2TestChainID, common.HexToAddress(to), 0, 3, data)
	}
	unit := func(index uint64, hash, raw string) *ClaimUnitV2 {
		return &ClaimUnitV2{Vault: daemon.cfg.Vault, NoID: 7, Index: index, Status: "uncertain", TxHash: hash, RawTxHex: raw}
	}
	cap := new(big.Int).SetUint64(claimV2TestBatchCap)
	hash, raw := sign(claimV2TestBatcher, stClaimBatcher.PackClaimMany(common.HexToAddress(daemon.cfg.Vault), items, cap))
	for place, index := range []uint64{1, 2} {
		tx, intent, from, err := authenticateSignedClaimV2(daemon.cfg, unit(index, hash, raw))
		if err != nil || intent.Item != place || intent.Items != 2 || intent.Account.Index.Uint64() != index || from != crypto.PubkeyToAddress(daemon.key.PublicKey) || tx.Hash().Hex() != common.HexToHash(hash).Hex() {
			t.Fatalf("unit %d: %+v %v", index, intent, err)
		}
	}
	twice := stClaimBatcher.PackClaimMany(common.HexToAddress(daemon.cfg.Vault), []stabi.STClaimBatcherClaim{items[0], items[0]}, cap)
	for _, test := range []struct {
		name, to string
		data     []byte
		index    uint64
		batcher  string
		want     string
	}{
		{"no batcher configured", claimV2TestBatcher, stClaimBatcher.PackClaimMany(common.HexToAddress(daemon.cfg.Vault), items, cap), 1, "", "signed v2 claim has another chain, target or value"},
		{"another target", "0x00000000000000000000000000000000000000bb", stClaimBatcher.PackClaimMany(common.HexToAddress(daemon.cfg.Vault), items, cap), 1, claimV2TestBatcher, "signed v2 claim has another chain, target or value"},
		{"another vault", claimV2TestBatcher, stClaimBatcher.PackClaimMany(common.HexToAddress("0x00000000000000000000000000000000000000cc"), items, cap), 1, claimV2TestBatcher, "v2 claim batch names another vault"},
		{"another account", claimV2TestBatcher, stClaimBatcher.PackClaimMany(common.HexToAddress(daemon.cfg.Vault), items, cap), 3, claimV2TestBatcher, "signed v2 claim calldata names another account"},
		{"twice", claimV2TestBatcher, twice, 1, claimV2TestBatcher, "v2 claim batch names the account twice"},
		{"trailing bytes", claimV2TestBatcher, append(stClaimBatcher.PackClaimMany(common.HexToAddress(daemon.cfg.Vault), items, cap), 0), 1, claimV2TestBatcher, "v2 claim batch calldata is not canonical"},
		{"not a batch", claimV2TestBatcher, claimV2Calldata(daemon.account(1)), 1, claimV2TestBatcher, "calldata is not a v2 claim batch"},
		{"malformed", claimV2TestBatcher, stClaimBatcherClaimMany.ID, 1, claimV2TestBatcher, "abi: "},
	} {
		daemon.cfg.Batcher = test.batcher
		hash, raw := sign(test.to, test.data)
		if _, _, _, err := authenticateSignedClaimV2(daemon.cfg, unit(test.index, hash, raw)); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Fatalf("%s: %v, want %q", test.name, err, test.want)
		}
	}
}

// CL-5: a batched claim's Claimed event must be its account's only one, from
// the vault, at the receipt's inclusion, matching the intent, the batcher's
// amount and the batcher as caller.
func TestClaimV2BatchedReceiptVerification(t *testing.T) {
	vault, batcher := common.HexToAddress(claimV2TestVault), common.HexToAddress(claimV2TestBatcher)
	tx := types.NewTx(&types.LegacyTx{To: &batcher})
	intent := &claimIntentV2{NoID: big.NewInt(7), RootIndex: big.NewInt(0), Account: stabi.LedgerTreeAccount{Index: big.NewInt(1), Coldkey: claimV2TestColdkey, Cumulative: big.NewInt(5)}, Item: 0, Items: 1}
	receipt := &types.Receipt{TxHash: common.Hash{0x7}, BlockHash: common.Hash{0xb}, BlockNumber: big.NewInt(9)}
	claimed := func(change func(*claimIntentV2, *big.Int, *common.Address)) *types.Log {
		event, amount, caller := *intent, big.NewInt(5), batcher
		event.Account.Cumulative = big.NewInt(5)
		if change != nil {
			change(&event, amount, &caller)
		}
		return claimV2TestClaimedLog(vault, &event, uint256.MustFromBig(amount), caller, receipt)
	}
	other := claimed(func(event *claimIntentV2, _ *big.Int, _ *common.Address) { event.Account.Index = big.NewInt(2) })
	foreign := claimed(nil)
	foreign.Address = batcher
	receipt.Logs = []*types.Log{nil, foreign, other, claimed(nil)}
	if event, err := verifyBatchedClaimV2Receipt(vault, tx, intent, big.NewInt(5), receipt); err != nil || event.Amount.Int64() != 5 {
		t.Fatalf("matching Claimed: %v", err)
	}
	removed := claimed(nil)
	removed.Removed = true
	for _, test := range []struct {
		name string
		logs []*types.Log
		want string
	}{
		{"none", []*types.Log{other}, "exact batched v2 claim has no Claimed event"},
		{"twice", []*types.Log{claimed(nil), claimed(nil)}, "differs from the exact batched intent"},
		{"removed", []*types.Log{removed}, "differs from the exact batched intent"},
		{"root", []*types.Log{claimed(func(event *claimIntentV2, _ *big.Int, _ *common.Address) { event.RootIndex = big.NewInt(1) })}, "differs from the exact batched intent"},
		{"cumulative", []*types.Log{claimed(func(event *claimIntentV2, _ *big.Int, _ *common.Address) { event.Account.Cumulative = big.NewInt(6) })}, "differs from the exact batched intent"},
		{"amount", []*types.Log{claimed(func(_ *claimIntentV2, amount *big.Int, _ *common.Address) { amount.SetInt64(4) })}, "differs from the exact batched intent"},
		{"caller", []*types.Log{claimed(func(_ *claimIntentV2, _ *big.Int, caller *common.Address) { *caller = vault })}, "differs from the exact batched intent"},
	} {
		receipt.Logs = test.logs
		if _, err := verifyBatchedClaimV2Receipt(vault, tx, intent, big.NewInt(5), receipt); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Fatalf("%s: %v, want %q", test.name, err, test.want)
		}
	}
}

// CL-5: a configured batcher is a lowercase 0x address other than the vault.
func TestClaimV2ConfigBatcher(t *testing.T) {
	chain := newClaimV2TestChain(t)
	_, keyFile := writeClaimV2TestKey(t)
	path := writeClaimV2TestConfig(t, chain, keyFile, t.TempDir(), claimV2TestColdkey)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for batcher, valid := range map[string]bool{
		claimV2TestBatcher:                             true,
		strings.ToUpper(claimV2TestBatcher[2:]):        false,
		"0x" + strings.ToUpper(claimV2TestBatcher[2:]): false,
		claimV2TestBatcher[2:]:                         false,
		"0x1234":                                       false,
		strings.ToLower(chain.vault.Hex()):             false,
	} {
		if err := os.WriteFile(path, append(append([]byte(nil), raw...), "batcher: \""+batcher+"\"\n"...), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := LoadClaimDaemonV2Config(path)
		if valid && (err != nil || cfg.Batcher != batcher) || !valid && (cfg != nil || err == nil || err.Error() != "invalid v2 claim daemon configuration") {
			t.Fatalf("batcher %q: %+v %v", batcher, cfg, err)
		}
	}
}

// E-5, CL-5: a pass whose due units all turn out covered by their finalized
// watermarks sends nothing.
func TestClaimV2BatchWithNothingDueSendsNothing(t *testing.T) {
	chain, daemon := newClaimV2BatchTest(t)
	chain.with(func() {
		for index, alpha := range map[uint64]float64{1: 5, 2: 6, 3: 7} {
			chain.claimed[index] = claimV2TestRao(alpha)
		}
		chain.account = func(entry map[string]any) { entry["claimed"] = "0" }
	})
	daemon.step(false)
	if len(chain.sent()) != 0 || chain.count("estimate") != 0 {
		t.Fatalf("covered units sent %d transactions", len(chain.sent()))
	}
	for index, alpha := range map[uint64]float64{1: 5, 2: 6, 3: 7} {
		if unit := daemon.durable(index); unit.Status != "idle" || unit.Claimed != claimV2TestDec(alpha) {
			t.Fatalf("unit %d: %+v", index, unit)
		}
	}
}

// E-5, CL-5: signed bytes that do not authenticate for their unit are never
// broadcast, and a unit that shares a batch's bytes but is not in it is
// refused while the batch's own units settle.
func TestClaimV2BatchRefusesBytesOfAnotherUnit(t *testing.T) {
	chain, daemon := newClaimV2BatchTest(t)
	snapshot, err := readClaimSnapshotV2(daemon.ctx, daemon.strategy, daemon.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := daemon.observe(snapshot, false); err != nil {
		t.Fatal(err)
	}
	err = daemon.submit(daemon.ctx, []*ClaimUnitV2{daemon.unit(3)}, common.HexToAddress(claimV2TestBatcher), daemon.batchCalldata(1, 2), 4_000_000)
	if err == nil || !strings.Contains(err.Error(), "signed v2 claim calldata names another account") || len(chain.sent()) != 0 {
		t.Fatalf("bytes of another unit: %v, %d sends", err, len(chain.sent()))
	}

	chain, daemon = newClaimV2BatchTest(t)
	chain.with(func() { chain.sendFailures = 1 })
	daemon.step(false)
	lost := daemon.durable(1)
	stray := *daemon.unit(3)
	stray.Index = 9
	daemon.queue.Units[claimUnitV2Key(daemon.cfg.Vault, 7, 9)] = &stray
	daemon.advance(time.Minute)
	daemon.step(false)
	daemon.advance(time.Minute)
	daemon.step(false)
	if unit := daemon.durable(9); unit.Status != "uncertain" || unit.RawTxHex != lost.RawTxHex || !strings.Contains(unit.LastError, "signed v2 claim calldata names another account") {
		t.Fatalf("stray unit: %+v", unit)
	}
	for index, alpha := range map[uint64]float64{1: 5, 2: 6, 3: 7} {
		daemon.requireIdle(index, alpha)
	}
}

// CL-5: the batch's endpoint is the first that serves the configured chain.
func TestClaimV2DialFailsOver(t *testing.T) {
	chain := newClaimV2TestChain(t)
	cfg := &ClaimDaemonV2Config{ChainID: claimV2TestChainID, RPC: []string{"ftp://rpc.example", chain.server.URL}}
	client, err := dialClaimV2(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	client.Close()
	cfg.ChainID = 946
	if _, err := dialClaimV2(t.Context(), cfg); err == nil || !strings.Contains(err.Error(), "ftp://rpc.example: ") || !strings.Contains(err.Error(), chain.server.URL+": chain id 945 (<nil>), want 946") {
		t.Fatalf("no endpoint of chain 946: %v", err)
	}
}

// E-5, CL-5: a storage failure stops a batch pass with its error, whichever
// save fails: a unit's retry or idle after verification, the submitting
// boundary, a refused preflight, a retry for an unsized batch, and a
// settlement.
func TestClaimV2BatchStorageFailureStops(t *testing.T) {
	for _, test := range []struct {
		name  string
		setup func(*claimV2TestChain)
	}{
		{"verification retry", func(chain *claimV2TestChain) {
			chain.account = func(entry map[string]any) {
				if entry["index"] == 1 {
					entry["cumulative"] = claimV2TestDec(8)
				}
			}
		}},
		{"idle", func(chain *claimV2TestChain) {
			chain.claimed[1] = claimV2TestRao(5)
			chain.account = func(entry map[string]any) { entry["claimed"] = "0" }
		}},
		{"submitting", func(*claimV2TestChain) {}},
		{"refused preflight", func(chain *claimV2TestChain) { chain.refuseIndex[1] = true }},
		{"unsized batch", func(chain *claimV2TestChain) { chain.batch.refuseCalls = true }},
		{"settlement", func(chain *claimV2TestChain) { chain.beforeMine = func() { chain.batch.itemGas = 1_200_000 } }},
	} {
		t.Run(test.name, func(t *testing.T) {
			chain, daemon := newClaimV2BatchTest(t)
			chain.with(func() { test.setup(chain) })
			snapshot, err := readClaimSnapshotV2(daemon.ctx, daemon.strategy, daemon.cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err := daemon.observe(snapshot, false); err != nil {
				t.Fatal(err)
			}
			syncs := 0
			fail := map[string]int{"settlement": 3}[test.name]
			if fail == 0 {
				fail = 1
			}
			daemon.store.owner.afterSync = func(*os.File) error {
				if syncs++; syncs >= fail {
					return syscall.EIO
				}
				return nil
			}
			if err := daemon.poll(daemon.ctx); !errors.Is(err, durablehead.ErrUncertain) {
				t.Fatalf("storage failure: %v", err)
			}
		})
	}
}

// A pool whose accounts 1..due all belong to the configured coldkey, each
// with 5 α to claim, and a daemon claiming through the batcher.
func newClaimV2ManyDueTest(t *testing.T, due int) (*claimV2TestChain, *claimV2TestDaemon) {
	t.Helper()
	chain := newClaimV2TestChain(t)
	chain.batch = &claimV2TestBatcherModel{address: common.HexToAddress(claimV2TestBatcher)}
	accounts := []merkle.LedgerAccount{claimV2TestAccount(claimV2TestOther, 1)}
	for range due {
		accounts = append(accounts, claimV2TestAccount(claimV2TestColdkey, 5))
	}
	chain.finalize(accounts...)
	daemon := newClaimV2TestDaemon(t, chain, ClaimV2DefaultThresholdRao)
	daemon.cfg.Batcher = claimV2TestBatcher
	return chain, daemon
}

// CL-5 (review of 77f86ead): every unit of a batch keeps the batch's whole
// signed transaction, so a batch of n units puts n copies of n claims in the
// queue, whose durable head holds 16 MiB; at depth 29 about 62 claims fill
// it, and a batch of all of them could never be checkpointed, so each pass
// would prepare it again and nothing would be sent. The daemon batches at
// most onchain.ClaimBatchMaxClaims due units, in due order: 66 due accounts
// go out as two full batches of 32 and then the other 2, and every one is
// paid.
func TestClaimV2BatchHoldsAtMostTheCap(t *testing.T) {
	const due = 66
	chain, daemon := newClaimV2ManyDueTest(t, due)
	// Due units of equal deadlines are in key order, which orders their
	// indexes as text.
	order := make([]string, due)
	for i := range order {
		order[i] = fmt.Sprint(i + 1)
	}
	sort.Strings(order)
	for pass, size := range []int{32, 32, 2} {
		daemon.step(false)
		sends := chain.sent()
		if len(sends) != pass+1 {
			unit := daemon.durable(1)
			t.Fatalf("pass %d sent %d transactions; unit 1: %+v", pass+1, len(sends), unit)
		}
		indexes := make([]uint64, size)
		for i := range indexes {
			index, err := strconv.ParseUint(order[32*pass+i], 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			indexes[i] = index
		}
		daemon.requireBatch(sends[pass], uint64(size), daemon.batchCalldata(indexes...))
	}
	for index := uint64(1); index <= due; index++ {
		daemon.requireIdle(index, 5)
	}
}

// CL-5, §10.1 Refusals (review of 77f86ead): a unit refused at a batch's
// preflight counts its attempt as a claim refused alone does, so the backoff
// doubles alike, from a minute to an hour; uncounted, two or more refused
// units would be preflighted every minute for as long as the runtime
// refuses them.
func TestClaimV2BatchPreflightRefusalsBackOff(t *testing.T) {
	delays := func(batcher string) []time.Duration {
		chain, daemon := newClaimV2BatchTest(t)
		daemon.cfg.Batcher = batcher
		chain.with(func() { chain.refuseClaims = true })
		var delays []time.Duration
		for range 5 {
			// One poll handles one lone claim, so step until unit 1 was refused again.
			for range 3 {
				daemon.step(false)
			}
			unit := daemon.durable(1)
			next, err := time.Parse(time.RFC3339Nano, unit.NextRetryAt)
			if err != nil || unit.Status != "open" || !strings.Contains(unit.LastError, "runtime transfer refusal") {
				t.Fatalf("refused unit: %+v", unit)
			}
			delays = append(delays, next.Sub(daemon.clock))
			daemon.advance(next.Sub(daemon.clock))
		}
		if len(chain.sent()) != 0 {
			t.Fatalf("a refused claim was sent")
		}
		return delays
	}
	alone, batched := delays(""), delays(claimV2TestBatcher)
	want := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute}
	if !slices.Equal(alone, want) || !slices.Equal(batched, want) {
		t.Fatalf("retry delays after each refused preflight: alone %v, with a batcher %v, want %v", alone, batched, want)
	}
}

// CL-5: the daemon refuses to start with a batcher whose code is not exactly
// the release claim batcher's, which would refuse or misreport every batch,
// and with one whose code it cannot read; it starts with the release code.
func TestClaimV2DaemonRefusesAnotherBatcher(t *testing.T) {
	batcher := common.HexToAddress(claimV2TestBatcher).Hex()
	other := []byte{0x60, 0x00}
	for _, test := range []struct {
		name   string
		mutate func(*claimV2TestChain)
		want   string
	}{
		{"no code", func(chain *claimV2TestChain) { chain.batch.code = []byte{} }, "claim batcher " + batcher + " has no code"},
		{"another contract", func(chain *claimV2TestChain) { chain.batch.code = other }, batcher + " is not the release claim batcher: its code hash is " + crypto.Keccak256Hash(other).Hex() +
			", the release runtime's " + crypto.Keccak256Hash(common.FromHex(onchain.ClaimBatcherRuntime)).Hex()},
		{"unread code", func(chain *claimV2TestChain) { chain.failMethod = "eth_getCode" }, "claim batcher " + batcher + " code: synthetic eth_getCode refusal"},
		{"no endpoint", func(chain *claimV2TestChain) { chain.failMethod = "eth_chainId" }, "claim batcher " + claimV2TestBatcher + ": " + chain0URL},
	} {
		t.Run(test.name, func(t *testing.T) {
			chain, daemon := newClaimV2BatchTest(t)
			chain.with(func() { test.mutate(chain) })
			want := strings.ReplaceAll(test.want, chain0URL, chain.server.URL)
			err := runClaimDaemonV2WithStore(daemon.ctx, daemon.cfg, daemon.store, &claimAdmission{}, ClaimV2DefaultThresholdRao, false)
			if err == nil || !strings.HasPrefix(err.Error(), want) || len(chain.sent()) != 0 {
				t.Fatalf("daemon start: %v, want %q", err, want)
			}
		})
	}
	chain, daemon := newClaimV2BatchTest(t)
	if err := verifyClaimV2Batcher(daemon.ctx, daemon.cfg); err != nil || chain.count("code") != 1 {
		t.Fatalf("the release batcher: %v, %d code reads", err, chain.count("code"))
	}
	// A shutdown during the check is the shutdown, never a failure.
	canceled, cancel := context.WithCancel(daemon.ctx)
	cancel()
	if err := runClaimDaemonV2WithStore(canceled, daemon.cfg, daemon.store, &claimAdmission{}, ClaimV2DefaultThresholdRao, false); err != nil {
		t.Fatalf("shutdown during the batcher check: %v", err)
	}
}

// chain0URL stands for the fixture's endpoint in an expected error.
const chain0URL = "<endpoint>"

// CL-5, §10.1: a batch in flight keeps its exact bytes, which name the
// configured batcher, so removing or changing `batcher` while one is in
// flight stops the daemon at start: the bytes no longer authenticate, and
// nonce custody fails closed.
func TestClaimV2BatchInFlightNeedsItsBatcher(t *testing.T) {
	chain, daemon := newClaimV2BatchTest(t)
	chain.with(func() { chain.sendFailures = 1 })
	daemon.step(false)
	if unit := daemon.durable(1); unit.Status != "uncertain" || unit.RawTxHex == "" {
		t.Fatalf("batch in flight: %+v", unit)
	}
	removed := *daemon.cfg
	removed.Batcher = ""
	changed := *daemon.cfg
	changed.Batcher = "0x00000000000000000000000000000000000b47c5"
	chain.with(func() { chain.batch.address = common.HexToAddress(changed.Batcher) })
	sent := len(chain.sent())
	for name, cfg := range map[string]*ClaimDaemonV2Config{"removed": &removed, "changed": &changed} {
		err := runClaimDaemonV2WithStore(daemon.ctx, cfg, daemon.store, &claimAdmission{}, ClaimV2DefaultThresholdRao, false)
		if err == nil || !strings.Contains(err.Error(), "signed v2 claim has another chain, target or value") || len(chain.sent()) != sent {
			t.Fatalf("batcher %s with a batch in flight: %v", name, err)
		}
	}
}
