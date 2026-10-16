// v2 claims read the public ledger account endpoint (§8.3), then verify each
// proof locally against the finalized on-chain root before anything is signed.
package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/holiman/uint256"
	"github.com/urnetwork/connect"

	"github.com/urfoundation/sn/evmrpc"
	"github.com/urfoundation/sn/merkle"
	"github.com/urfoundation/sn/miner/onchain"
	"github.com/urfoundation/sn/stabi"
)

var stSettlementVault2 = stabi.NewSTSettlementVault2()

var stClaimBatcher = stabi.NewSTClaimBatcher()

// The generated ABIs parse, as NewSTSettlementVault2 and NewSTClaimBatcher
// already require.
var stSettlementVault2Claim = func() abi.Method {
	parsed, err := stabi.STSettlementVault2MetaData.ParseABI()
	if err != nil {
		panic(err)
	}
	return parsed.Methods["claim"]
}()

var stClaimBatcherClaimMany = func() abi.Method {
	parsed, err := stabi.STClaimBatcherMetaData.ParseABI()
	if err != nil {
		panic(err)
	}
	return parsed.Methods["claimMany"]
}()

var errClaimV2Proof = errors.New("v2 claim proof does not verify against a finalized on-chain root")

// One finalized account of a configured coldkey in the configured vault.
type claimAccountV2 struct {
	Vault      string
	NoID       uint64
	Index      uint64
	RootIndex  uint64
	Coldkey    [32]byte
	Cumulative uint256.Int
	Claimed    uint256.Int
	Proof      []merkle.LedgerNode
}

// Every configured coldkey's finalized accounts, keyed by unit. An account
// only in the pending root, or in another vault, has nothing to claim here.
func readClaimSnapshotV2(ctx context.Context, strategy *connect.ClientStrategy, cfg *ClaimDaemonV2Config) (map[string]*claimAccountV2, error) {
	snapshot := map[string]*claimAccountV2{}
	for _, coldkey := range cfg.coldkeys {
		accounts, err := retryClaimApiRead(ctx, claimReadRetryHooks{}, func(ctx context.Context) ([]*claimAccountV2, error) {
			body, err := connect.HttpGetWithStrategyRaw(ctx, strategy, cfg.APIURL+"/sn/ledger/account?coldkey=0x"+hex.EncodeToString(coldkey[:]), "")
			if err != nil {
				return nil, err
			}
			return parseClaimAccountsV2(body, cfg.Vault, coldkey)
		})
		if err != nil {
			return nil, err
		}
		for _, account := range accounts {
			snapshot[claimUnitV2Key(account.Vault, account.NoID, account.Index)] = account
		}
	}
	return snapshot, nil
}

