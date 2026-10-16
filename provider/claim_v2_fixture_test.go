//go:build linux || darwin

// A synthetic vault2 pool behind real JSON-RPC and the pinned ledger account
// endpoint. Keys, coldkeys, chain identity and block hashes are synthetic; the
// trees, proofs, calldata, signatures and receipts are real.
package main

import (
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"
	"github.com/urnetwork/connect"
	"gopkg.in/yaml.v3"

	"github.com/urfoundation/sn/merkle"
	"github.com/urfoundation/sn/ss58"
	"github.com/urfoundation/sn/stabi"
)

const (
	claimV2TestChainID = 945
	claimV2TestVault   = "0x00000000000000000000000000000000000a2a2a"
)

var (
	claimV2TestColdkey = [32]byte{0xc0, 0x1d, 0x01}
	claimV2TestOther   = [32]byte{0xc0, 0x1d, 0x02}
	claimV2TestABI     = func() *abi.ABI {
		parsed, err := stabi.STSettlementVault2MetaData.ParseABI()
		if err != nil {
			panic(err)
		}
		return parsed
	}()
)

// Rao from α with nine decimals, as the vault counts it.
func claimV2TestRao(alpha float64) uint256.Int {
	return *uint256.NewInt(uint64(alpha * 1e9))
}

func claimV2TestAccount(coldkey [32]byte, alpha float64) merkle.LedgerAccount {
	return merkle.LedgerAccount{Coldkey: coldkey, Cumulative: claimV2TestRao(alpha)}
}

func claimV2TestBlockHash(number uint64) common.Hash {
	return common.BigToHash(new(big.Int).SetUint64(number + 0xb10c))
}

type claimV2TestRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func claimV2TestRevert(name string) *claimV2TestRPCError {
	return &claimV2TestRPCError{Code: 3, Message: "execution reverted: " + name}
}

// One pool of one vault. Every mined block is final at once; the knobs model
// a lost broadcast, a runtime refusal at preflight or after it, a third
// party's claim landing first, a lagging nonce view, a failing method, a
// moved finalized head and a receipt without its event. All fields are
// guarded by stateLock.
type claimV2TestChain struct {
	t            *testing.T
	stateLock    sync.Mutex
	vault        common.Address
	noID         uint64
	ledgers      [][]merkle.LedgerAccount
	trees        []*merkle.LedgerSumTree
	claimed      map[uint64]uint256.Int
	head         uint64
	nonce        uint64
	nonceView    *uint64
	receipts     map[common.Hash]*types.Receipt
	sends        []string
	sendFailures int
	revertMined  bool
	refuseClaims bool
	refuseIndex  map[uint64]bool
	dropLogs     bool
	estimate     uint64
	// claimGas, when set, is the gas vault2's transfer needs, as runtime 478
	// sizes it to vault2's staking-hotkey list: an eth_call of a claim that
	// names less, and a claim mined with less, runs out of gas.
	claimGas        uint64
	calls           []string
	accountStatus   int
	failMethod      string
	finalized       *uint64
	beforePreflight func()
	beforeEstimate  func()
	beforeMine      func()
	afterView       func()
	account         func(map[string]any)
	server          *httptest.Server
	// batch models the claim batcher (claim_v2_batch_test.go); nil means
	// the chain has none.
	batch *claimV2TestBatcherModel
}

func newClaimV2TestChain(t *testing.T) *claimV2TestChain {
	t.Helper()
	chain := &claimV2TestChain{t: t, vault: common.HexToAddress(claimV2TestVault), noID: 7, claimed: map[uint64]uint256.Int{}, refuseIndex: map[uint64]bool{}, head: 100, receipts: map[common.Hash]*types.Receipt{}}
	chain.server = httptest.NewServer(chain)
	t.Cleanup(chain.server.Close)
	return chain
}

// Appends a finalized root over the accounts in position order.
func (self *claimV2TestChain) finalize(accounts ...merkle.LedgerAccount) {
	self.t.Helper()
	tree, err := merkle.BuildLedgerSumTree(context.Background(), accounts)
	if err != nil {
		self.t.Fatal(err)
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.ledgers = append(self.ledgers, accounts)
	self.trees = append(self.trees, tree)
}

func (self *claimV2TestChain) with(change func()) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	change()
}

