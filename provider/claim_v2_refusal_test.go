//go:build linux || darwin

// Each v2 claim daemon refusal (§10.1), triggered by exactly its own cause.
package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/holiman/uint256"
	"gopkg.in/yaml.v3"

	"github.com/urfoundation/sn/merkle"
	"github.com/urfoundation/sn/miner/onchain"
	"github.com/urfoundation/sn/ss58"
	"github.com/urnetwork/connect/internal/durablehead"
)

// A proven account of noId 7 in a tree of the given accounts.
func claimV2TestProven(t *testing.T, index uint64, accounts ...merkle.LedgerAccount) *claimAccountV2 {
	t.Helper()
	tree, err := merkle.BuildLedgerSumTree(t.Context(), accounts)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := tree.Proof(index)
	if err != nil {
		t.Fatal(err)
	}
	return &claimAccountV2{Vault: claimV2TestVault, NoID: 7, Index: index, Coldkey: accounts[index].Coldkey, Cumulative: accounts[index].Cumulative, Proof: proof}
}

func claimV2TestRefused(t *testing.T, name string, err error, want string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("%s: error %v, want %q", name, err, want)
	}
}

// E-5, §10.1 config: one strict YAML document pinning chain and vault; relative
// paths are anchored at the config and the poll defaults to 30 seconds.
func TestClaimV2ConfigRefusals(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "relayer.key"), []byte(strings.Repeat("01", 32)), 0o600); err != nil {
		t.Fatal(err)
	}
	coldkey, err := ss58.Encode(claimV2TestColdkey, ss58.BittensorPrefix)
	if err != nil {
		t.Fatal(err)
	}
	write := func(change func(map[string]any), suffix string) string {
		value := map[string]any{"schema_version": 2, "api_url": "http://operator.example", "rpc": []string{"http://rpc.example"}, "chain_id": 945,
			"vault": claimV2TestVault, "coldkeys": []string{coldkey}, "key_file": "relayer.key", "state_dir": "claims-v2"}
		if change != nil {
			change(value)
		}
		raw, err := yaml.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "claim-v2.yml")
		if err := os.WriteFile(path, append(raw, suffix...), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	cfg, err := LoadClaimDaemonV2Config(write(nil, ""))
	if err != nil || cfg.KeyFile != filepath.Join(dir, "relayer.key") || cfg.StateDir != filepath.Join(dir, "claims-v2") || cfg.PollSeconds != 30 || len(cfg.coldkeys) != 1 || cfg.coldkeys[0] != claimV2TestColdkey {
		t.Fatalf("valid config: %+v %v", cfg, err)
	}
	invalid := "invalid v2 claim daemon configuration"
	for _, test := range []struct {
		name   string
		change func(map[string]any)
		suffix string
		want   string
	}{
		{name: "unknown field", change: func(v map[string]any) { v["release"] = "1.0" }, want: "field release not found"},
		{name: "second document", suffix: "---\n{}\n", want: "must be one YAML document"},
		{name: "schema", change: func(v map[string]any) { v["schema_version"] = 1 }, want: invalid},
		{name: "api", change: func(v map[string]any) { v["api_url"] = "" }, want: invalid},
		{name: "rpc", change: func(v map[string]any) { v["rpc"] = []string{} }, want: invalid},
		{name: "chain", change: func(v map[string]any) { v["chain_id"] = 0 }, want: invalid},
		{name: "fast poll", change: func(v map[string]any) { v["poll_seconds"] = 4 }, want: invalid},
		{name: "slow poll", change: func(v map[string]any) { v["poll_seconds"] = 3601 }, want: invalid},
		{name: "vault case", change: func(v map[string]any) { v["vault"] = "0x00000000000000000000000000000000000A2A2A" }, want: invalid},
		{name: "vault prefix", change: func(v map[string]any) { v["vault"] = strings.TrimPrefix(claimV2TestVault, "0x") }, want: invalid},
		{name: "vault length", change: func(v map[string]any) { v["vault"] = "0x1234" }, want: invalid},
		{name: "no coldkeys", change: func(v map[string]any) { v["coldkeys"] = []string{} }, want: invalid},
		{name: "no key file", change: func(v map[string]any) { v["key_file"] = "" }, want: invalid},
		{name: "no state dir", change: func(v map[string]any) { v["state_dir"] = "" }, want: invalid},
		{name: "coldkey", change: func(v map[string]any) { v["coldkeys"] = []string{coldkey, "not-ss58"} }, want: "claim coldkey 1"},
		{name: "key file", change: func(v map[string]any) { v["key_file"] = "missing.key" }, want: "claim relayer key"},
	} {
		cfg, err := LoadClaimDaemonV2Config(write(test.change, test.suffix))
		if cfg != nil {
			t.Errorf("%s: config accepted", test.name)
		}
		claimV2TestRefused(t, test.name, err, test.want)
	}
	if _, err := LoadClaimDaemonV2Config(filepath.Join(dir, "absent.yml")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("absent config: %v", err)
	}
}

