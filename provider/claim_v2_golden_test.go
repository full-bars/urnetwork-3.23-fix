//go:build linux || darwin

// The v2 signed and digest bytes, pinned as offline literals against the
// upstream 4ca8316e builders. Every value below was produced by
//
//	go run ./provider/testdata/goldengen
//
// which uses the pinned sn module's own stabi bindings (the module pin is
// exactly 4ca8316e) plus the go-ethereum signer, from the same fixed inputs
// this test repeats verbatim. The test recomputes each value through the
// ported code and requires byte equality, then authenticates the signed bytes
// through the ported v2 verifier. It runs offline and never invokes the
// generator.
package main

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/hex"
	"math/big"
	"os"
	"path/filepath"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"
	"github.com/urfoundation/sn/merkle"
	"github.com/urfoundation/sn/stabi"
)

const (
	claimV2GoldenChainID        = 945
	claimV2GoldenVault          = "0x00000000000000000000000000000000000a2a2a"
	claimV2GoldenBatcher        = "0x00000000000000000000000000000000000b47c4"
	claimV2GoldenRelayerKey     = "0x0c911cbab4cd877cf6dcaebbe8e97c34b0b5710c698115c2fa4d940a611fec6f"
	claimV2GoldenRelayerAddress = "0xB4f4D197609880E52600a1353c28E68e8243e970"
	claimV2GoldenVaultCalldata  = "0xb7a6b907000000000000000000000000000000000000000000000000000000000000000700000000000000000000000000000000000000000000000000000000000000460000000000000000000000000000000000000000000000000000000000000001c01d010000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000ab54a98ceb1f0ad200000000000000000000000000000000000000000000000000000000000000c00000000000000000000000000000000000000000000000000000000000000002e9bfe37d5a72ff7d85f3100b75da57ed5cdfef153e39b8bb08ded688edbf1f53000000000000000000000000000000000000000000000000000000000000000be2c6438f4f74e10608bce8fb9d248dfbbde53be7c4cefebf875e5d51293f0c590000000000000000000000000000000000000000000000000000000000000016"
	claimV2GoldenBatchCalldata  = "0xd77e363f00000000000000000000000000000000000000000000000000000000000a2a2a0000000000000000000000000000000000000000000000000000000000000060000000000000000000000000000000000000000000000000000000000003a9800000000000000000000000000000000000000000000000000000000000000002000000000000000000000000000000000000000000000000000000000000004000000000000000000000000000000000000000000000000000000000000001a0000000000000000000000000000000000000000000000000000000000000000700000000000000000000000000000000000000000000000000000000000000460000000000000000000000000000000000000000000000000000000000000001c01d010000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000ab54a98ceb1f0ad200000000000000000000000000000000000000000000000000000000000000c00000000000000000000000000000000000000000000000000000000000000002e9bfe37d5a72ff7d85f3100b75da57ed5cdfef153e39b8bb08ded688edbf1f53000000000000000000000000000000000000000000000000000000000000000be2c6438f4f74e10608bce8fb9d248dfbbde53be7c4cefebf875e5d51293f0c590000000000000000000000000000000000000000000000000000000000000016000000000000000000000000000000000000000000000000000000000000000700000000000000000000000000000000000000000000000000000000000000460000000000000000000000000000000000000000000000000000000000000002c01d0100000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000055aa54d38e5267eea00000000000000000000000000000000000000000000000000000000000000c00000000000000000000000000000000000000000000000000000000000000002e9bfe37d5a72ff7d85f3100b75da57ed5cdfef153e39b8bb08ded688edbf1f53000000000000000000000000000000000000000000000000000000000000000be2c6438f4f74e10608bce8fb9d248dfbbde53be7c4cefebf875e5d51293f0c590000000000000000000000000000000000000000000000000000000000000016"
	claimV2GoldenVaultRawTx     = "0xf901c880018327ac409400000000000000000000000000000000000a2a2a80b90164b7a6b907000000000000000000000000000000000000000000000000000000000000000700000000000000000000000000000000000000000000000000000000000000460000000000000000000000000000000000000000000000000000000000000001c01d010000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000ab54a98ceb1f0ad200000000000000000000000000000000000000000000000000000000000000c00000000000000000000000000000000000000000000000000000000000000002e9bfe37d5a72ff7d85f3100b75da57ed5cdfef153e39b8bb08ded688edbf1f53000000000000000000000000000000000000000000000000000000000000000be2c6438f4f74e10608bce8fb9d248dfbbde53be7c4cefebf875e5d51293f0c590000000000000000000000000000000000000000000000000000000000000016820786a057f79d18b61616b21d04791f0579ee062c56c6ce0f6d83893d9230d6e9b76fbda0644023675813179cf974a0e719cffafeeb7123de59caf6d21c5e55b3d89f4bdc"
	claimV2GoldenVaultTxHash    = "0x37cc7b1b0f55a80997010594c67f481ba07b722c6178e82be54b3e2107a3826e"
	claimV2GoldenBatchRawTx     = "0xf903e880018327ac409400000000000000000000000000000000000b47c480b90384d77e363f00000000000000000000000000000000000000000000000000000000000a2a2a0000000000000000000000000000000000000000000000000000000000000060000000000000000000000000000000000000000000000000000000000003a9800000000000000000000000000000000000000000000000000000000000000002000000000000000000000000000000000000000000000000000000000000004000000000000000000000000000000000000000000000000000000000000001a0000000000000000000000000000000000000000000000000000000000000000700000000000000000000000000000000000000000000000000000000000000460000000000000000000000000000000000000000000000000000000000000001c01d010000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000ab54a98ceb1f0ad200000000000000000000000000000000000000000000000000000000000000c00000000000000000000000000000000000000000000000000000000000000002e9bfe37d5a72ff7d85f3100b75da57ed5cdfef153e39b8bb08ded688edbf1f53000000000000000000000000000000000000000000000000000000000000000be2c6438f4f74e10608bce8fb9d248dfbbde53be7c4cefebf875e5d51293f0c590000000000000000000000000000000000000000000000000000000000000016000000000000000000000000000000000000000000000000000000000000000700000000000000000000000000000000000000000000000000000000000000460000000000000000000000000000000000000000000000000000000000000002c01d0100000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000055aa54d38e5267eea00000000000000000000000000000000000000000000000000000000000000c00000000000000000000000000000000000000000000000000000000000000002e9bfe37d5a72ff7d85f3100b75da57ed5cdfef153e39b8bb08ded688edbf1f53000000000000000000000000000000000000000000000000000000000000000be2c6438f4f74e10608bce8fb9d248dfbbde53be7c4cefebf875e5d51293f0c590000000000000000000000000000000000000000000000000000000000000016820785a0c0ce9b8e3cff1b3c461dbdd015d5687b93ed22cb5d256cc94daca720d9e4ef52a061904efef19c98e300e50479a0578853b276d4e81c0b45d3278a8429e5a5f491"
	claimV2GoldenBatchTxHash    = "0xcf812e76701be68e67f34fa26e45ba0b358ef791f61eb5a21b15b383ad16ad8e"
)