func (self *claimV2TestChain) claimedOf(index uint64) uint256.Int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.claimed[index]
}

func (self *claimV2TestChain) sent() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]string(nil), self.sends...)
}

func (self *claimV2TestChain) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Method == http.MethodGet && request.URL.Path == "/sn/ledger/account" {
		self.serveAccounts(writer, request)
		return
	}
	var call struct {
		ID     json.RawMessage   `json:"id"`
		Method string            `json:"method"`
		Params []json.RawMessage `json:"params"`
	}
	if err := json.NewDecoder(request.Body).Decode(&call); err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	}
	result, callErr := self.rpc(call.Method, call.Params)
	response := map[string]any{"jsonrpc": "2.0", "id": call.ID, "result": result}
	if callErr != nil {
		delete(response, "result")
		response["error"] = callErr
	}
	if err := json.NewEncoder(writer).Encode(response); err != nil && !claimV2TestAbandoned(err) {
		self.t.Error(err)
	}
}

// claimV2TestAbandoned reports a write to a client that abandoned its
// request: the read strategy races connections and closes the losers, which a
// large body can still be writing to.
func claimV2TestAbandoned(err error) bool {
	return errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNRESET)
}

// The pinned GET /sn/ledger/account body for the latest finalized root.
func (self *claimV2TestChain) serveAccounts(writer http.ResponseWriter, request *http.Request) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.accountStatus != 0 {
		http.Error(writer, "synthetic account refusal", self.accountStatus)
		return
	}
	coldkey := request.URL.Query().Get("coldkey")
	accounts := []map[string]any{}
	if last := len(self.trees) - 1; last >= 0 {
		for index, account := range self.ledgers[last] {
			if hexutil.Encode(account.Coldkey[:]) != coldkey {
				continue
			}
			proof, err := self.trees[last].Proof(uint64(index))
			if err != nil {
				self.t.Error(err)
			}
			nodes := []map[string]any{}
			for _, node := range proof {
				nodes = append(nodes, map[string]any{"hash": hexutil.Encode(node.Hash[:]), "sum": node.Sum.Dec()})
			}
			claimed := self.claimed[uint64(index)]
			entry := map[string]any{"vault": strings.ToLower(self.vault.Hex()), "no_id": self.noID, "index": index, "root_index": last,
				"cumulative": account.Cumulative.Dec(), "proof": nodes, "claimed": claimed.Dec(), "pending_cumulative": nil}
			if self.account != nil {
				self.account(entry)
			}
			accounts = append(accounts, entry)
		}
	}
	if err := json.NewEncoder(writer).Encode(map[string]any{"coldkey": coldkey, "accounts": accounts}); err != nil && !claimV2TestAbandoned(err) {
		self.t.Error(err)
	}
}

