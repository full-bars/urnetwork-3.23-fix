// Claim crash recovery must use the same explicit EVM identity as the
// original transaction finality check, including synthetic block hashes.
package main

import (
	"context"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// Replays a finalized receipt and its explicit canonical block over real RPC.
func claimReceiptIdentityTestRPC(t *testing.T, receipt *types.Receipt, canonical any) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		defer request.Body.Close()
		var call struct {
			ID     json.RawMessage   `json:"id"`
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(request.Body).Decode(&call); err != nil {
			http.Error(writer, err.Error(), http.StatusBadRequest)
			return
		}
		var result any
		switch call.Method {
		case "eth_chainId":
			result = "0x3b1"
		case "eth_getTransactionReceipt":
			result = receipt
		case "eth_getBlockByNumber":
			if len(call.Params) != 2 || string(call.Params[1]) != "false" {
				t.Errorf("canonical block parameters = %s", call.Params)
			} else if string(call.Params[0]) == `"finalized"` || string(call.Params[0]) == `"0xc"` {
				result = map[string]any{"number": "0xc", "hash": common.Hash{0x42}.Hex()}
			} else if string(call.Params[0]) == `"0xa"` {
				result = canonical
			} else {
				t.Errorf("unexpected canonical selector = %s", call.Params)
			}
		default:
			t.Errorf("unexpected claim receipt method %s", call.Method)
		}
		if err := json.NewEncoder(writer).Encode(map[string]any{"jsonrpc": "2.0", "id": call.ID, "result": result}); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// Keeps all local Ethereum header fields valid while using Subtensor's
// distinct externally supplied EVM hash for the block and receipt.
func claimReceiptIdentityFixture(t *testing.T) (*types.Receipt, map[string]any) {
	t.Helper()
	hash := common.HexToHash("0xaad46c25ee81b4f9f636677c1b9197a146733e8f16d57114269030ddf26790e2")
	header := &types.Header{Number: big.NewInt(10), Difficulty: big.NewInt(0), Extra: []byte("synthetic")}
	if header.Hash() == hash {
		t.Fatal("fixture does not distinguish the explicit and recomputed block hash")
	}
	encoded, err := json.Marshal(header)
	if err != nil {
		t.Fatal(err)
	}
	var block map[string]any
	if err := json.Unmarshal(encoded, &block); err != nil {
		t.Fatal(err)
	}
	block["hash"] = hash.Hex()
	receipt := &types.Receipt{BlockNumber: big.NewInt(10), BlockHash: hash, TxHash: common.HexToHash("0x1234"), Status: types.ReceiptStatusSuccessful, Logs: []*types.Log{}}
	return receipt, block
}

// Recovery after a crash accepts canonical synthetic inclusion evidence.
func TestFinalizedClaimReceiptPreservesSyntheticRPCIdentity(t *testing.T) {
	receipt, block := claimReceiptIdentityFixture(t)
	endpoint := claimReceiptIdentityTestRPC(t, receipt, block)
	got, err := finalizedClaimReceipt(context.Background(), &ClaimDaemonConfig{RPC: []string{endpoint}}, receipt.TxHash.Hex(), big.NewInt(945))
	if err != nil || got == nil || got.BlockHash != receipt.BlockHash || got.BlockNumber.Cmp(receipt.BlockNumber) != 0 {
		t.Fatalf("synthetic finalized receipt = %+v, %v", got, err)
	}
}

// Receipt reads preserve canonical failure evidence without authorizing a
// new signature; signed-outcome reconciliation owns that disposition.
func TestFinalizedClaimReceiptRetainsFailedSyntheticReceipt(t *testing.T) {
	receipt, block := claimReceiptIdentityFixture(t)
	receipt.Status = types.ReceiptStatusFailed
	endpoint := claimReceiptIdentityTestRPC(t, receipt, block)
	got, err := finalizedClaimReceipt(context.Background(), &ClaimDaemonConfig{RPC: []string{endpoint}}, receipt.TxHash.Hex(), big.NewInt(945))
	if err != nil || got == nil || got.Status != types.ReceiptStatusFailed {
		t.Fatalf("failed receipt = %+v, %v", got, err)
	}
}

// Both claim recovery paths fail closed for substituted identities, null
// blocks, malformed hashes, zero block hashes, and wrong canonical heights.
func TestClaimReceiptIdentityRejectsAdjacentRPCFailures(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*types.Receipt, map[string]any) any
	}{
		{name: "null block", mutate: func(_ *types.Receipt, _ map[string]any) any { return nil }},
		{name: "wrong block number", mutate: func(_ *types.Receipt, block map[string]any) any { block["number"] = "0xb"; return block }},
		{name: "missing block number", mutate: func(_ *types.Receipt, block map[string]any) any { delete(block, "number"); return block }},
		{name: "zero block hash", mutate: func(_ *types.Receipt, block map[string]any) any { block["hash"] = common.Hash{}.Hex(); return block }},
		{name: "malformed block hash", mutate: func(_ *types.Receipt, block map[string]any) any { block["hash"] = "0x11"; return block }},
		{name: "same height reorg", mutate: func(_ *types.Receipt, block map[string]any) any {
			block["hash"] = common.HexToHash("0x20").Hex()
			return block
		}},
		{name: "wrong receipt transaction", mutate: func(receipt *types.Receipt, block map[string]any) any {
			receipt.TxHash = common.HexToHash("0x4321")
			return block
		}},
		{name: "zero receipt hash", mutate: func(receipt *types.Receipt, block map[string]any) any {
			receipt.BlockHash = common.Hash{}
			return block
		}},
		{name: "missing receipt number", mutate: func(receipt *types.Receipt, block map[string]any) any { receipt.BlockNumber = nil; return block }},
		{name: "zero receipt number", mutate: func(receipt *types.Receipt, block map[string]any) any {
			receipt.BlockNumber = big.NewInt(0)
			return block
		}},
	}
	for _, test := range cases {
		receipt, block := claimReceiptIdentityFixture(t)
		hash := receipt.TxHash.Hex()
		receipt.Status = types.ReceiptStatusFailed
		canonical := test.mutate(receipt, block)
		endpoint := claimReceiptIdentityTestRPC(t, receipt, canonical)
		cfg := &ClaimDaemonConfig{RPC: []string{endpoint}}
		if recovered, err := finalizedClaimReceipt(context.Background(), cfg, hash, big.NewInt(945)); recovered != nil || err == nil {
			t.Errorf("%s: recovered=%+v error=%v", test.name, recovered, err)
		}
	}
}

// A null or wrong chain response cannot qualify otherwise matching receipts.
func TestFinalizedClaimReceiptRejectsMalformedChainIdentity(t *testing.T) {
	for _, chainId := range []any{nil, "0x3b2", "not-a-chain"} {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			var call struct {
				Id     json.RawMessage `json:"id"`
				Method string          `json:"method"`
			}
			if err := json.NewDecoder(request.Body).Decode(&call); err != nil {
				t.Error(err)
				return
			}
			if call.Method != "eth_chainId" {
				t.Errorf("unverified chain read receipt: %s", call.Method)
			}
			_ = json.NewEncoder(writer).Encode(map[string]any{"jsonrpc": "2.0", "id": call.Id, "result": chainId})
		}))
		cfg := &ClaimDaemonConfig{RPC: []string{server.URL}}
		receipt, err := finalizedClaimReceipt(context.Background(), cfg, common.HexToHash("0x12").Hex(), big.NewInt(945))
		server.Close()
		if receipt != nil || err == nil {
			t.Fatalf("malformed chain %v admitted receipt: %+v %v", chainId, receipt, err)
		}
	}
}