// The synthetic relayer key: sha256 of a literal string, identical in the
// generator. Never a production key.
func claimV2GoldenKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	keyBytes := sha256.Sum256([]byte("urnetwork d17-4 claim-daemon-v2 golden relayer"))
	key, err := crypto.ToECDSA(keyBytes[:])
	if err != nil {
		t.Fatal(err)
	}
	if hexutil.Encode(crypto.FromECDSA(key)) != claimV2GoldenRelayerKey {
		t.Fatal("relayer key derivation drifted from the golden literal")
	}
	if crypto.PubkeyToAddress(key.PublicKey) != common.HexToAddress(claimV2GoldenRelayerAddress) {
		t.Fatal("relayer address drifted from the golden literal")
	}
	return key
}

func claimV2GoldenProof() []merkle.LedgerNode {
	first, second := new(uint256.Int), new(uint256.Int)
	first.SetUint64(11)
	second.SetUint64(22)
	return []merkle.LedgerNode{
		{Hash: crypto.Keccak256Hash([]byte("urnetwork-d17-4-proof-1")), Sum: *first},
		{Hash: crypto.Keccak256Hash([]byte("urnetwork-d17-4-proof-2")), Sum: *second},
	}
}

func claimV2GoldenAccount(index uint64, cumulative string) *claimAccountV2 {
	account := &claimAccountV2{Vault: claimV2GoldenVault, NoID: 7, Index: index, RootIndex: 70, Coldkey: [32]byte{0xc0, 0x1d, 0x01}, Proof: claimV2GoldenProof()}
	if err := account.Cumulative.SetFromDecimal(cumulative); err != nil {
		panic(err)
	}
	return account
}