func (self *claimV2TestChain) rpc(method string, params []json.RawMessage) (any, *claimV2TestRPCError) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if method == self.failMethod {
		return nil, &claimV2TestRPCError{Code: -32601, Message: "synthetic " + method + " refusal"}
	}
	switch method {
	case "eth_chainId":
		return hexutil.EncodeUint64(claimV2TestChainID), nil
	case "eth_gasPrice":
		return "0x1", nil
	case "eth_getTransactionCount":
		if self.nonceView != nil {
			return hexutil.EncodeUint64(*self.nonceView), nil
		}
		return hexutil.EncodeUint64(self.nonce), nil
	case "eth_getBlockByNumber":
		var selector string
		if err := json.Unmarshal(params[0], &selector); err != nil {
			self.t.Error(err)
		}
		number := self.head
		if selector == "finalized" && self.finalized != nil {
			number = *self.finalized
		}
		if selector != "finalized" && selector != "latest" {
			value, err := hexutil.DecodeUint64(selector)
			if err != nil || value > self.head {
				return nil, nil
			}
			number = value
		}
		return map[string]any{"number": hexutil.EncodeUint64(number), "hash": claimV2TestBlockHash(number).Hex(), "gasLimit": hexutil.EncodeUint64(75_000_000)}, nil
	case "eth_call", "eth_estimateGas":
		var message struct {
			To    *common.Address `json:"to"`
			Gas   *hexutil.Uint64 `json:"gas"`
			Input hexutil.Bytes   `json:"input"`
			Data  hexutil.Bytes   `json:"data"`
		}
		if err := json.Unmarshal(params[0], &message); err != nil {
			self.t.Error(err)
		}
		data := message.Input
		if len(data) == 0 {
			data = message.Data
		}
		if self.batch != nil && message.To != nil && *message.To == self.batch.address {
			return self.answerBatch(method, message.Gas, data)
		}
		if method == "eth_estimateGas" {
			self.calls = append(self.calls, "estimate")
			if self.beforeEstimate != nil {
				self.beforeEstimate()
			}
			if _, _, err := self.execute(data); err != nil {
				return nil, err
			}
			return hexutil.EncodeUint64(max(self.estimate, 200_000)), nil
		}
		if called, err := claimV2TestABI.MethodById(data[:4]); err == nil && called.Name == "claim" && message.Gas != nil && uint64(*message.Gas) < self.claimGas {
			return nil, &claimV2TestRPCError{Code: -32603, Message: "out of gas"}
		}
		return self.view(data)
	case "eth_getCode":
		return self.answerCode(params)
	case "eth_sendRawTransaction":
		return self.mine(params[0])
	case "eth_getTransactionReceipt":
		var hash common.Hash
		if err := json.Unmarshal(params[0], &hash); err != nil {
			self.t.Error(err)
		}
		if receipt := self.receipts[hash]; receipt != nil {
			return receipt, nil
		}
		return nil, nil
	}
	self.t.Errorf("unexpected v2 claim rpc %s", method)
	return nil, &claimV2TestRPCError{Code: -32601, Message: "method not found"}
}

func (self *claimV2TestChain) view(data []byte) (any, *claimV2TestRPCError) {
	word := func(index int) uint64 { return new(big.Int).SetBytes(data[4+32*index : 4+32*(index+1)]).Uint64() }
	method, err := claimV2TestABI.MethodById(data[:4])
	if err != nil {
		self.t.Error(err)
		return nil, claimV2TestRevert("unknown selector")
	}
	var out []byte
	switch method.Name {
	case "rootCount":
		out, err = method.Outputs.Pack(big.NewInt(int64(len(self.trees))))
	case "roots":
		if word(1) >= uint64(len(self.trees)) {
			out, err = method.Outputs.Pack(stabi.ISTSettlementVault2Root{Total: new(big.Int)})
			break
		}
		tree := self.trees[word(1)]
		root, total := tree.Root()
		out, err = method.Outputs.Pack(stabi.ISTSettlementVault2Root{Root: root, Total: total.ToBig(), LeafCount: tree.LeafCount(), Epoch: word(1) + 1, LedgerHash: [32]byte{0x1e}})
	case "claimed":
		claimed := self.claimed[word(1)]
		out, err = method.Outputs.Pack(claimed.ToBig())
	case "claim":
		self.calls = append(self.calls, "preflight")
		if self.beforePreflight != nil {
			self.beforePreflight()
		}
		_, amount, refusal := self.execute(data)
		if refusal != nil {
			return nil, refusal
		}
		out, err = method.Outputs.Pack(amount.ToBig())
	default:
		self.t.Errorf("unexpected vault2 view %s", method.Name)
	}
	if err != nil {
		self.t.Error(err)
	}
	if self.afterView != nil {
		self.afterView()
	}
	return hexutil.Encode(out), nil
}

