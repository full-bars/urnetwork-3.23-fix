package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/urnetwork/connect"

	"github.com/urfoundation/sn/merkle"
	"github.com/urfoundation/sn/miner/onchain"
	"github.com/urfoundation/sn/stabi"
)

type claimApiFunction func(context.Context, *connect.SnPoolClaimArgs) (*connect.SnPoolClaimResult, error)

func (self claimApiFunction) SnPoolClaimSyncWithContext(ctx context.Context, args *connect.SnPoolClaimArgs) (*connect.SnPoolClaimResult, error) {
	return self(ctx, args)
}

// Every key, epoch, coldkey, vault and chain identity is generated or synthetic.
func signedClaimFixture(t *testing.T, epoch int64, nonce uint64) (*ClaimDaemonConfig, *connect.SnPoolClaimResult, *ClaimQueueEntry, *types.Receipt, map[string]any) {
	t.Helper()
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ClaimDaemonConfig{KeyFile: filepath.Join(t.TempDir(), "synthetic-relayer.key"), StateDir: filepath.Join(t.TempDir(), "claims")}
	if err := os.WriteFile(cfg.KeyFile, []byte(hex.EncodeToString(crypto.FromECDSA(key))), 0o600); err != nil {
		t.Fatal(err)
	}
	coldkey := [32]byte{1, 2, 3}
	intent := onchain.ClaimIntent{E: big.NewInt(epoch), NoID: big.NewInt(7), Coldkey: coldkey, ShareBps: big.NewInt(10_000)}
	data, err := onchain.BuildClaimCalldata(intent)
	if err != nil {
		t.Fatal(err)
	}
	vault := common.HexToAddress("0x1234")
	tx, err := types.SignTx(types.NewTx(&types.LegacyTx{Nonce: nonce, GasPrice: big.NewInt(1), Gas: 100_000, To: &vault, Value: big.NewInt(0), Data: data}), types.LatestSignerForChainID(big.NewInt(945)), key)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := tx.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	entry := &ClaimQueueEntry{Epoch: epoch, Status: "uncertain", Attempts: 1, TxHash: strings.ToLower(tx.Hash().Hex()), RawTxHex: "0x" + hex.EncodeToString(raw)}
	root := merkle.PayoutLeaf(coldkey, intent.ShareBps)
	claim := &connect.SnPoolClaimResult{Epoch: uint64(epoch), NoId: intent.NoID.Bytes(), Coldkey: coldkey[:], ShareBps: 10_000, PayoutRoot: root[:], ContractAddress: vault.Hex(), ChainId: 945}
	receipt, block := claimReceiptIdentityFixture(t)
	receipt.TxHash = tx.Hash()
	parsed, err := stabi.STSettlementVaultMetaData.ParseABI()
	if err != nil {
		t.Fatal(err)
	}
	event := parsed.Events["Claimed"]
	eventData, err := event.Inputs.NonIndexed().Pack(intent.ShareBps, big.NewInt(25), crypto.PubkeyToAddress(key.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	receipt.Logs = []*types.Log{{Address: vault, Topics: []common.Hash{event.ID, common.BigToHash(intent.E), common.BigToHash(intent.NoID), common.Hash(coldkey)}, Data: eventData, BlockNumber: receipt.BlockNumber.Uint64(), BlockHash: receipt.BlockHash, TxHash: receipt.TxHash}}
	return cfg, claim, entry, receipt, block
}

func TestSignedClaimReconcilesCanonicalReceiptWithoutHistoricalAPI(t *testing.T) {
	cfg, _, entry, receipt, block := signedClaimFixture(t, 70, 23)
	cfg.RPC = []string{claimReceiptIdentityTestRPC(t, receipt, block)}
	api := claimApiFunction(func(context.Context, *connect.SnPoolClaimArgs) (*connect.SnPoolClaimResult, error) {
		t.Fatal("receipt-first recovery depended on the pruned historical API")
		return nil, errors.New("pruned")
	})
	beforeHash, beforeRaw := entry.TxHash, entry.RawTxHex
	status, err := reconcileClaimEntryTest(t, context.Background(), cfg, api, entry)
	if err != nil || status != "finalized" || entry.FinalizedBlock != receipt.BlockNumber.Uint64() || entry.FinalizedBlockHash != strings.ToLower(receipt.BlockHash.Hex()) || entry.ReceiptStatus != 1 || entry.ReceiptLogsHash == "" || entry.TxHash != beforeHash || entry.RawTxHex != beforeRaw {
		t.Fatalf("canonical signed recovery = %s, %v, %+v", status, err, entry)
	}
}

func TestSignedClaimRejectsReceiptWithoutMatchingIntentEvent(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*types.Receipt)
	}{
		{name: "failed", mutate: func(receipt *types.Receipt) { receipt.Status = 0 }},
		{name: "missing event", mutate: func(receipt *types.Receipt) { receipt.Logs = []*types.Log{} }},
		{name: "wrong epoch", mutate: func(receipt *types.Receipt) { receipt.Logs[0].Topics[1] = common.BigToHash(big.NewInt(71)) }},
		{name: "wrong operator", mutate: func(receipt *types.Receipt) { receipt.Logs[0].Topics[2] = common.BigToHash(big.NewInt(8)) }},
		{name: "wrong coldkey", mutate: func(receipt *types.Receipt) { receipt.Logs[0].Topics[3] = common.HexToHash("0x99") }},
		{name: "wrong vault", mutate: func(receipt *types.Receipt) { receipt.Logs[0].Address = common.HexToAddress("0x99") }},
		{name: "wrong transaction", mutate: func(receipt *types.Receipt) { receipt.Logs[0].TxHash = common.HexToHash("0x99") }},
		{name: "removed", mutate: func(receipt *types.Receipt) { receipt.Logs[0].Removed = true }},
		{name: "duplicate", mutate: func(receipt *types.Receipt) { receipt.Logs = append(receipt.Logs, receipt.Logs[0]) }},
	}
	for _, test := range cases {
		cfg, _, entry, receipt, block := signedClaimFixture(t, 70, 23)
		test.mutate(receipt)
		cfg.RPC = []string{claimReceiptIdentityTestRPC(t, receipt, block)}
		before := *entry
		status, err := reconcileClaimEntryTest(t, context.Background(), cfg, fakeClaimAPI{err: errors.New("pruned")}, entry)
		var unresolved *claimSignedOutcomeError
		if status != "" || !errors.As(err, &unresolved) || *entry != before {
			t.Fatalf("%s disposed signed history: status=%s error=%v entry=%+v", test.name, status, err, entry)
		}
	}
}