// E-5, §8.3 account body: other vaults and pending-only accounts are skipped, a
// vault in another letter case is the same vault, and anything malformed
// refuses the reply.
func TestClaimV2AccountResponseRefusals(t *testing.T) {
	coldkey := "0x" + hex.EncodeToString(claimV2TestColdkey[:])
	entry := func() map[string]any {
		return map[string]any{"vault": claimV2TestVault, "no_id": 7, "index": 1, "root_index": 0, "cumulative": "5000000000",
			"proof": []map[string]any{{"hash": "0x" + strings.Repeat("11", 32), "sum": "1000000000"}}, "claimed": "0", "pending_cumulative": nil}
	}
	body := func(coldkey string, accounts any) []byte {
		raw, err := json.Marshal(map[string]any{"coldkey": coldkey, "accounts": accounts})
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	upper, pending, foreign := entry(), entry(), entry()
	upper["vault"] = "0x00000000000000000000000000000000000A2A2A"
	pending["root_index"], pending["cumulative"], pending["proof"] = nil, "0", nil
	foreign["vault"] = "0x" + strings.Repeat("99", 20)
	accounts, err := parseClaimAccountsV2(body(coldkey, []any{upper, pending, foreign}), claimV2TestVault, claimV2TestColdkey)
	if err != nil || len(accounts) != 1 || accounts[0].Vault != claimV2TestVault || accounts[0].NoID != 7 || accounts[0].Index != 1 || accounts[0].Coldkey != claimV2TestColdkey ||
		accounts[0].Cumulative.Uint64() != 5_000_000_000 || accounts[0].Proof[0].Sum.Uint64() != 1_000_000_000 || accounts[0].Proof[0].Hash != [32]byte(common.FromHex(strings.Repeat("11", 32))) {
		t.Fatalf("parsed accounts: %+v %v", accounts, err)
	}
	for name, change := range map[string]func(map[string]any){
		"cumulative leading zero": func(v map[string]any) { v["cumulative"] = "05000000000" },
		"cumulative sign":         func(v map[string]any) { v["cumulative"] = "+5000000000" },
		"cumulative empty":        func(v map[string]any) { v["cumulative"] = "" },
		"cumulative negative":     func(v map[string]any) { v["cumulative"] = "-5" },
		"claimed fraction":        func(v map[string]any) { v["claimed"] = "1.5" },
		"proof sum":               func(v map[string]any) { v["proof"].([]map[string]any)[0]["sum"] = "one" },
		"proof hash prefix":       func(v map[string]any) { v["proof"].([]map[string]any)[0]["hash"] = strings.Repeat("11", 32) },
		"proof hash length":       func(v map[string]any) { v["proof"].([]map[string]any)[0]["hash"] = "0x1111" },
	} {
		value := entry()
		change(value)
		_, err := parseClaimAccountsV2(body(coldkey, []any{value}), claimV2TestVault, claimV2TestColdkey)
		claimV2TestRefused(t, name, err, "v2 account 7/1 is malformed")
	}
	_, err = parseClaimAccountsV2([]byte("{"), claimV2TestVault, claimV2TestColdkey)
	claimV2TestRefused(t, "json", err, "unexpected end of JSON input")
	for name, raw := range map[string][]byte{
		"other coldkey": body("0x"+hex.EncodeToString(claimV2TestOther[:]), []any{}),
		"no accounts":   body(coldkey, nil),
	} {
		_, err := parseClaimAccountsV2(raw, claimV2TestVault, claimV2TestColdkey)
		claimV2TestRefused(t, name, err, "names another coldkey or lists no accounts")
	}
}

// E-5, §8.3: a refused or malformed reply for any coldkey fails the snapshot.
func TestClaimV2SnapshotRefusals(t *testing.T) {
	chain := newClaimV2TestChain(t)
	chain.finalize(claimV2TestAccount(claimV2TestOther, 1), claimV2TestAccount(claimV2TestColdkey, 5))
	daemon := newClaimV2TestDaemon(t, chain, ClaimV2DefaultThresholdRao)
	chain.with(func() { chain.accountStatus = 400 })
	_, err := readClaimSnapshotV2(daemon.ctx, daemon.strategy, daemon.cfg)
	claimV2TestRefused(t, "status", err, "synthetic account refusal")
	chain.with(func() {
		chain.accountStatus = 0
		chain.account = func(entry map[string]any) { entry["claimed"] = "x" }
	})
	_, err = readClaimSnapshotV2(daemon.ctx, daemon.strategy, daemon.cfg)
	claimV2TestRefused(t, "malformed", err, "is malformed")
}

// E-5, §10.1 finalized read: another chain, an unreadable chain or finalized head,
// a refused view and a regressed finalized head are refused per endpoint; the
// next endpoint is then tried.
func TestClaimV2FinalizedReadRefusals(t *testing.T) {
	chain := newClaimV2TestChain(t)
	chain.finalize(claimV2TestAccount(claimV2TestOther, 1), claimV2TestAccount(claimV2TestColdkey, 5))
	daemon := newClaimV2TestDaemon(t, chain, ClaimV2DefaultThresholdRao)
	account := daemon.account(1)
	failover := *daemon.cfg
	failover.RPC = []string{"://unparseable", chain.server.URL}
	if claimed, err := readClaimedV2(daemon.ctx, &failover, account); err != nil || !claimed.IsZero() {
		t.Fatalf("failover read: %v %v", claimed, err)
	}
	beyond, behind := uint64(105), uint64(99)
	for _, test := range []struct {
		name  string
		setup func(*ClaimDaemonV2Config)
		want  string
	}{
		{name: "dial", setup: func(cfg *ClaimDaemonV2Config) { cfg.RPC = []string{"://unparseable"} }, want: "missing protocol scheme"},
		{name: "chain read", setup: func(*ClaimDaemonV2Config) { chain.failMethod = "eth_chainId" }, want: "synthetic eth_chainId refusal"},
		{name: "other chain", setup: func(cfg *ClaimDaemonV2Config) { cfg.ChainID = 946 }, want: "chain id 945, want 946"},
		{name: "finalized head", setup: func(*ClaimDaemonV2Config) { chain.finalized = &beyond }, want: "read claim EVM canonical block"},
		{name: "view", setup: func(*ClaimDaemonV2Config) { chain.failMethod = "eth_call" }, want: "synthetic eth_call refusal"},
		{name: "regressed head", setup: func(*ClaimDaemonV2Config) { chain.afterView = func() { chain.finalized = &behind } }, want: "regressed below the observed state"},
	} {
		cfg := *daemon.cfg
		chain.with(func() { test.setup(&cfg) })
		_, err := readClaimedV2(daemon.ctx, &cfg, account)
		claimV2TestRefused(t, test.name, err, test.want)
		chain.with(func() { chain.failMethod, chain.finalized, chain.afterView = "", nil, nil })
	}
}

// E-5, §10.1 custody: retained bytes reconcile only as this unit's relayer-signed
// v2 claim on the configured chain and vault, in canonical calldata.
func TestClaimV2AuthenticationRefusals(t *testing.T) {
	key, keyFile := writeClaimV2TestKey(t)
	other, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ClaimDaemonV2Config{ChainID: claimV2TestChainID, Vault: claimV2TestVault, KeyFile: keyFile}
	account := claimV2TestProven(t, 1, claimV2TestAccount(claimV2TestOther, 1), claimV2TestAccount(claimV2TestColdkey, 5))
	vault, data := common.HexToAddress(claimV2TestVault), claimV2Calldata(account)
	unit := func(hash, raw string) *ClaimUnitV2 {
		return &ClaimUnitV2{Vault: claimV2TestVault, NoID: 7, Index: 1, Status: "uncertain", TxHash: hash, RawTxHex: raw}
	}
	hash, raw := claimV2TestSign(t, key, claimV2TestChainID, vault, 0, 3, data)
	tx, intent, from, err := authenticateSignedClaimV2(cfg, unit(hash, raw))
	if err != nil || tx.Nonce() != 3 || from != crypto.PubkeyToAddress(key.PublicKey) || intent.NoID.Uint64() != 7 || intent.RootIndex.Sign() != 0 || intent.Account.Index.Uint64() != 1 ||
		intent.Account.Coldkey != claimV2TestColdkey || intent.Account.Cumulative.Uint64() != 5_000_000_000 || len(intent.Proof) != 1 {
		t.Fatalf("authentic claim: %+v %v", intent, err)
	}
	if back := intent.account(claimV2TestVault); back.NoID != 7 || back.Index != 1 || back.Coldkey != claimV2TestColdkey || !back.Cumulative.Eq(&account.Cumulative) || back.Proof[0] != account.Proof[0] {
		t.Fatalf("intent account: %+v", back)
	}
	renamed, reindexed, unkeyed := unit(hash, raw), unit(hash, raw), *cfg
	renamed.NoID, reindexed.Index, unkeyed.KeyFile = 8, 2, filepath.Join(t.TempDir(), "absent.key")
	for _, test := range []struct {
		name string
		cfg  *ClaimDaemonV2Config
		unit *ClaimUnitV2
		want string
	}{
		{name: "bytes", unit: unit(hash, "0xzz"), want: "bytes do not decode to their retained hash"},
		{name: "hash", unit: unit("0x"+strings.Repeat("00", 32), raw), want: "bytes do not decode to their retained hash"},
		{name: "chain", unit: unit(claimV2TestSign(t, key, 946, vault, 0, 3, data)), want: "has another chain, target or value"},
		{name: "target", unit: unit(claimV2TestSign(t, key, claimV2TestChainID, common.HexToAddress("0x99"), 0, 3, data)), want: "has another chain, target or value"},
		{name: "value", unit: unit(claimV2TestSign(t, key, claimV2TestChainID, vault, 1, 3, data)), want: "has another chain, target or value"},
		{name: "selector", unit: unit(claimV2TestSign(t, key, claimV2TestChainID, vault, 0, 3, []byte{1, 2, 3, 4})), want: "calldata is not a v2 vault claim"},
		{name: "short", unit: unit(claimV2TestSign(t, key, claimV2TestChainID, vault, 0, 3, data[:3])), want: "calldata is not a v2 vault claim"},
		{name: "truncated", unit: unit(claimV2TestSign(t, key, claimV2TestChainID, vault, 0, 3, data[:100])), want: "abi:"},
		{name: "trailing", unit: unit(claimV2TestSign(t, key, claimV2TestChainID, vault, 0, 3, append(append([]byte(nil), data...), 0))), want: "v2 claim calldata is not canonical"},
		{name: "pool", unit: renamed, want: "calldata names another account"},
		{name: "index", unit: reindexed, want: "calldata names another account"},
		{name: "signer", unit: unit(claimV2TestSign(t, other, claimV2TestChainID, vault, 0, 3, data)), want: "signer is not the configured relayer"},
		{name: "key file", cfg: &unkeyed, unit: unit(hash, raw), want: "signer is not the configured relayer"},
	} {
		if test.cfg == nil {
			test.cfg = cfg
		}
		_, _, _, err := authenticateSignedClaimV2(test.cfg, test.unit)
		claimV2TestRefused(t, test.name, err, test.want)
	}
}

// E-5, §10.1 settlement: only exactly one canonical Claimed event that matches
// the signed intent settles a claim. Foreign logs are ignored.
func TestClaimV2ReceiptRefusals(t *testing.T) {
	key, keyFile := writeClaimV2TestKey(t)
	cfg := &ClaimDaemonV2Config{ChainID: claimV2TestChainID, Vault: claimV2TestVault, KeyFile: keyFile}
	account := claimV2TestProven(t, 1, claimV2TestAccount(claimV2TestOther, 1), claimV2TestAccount(claimV2TestColdkey, 5))
	vault := common.HexToAddress(claimV2TestVault)
	hash, raw := claimV2TestSign(t, key, claimV2TestChainID, vault, 0, 3, claimV2Calldata(account))
	tx, intent, from, err := authenticateSignedClaimV2(cfg, &ClaimUnitV2{Vault: claimV2TestVault, NoID: 7, Index: 1, TxHash: hash, RawTxHex: raw})
	if err != nil {
		t.Fatal(err)
	}
	amount := uint256.NewInt(5_000_000_000)
	receipt := func() *types.Receipt {
		receipt := &types.Receipt{Status: types.ReceiptStatusSuccessful, TxHash: tx.Hash(), BlockHash: claimV2TestBlockHash(101), BlockNumber: big.NewInt(101)}
		receipt.Logs = []*types.Log{claimV2TestClaimedLog(vault, intent, amount, from, receipt)}
		return receipt
	}
	pack := func(rootIndex, cumulative, amount int64, caller common.Address) []byte {
		data, err := claimV2TestABI.Events["Claimed"].Inputs.NonIndexed().Pack(big.NewInt(rootIndex), big.NewInt(cumulative), big.NewInt(amount), caller)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	event, err := verifySignedClaimV2Receipt(tx, intent, from, receipt())
	if err != nil || event.Cumulative.Uint64() != 5_000_000_000 || event.Amount.Uint64() != 5_000_000_000 || event.Caller != from || event.RootIndex.Sign() != 0 {
		t.Fatalf("matching event: %+v %v", event, err)
	}
	for name, mutate := range map[string]func(*types.Receipt){
		"foreign contract": func(receipt *types.Receipt) { receipt.Logs[0].Address = common.HexToAddress("0x99") },
		"other event":      func(receipt *types.Receipt) { receipt.Logs[0].Topics[0] = common.Hash{0xee} },
		"no logs":          func(receipt *types.Receipt) { receipt.Logs = nil },
	} {
		value := receipt()
		mutate(value)
		_, err := verifySignedClaimV2Receipt(tx, intent, from, value)
		claimV2TestRefused(t, name, err, "exact signed v2 claim has no Claimed event")
	}
	other := common.HexToAddress("0x77")
	for name, mutate := range map[string]func(*types.Receipt){
		"removed":           func(receipt *types.Receipt) { receipt.Logs[0].Removed = true },
		"transaction":       func(receipt *types.Receipt) { receipt.Logs[0].TxHash = common.Hash{0x01} },
		"block hash":        func(receipt *types.Receipt) { receipt.Logs[0].BlockHash = common.Hash{0x01} },
		"block number":      func(receipt *types.Receipt) { receipt.Logs[0].BlockNumber++ },
		"transaction index": func(receipt *types.Receipt) { receipt.Logs[0].TxIndex = 1 },
		"pool":              func(receipt *types.Receipt) { receipt.Logs[0].Topics[1] = common.BigToHash(big.NewInt(8)) },
		"index":             func(receipt *types.Receipt) { receipt.Logs[0].Topics[2] = common.BigToHash(big.NewInt(2)) },
		"coldkey":           func(receipt *types.Receipt) { receipt.Logs[0].Topics[3] = common.Hash{0x99} },
		"root index":        func(receipt *types.Receipt) { receipt.Logs[0].Data = pack(1, 5_000_000_000, 5_000_000_000, from) },
		"cumulative":        func(receipt *types.Receipt) { receipt.Logs[0].Data = pack(0, 6_000_000_000, 5_000_000_000, from) },
		"amount":            func(receipt *types.Receipt) { receipt.Logs[0].Data = pack(0, 5_000_000_000, 0, from) },
		"caller":            func(receipt *types.Receipt) { receipt.Logs[0].Data = pack(0, 5_000_000_000, 5_000_000_000, other) },
		"duplicate":         func(receipt *types.Receipt) { receipt.Logs = append(receipt.Logs, receipt.Logs[0]) },
	} {
		value := receipt()
		mutate(value)
		_, err := verifySignedClaimV2Receipt(tx, intent, from, value)
		claimV2TestRefused(t, name, err, "canonical v2 Claimed event differs from the exact signed intent")
	}
}

// E-5, §10.1 state: a v2 queue loads only its own schema with well-formed units.
func TestClaimV2QueueRefusesMalformedState(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "claims-v2")
	store, err := newClaimQueueV2Store(claimQueueTestContext(t, t.Context(), dir), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.close()
	key := claimUnitV2Key(claimV2TestVault, 7, 1)
	unit := func(fields string) string {
		return fmt.Sprintf(`{"schema":%q,"units":{%q:{"vault":%q,"no_id":7,"index":1,%s}}}`, ClaimQueueV2Schema, key, claimV2TestVault, fields)
	}
	for _, test := range []struct{ name, raw, want string }{
		{name: "json", raw: "{", want: "unexpected end of JSON input"},
		{name: "v1 schema", raw: `{"schema":"urnetwork-provider-claim-queue-v1","last_discovered":1,"entries":{}}`, want: "invalid v2 claim queue schema"},
		{name: "no units", raw: fmt.Sprintf(`{"schema":%q}`, ClaimQueueV2Schema), want: "invalid v2 claim queue schema"},
		{name: "nil unit", raw: fmt.Sprintf(`{"schema":%q,"units":{%q:null}}`, ClaimQueueV2Schema, key), want: "is malformed"},
		{name: "key", raw: strings.Replace(unit(`"status":"idle"`), key, key+"0", 1), want: "is malformed"},
		{name: "status", raw: unit(`"status":"done"`), want: "is malformed"},
		{name: "uncertain unsigned", raw: unit(`"status":"uncertain"`), want: "is malformed"},
		{name: "idle signed", raw: unit(`"status":"idle","tx_hash":"0x01"`), want: "is malformed"},
		{name: "open signed", raw: unit(`"status":"open","raw_tx_hex":"0x01"`), want: "is malformed"},
		{name: "deadline", raw: unit(`"status":"open","next_retry_at":"tomorrow"`), want: "invalid retry deadline"},
	} {
		if err := store.owner.head.Publish([]byte(test.raw), nil); err != nil {
			t.Fatal(err)
		}
		_, err := store.load()
		claimV2TestRefused(t, test.name, err, test.want)
	}
}

// E-5, §10.1 custody: an uncertain publication refuses later saves until a new
// owner reconciles it; a closed store refuses everything; a lost queue file
// or malformed acknowledged bytes refuse loads and broadcast custody, which
// also needs the exact bytes; a queue needs a declared durable volume.
func TestClaimV2StoreRefusals(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "claims-v2")
	ctx := claimQueueTestContext(t, t.Context(), dir)
	if _, err := newClaimQueueV2Store(t.Context(), dir); err == nil {
		t.Fatal("v2 queue opened without a durable-volume declaration")
	}
	store, err := newClaimQueueV2Store(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	queue, err := store.load()
	if err != nil {
		t.Fatal(err)
	}
	key, _ := writeClaimV2TestKey(t)
	_, raw := claimV2TestSign(t, key, claimV2TestChainID, common.HexToAddress(claimV2TestVault), 0, 0, nil)
	var tx types.Transaction
	if err := tx.UnmarshalBinary(common.FromHex(raw)); err != nil {
		t.Fatal(err)
	}
	claimV2TestRefused(t, "absent bytes", store.requireSigned(ctx, &tx), "absent from acknowledged custody")
	store.owner.afterSync = func(*os.File) error { return syscall.EIO }
	queue.Units["k"] = &ClaimUnitV2{}
	for attempt := range 2 {
		if err := store.save(queue); !errors.Is(err, durablehead.ErrUncertain) {
			t.Fatalf("uncertain save %d: %v", attempt, err)
		}
	}
	if err := store.close(); err != nil {
		t.Fatal(err)
	}
	for name, err := range map[string]error{"save": store.save(queue), "signed": store.requireSigned(ctx, &tx)} {
		claimV2TestRefused(t, name, err, "no durable owner")
	}
	_, err = store.load()
	claimV2TestRefused(t, "load", err, "no durable owner")
	store, err = newClaimQueueV2Store(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.close()
	if err := store.save(&ClaimQueueV2{Schema: ClaimQueueV2Schema, Units: map[string]*ClaimUnitV2{}}); err != nil {
		t.Fatalf("a new owner did not reconcile the uncertain publication: %v", err)
	}
	if err := store.owner.head.Publish([]byte("{"), nil); err != nil {
		t.Fatal(err)
	}
	claimV2TestRefused(t, "malformed custody", store.requireSigned(ctx, &tx), "unexpected end of JSON input")
	if err := os.Remove(filepath.Join(dir, "claim-queue.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.load(); err == nil {
		t.Fatal("a v2 queue loaded after its file was lost")
	}
}

// E-5, §10.1 nonce admission: startup seeds the shared floor from every retained
// v2 signature, and forged bytes or a nonce with no successor stop signing.
func TestClaimV2SeedRefusals(t *testing.T) {
	key, keyFile := writeClaimV2TestKey(t)
	cfg := &ClaimDaemonV2Config{ChainID: claimV2TestChainID, Vault: claimV2TestVault, KeyFile: keyFile}
	data := claimV2Calldata(claimV2TestProven(t, 1, claimV2TestAccount(claimV2TestOther, 1), claimV2TestAccount(claimV2TestColdkey, 5)))
	queue := func(hash, raw string) *ClaimQueueV2 {
		key := claimUnitV2Key(claimV2TestVault, 7, 1)
		return &ClaimQueueV2{Schema: ClaimQueueV2Schema, Units: map[string]*ClaimUnitV2{key: {Vault: claimV2TestVault, NoID: 7, Index: 1, Status: "uncertain", TxHash: hash, RawTxHex: raw}, "idle": {Status: "idle"}}}
	}
	admission := &claimAdmission{}
	if err := admission.seedClaimV2(cfg, queue(claimV2TestSign(t, key, claimV2TestChainID, common.HexToAddress(claimV2TestVault), 0, 5, data))); err != nil || admission.nonceMinimum() != 6 {
		t.Fatalf("seeded floor %d: %v", admission.nonceMinimum(), err)
	}
	for _, test := range []struct {
		name  string
		queue *ClaimQueueV2
		want  string
	}{
		{name: "forged", queue: queue("0x"+strings.Repeat("00", 32), "0x00"), want: "bytes do not decode to their retained hash"},
		{name: "no successor", queue: queue(claimV2TestSign(t, key, claimV2TestChainID, common.HexToAddress(claimV2TestVault), 0, math.MaxUint64, data)), want: "signed nonce has no successor"},
	} {
		admission := &claimAdmission{}
		err := admission.seedClaimV2(cfg, test.queue)
		var custody *claimNonceSafetyError
		if !errors.As(err, &custody) || !strings.Contains(err.Error(), "v2 claim unit "+claimUnitV2Key(claimV2TestVault, 7, 1)) {
			t.Errorf("%s: seed error %v", test.name, err)
		}
		claimV2TestRefused(t, test.name, err, test.want)
		if _, _, err := admission.acquire(t.Context(), "other", []claimPollCandidate{{entry: &ClaimQueueEntry{}}}); !errors.As(err, &custody) {
			t.Errorf("%s: admission still signs: %v", test.name, err)
		}
	}
}

// E-5, §10.1 one operation at a time: a poll that is not admitted, or whose
// context ended, changes nothing; a nonce custody failure stops the daemon.
func TestClaimV2PollAdmission(t *testing.T) {
	chain := newClaimV2TestChain(t)
	chain.finalize(claimV2TestAccount(claimV2TestOther, 1), claimV2TestAccount(claimV2TestColdkey, 5))
	daemon := newClaimV2TestDaemon(t, chain, ClaimV2DefaultThresholdRao)
	snapshot, err := readClaimSnapshotV2(daemon.ctx, daemon.strategy, daemon.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := daemon.observe(snapshot, false); err != nil {
		t.Fatal(err)
	}
	if _, admitted, err := daemon.admission.acquire(daemon.ctx, "other", []claimPollCandidate{{entry: &ClaimQueueEntry{Epoch: math.MaxInt64}}}); err != nil || !admitted {
		t.Fatalf("sibling admission: %v", err)
	}
	if err := daemon.poll(daemon.ctx); err != nil || len(chain.sent()) != 0 || daemon.unit(1).Status != "open" {
		t.Fatalf("unadmitted poll acted: %v", err)
	}
	daemon.admission.release("other")
	canceled, cancel := context.WithCancel(daemon.ctx)
	cancel()
	if err := daemon.poll(canceled); err != nil || len(chain.sent()) != 0 {
		t.Fatalf("canceled poll acted: %v", err)
	}
	daemon.admission.chainId = big.NewInt(946)
	err = daemon.poll(daemon.ctx)
	var custody *claimNonceSafetyError
	if !errors.As(err, &custody) || len(chain.sent()) != 0 {
		t.Fatalf("chain custody failure did not stop the daemon: %v", err)
	}
	if unit := daemon.durable(1); unit.Status != "open" || unit.Attempts != 1 || !strings.Contains(unit.LastError, "different chain") {
		t.Fatalf("custody failure lost the unit: %+v", unit)
	}
	daemon.advance(time.Hour)
	if err := daemon.poll(daemon.ctx); !errors.As(err, &custody) {
		t.Fatalf("stopped admission admitted again: %v", err)
	}
}

// E-5, §10.1 submission and settlement refusals: an unreadable relayer key backs
// off unsigned; a lost acknowledgement of prepared bytes still reserves their
// nonce; unauthenticated bytes or a receipt without the event stay in flight.
func TestClaimV2SubmitAndSettleRefusals(t *testing.T) {
	t.Run("relayer key", func(t *testing.T) {
		chain := newClaimV2TestChain(t)
		chain.finalize(claimV2TestAccount(claimV2TestOther, 1), claimV2TestAccount(claimV2TestColdkey, 5))
		daemon := newClaimV2TestDaemon(t, chain, ClaimV2DefaultThresholdRao)
		if err := os.WriteFile(daemon.cfg.KeyFile, []byte("not a key"), 0o600); err != nil {
			t.Fatal(err)
		}
		daemon.step(false)
		if unit := daemon.durable(1); len(chain.sent()) != 0 || unit.Status != "open" || !strings.Contains(unit.LastError, "key_file") || unit.NextRetryAt != daemon.clock.Add(claimRetry(1)).Format(time.RFC3339Nano) {
			t.Fatalf("unreadable relayer key: %+v", unit)
		}
	})
	t.Run("lost prepared acknowledgement", func(t *testing.T) {
		chain := newClaimV2TestChain(t)
		chain.finalize(claimV2TestAccount(claimV2TestOther, 1), claimV2TestAccount(claimV2TestColdkey, 5))
		daemon := newClaimV2TestDaemon(t, chain, ClaimV2DefaultThresholdRao)
		account := daemon.account(1)
		daemon.queue.merge(map[string]*claimAccountV2{claimUnitV2Key(daemon.cfg.Vault, 7, 1): account}, daemon.threshold, false, daemon.stamp())
		if err := daemon.store.save(daemon.queue); err != nil {
			t.Fatal(err)
		}
		daemon.store.owner.afterSync = func(*os.File) error { return syscall.EIO }
		unit := daemon.unit(1)
		err := daemon.submit(daemon.ctx, []*ClaimUnitV2{unit}, common.HexToAddress(daemon.cfg.Vault), claimV2Calldata(account), 0)
		if !errors.Is(err, durablehead.ErrUncertain) || len(chain.sent()) != 0 || daemon.admission.nonceMinimum() != 1 || unit.signed() {
			t.Fatalf("uncertain prepared bytes lost nonce custody: err=%v floor=%d", err, daemon.admission.nonceMinimum())
		}
	})
	t.Run("unauthenticated bytes", func(t *testing.T) {
		chain := newClaimV2TestChain(t)
		chain.finalize(claimV2TestAccount(claimV2TestOther, 1), claimV2TestAccount(claimV2TestColdkey, 5))
		daemon := newClaimV2TestDaemon(t, chain, ClaimV2DefaultThresholdRao)
		chain.with(func() { chain.sendFailures = 1 })
		daemon.step(false)
		unit := daemon.unit(1)
		unit.RawTxHex = unit.RawTxHex + "00"
		daemon.advance(time.Minute)
		daemon.step(false)
		if unit := daemon.durable(1); unit.Status != "uncertain" || !strings.Contains(unit.LastError, "signed identity cannot be authenticated") || len(chain.sent()) != 1 {
			t.Fatalf("unauthenticated bytes: %+v", unit)
		}
	})
	t.Run("receipt without event", func(t *testing.T) {
		chain := newClaimV2TestChain(t)
		chain.finalize(claimV2TestAccount(claimV2TestOther, 1), claimV2TestAccount(claimV2TestColdkey, 5))
		daemon := newClaimV2TestDaemon(t, chain, ClaimV2DefaultThresholdRao)
		chain.with(func() { chain.dropLogs = true })
		daemon.step(false)
		if unit := daemon.durable(1); unit.Status != "uncertain" || unit.RawTxHex == "" || !strings.Contains(unit.LastError, "canonical receipt does not prove this claim succeeded") {
			t.Fatalf("eventless receipt settled: %+v", unit)
		}
	})
}

// E-5, §10.1: a failed submitting checkpoint stops the operation before signing;
// a failed startup checkpoint or a custody failure stops the daemon, which a
// shutdown never reports as a failure.
func TestClaimV2CheckpointRefusals(t *testing.T) {
	chain := newClaimV2TestChain(t)
	chain.finalize(claimV2TestAccount(claimV2TestOther, 1), claimV2TestAccount(claimV2TestColdkey, 5))
	daemon := newClaimV2TestDaemon(t, chain, ClaimV2DefaultThresholdRao)
	snapshot, err := readClaimSnapshotV2(daemon.ctx, daemon.strategy, daemon.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := daemon.observe(snapshot, false); err != nil {
		t.Fatal(err)
	}
	daemon.store.owner.afterSync = func(*os.File) error { return syscall.EIO }
	if err := daemon.poll(daemon.ctx); !errors.Is(err, durablehead.ErrUncertain) || len(chain.sent()) != 0 {
		t.Fatalf("failed submitting checkpoint: %v sends=%d", err, len(chain.sent()))
	}
	daemon.restart()
	stopped := &claimAdmission{}
	stopped.failNonce(errors.New("synthetic custody loss"))
	claimV2TestRefused(t, "custody", runClaimDaemonV2WithStore(daemon.ctx, daemon.cfg, daemon.store, stopped, ClaimV2DefaultThresholdRao, false), "synthetic custody loss")
	canceled, cancel := context.WithCancel(daemon.ctx)
	cancel()
	if err := runClaimDaemonV2WithStore(canceled, daemon.cfg, daemon.store, stopped, ClaimV2DefaultThresholdRao, false); err != nil {
		t.Fatalf("shutdown reported as a failure: %v", err)
	}
	daemon.store.owner.afterSync = func(*os.File) error { return syscall.EIO }
	err = runClaimDaemonV2WithStore(daemon.ctx, daemon.cfg, daemon.store, &claimAdmission{}, ClaimV2DefaultThresholdRao, false)
	if !errors.Is(err, durablehead.ErrUncertain) || len(chain.sent()) != 0 {
		t.Fatalf("failed startup checkpoint: %v", err)
	}
}

// E-5, the orchestrator's runtime-475 measurement: the staking precompile needs a
// far higher gas limit than a claim uses. The limit comes from eth_estimateGas
// after a successful eth_call preflight, with v1's 20% headroom; a refused
// preflight is neither estimated nor sent. Runtime 478's estimate follows
// vault2's own staking-hotkey list and is far smaller, and a claim is never
// signed below onchain.ClaimBatchGasPerClaim.
func TestClaimV2GasLimitFollowsTheEstimate(t *testing.T) {
	for _, test := range []struct{ estimate, want uint64 }{
		{2_400_000, 2_400_000 + 2_400_000/5},
		{206_967, onchain.ClaimBatchGasPerClaim},
	} {
		chain := newClaimV2TestChain(t)
		chain.finalize(claimV2TestAccount(claimV2TestOther, 1), claimV2TestAccount(claimV2TestColdkey, 5))
		daemon := newClaimV2TestDaemon(t, chain, ClaimV2DefaultThresholdRao)
		chain.with(func() { chain.refuseClaims, chain.estimate = true, test.estimate })
		daemon.step(false)
		chain.with(func() {
			if len(chain.calls) != 1 || chain.calls[0] != "preflight" {
				t.Errorf("a refused preflight went further: %v", chain.calls)
			}
			chain.refuseClaims, chain.calls = false, nil
		})
		daemon.advance(claimRetry(1))
		daemon.step(false)
		sends := chain.sent()
		var tx types.Transaction
		if len(sends) != 1 || tx.UnmarshalBinary(common.FromHex(sends[0])) != nil || tx.Gas() != test.want {
			t.Fatalf("claim gas limit at an estimate of %d: sends=%d gas=%d", test.estimate, len(sends), tx.Gas())
		}
		chain.with(func() {
			if len(chain.calls) != 3 || chain.calls[0] != "preflight" || chain.calls[1] != "estimate" || chain.calls[2] != "send" {
				t.Errorf("claim submission order: %v", chain.calls)
			}
		})
	}
}

// E-5, §10.1: a signed claim whose broadcast was lost still pays after third
// parties fill vault2's staking-hotkey list. Runtime 478 then needs 1,273,292
// gas of a claim it estimated at 206,967 (R-3, R-6). The rebroadcast preflights
// the exact bytes at their own gas limit: at the estimate with 20% that call
// runs out of gas, and such bytes would never be sent again; signed with
// onchain.ClaimBatchGasPerClaim, they pass, are sent and pay.
func TestClaimV2SignedClaimOutlastsAGrownStakingHotkeyList(t *testing.T) {
	chain := newClaimV2TestChain(t)
	chain.finalize(claimV2TestAccount(claimV2TestOther, 1), claimV2TestAccount(claimV2TestColdkey, 5))
	daemon := newClaimV2TestDaemon(t, chain, ClaimV2DefaultThresholdRao)
	chain.with(func() { chain.estimate, chain.sendFailures = 206_967, 1 })
	daemon.step(false)
	lost := daemon.durable(1)
	var tx types.Transaction
	if lost.Status != "uncertain" || tx.UnmarshalBinary(common.FromHex(lost.RawTxHex)) != nil || tx.Gas() != onchain.ClaimBatchGasPerClaim {
		t.Fatalf("lost broadcast: gas %d, unit %+v", tx.Gas(), lost)
	}
	chain.with(func() { chain.claimGas = 1_273_292 })
	client, err := ethclient.Dial(chain.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	for gas, paid := range map[uint64]bool{206_967 + 206_967/5: false, tx.Gas(): true} {
		if _, err := client.CallContract(t.Context(), ethereum.CallMsg{To: tx.To(), Gas: gas, Data: tx.Data()}, nil); (err == nil) != paid {
			t.Fatalf("the claim's preflight with %d gas: %v", gas, err)
		}
	}
	for range 2 {
		daemon.advance(time.Minute)
		daemon.step(false)
	}
	unit, claimed, sends := daemon.durable(1), chain.claimedOf(1), chain.sent()
	if unit.Status != "idle" || unit.Claimed != claimV2TestDec(5) || claimed.Dec() != claimV2TestDec(5) || len(sends) != 2 || sends[1] != lost.RawTxHex {
		t.Fatalf("the signed claim after the list grew: claimed=%s sends=%d unit=%+v", claimed.Dec(), len(sends), unit)
	}
}

// E-5, §10.1 loop: a snapshot is merged and claimed, a tick polls, cancellation
// stops cleanly; a failing poll or save stops the loop.
func TestClaimV2RunLoop(t *testing.T) {
	chain := newClaimV2TestChain(t)
	chain.finalize(claimV2TestAccount(claimV2TestOther, 1), claimV2TestAccount(claimV2TestColdkey, 1))
	daemon := newClaimV2TestDaemon(t, chain, ClaimV2DefaultThresholdRao)
	snapshot, err := readClaimSnapshotV2(daemon.ctx, daemon.strategy, daemon.cfg)
	if err != nil {
		t.Fatal(err)
	}
	snapshots, polls := make(chan map[string]*claimAccountV2, 1), make(chan time.Time)
	ctx, cancel := context.WithCancel(daemon.ctx)
	done := make(chan error, 1)
	go func() { done <- daemon.run(ctx, snapshots, polls, true) }()
	snapshots <- snapshot
	for deadline := time.Now().Add(time.Minute); ; time.Sleep(10 * time.Millisecond) {
		if claimed := chain.claimedOf(1); claimed.Dec() == claimV2TestDec(1) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the demanded claim was not made")
		}
	}
	polls <- time.Now()
	snapshots <- snapshot
	polls <- time.Now()
	cancel()
	if err := <-done; err != nil || len(chain.sent()) != 1 || daemon.unit(1).Status != "idle" {
		t.Fatalf("loop: %v sends=%d", err, len(chain.sent()))
	}
	if err := daemon.store.close(); err != nil {
		t.Fatal(err)
	}
	// The demand changes the unit, so the merge must be saved.
	snapshots <- snapshot
	err = daemon.run(daemon.ctx, snapshots, polls, true)
	claimV2TestRefused(t, "closed store", err, "no durable owner")
	daemon.admission.failNonce(errors.New("synthetic custody loss"))
	err = daemon.run(daemon.ctx, snapshots, polls, false)
	claimV2TestRefused(t, "custody", err, "synthetic custody loss")
}

// E-5, §10.1 loop: a snapshot that arrives while the loop waits is merged by
// the wait itself, and its failing save stops the loop.
func TestClaimV2RunLoopStopsOnASaveInTheWait(t *testing.T) {
	chain := newClaimV2TestChain(t)
	chain.finalize(claimV2TestAccount(claimV2TestOther, 1), claimV2TestAccount(claimV2TestColdkey, 1))
	daemon := newClaimV2TestDaemon(t, chain, ClaimV2DefaultThresholdRao)
	snapshot, err := readClaimSnapshotV2(daemon.ctx, daemon.strategy, daemon.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := daemon.store.close(); err != nil {
		t.Fatal(err)
	}
	// The poll reads the clock after the loop's priority read, so the snapshot
	// it delivers can only be merged by the wait.
	snapshots, clock, delivered := make(chan map[string]*claimAccountV2, 1), daemon.now, false
	daemon.now = func() time.Time {
		if !delivered {
			delivered = true
			snapshots <- snapshot
		}
		return clock()
	}
	err = daemon.run(daemon.ctx, snapshots, make(chan time.Time), true)
	claimV2TestRefused(t, "closed store", err, "no durable owner")
	if !delivered || len(snapshots) != 0 {
		t.Fatal("the snapshot delivered during the poll was not merged by the wait")
	}
}

// E-5, §10.1 public entry: a v2-only daemon claims on demand from its own queue
// and stops cleanly; each startup refusal precedes any signature.
func TestClaimV2PublicEntrypoint(t *testing.T) {
	chain := newClaimV2TestChain(t)
	chain.finalize(claimV2TestAccount(claimV2TestOther, 1), claimV2TestAccount(claimV2TestColdkey, 1))
	_, keyFile := writeClaimV2TestKey(t)
	stateDir := filepath.Join(t.TempDir(), "claims-v2")
	path := writeClaimV2TestConfig(t, chain, keyFile, stateDir, claimV2TestColdkey)
	ctx, cancel := context.WithTimeout(claimQueueTestContext(t, t.Context(), stateDir), time.Minute)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- RunClaimDaemonV2(ctx, path, "", ClaimV2DefaultThresholdRao, true) }()
	for {
		if unit := claimV2TestQueued(stateDir, 1); unit != nil && unit.Claimed == claimV2TestDec(1) {
			break
		}
		if ctx.Err() != nil {
			t.Fatal("the v2 daemon did not claim on demand")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil || len(chain.sent()) != 1 {
		t.Fatalf("v2 daemon run: %v", err)
	}
}

// E-5, §10.1 startup refusals of the public entry and the command, each before
// any queue is opened for signing.
func TestClaimV2StartupRefusals(t *testing.T) {
	chain := newClaimV2TestChain(t)
	_, keyFile := writeClaimV2TestKey(t)
	root := t.TempDir()
	v2Dir, v1Dir, busyDir := filepath.Join(root, "claims-v2"), filepath.Join(root, "claims-v1"), filepath.Join(root, "busy")
	path := writeClaimV2TestConfig(t, chain, keyFile, v2Dir, claimV2TestColdkey)
	writeV1 := func(keyFile, stateDir string) string {
		raw, err := yaml.Marshal(ClaimDaemonConfig{SchemaVersion: 1, Release: "1.0", APIURL: "http://operator.example", RPC: []string{"http://rpc.example"}, KeyFile: keyFile, StateDir: stateDir, PollSeconds: 5, LookbackEpochs: 2})
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "claim.yml")
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	_, otherKey := writeClaimV2TestKey(t)
	ctx := claimQueueTestContext(t, t.Context(), v2Dir, v1Dir, busyDir)
	busy, err := newClaimQueueStore(busyDir, ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer busy.close()
	for _, test := range []struct {
		name, config, v1 string
		threshold        uint64
		ctx              context.Context
		want             string
	}{
		{name: "threshold", config: path, threshold: ClaimV2TransferFloorRao - 1, want: "below the 15200000 rao transfer floor"},
		{name: "declaration", config: path, ctx: t.Context(), want: "durable-volume declaration"},
		{name: "config", config: filepath.Join(root, "absent.yml"), want: "no such file"},
		{name: "v1 config", config: path, v1: filepath.Join(root, "absent-v1.yml"), want: "no such file"},
		{name: "relayer key", config: path, v1: writeV1(otherKey, v1Dir), want: "must share one relayer key"},
		{name: "v2 queue owner", config: writeClaimV2TestConfig(t, chain, keyFile, busyDir, claimV2TestColdkey), want: "already has an owner"},
		{name: "v1 queue owner", config: path, v1: writeV1(keyFile, busyDir), want: "already has an owner"},
	} {
		if test.ctx == nil {
			test.ctx = ctx
		}
		if test.threshold == 0 {
			test.threshold = ClaimV2DefaultThresholdRao
		}
		claimV2TestRefused(t, test.name, RunClaimDaemonV2(test.ctx, test.config, test.v1, test.threshold, false), test.want)
	}
	for _, test := range []struct{ name, threshold, want string }{
		{name: "threshold text", threshold: "--threshold-rao=1.9", want: "--threshold-rao"},
		{name: "threshold floor", threshold: "--threshold-rao=1", want: "transfer floor"},
		{name: "default threshold", want: "durable-volume declaration"},
	} {
		argv := []string{"claim-daemon-v2", "--config=" + path}
		if test.threshold != "" {
			argv = append(argv, test.threshold)
		}
		claimV2TestRefused(t, test.name, runClaimDaemonV2Command(t.Context(), parseArgsForTest(t, argv)), test.want)
	}
}

// E-5, §10.1 startup custody: a malformed v2 queue or forged retained bytes stop
// the v2 daemon alone and beside v1; forged v1 bytes stop the pair.
func TestClaimV2StartupCustodyRefusals(t *testing.T) {
	chain := newClaimV2TestChain(t)
	key, keyFile := writeClaimV2TestKey(t)
	root := t.TempDir()
	v2Dir, v1Dir := filepath.Join(root, "claims-v2"), filepath.Join(root, "claims-v1")
	path := writeClaimV2TestConfig(t, chain, keyFile, v2Dir, claimV2TestColdkey)
	raw, err := yaml.Marshal(ClaimDaemonConfig{SchemaVersion: 1, Release: "1.0", APIURL: "http://operator.example", RPC: []string{"http://rpc.example"}, KeyFile: keyFile, StateDir: v1Dir, PollSeconds: 5, LookbackEpochs: 2})
	if err != nil {
		t.Fatal(err)
	}
	v1Path := filepath.Join(t.TempDir(), "claim.yml")
	if err := os.WriteFile(v1Path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := claimQueueTestContext(t, t.Context(), v2Dir, v1Dir)
	publish := func(dir, raw string) {
		store, err := newClaimQueueStore(dir, ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := errors.Join(store.head.Publish([]byte(raw), nil), store.close()); err != nil {
			t.Fatal(err)
		}
	}
	hash, signed := claimV2TestSign(t, key, claimV2TestChainID, common.HexToAddress("0x99"), 0, 0, nil)
	forged := fmt.Sprintf(`{"schema":%q,"units":{%q:{"vault":%q,"no_id":7,"index":1,"status":"uncertain","tx_hash":%q,"raw_tx_hex":%q}}}`, ClaimQueueV2Schema, claimUnitV2Key(claimV2TestVault, 7, 1), claimV2TestVault, hash, signed)
	for _, v1 := range []string{"", v1Path} {
		publish(v2Dir, "{")
		claimV2TestRefused(t, "malformed queue", RunClaimDaemonV2(ctx, path, v1, ClaimV2DefaultThresholdRao, false), "unexpected end of JSON input")
		publish(v2Dir, forged)
		claimV2TestRefused(t, "forged v2 bytes", RunClaimDaemonV2(ctx, path, v1, ClaimV2DefaultThresholdRao, false), "has another chain, target or value")
	}
	publish(v2Dir, fmt.Sprintf(`{"schema":%q,"units":{}}`, ClaimQueueV2Schema))
	publish(v1Dir, `{"schema":"urnetwork-provider-claim-queue-v1","last_discovered":70,"entries":{"70":{"epoch":70,"status":"uncertain","tx_hash":"0x01","raw_tx_hex":"0x00"}}}`)
	claimV2TestRefused(t, "forged v1 bytes", RunClaimDaemonV2(ctx, path, v1Path, ClaimV2DefaultThresholdRao, false), "claim epoch 70")
	if len(chain.sent()) != 0 {
		t.Fatal("a refused startup signed a claim")
	}
}

// E-5: the command line names the v2 daemon, its v1 sibling, the rao threshold and
// the on-demand claim.
func TestMainUsageClaimDaemonV2(t *testing.T) {
	opts := parseArgsForTest(t, []string{"claim-daemon-v2", "--config=/etc/claim-v2.yml", "--v1-config=/etc/claim.yml", "--threshold-rao=20000000", "--claim-now"})
	if run, _ := opts.Bool("claim-daemon-v2"); !run || fleetOpt(opts, "--config") != "/etc/claim-v2.yml" || fleetOpt(opts, "--v1-config") != "/etc/claim.yml" || fleetOpt(opts, "--threshold-rao") != "20000000" {
		t.Fatalf("claim-daemon-v2 options: %v", opts)
	}
	if demand, _ := opts.Bool("--claim-now"); !demand {
		t.Fatal("--claim-now not set")
	}
	opts = parseArgsForTest(t, []string{"claim-daemon-v2", "--config=/etc/claim-v2.yml"})
	if v1, demand := fleetOpt(opts, "--v1-config"), opts["--claim-now"]; v1 != "" || demand != false {
		t.Fatalf("optional claim-daemon-v2 options set: %v", opts)
	}
	// The v33 provider's frozen grammar names the options without the numbers
	// (the docs family owns the usage text); the default and floor relationship
	// is asserted on the constants themselves.
	if ClaimV2DefaultThresholdRao < ClaimV2TransferFloorRao {
		t.Fatalf("default threshold %d is below the transfer floor %d", ClaimV2DefaultThresholdRao, ClaimV2TransferFloorRao)
	}
}