// vault2.claim's checks in the SPEC's order, without moving stake.
func (self *claimV2TestChain) execute(data []byte) (*claimIntentV2, *uint256.Int, *claimV2TestRPCError) {
	intent, err := decodeClaimV2Calldata(data)
	if err != nil {
		return nil, nil, claimV2TestRevert("malformed claim")
	}
	if intent.RootIndex.Uint64() >= uint64(len(self.trees)) {
		return nil, nil, claimV2TestRevert("UnknownRoot")
	}
	tree := self.trees[intent.RootIndex.Uint64()]
	root, total := tree.Root()
	cumulative, _ := uint256.FromBig(intent.Account.Cumulative)
	proof := make([]merkle.LedgerNode, len(intent.Proof))
	for index, node := range intent.Proof {
		proof[index].Hash = node.Hash
		proof[index].Sum.SetFromBig(node.Sum)
	}
	index := intent.Account.Index.Uint64()
	if !merkle.VerifyLedgerProof(root, &total, tree.LeafCount(), index, intent.Account.Coldkey, cumulative, proof) {
		return nil, nil, claimV2TestRevert("InvalidProof")
	}
	claimed := self.claimed[index]
	if !claimed.Lt(cumulative) {
		return nil, nil, claimV2TestRevert("NothingToClaim")
	}
	if self.refuseClaims || self.refuseIndex[index] {
		return nil, nil, claimV2TestRevert("runtime transfer refusal")
	}
	return intent, new(uint256.Int).Sub(cumulative, &claimed), nil
}

func (self *claimV2TestChain) mine(param json.RawMessage) (any, *claimV2TestRPCError) {
	var encoded hexutil.Bytes
	var tx types.Transaction
	if err := json.Unmarshal(param, &encoded); err != nil || tx.UnmarshalBinary(encoded) != nil {
		self.t.Error("v2 claim send is not one signed transaction")
		return nil, &claimV2TestRPCError{Code: -32602, Message: "invalid transaction"}
	}
	from, err := types.Sender(types.LatestSignerForChainID(tx.ChainId()), &tx)
	if err != nil {
		self.t.Error(err)
	}
	self.sends, self.calls = append(self.sends, hexutil.Encode(encoded)), append(self.calls, "send")
	if self.sendFailures > 0 {
		self.sendFailures--
		return nil, &claimV2TestRPCError{Code: -32000, Message: "synthetic send outage"}
	}
	if self.receipts[tx.Hash()] != nil {
		return nil, &claimV2TestRPCError{Code: -32000, Message: "already known"}
	}
	// Nonces are mined in order; a gap waits for its predecessor's rebroadcast.
	if tx.Nonce() < self.nonce {
		return nil, &claimV2TestRPCError{Code: -32000, Message: "nonce too low"}
	}
	if tx.Nonce() > self.nonce {
		return nil, &claimV2TestRPCError{Code: -32000, Message: "nonce too high"}
	}
	if self.beforeMine != nil {
		self.beforeMine()
	}
	self.head++
	self.nonce++
	receipt := &types.Receipt{Type: types.LegacyTxType, Status: types.ReceiptStatusFailed, CumulativeGasUsed: 200_000, GasUsed: 200_000, Logs: []*types.Log{},
		TxHash: tx.Hash(), BlockHash: claimV2TestBlockHash(self.head), BlockNumber: new(big.Int).SetUint64(self.head)}
	if self.batch != nil && *tx.To() == self.batch.address {
		self.mineBatch(&tx, receipt)
		self.receipts[tx.Hash()] = receipt
		return tx.Hash().Hex(), nil
	}
	intent, amount, refusal := self.execute(tx.Data())
	if refusal == nil && !self.revertMined && tx.Gas() >= self.claimGas {
		self.claimed[intent.Account.Index.Uint64()] = *uint256.MustFromBig(intent.Account.Cumulative)
		receipt.Status = types.ReceiptStatusSuccessful
		if !self.dropLogs {
			receipt.Logs = append(receipt.Logs, claimV2TestClaimedLog(*tx.To(), intent, amount, from, receipt))
		}
	}
	self.receipts[tx.Hash()] = receipt
	return tx.Hash().Hex(), nil
}

