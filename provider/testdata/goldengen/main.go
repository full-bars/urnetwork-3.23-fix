// Golden-byte generator for provider/claim_v2_golden_test.go.
//
// This program is not part of any build: the go tool skips provider/testdata
// in wildcard patterns. Run it explicitly to reproduce the literals in the
// golden test:
//
//	go run ./provider/testdata/goldengen
//
// It computes the v2 claim bytes with the pinned sn module's own builders
// (github.com/urfoundation/sn/stabi at the 4ca8316e pin, the generation the
// daemon was ported from) plus go-ethereum's signer, from the same fixed
// inputs provider/claim_v2_golden_test.go repeats verbatim. The golden test
// embeds the output and never runs this program, so it works offline.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/urfoundation/sn/stabi"
)

// The fixed inputs, identical in the golden test.
const (
	goldenChainID = 945
	goldenVault   = "0x00000000000000000000000000000000000a2a2a"
	goldenBatcher = "0x00000000000000000000000000000000000b47c4"
)

var (
	goldenColdkey     = [32]byte{0xc0, 0x1d, 0x01}
	goldenCumulative  = "12345678901234567890"
	goldenCumulative2 = "98765432109876543210"
)

var stSettlementVault2 = stabi.NewSTSettlementVault2()
var stClaimBatcher = stabi.NewSTClaimBatcher()

func goldenProof() []stabi.LedgerTreeProofNode {
	return []stabi.LedgerTreeProofNode{
		{Hash: crypto.Keccak256Hash([]byte("urnetwork-d17-4-proof-1")), Sum: big.NewInt(11)},
		{Hash: crypto.Keccak256Hash([]byte("urnetwork-d17-4-proof-2")), Sum: big.NewInt(22)},
	}
}

func goldenItem(noID, rootIndex, index uint64, cumulative string) stabi.STClaimBatcherClaim {
	total, ok := new(big.Int).SetString(cumulative, 10)
	if !ok {
		panic(cumulative)
	}
	return stabi.STClaimBatcherClaim{
		NoId:      new(big.Int).SetUint64(noID),
		RootIndex: new(big.Int).SetUint64(rootIndex),
		Account:   stabi.LedgerTreeAccount{Index: new(big.Int).SetUint64(index), Coldkey: goldenColdkey, Cumulative: total},
		Proof:     goldenProof(),
	}
}

func main() {
	// The synthetic relayer key, identical in the golden test.
	keyBytes := sha256.Sum256([]byte("urnetwork d17-4 claim-daemon-v2 golden relayer"))
	key, err := crypto.ToECDSA(keyBytes[:])
	if err != nil {
		panic(err)
	}
	keyHex := hexutil.Encode(crypto.FromECDSA(key))

	first := goldenItem(7, 70, 1, goldenCumulative)
	second := goldenItem(7, 70, 2, goldenCumulative2)
	vaultCalldata := stSettlementVault2.PackClaim(first.NoId, first.RootIndex, first.Account, first.Proof)
	batchCalldata := stClaimBatcher.PackClaimMany(common.HexToAddress(goldenVault), []stabi.STClaimBatcherClaim{first, second}, big.NewInt(240_000))

	fmt.Printf("relayer key:          %s\n", keyHex)
	fmt.Printf("relayer address:      %s\n", crypto.PubkeyToAddress(key.PublicKey).Hex())
	fmt.Printf("vault calldata:       %s\n", hexutil.Encode(vaultCalldata))
	fmt.Printf("batch calldata:       %s\n", hexutil.Encode(batchCalldata))

	signer := types.LatestSignerForChainID(new(big.Int).SetUint64(goldenChainID))
	for _, target := range []struct {
		name     string
		to       common.Address
		calldata []byte
	}{
		{"vault", common.HexToAddress(goldenVault), vaultCalldata},
		{"batch", common.HexToAddress(goldenBatcher), batchCalldata},
	} {
		tx := types.NewTx(&types.LegacyTx{Nonce: 0, GasPrice: big.NewInt(1), Gas: 2_600_000, To: &target.to, Value: big.NewInt(0), Data: target.calldata})
		signed, err := types.SignTx(tx, signer, key)
		if err != nil {
			panic(err)
		}
		raw, err := signed.MarshalBinary()
		if err != nil {
			panic(err)
		}
		fmt.Printf("signed tx %-6s raw:  0x%s\n", target.name, hex.EncodeToString(raw))
		fmt.Printf("signed tx %-6s hash: %s\n", target.name, signed.Hash().Hex())
	}
}