func TestSignedClaimMissingReceiptCannotBecomeAPINoClaim(t *testing.T) {
	cfg, claim, entry, _, _ := signedClaimFixture(t, 70, 23)
	cfg.RPC = nil // unavailable chain outcome cannot be replaced by an API status
	claim.NoId = nil
	before := *entry
	status, err := reconcileClaimEntryTest(t, context.Background(), cfg, fakeClaimAPI{result: claim}, entry)
	var unresolved *claimSignedOutcomeError
	if status != "" || !errors.As(err, &unresolved) || *entry != before {
		t.Fatalf("API no-claim disposed signed uncertainty: %s %v %+v", status, err, entry)
	}
}

func TestClaimQueueCannotClearSignedHistoryForRetryOrNoClaim(t *testing.T) {
	for _, disposition := range []string{"retry", "no-claim"} {
		entry := &ClaimQueueEntry{Epoch: 3, Status: "uncertain", TxHash: "synthetic retained hash", RawTxHex: "synthetic retained raw", Attempts: 1}
		queue := &ClaimQueue{LastDiscovered: 3, Entries: map[string]*ClaimQueueEntry{"3": entry}}
		hooks := claimPollTestHooks(t, time.Now())
		hooks.save = func(*ClaimQueue) error { return nil }
		hooks.reconcile = func(context.Context, *ClaimQueueEntry) (string, error) { return disposition, nil }
		if err := pollClaimQueue(context.Background(), queue, hooks); err != nil {
			t.Fatal(err)
		}
		if entry.Status != "uncertain" || entry.TxHash != "synthetic retained hash" || entry.RawTxHex != "synthetic retained raw" || entry.Attempts != 1 || entry.NextRetryAt == "" {
			t.Fatalf("%s cleared signed custody: %+v", disposition, entry)
		}
	}
}