func mustHex(t *testing.T, text string) []byte {
	t.Helper()
	raw, err := hexutil.Decode(text)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestClaimV2GoldenVaultCalldata(t *testing.T) {
	account := claimV2GoldenAccount(1, "12345678901234567890")
	if got := hexutil.Encode(claimV2Calldata(account)); got != claimV2GoldenVaultCalldata {
		t.Fatalf("vault calldata drifted from the 4ca8316e builder literal:\n got %s\nwant %s", got, claimV2GoldenVaultCalldata)
	}
	intent, err := decodeClaimV2Calldata(mustHex(t, claimV2GoldenVaultCalldata))
	if err != nil {
		t.Fatal(err)
	}
	if intent.NoID.Uint64() != 7 || intent.RootIndex.Uint64() != 70 || intent.Account.Index.Uint64() != 1 ||
		intent.Account.Cumulative.String() != "12345678901234567890" || intent.Account.Coldkey != account.Coldkey || len(intent.Proof) != 2 {
		t.Fatalf("decoded intent: %+v", intent)
	}
}

func TestClaimV2GoldenBatchCalldata(t *testing.T) {
	// Built explicitly through the pinned module's own binding, so the literal
	// is checked against the upstream builder, not only against the port.
	first, second := claimV2GoldenAccount(1, "12345678901234567890"), claimV2GoldenAccount(2, "98765432109876543210")
	items := []stabi.STClaimBatcherClaim{claimV2Item(first), claimV2Item(second)}
	if got := hexutil.Encode(stabi.NewSTClaimBatcher().PackClaimMany(common.HexToAddress(claimV2GoldenVault), items, big.NewInt(240_000))); got != claimV2GoldenBatchCalldata {
		t.Fatalf("batch calldata drifted from the 4ca8316e builder literal:\n got %s\nwant %s", got, claimV2GoldenBatchCalldata)
	}
	for index, want := range map[uint64]int{1: 0, 2: 1} {
		unit := &ClaimUnitV2{Vault: claimV2GoldenVault, NoID: 7, Index: index}
		intent, err := decodeClaimV2BatchCalldata(mustHex(t, claimV2GoldenBatchCalldata), claimV2GoldenVault, unit)
		if err != nil {
			t.Fatal(err)
		}
		if intent.Item != want || intent.Items != 2 || intent.NoID.Uint64() != 7 || intent.Account.Index.Uint64() != index {
			t.Fatalf("unit %d intent: %+v", index, intent)
		}
	}
}

func claimV2GoldenKeyFile(t *testing.T, key *ecdsa.PrivateKey) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "relayer.key")
	if err := os.WriteFile(path, []byte(hexutil.Encode(crypto.FromECDSA(key))), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func claimV2GoldenSignedTx(t *testing.T, key *ecdsa.PrivateKey, to string, calldata string) (*types.Transaction, []byte) {
	t.Helper()
	target := common.HexToAddress(to)
	tx := types.NewTx(&types.LegacyTx{Nonce: 0, GasPrice: big.NewInt(1), Gas: 2_600_000, To: &target, Value: big.NewInt(0), Data: mustHex(t, calldata)})
	signed, err := types.SignTx(tx, types.LatestSignerForChainID(new(big.Int).SetUint64(claimV2GoldenChainID)), key)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := signed.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	return signed, raw
}

// Re-signing the same fixed inputs must reproduce the exact upstream bytes,
// and the ported verifier must accept exactly those bytes.
func TestClaimV2GoldenSignedVaultBytes(t *testing.T) {
	key := claimV2GoldenKey(t)
	keyFile := claimV2GoldenKeyFile(t, key)
	signed, raw := claimV2GoldenSignedTx(t, key, claimV2GoldenVault, claimV2GoldenVaultCalldata)
	if got := "0x" + hex.EncodeToString(raw); got != claimV2GoldenVaultRawTx || signed.Hash().Hex() != claimV2GoldenVaultTxHash {
		t.Fatalf("signed vault bytes drifted:\n got raw  %s\n got hash %s", got, signed.Hash().Hex())
	}
	cfg := &ClaimDaemonV2Config{ChainID: claimV2GoldenChainID, Vault: claimV2GoldenVault, KeyFile: keyFile}
	unit := &ClaimUnitV2{Vault: claimV2GoldenVault, NoID: 7, Index: 1, TxHash: claimV2GoldenVaultTxHash, RawTxHex: claimV2GoldenVaultRawTx}
	tx, intent, from, err := authenticateSignedClaimV2(cfg, unit)
	if err != nil {
		t.Fatal(err)
	}
	if tx.Hash().Hex() != claimV2GoldenVaultTxHash || from != common.HexToAddress(claimV2GoldenRelayerAddress) || intent.Items != 0 || tx.Nonce() != 0 || tx.Gas() != 2_600_000 || tx.GasPrice().Cmp(big.NewInt(1)) != 0 || tx.Value().Sign() != 0 || *tx.To() != common.HexToAddress(claimV2GoldenVault) {
		t.Fatalf("authenticated vault transaction: %v %v %v", tx.Hash().Hex(), from, intent)
	}
	if got := hexutil.Encode(tx.Data()); got != claimV2GoldenVaultCalldata {
		t.Fatalf("authenticated calldata: %s", got)
	}
}

func TestClaimV2GoldenSignedBatchBytes(t *testing.T) {
	key := claimV2GoldenKey(t)
	keyFile := claimV2GoldenKeyFile(t, key)
	signed, raw := claimV2GoldenSignedTx(t, key, claimV2GoldenBatcher, claimV2GoldenBatchCalldata)
	if got := "0x" + hex.EncodeToString(raw); got != claimV2GoldenBatchRawTx || signed.Hash().Hex() != claimV2GoldenBatchTxHash {
		t.Fatalf("signed batch bytes drifted:\n got raw  %s\n got hash %s", got, signed.Hash().Hex())
	}
	cfg := &ClaimDaemonV2Config{ChainID: claimV2GoldenChainID, Vault: claimV2GoldenVault, Batcher: claimV2GoldenBatcher, KeyFile: keyFile}
	unit := &ClaimUnitV2{Vault: claimV2GoldenVault, NoID: 7, Index: 1, TxHash: claimV2GoldenBatchTxHash, RawTxHex: claimV2GoldenBatchRawTx}
	tx, intent, from, err := authenticateSignedClaimV2(cfg, unit)
	if err != nil {
		t.Fatal(err)
	}
	if tx.Hash().Hex() != claimV2GoldenBatchTxHash || from != common.HexToAddress(claimV2GoldenRelayerAddress) || intent.Items != 2 || intent.Item != 0 || intent.NoID.Uint64() != 7 || intent.Account.Index.Uint64() != 1 {
		t.Fatalf("authenticated batch transaction: %v %v %+v", tx.Hash().Hex(), from, intent)
	}
}