// The vault's exact Claimed(noId, rootIndex, index, coldkey, cumulative, amount, caller).
func claimV2TestClaimedLog(vault common.Address, intent *claimIntentV2, amount *uint256.Int, caller common.Address, receipt *types.Receipt) *types.Log {
	event := claimV2TestABI.Events["Claimed"]
	data, err := event.Inputs.NonIndexed().Pack(intent.RootIndex, intent.Account.Cumulative, amount.ToBig(), caller)
	if err != nil {
		panic(err)
	}
	return &types.Log{Address: vault, Topics: []common.Hash{event.ID, common.BigToHash(intent.NoID), common.BigToHash(intent.Account.Index), common.Hash(intent.Account.Coldkey)},
		Data: data, BlockNumber: receipt.BlockNumber.Uint64(), TxHash: receipt.TxHash, BlockHash: receipt.BlockHash}
}

// A v2 daemon over the fixture chain, stepped by hand: one snapshot read,
// one merge and one admitted poll per step, at a test-owned clock.
type claimV2TestDaemon struct {
	*claimDaemonV2
	t        *testing.T
	chain    *claimV2TestChain
	ctx      context.Context
	clock    time.Time
	key      *ecdsa.PrivateKey
	strategy *connect.ClientStrategy
}

func writeClaimV2TestConfig(t *testing.T, chain *claimV2TestChain, keyFile, stateDir string, coldkeys ...[32]byte) string {
	t.Helper()
	config := ClaimDaemonV2Config{SchemaVersion: 2, APIURL: chain.server.URL, RPC: []string{chain.server.URL}, ChainID: claimV2TestChainID,
		Vault: strings.ToLower(chain.vault.Hex()), KeyFile: keyFile, StateDir: stateDir, PollSeconds: 5}
	for _, coldkey := range coldkeys {
		address, err := ss58.Encode(coldkey, ss58.BittensorPrefix)
		if err != nil {
			t.Fatal(err)
		}
		config.Coldkeys = append(config.Coldkeys, address)
	}
	raw, err := yaml.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "claim-v2.yml")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeClaimV2TestKey(t *testing.T) (*ecdsa.PrivateKey, string) {
	t.Helper()
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "synthetic-relayer.key")
	if err := os.WriteFile(path, []byte(hex.EncodeToString(crypto.FromECDSA(key))), 0o600); err != nil {
		t.Fatal(err)
	}
	return key, path
}

func newClaimV2TestDaemon(t *testing.T, chain *claimV2TestChain, threshold uint64, coldkeys ...[32]byte) *claimV2TestDaemon {
	t.Helper()
	if len(coldkeys) == 0 {
		coldkeys = [][32]byte{claimV2TestColdkey}
	}
	key, keyFile := writeClaimV2TestKey(t)
	path := writeClaimV2TestConfig(t, chain, keyFile, filepath.Join(t.TempDir(), "claims-v2"), coldkeys...)
	cfg, err := LoadClaimDaemonV2Config(path)
	if err != nil {
		t.Fatal(err)
	}
	strategy := connect.NewClientStrategy(t.Context(), defaultClaimReadStrategySettings())
	t.Cleanup(strategy.Close)
	daemon := &claimV2TestDaemon{t: t, chain: chain, ctx: claimQueueTestContext(t, t.Context(), cfg.StateDir), clock: time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC), key: key, strategy: strategy}
	daemon.open(cfg, &claimAdmission{}, threshold)
	return daemon
}

// Opens the queue as a fresh process would, seeding a fresh admission.
func (self *claimV2TestDaemon) open(cfg *ClaimDaemonV2Config, admission *claimAdmission, threshold uint64) {
	self.t.Helper()
	store, err := newClaimQueueV2Store(self.ctx, cfg.StateDir)
	if err != nil {
		self.t.Fatal(err)
	}
	self.t.Cleanup(func() { _ = store.close() })
	queue, err := store.load()
	if err != nil {
		self.t.Fatal(err)
	}
	if err := admission.seedClaimV2(cfg, queue); err != nil {
		self.t.Fatal(err)
	}
	self.claimDaemonV2 = &claimDaemonV2{cfg: cfg, store: store, queue: queue, admission: admission, threshold: uint256.NewInt(threshold), accounts: map[string]*claimAccountV2{}, now: func() time.Time { return self.clock }}
}