// Startup seeds all member files before workers run, and rejects any forged
// raw/hash, foreign relayer or second chain rather than guessing a nonce floor.
func TestClaimNonceFloorSeedsAllDurableMembersAndRejectsForgedIdentity(t *testing.T) {
	cfg, _, entry, _, _ := signedClaimFixture(t, 70, 23)
	store := newClaimQueueTestStore(t, cfg.StateDir)
	queue := &ClaimQueue{Schema: "urnetwork-provider-claim-queue-v1", LastDiscovered: 70, Entries: map[string]*ClaimQueueEntry{"70": entry}}
	if err := store.save(queue); err != nil {
		t.Fatal(err)
	}
	admission := &claimAdmission{}
	if err := admission.seedMember(cfg, store); err != nil || admission.nonceMinimum() != 24 {
		t.Fatalf("seed floor=%d error=%v", admission.nonceMinimum(), err)
	}
	key, err := onchain.LoadKeyFile(cfg.KeyFile)
	if err != nil {
		t.Fatal(err)
	}
	original := *entry
	cases := []struct {
		name   string
		mutate func()
	}{
		{name: "hash", mutate: func() { entry.TxHash = common.HexToHash("0x99").Hex() }},
		{name: "relayer", mutate: func() {
			other, err := crypto.GenerateKey()
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(cfg.KeyFile, []byte(hex.EncodeToString(crypto.FromECDSA(other))), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "chain", mutate: func() {
			raw, _ := hex.DecodeString(strings.TrimPrefix(entry.RawTxHex, "0x"))
			var tx types.Transaction
			if err := tx.UnmarshalBinary(raw); err != nil {
				t.Fatal(err)
			}
			forged, err := types.SignTx(&tx, types.LatestSignerForChainID(big.NewInt(946)), key)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ = forged.MarshalBinary()
			entry.RawTxHex = "0x" + hex.EncodeToString(raw)
			entry.TxHash = forged.Hash().Hex()
		}},
	}
	for _, test := range cases {
		*entry = original
		if err := os.WriteFile(cfg.KeyFile, []byte(hex.EncodeToString(crypto.FromECDSA(key))), 0o600); err != nil {
			t.Fatal(err)
		}
		admission = &claimAdmission{}
		if err := admission.rememberSigned(cfg, entry); err != nil {
			t.Fatal(err)
		}
		test.mutate()
		if err := store.save(queue); err != nil {
			t.Fatal(err)
		}
		if err := admission.seedMember(cfg, store); err == nil || admission.nonceMinimum() != 24 {
			t.Fatalf("%s allowed forged nonce seed: floor=%d error=%v", test.name, admission.nonceMinimum(), err)
		}
	}
}

func TestClaimArtifactRootMismatchRemainsDistinctCorrectnessFailure(t *testing.T) {
	cfg, claim, entry, _, _ := signedClaimFixture(t, 70, 23)
	entry.Status, entry.TxHash, entry.RawTxHex = "pending", "", ""
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var call struct {
			Id     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.NewDecoder(request.Body).Decode(&call); err != nil {
			t.Error(err)
			return
		}
		var result any
		switch call.Method {
		case "eth_chainId":
			result = "0x3b1"
		case "eth_getBlockByNumber":
			result = map[string]any{"number": "0xc", "hash": common.Hash{0x42}.Hex()}
		case "eth_call":
			result = "0x" + strings.Repeat("00", 32*7)
		default:
			t.Errorf("unexpected RPC %s", call.Method)
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{"jsonrpc": "2.0", "id": call.Id, "result": result})
	}))
	defer server.Close()
	cfg.RPC = []string{server.URL}
	status, err := reconcileClaimEntryTest(t, context.Background(), cfg, fakeClaimAPI{result: claim}, entry)
	var mismatch *claimArtifactRootMismatchError
	if status != "" || !errors.As(err, &mismatch) || mismatch.epoch != 70 {
		t.Fatalf("root mismatch was hidden as %s: %v", status, err)
	}
	if entry.Attempts != 1 {
		t.Fatalf("readiness changed submission count: %+v", entry)
	}
}