// The pinned GET /sn/ledger/account body. Amounts are canonical decimal rao.
func parseClaimAccountsV2(body []byte, vault string, coldkey [32]byte) ([]*claimAccountV2, error) {
	var response struct {
		Coldkey  string `json:"coldkey"`
		Accounts []struct {
			Vault      string  `json:"vault"`
			NoID       uint64  `json:"no_id"`
			Index      uint64  `json:"index"`
			RootIndex  *uint64 `json:"root_index"`
			Cumulative string  `json:"cumulative"`
			Proof      []struct {
				Hash string `json:"hash"`
				Sum  string `json:"sum"`
			} `json:"proof"`
			Claimed string `json:"claimed"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, err
	}
	if response.Coldkey != "0x"+hex.EncodeToString(coldkey[:]) || response.Accounts == nil {
		return nil, errors.New("v2 account response names another coldkey or lists no accounts")
	}
	accounts := []*claimAccountV2{}
	for _, item := range response.Accounts {
		if item.RootIndex == nil || !strings.EqualFold(item.Vault, vault) {
			continue
		}
		account := &claimAccountV2{Vault: vault, NoID: item.NoID, Index: item.Index, RootIndex: *item.RootIndex, Coldkey: coldkey, Proof: make([]merkle.LedgerNode, len(item.Proof))}
		err := errors.Join(parseClaimAmountV2(item.Cumulative, &account.Cumulative), parseClaimAmountV2(item.Claimed, &account.Claimed))
		for index, node := range item.Proof {
			hash, hashErr := hexutil.Decode(node.Hash)
			if hashErr == nil && len(hash) != common.HashLength {
				hashErr = fmt.Errorf("proof hash %q is not 32 bytes", node.Hash)
			}
			copy(account.Proof[index].Hash[:], hash)
			err = errors.Join(err, hashErr, parseClaimAmountV2(node.Sum, &account.Proof[index].Sum))
		}
		if err != nil {
			return nil, fmt.Errorf("v2 account %d/%d is malformed: %w", item.NoID, item.Index, err)
		}
		accounts = append(accounts, account)
	}
	return accounts, nil
}

func parseClaimAmountV2(text string, value *uint256.Int) error {
	if err := value.SetFromDecimal(text); err != nil || value.Dec() != text {
		return fmt.Errorf("amount %q is not a canonical decimal", text)
	}
	return nil
}

// The on-chain claimed watermark of one account, read at the finalized block
// whose root the account's proof must reproduce. Endpoints fail over as v1's.
func readClaimedV2(ctx context.Context, cfg *ClaimDaemonV2Config, account *claimAccountV2) (*uint256.Int, error) {
	var failures []error
	for _, endpoint := range cfg.RPC {
		claimed, err := readClaimedV2At(ctx, cfg, endpoint, account)
		if err == nil {
			return claimed, nil
		}
		failures = append(failures, fmt.Errorf("%s: %w", endpoint, err))
	}
	return nil, errors.Join(failures...)
}

// The first RPC endpoint that serves the configured chain, failing over as
// v1's reads do.
func dialClaimV2(ctx context.Context, cfg *ClaimDaemonV2Config) (*ethclient.Client, error) {
	var failures []error
	for _, endpoint := range cfg.RPC {
		client, err := evmrpc.DialContext(ctx, endpoint)
		if err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", endpoint, err))
			continue
		}
		chainID, err := client.ChainID(ctx)
		if err == nil && chainID.Cmp(new(big.Int).SetUint64(cfg.ChainID)) == 0 {
			return client, nil
		}
		client.Close()
		failures = append(failures, fmt.Errorf("%s: chain id %v (%v), want %d", endpoint, chainID, err, cfg.ChainID))
	}
	return nil, errors.Join(failures...)
}

func readClaimedV2At(ctx context.Context, cfg *ClaimDaemonV2Config, endpoint string, account *claimAccountV2) (*uint256.Int, error) {
	client, err := evmrpc.DialContext(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	defer client.Close()
	chainID, err := client.ChainID(ctx)
	if err != nil {
		return nil, err
	}
	if chainID.Cmp(new(big.Int).SetUint64(cfg.ChainID)) != 0 {
		return nil, fmt.Errorf("chain id %s, want %d", chainID, cfg.ChainID)
	}
	finalized, err := claimFinalizedEVM(ctx, client)
	if err != nil {
		return nil, err
	}
	vault, at := common.HexToAddress(cfg.Vault), new(big.Int).SetUint64(finalized.Number)
	noID, rootIndex := new(big.Int).SetUint64(account.NoID), new(big.Int).SetUint64(account.RootIndex)
	count, countErr := callClaimV2(ctx, client, vault, at, stSettlementVault2.PackRootCount(noID), stSettlementVault2.UnpackRootCount)
	root, rootErr := callClaimV2(ctx, client, vault, at, stSettlementVault2.PackRoots(noID, rootIndex), stSettlementVault2.UnpackRoots)
	claimed, claimedErr := callClaimV2(ctx, client, vault, at, stSettlementVault2.PackClaimed(noID, new(big.Int).SetUint64(account.Index)), stSettlementVault2.UnpackClaimed)
	if err := errors.Join(countErr, rootErr, claimedErr); err != nil {
		return nil, err
	}
	if err := closeClaimFinalizedEVM(ctx, client, finalized); err != nil {
		return nil, err
	}
	// ABI uint256 values always fit.
	total, _ := uint256.FromBig(root.Total)
	if count.Cmp(rootIndex) <= 0 || !merkle.VerifyLedgerProof(root.Root, total, root.LeafCount, account.Index, account.Coldkey, &account.Cumulative, account.Proof) {
		return nil, errClaimV2Proof
	}
	value, _ := uint256.FromBig(claimed)
	return value, nil
}

func callClaimV2[T any](ctx context.Context, client *ethclient.Client, vault common.Address, at *big.Int, data []byte, unpack func([]byte) (T, error)) (T, error) {
	out, err := client.CallContract(ctx, ethereum.CallMsg{To: &vault, Data: data}, at)
	if err != nil {
		var zero T
		return zero, err
	}
	return unpack(out)
}

type claimIntentV2 struct {
	NoID      *big.Int
	RootIndex *big.Int
	Account   stabi.LedgerTreeAccount
	Proof     []stabi.LedgerTreeProofNode
	// Items is the claim count of a batch through the claim batcher, and
	// Item this claim's place in it; Items is 0 for a claim sent to vault2.
	Item, Items int
}

// The account a signed claim names; its watermark is re-read after a revert.
func (self *claimIntentV2) account(vault string) *claimAccountV2 {
	account := &claimAccountV2{Vault: vault, NoID: self.NoID.Uint64(), Index: self.Account.Index.Uint64(), RootIndex: self.RootIndex.Uint64(), Coldkey: self.Account.Coldkey, Proof: make([]merkle.LedgerNode, len(self.Proof))}
	account.Cumulative.SetFromBig(self.Account.Cumulative)
	for index, node := range self.Proof {
		account.Proof[index].Hash = node.Hash
		account.Proof[index].Sum.SetFromBig(node.Sum)
	}
	return account
}

// The account's claim arguments, for vault2 or the claim batcher.
func claimV2Item(account *claimAccountV2) stabi.STClaimBatcherClaim {
	proof := make([]stabi.LedgerTreeProofNode, len(account.Proof))
	for index, node := range account.Proof {
		proof[index] = stabi.LedgerTreeProofNode{Hash: node.Hash, Sum: node.Sum.ToBig()}
	}
	leaf := stabi.LedgerTreeAccount{Index: new(big.Int).SetUint64(account.Index), Coldkey: account.Coldkey, Cumulative: account.Cumulative.ToBig()}
	return stabi.STClaimBatcherClaim{NoId: new(big.Int).SetUint64(account.NoID), RootIndex: new(big.Int).SetUint64(account.RootIndex), Account: leaf, Proof: proof}
}

func claimV2Calldata(account *claimAccountV2) []byte {
	item := claimV2Item(account)
	return stSettlementVault2.PackClaim(item.NoId, item.RootIndex, item.Account, item.Proof)
}

// Repacking excludes trailing bytes and alternative encodings, as in v1.
func decodeClaimV2Calldata(data []byte) (*claimIntentV2, error) {
	if len(data) < 4 || !bytes.Equal(data[:4], stSettlementVault2Claim.ID) {
		return nil, errors.New("calldata is not a v2 vault claim")
	}
	values, err := stSettlementVault2Claim.Inputs.Unpack(data[4:])
	if err != nil {
		return nil, err
	}
	intent := &claimIntentV2{
		NoID:      abi.ConvertType(values[0], new(big.Int)).(*big.Int),
		RootIndex: abi.ConvertType(values[1], new(big.Int)).(*big.Int),
		Account:   *abi.ConvertType(values[2], new(stabi.LedgerTreeAccount)).(*stabi.LedgerTreeAccount),
		Proof:     *abi.ConvertType(values[3], new([]stabi.LedgerTreeProofNode)).(*[]stabi.LedgerTreeProofNode),
	}
	if !bytes.Equal(stSettlementVault2.PackClaim(intent.NoID, intent.RootIndex, intent.Account, intent.Proof), data) {
		return nil, errors.New("v2 claim calldata is not canonical")
	}
	return intent, nil
}

// A batch's calldata, canonical as v2 claims' is: claimMany of the
// configured vault, whose claim of the unit's account it returns as the
// intent, with its place in the batch.
func decodeClaimV2BatchCalldata(data []byte, vault string, unit *ClaimUnitV2) (*claimIntentV2, error) {
	method := stClaimBatcherClaimMany
	if len(data) < 4 || !bytes.Equal(data[:4], method.ID) {
		return nil, errors.New("calldata is not a v2 claim batch")
	}
	values, err := method.Inputs.Unpack(data[4:])
	if err != nil {
		return nil, err
	}
	target := abi.ConvertType(values[0], new(common.Address)).(*common.Address)
	items := *abi.ConvertType(values[1], new([]stabi.STClaimBatcherClaim)).(*[]stabi.STClaimBatcherClaim)
	perClaim := abi.ConvertType(values[2], new(big.Int)).(*big.Int)
	if !bytes.Equal(stClaimBatcher.PackClaimMany(*target, items, perClaim), data) {
		return nil, errors.New("v2 claim batch calldata is not canonical")
	}
	if !strings.EqualFold(target.Hex(), vault) {
		return nil, errors.New("v2 claim batch names another vault")
	}
	var intent *claimIntentV2
	for index, item := range items {
		if item.NoId.IsUint64() && item.NoId.Uint64() == unit.NoID && item.Account.Index.IsUint64() && item.Account.Index.Uint64() == unit.Index {
			if intent != nil {
				return nil, errors.New("v2 claim batch names the account twice")
			}
			intent = &claimIntentV2{NoID: item.NoId, RootIndex: item.RootIndex, Account: item.Account, Proof: item.Proof, Item: index, Items: len(items)}
		}
	}
	if intent == nil {
		return nil, errors.New("signed v2 claim calldata names another account")
	}
	return intent, nil
}

// The retained bytes must be exactly a relayer-signed claim of this unit on
// the configured chain and vault, sent to vault2 or in a batch to the
// configured claim batcher.
func authenticateSignedClaimV2(cfg *ClaimDaemonV2Config, unit *ClaimUnitV2) (*types.Transaction, *claimIntentV2, common.Address, error) {
	refuse := func(reason string) (*types.Transaction, *claimIntentV2, common.Address, error) {
		return nil, nil, common.Address{}, errors.New("signed v2 claim " + reason)
	}
	var tx types.Transaction
	raw, err := hexutil.Decode(unit.RawTxHex)
	if err != nil || tx.UnmarshalBinary(raw) != nil || !strings.EqualFold(tx.Hash().Hex(), unit.TxHash) {
		return refuse("bytes do not decode to their retained hash")
	}
	if tx.ChainId().Cmp(new(big.Int).SetUint64(cfg.ChainID)) != 0 || tx.To() == nil || tx.Value().Sign() != 0 {
		return refuse("has another chain, target or value")
	}
	var intent *claimIntentV2
	switch {
	case strings.EqualFold(tx.To().Hex(), cfg.Vault):
		intent, err = decodeClaimV2Calldata(tx.Data())
	case cfg.Batcher != "" && strings.EqualFold(tx.To().Hex(), cfg.Batcher):
		intent, err = decodeClaimV2BatchCalldata(tx.Data(), cfg.Vault, unit)
	default:
		return refuse("has another chain, target or value")
	}
	if err != nil {
		return nil, nil, common.Address{}, err
	}
	if !intent.NoID.IsUint64() || intent.NoID.Uint64() != unit.NoID || !intent.Account.Index.IsUint64() || intent.Account.Index.Uint64() != unit.Index {
		return refuse("calldata names another account")
	}
	from, err := types.Sender(types.LatestSignerForChainID(tx.ChainId()), &tx)
	key, keyErr := onchain.LoadKeyFile(cfg.KeyFile)
	if err != nil || keyErr != nil || crypto.PubkeyToAddress(key.PublicKey) != from {
		return refuse("signer is not the configured relayer")
	}
	return &tx, intent, from, nil
}

// A batched claim the batcher reports paid must have exactly one canonical
// Claimed event of its account from the vault, matching the signed intent,
// with the batcher as caller and the batcher's amount.
func verifyBatchedClaimV2Receipt(vault common.Address, tx *types.Transaction, intent *claimIntentV2, amount *big.Int, receipt *types.Receipt) (*stabi.STSettlementVault2Claimed, error) {
	var claimed *stabi.STSettlementVault2Claimed
	for _, log := range receipt.Logs {
		if log == nil || log.Address != vault {
			continue
		}
		event, err := stSettlementVault2.UnpackClaimedEvent(log)
		if err != nil || event.NoId.Cmp(intent.NoID) != 0 || event.Index.Cmp(intent.Account.Index) != 0 {
			continue
		}
		if claimed != nil || log.Removed || log.TxHash != receipt.TxHash || log.BlockHash != receipt.BlockHash || log.BlockNumber != receipt.BlockNumber.Uint64() || log.TxIndex != receipt.TransactionIndex ||
			event.RootIndex.Cmp(intent.RootIndex) != 0 || event.Coldkey != intent.Account.Coldkey || event.Cumulative.Cmp(intent.Account.Cumulative) != 0 || event.Amount.Cmp(amount) != 0 || event.Caller != *tx.To() {
			return nil, errors.New("canonical v2 Claimed event differs from the exact batched intent")
		}
		claimed = event
	}
	if claimed == nil {
		return nil, errors.New("exact batched v2 claim has no Claimed event")
	}
	return claimed, nil
}

// Exactly one canonical Claimed event must match the signed intent; its
// cumulative is then the account's claimed watermark.
func verifySignedClaimV2Receipt(tx *types.Transaction, intent *claimIntentV2, from common.Address, receipt *types.Receipt) (*stabi.STSettlementVault2Claimed, error) {
	var claimed *stabi.STSettlementVault2Claimed
	for _, log := range receipt.Logs {
		if log == nil || log.Address != *tx.To() {
			continue
		}
		event, err := stSettlementVault2.UnpackClaimedEvent(log)
		if err != nil {
			continue
		}
		if claimed != nil || log.Removed || log.TxHash != receipt.TxHash || log.BlockHash != receipt.BlockHash || log.BlockNumber != receipt.BlockNumber.Uint64() || log.TxIndex != receipt.TransactionIndex ||
			event.NoId.Cmp(intent.NoID) != 0 || event.RootIndex.Cmp(intent.RootIndex) != 0 || event.Index.Cmp(intent.Account.Index) != 0 || event.Coldkey != intent.Account.Coldkey ||
			event.Cumulative.Cmp(intent.Account.Cumulative) != 0 || event.Amount.Sign() <= 0 || event.Caller != from {
			return nil, errors.New("canonical v2 Claimed event differs from the exact signed intent")
		}
		claimed = event
	}
	if claimed == nil {
		return nil, errors.New("exact signed v2 claim has no Claimed event")
	}
	return claimed, nil
}