// Closes the queue and opens it again, as a process restart does.
func (self *claimV2TestDaemon) restart() {
	self.t.Helper()
	if err := self.store.close(); err != nil {
		self.t.Fatal(err)
	}
	self.open(self.cfg, &claimAdmission{}, self.threshold.Uint64())
}

func (self *claimV2TestDaemon) step(demand bool) {
	self.t.Helper()
	snapshot, err := readClaimSnapshotV2(self.ctx, self.strategy, self.cfg)
	if err != nil {
		self.t.Fatal(err)
	}
	if err := self.observe(snapshot, demand); err != nil {
		self.t.Fatal(err)
	}
	if err := self.poll(self.ctx); err != nil {
		self.t.Fatal(err)
	}
}

func (self *claimV2TestDaemon) advance(delay time.Duration) { self.clock = self.clock.Add(delay) }

func (self *claimV2TestDaemon) unit(index uint64) *ClaimUnitV2 {
	self.t.Helper()
	unit := self.queue.Units[claimUnitV2Key(self.cfg.Vault, self.chain.noID, index)]
	if unit == nil {
		self.t.Fatalf("v2 claim unit %d is absent", index)
	}
	return unit
}

// The acknowledged on-disk unit, independent of the in-memory queue.
func (self *claimV2TestDaemon) durable(index uint64) ClaimUnitV2 {
	self.t.Helper()
	queue, err := self.store.load()
	if err != nil {
		self.t.Fatal(err)
	}
	unit := queue.Units[claimUnitV2Key(self.cfg.Vault, self.chain.noID, index)]
	if unit == nil {
		self.t.Fatalf("durable v2 claim unit %d is absent", index)
	}
	return *unit
}

// A unit of noId 7 in the v2 queue file a running daemon last published.
func claimV2TestQueued(stateDir string, index uint64) *ClaimUnitV2 {
	var queue ClaimQueueV2
	raw, err := os.ReadFile(filepath.Join(stateDir, "claim-queue.json"))
	if err != nil || json.Unmarshal(raw, &queue) != nil || queue.Schema != ClaimQueueV2Schema {
		return nil
	}
	return queue.Units[claimUnitV2Key(claimV2TestVault, 7, index)]
}

func claimV2TestNonce(t *testing.T, raw string) uint64 {
	t.Helper()
	encoded, err := hexutil.Decode(raw)
	var tx types.Transaction
	if err != nil || tx.UnmarshalBinary(encoded) != nil {
		t.Fatalf("not a signed transaction: %s", raw)
	}
	return tx.Nonce()
}

// Signs a claim of the account's current proof as the relayer would.
func (self *claimV2TestDaemon) sign(account *claimAccountV2, nonce uint64) (string, string) {
	self.t.Helper()
	return claimV2TestSign(self.t, self.key, claimV2TestChainID, common.HexToAddress(self.cfg.Vault), 0, nonce, claimV2Calldata(account))
}

// Any legacy transaction, signed as the relayer signs its claims.
func claimV2TestSign(t *testing.T, key *ecdsa.PrivateKey, chainID int64, to common.Address, value int64, nonce uint64, data []byte) (string, string) {
	t.Helper()
	tx, err := types.SignTx(types.NewTx(&types.LegacyTx{Nonce: nonce, GasPrice: big.NewInt(1), Gas: 240_000, To: &to, Value: big.NewInt(value), Data: data}), types.LatestSignerForChainID(big.NewInt(chainID)), key)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := tx.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	return strings.ToLower(tx.Hash().Hex()), hexutil.Encode(raw)
}

// The account as the endpoint serves it now.
func (self *claimV2TestDaemon) account(index uint64) *claimAccountV2 {
	self.t.Helper()
	snapshot, err := readClaimSnapshotV2(self.ctx, self.strategy, self.cfg)
	if err != nil {
		self.t.Fatal(err)
	}
	account := snapshot[claimUnitV2Key(self.cfg.Vault, self.chain.noID, index)]
	if account == nil {
		self.t.Fatalf("account %d is not served", index)
	}
	return account
}