// A consumed nonce is evidence against exact replay, not permission to erase
// its raw bytes and sign a replacement whose history cannot yet be represented.
func TestSignedClaimConsumedNonceRetainsExactLiability(t *testing.T) {
	cfg, claim, entry, _, _ := signedClaimFixture(t, 70, 23)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var call struct {
			Id     json.RawMessage   `json:"id"`
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(request.Body).Decode(&call); err != nil {
			t.Error(err)
			return
		}
		var result any
		switch call.Method {
		case "eth_chainId":
			result = "0x3b1"
		case "eth_getTransactionReceipt":
			result = nil
		case "eth_getBlockByNumber":
			result = map[string]any{"number": "0xc", "hash": common.Hash{0x42}.Hex()}
		case "eth_getTransactionCount":
			result = "0x18"
		case "eth_call":
			var params map[string]any
			if err := json.Unmarshal(call.Params[0], &params); err != nil {
				t.Error(err)
				return
			}
			data, _ := params["input"].(string)
			if data == "" {
				data, _ = params["data"].(string)
			}
			if strings.HasPrefix(data, "0x11f5fe7d") {
				result = "0x" + hex.EncodeToString(claim.PayoutRoot) + strings.Repeat("00", 32*6)
			} else {
				result = "0x" + strings.Repeat("00", 32)
			}
		default:
			t.Errorf("consumed nonce caused unexpected RPC %s", call.Method)
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{"jsonrpc": "2.0", "id": call.Id, "result": result})
	}))
	defer server.Close()
	cfg.RPC = []string{server.URL}
	before := *entry
	status, err := reconcileClaimEntryTest(t, context.Background(), cfg, fakeClaimAPI{result: claim}, entry)
	var unresolved *claimSignedOutcomeError
	if status != "" || !errors.As(err, &unresolved) || !strings.Contains(err.Error(), "nonce is consumed") || *entry != before {
		t.Fatalf("consumed nonce disposed exact liability: status=%s err=%v entry=%+v", status, err, entry)
	}
}

// Missing historical API state does not strand a prepared nonce. Only an
// identical raw transaction passes a finalized chain preflight and rebroadcast;
// a revert leaves the same durable row uncertain without a send or new sign.
func TestSignedClaimPrunedAPIReplaysExactBytesAfterFinalizedPreflight(t *testing.T) {
	for _, rejectPreflight := range []bool{false, true} {
		cfg, _, entry, _, _ := signedClaimFixture(t, 70, 23)
		var preflights, sends atomic.Int64
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			var call struct {
				Id     json.RawMessage   `json:"id"`
				Method string            `json:"method"`
				Params []json.RawMessage `json:"params"`
			}
			if err := json.NewDecoder(request.Body).Decode(&call); err != nil {
				t.Error(err)
				return
			}
			var result any
			switch call.Method {
			case "eth_chainId":
				result = "0x3b1"
			case "eth_getTransactionReceipt":
				result = nil
			case "eth_getBlockByNumber":
				result = map[string]any{"number": "0xc", "hash": common.Hash{0x42}.Hex()}
			case "eth_getTransactionCount":
				result = "0x17"
			case "eth_call":
				preflights.Add(1)
				if len(call.Params) != 2 || string(call.Params[1]) != `"0xc"` {
					t.Errorf("replay preflight lacks finalized block: %s", call.Params)
				}
				if rejectPreflight {
					_ = json.NewEncoder(writer).Encode(map[string]any{"jsonrpc": "2.0", "id": call.Id, "error": map[string]any{"code": -32000, "message": "synthetic payout proof is no longer valid"}})
					return
				}
				result = "0x"
			case "eth_sendRawTransaction":
				sends.Add(1)
				var raw string
				if err := json.Unmarshal(call.Params[0], &raw); err != nil {
					t.Error(err)
					return
				}
				if raw != entry.RawTxHex {
					t.Error("replay changed exact signed transaction")
				}
				result = entry.TxHash
			default:
				t.Errorf("replay attempted unexpected read/sign RPC %s", call.Method)
			}
			_ = json.NewEncoder(writer).Encode(map[string]any{"jsonrpc": "2.0", "id": call.Id, "result": result})
		}))
		cfg.RPC = []string{server.URL}
		before := *entry
		api := claimApiFunction(func(context.Context, *connect.SnPoolClaimArgs) (*connect.SnPoolClaimResult, error) {
			t.Fatal("exact signed replay depended on pruned API")
			return nil, nil
		})
		status, err := reconcileClaimEntryTest(t, context.Background(), cfg, api, entry)
		server.Close()
		wantSends := 1
		if rejectPreflight {
			wantSends = 0
		}
		var unresolved *claimSignedOutcomeError
		if status != "" || !errors.As(err, &unresolved) || *entry != before || preflights.Load() != 1 || sends.Load() != int64(wantSends) {
			t.Fatalf("replay changed outcome or lost exact intent: reject=%t status=%s error=%v preflights=%d sends=%d entry=%+v", rejectPreflight, status, err, preflights.Load(), sends.Load(), entry)
		}
	}
}
