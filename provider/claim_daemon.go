package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/durablevolume"
	"github.com/urnetwork/connect/internal/durablehead"
	"github.com/urnetwork/connect/internal/durablepath"
	"gopkg.in/yaml.v3"

	"github.com/urfoundation/sn/evmrpc"
	"github.com/urfoundation/sn/merkle"
	"github.com/urfoundation/sn/miner/onchain"
	"github.com/urfoundation/sn/protocol"
	"github.com/urfoundation/sn/ss58"
)

type ClaimDaemonConfig struct {
	SchemaVersion  int                         `yaml:"schema_version" json:"schema_version"`
	Release        string                      `yaml:"release" json:"release"`
	APIURL         string                      `yaml:"api_url" json:"api_url"`
	RPC            []string                    `yaml:"rpc" json:"rpc"`
	KeyFile        string                      `yaml:"key_file" json:"key_file"`
	LegacyColdkey  string                      `yaml:"legacy_coldkey,omitempty" json:"legacy_coldkey,omitempty"`
	JWTFile        string                      `yaml:"jwt_file,omitempty" json:"jwt_file,omitempty"`
	StateDir       string                      `yaml:"state_dir" json:"state_dir"`
	PollSeconds    int                         `yaml:"poll_seconds" json:"poll_seconds"`
	LookbackEpochs uint64                      `yaml:"lookback_epochs" json:"lookback_epochs"`
	ProgressPool   *protocol.ClaimProgressPool `yaml:"progress_pool,omitempty" json:"progress_pool,omitempty"`
	progress       *claimProgressOwner
}

func LoadClaimDaemonConfig(path string) (*ClaimDaemonConfig, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(abs)
	if err != nil {
		return nil, err
	}
	var cfg ClaimDaemonConfig
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err != nil {
			return nil, fmt.Errorf("claim daemon config contains trailing YAML: %w", err)
		}
		return nil, errors.New("claim daemon config contains multiple YAML documents")
	}
	base := filepath.Dir(abs)
	for name, value := range map[string]*string{"key_file": &cfg.KeyFile, "state_dir": &cfg.StateDir} {
		if *value == "" {
			return nil, fmt.Errorf("%s is empty", name)
		}
		if !filepath.IsAbs(*value) {
			*value = filepath.Join(base, *value)
		}
		*value = filepath.Clean(*value)
	}
	if cfg.JWTFile != "" {
		if !filepath.IsAbs(cfg.JWTFile) {
			cfg.JWTFile = filepath.Join(base, cfg.JWTFile)
		}
		cfg.JWTFile = filepath.Clean(cfg.JWTFile)
	}
	if cfg.PollSeconds == 0 {
		cfg.PollSeconds = 30
	}
	if cfg.LookbackEpochs == 0 {
		cfg.LookbackEpochs = 2
	}
	if cfg.SchemaVersion != 1 || cfg.Release != "1.0" || cfg.APIURL == "" || len(cfg.RPC) == 0 || cfg.PollSeconds < 5 || cfg.PollSeconds > 3600 || cfg.LookbackEpochs > 256 {
		return nil, errors.New("invalid release-1.0 claim daemon configuration")
	}
	if cfg.LegacyColdkey != "" {
		if _, err := ss58.DecodeWithPrefix(cfg.LegacyColdkey, ss58.BittensorPrefix); err != nil {
			return nil, fmt.Errorf("legacy original coldkey: %w", err)
		}
	}
	if cfg.ProgressPool != nil {
		if err := cfg.ProgressPool.Validate(); err != nil {
			return nil, err
		}
	}
	if _, err := os.Stat(cfg.KeyFile); err != nil {
		return nil, fmt.Errorf("claim relayer key: %w", err)
	}
	if cfg.JWTFile != "" {
		info, err := os.Stat(cfg.JWTFile)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("claim JWT: %w", err)
		}
		if err == nil && (!info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0) {
			return nil, errors.New("claim JWT must be a private regular file")
		}
	}
	return &cfg, nil
}

type ClaimQueueEntry struct {
	Epoch              int64                      `json:"epoch"`
	Status             string                     `json:"status"`
	Attempts           int                        `json:"attempts"`
	ReconcileAttempts  int                        `json:"reconcile_attempts,omitempty"`
	UpdatedAt          string                     `json:"updated_at"`
	NextRetryAt        string                     `json:"next_retry_at,omitempty"`
	TxHash             string                     `json:"tx_hash,omitempty"`
	RawTxHex           string                     `json:"raw_tx_hex,omitempty"`
	FinalizedBlock     uint64                     `json:"finalized_block,omitempty"`
	FinalizedBlockHash string                     `json:"finalized_block_hash,omitempty"`
	ReceiptStatus      uint64                     `json:"receipt_status,omitempty"`
	ReceiptLogsHash    string                     `json:"receipt_logs_sha256,omitempty"`
	LastError          string                     `json:"last_error,omitempty"`
	PublicObservation  *protocol.ClaimObservation `json:"public_observation,omitempty"`
}

type ClaimQueue struct {
	Schema         string                      `json:"schema"`
	LastDiscovered int64                       `json:"last_discovered"`
	Entries        map[string]*ClaimQueueEntry `json:"entries"`
}

// One daemon owns the locked physical directory until every worker has joined.
// Methods are not concurrent-safe. Only a successful file and directory sync
// can establish the same-process acknowledgement used for unchanged saves.
type claimQueueStore struct {
	path      string
	directory *os.File
	guard     *durablepath.Directory
	head      *durablehead.Owner
	ctx       context.Context
	afterSync func(*os.File) error // observes real file and directory syncs
	uncertain bool
	savedHash [sha256.Size]byte
	saved     bool
	progress  *claimProgressOwner
}

// Resolve aliases once, then retain a directory descriptor and its process
// lock. The queue itself is replaced atomically and cannot serve as the lock.
func newClaimQueueStore(stateDir string, contexts ...context.Context) (*claimQueueStore, error) {
	if len(contexts) != 1 {
		return nil, errors.New("claim queue requires one explicit durable-volume context")
	}
	ctx := contexts[0]
	if err := durablepath.Require(ctx); err != nil {
		return nil, err
	}
	if err := claimQueuePlatformSupported(); err != nil {
		return nil, err
	}
	physical, err := canonicalClaimStateDirectory(stateDir)
	if err != nil {
		return nil, err
	}
	guard, err := durablepath.Open(ctx, physical, durablevolume.ReadWrite, false)
	if err != nil {
		return nil, err
	}
	store := &claimQueueStore{path: filepath.Join(physical, "claim-queue.json"), directory: guard.File(), guard: guard, ctx: ctx}
	fail := func(err error) (*claimQueueStore, error) { return nil, errors.Join(err, store.close()) }
	if err := claimQueueLockDirectory(store.directory); err != nil {
		return fail(err)
	}
	spec := durablehead.Spec{Kind: "provider-claim-queue", Name: "claim-queue.json", MaximumBytes: maximumClaimQueueBytes}
	store.head, err = durablehead.Open(ctx, guard, store.directory, spec)
	// The opener has no live worker. Keep its original lock during one exact
	// pending-byte reconciliation; missing/partial history is never rebuilt.
	if errors.Is(err, durablehead.ErrUncertain) {
		store.head, err = durablehead.Reconcile(ctx, guard, store.directory, spec)
	}
	if err != nil {
		return fail(err)
	}
	if err := store.requireOwner(); err != nil {
		return fail(err)
	}
	return store, nil
}

// Closing the descriptor releases ownership on normal return and every error
// path. A closed store cannot resume reading or writing retained custody.
func (self *claimQueueStore) close() error {
	if self != nil {
		self.progress.unavailable()
	}
	if self == nil || self.directory == nil {
		return nil
	}
	guard := self.guard
	self.directory, self.guard, self.saved = nil, nil, false
	err := self.head.Close()
	self.head = nil
	return errors.Join(err, guard.Close())
}

// Reads retain identity without imposing unused write reserve. Missing named
// bytes remain custody loss even when every other file under the root is intact.
func (self *claimQueueStore) requireOwner() error {
	if self == nil || self.head == nil || self.directory == nil {
		return errors.New("claim queue store has no durable owner")
	}
	if self.uncertain {
		return durablehead.ErrUncertain
	}
	return self.head.Check()
}

// The current request, owned checkpoint and write reserve separately gate
// signing/publication. Cached byte equality never bypasses this boundary.
func (self *claimQueueStore) requireWrite(ctx context.Context) error {
	if ctx == nil {
		return errors.New("claim queue request context is absent")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if self == nil || self.head == nil || self.directory == nil {
		return errors.New("claim queue store has no durable owner")
	}
	if self.uncertain {
		return durablehead.ErrUncertain
	}
	return errors.Join(self.head.CheckWrite(), ctx.Err())
}

// Startup reads only from the held physical namespace, before any network
// worker can consume signed state or seed the shared nonce floor.
func (self *claimQueueStore) load() (*ClaimQueue, error) {
	if err := self.requireOwner(); err != nil {
		return nil, err
	}
	b, present, err := self.head.Read()
	if err != nil {
		return nil, err
	}
	if !present {
		return &ClaimQueue{Schema: "urnetwork-provider-claim-queue-v1", LastDiscovered: -1, Entries: map[string]*ClaimQueueEntry{}}, nil
	}
	if err != nil {
		return nil, err
	}
	var q ClaimQueue
	if err := json.Unmarshal(b, &q); err != nil {
		return nil, err
	}
	if q.Schema != "urnetwork-provider-claim-queue-v1" || q.Entries == nil {
		return nil, errors.New("invalid claim queue schema")
	}
	// The child writes the exact signed RLP before broadcast. A submitting
	// record with those bytes is recoverable; without them, the child had not
	// yet reached the broadcast boundary and a fresh attempt is safe.
	for _, entry := range q.Entries {
		if entry == nil {
			return nil, errors.New("claim queue contains a nil entry")
		}
		if entry.Status == "submitting" {
			if entry.RawTxHex != "" || entry.TxHash != "" {
				entry.Status = "uncertain"
				entry.LastError = "process restarted after exact transaction preparation; automatic canonical reconciliation pending"
			} else {
				entry.Status = "retry"
				entry.LastError = "process restarted before the durable broadcast boundary"
			}
		}
	}
	return &q, nil
}

// Compare acknowledged bytes only after physical head admission. Lost or
// replaced files cannot be recreated from the daemon's process-local cache.
func (self *claimQueueStore) save(q *ClaimQueue) (saveErr error) {
	if self == nil {
		return errors.New("claim queue store is absent")
	}
	if q == nil || q.Entries == nil {
		return errors.New("claim queue observation has no owned inventory")
	}
	defer func() {
		if saveErr != nil {
			self.progress.unavailable()
		}
	}()
	if err := self.requireWrite(self.ctx); err != nil {
		return err
	}
	b, err := marshalClaimQueue(q)
	if err != nil {
		return err
	}
	committed, b, omitted, err := self.observationCapacity(q, b)
	if err != nil {
		return err
	}
	hash := sha256.Sum256(b)
	if self.saved && self.savedHash == hash {
		prior, present, err := self.head.Read()
		if err != nil {
			return err
		}
		if present && bytes.Equal(prior, b) {
			if err := self.requireWrite(self.ctx); err != nil {
				return err
			}
			adoptClaimObservationCapacity(q, committed)
			self.progress.acknowledge(committed, hash, omitted)
			return nil
		}
	}
	self.saved = false
	if err := self.head.Publish(b, self.afterSync); err != nil {
		self.uncertain = errors.Is(err, durablehead.ErrUncertain)
		return err
	}
	if err := self.requireWrite(self.ctx); err != nil {
		self.uncertain = true
		return errors.Join(durablehead.ErrUncertain, err)
	}
	self.savedHash, self.saved = hash, true
	adoptClaimObservationCapacity(q, committed)
	self.progress.acknowledge(committed, hash, omitted)
	return nil
}

func claimRetry(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := time.Minute << min(attempt-1, 6)
	if d > time.Hour {
		return time.Hour
	}
	return d
}

func discoverClaims(q *ClaimQueue, current int64, lookback uint64) {
	lastClaimable := current - 1
	if lastClaimable < 0 {
		return
	}
	start := q.LastDiscovered + 1
	if q.LastDiscovered < 0 {
		start = lastClaimable - int64(lookback) + 1
		if start < 0 {
			start = 0
		}
	}
	for epoch := start; epoch <= lastClaimable; epoch++ {
		key := fmt.Sprint(epoch)
		if q.Entries[key] == nil {
			q.Entries[key] = &ClaimQueueEntry{Epoch: epoch, Status: "pending", UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
		}
		q.LastDiscovered = epoch
	}
}

func claimTxHash(output string) string {
	const marker = "sent: tx "
	index := strings.Index(output, marker)
	if index < 0 {
		return ""
	}
	value := output[index+len(marker):]
	if end := strings.IndexAny(value, " \r\n\t"); end >= 0 {
		value = value[:end]
	}
	if strings.HasPrefix(value, "0x") && len(value) == 66 {
		if raw, err := hex.DecodeString(value[2:]); err == nil && len(raw) == common.HashLength {
			return strings.ToLower(value)
		}
	}
	return ""
}

func claimPreparedTx(output string) (string, string) {
	const marker = "prepared: tx "
	index := strings.Index(output, marker)
	if index < 0 {
		return "", ""
	}
	line := output[index+len(marker):]
	if end := strings.IndexAny(line, "\r\n"); end >= 0 {
		line = line[:end]
	}
	parts := strings.Split(line, " raw ")
	if len(parts) != 2 {
		return "", ""
	}
	hash := strings.ToLower(strings.TrimSpace(parts[0]))
	rawHex := strings.ToLower(strings.TrimSpace(parts[1]))
	if claimTxHash("sent: tx "+hash) == "" || !strings.HasPrefix(rawHex, "0x") {
		return "", ""
	}
	raw, err := hex.DecodeString(strings.TrimPrefix(rawHex, "0x"))
	if err != nil || len(raw) == 0 {
		return "", ""
	}
	var tx types.Transaction
	if tx.UnmarshalBinary(raw) != nil || !strings.EqualFold(tx.Hash().Hex(), hash) {
		return "", ""
	}
	return hash, rawHex
}

type boundedClaimOutput struct {
	mu           sync.Mutex
	buf          []byte
	onTxHash     func(string)
	onPrepared   func(string, string) error
	seenHash     string
	seenPrepared string
}

func (w *boundedClaimOutput) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.buf = append(w.buf, p...)
	if len(w.buf) > 64<<10 {
		w.buf = append([]byte(nil), w.buf[len(w.buf)-(64<<10):]...)
	}
	hash := claimTxHash(string(w.buf))
	preparedHash, preparedRaw := claimPreparedTx(string(w.buf))
	callback := w.onTxHash
	preparedCallback := w.onPrepared
	if hash != "" && hash != w.seenHash {
		w.seenHash = hash
	} else {
		hash = ""
	}
	if preparedHash != "" && preparedHash != w.seenPrepared {
		w.seenPrepared = preparedHash
	} else {
		preparedHash, preparedRaw = "", ""
	}
	w.mu.Unlock()
	if preparedHash != "" && preparedCallback != nil {
		if err := preparedCallback(preparedHash, preparedRaw); err != nil {
			return 0, err
		}
	}
	if hash != "" && callback != nil {
		callback(hash)
	}
	return len(p), nil
}

func (w *boundedClaimOutput) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return string(append([]byte(nil), w.buf...))
}

func executeClaim(ctx context.Context, executable string, cfg *ClaimDaemonConfig, entry *ClaimQueueEntry, onPrepared func(string, string) error, onTxHash func(string)) (string, error) {
	args := []string{"claim", fmt.Sprintf("--epoch=%d", entry.Epoch), "--api_url=" + cfg.APIURL, "--key_file=" + cfg.KeyFile}
	for _, endpoint := range cfg.RPC {
		args = append(args, "--rpc="+endpoint)
	}
	cmd := exec.CommandContext(ctx, executable, args...)
	output := &boundedClaimOutput{onTxHash: onTxHash, onPrepared: onPrepared}
	cmd.Stdout, cmd.Stderr = output, output
	err := cmd.Run()
	text := output.String()
	return text, err
}

func claimKey(noID []byte, coldkey []byte) ([32]byte, error) {
	var out [32]byte
	if len(noID) == 0 || len(noID) > 32 || len(coldkey) != 32 {
		return out, fmt.Errorf("invalid claim identity: no_id=%d bytes coldkey=%d bytes", len(noID), len(coldkey))
	}
	encoded := make([]byte, 64)
	copy(encoded[32-len(noID):32], noID)
	copy(encoded[32:], coldkey)
	copy(out[:], crypto.Keccak256(encoded))
	return out, nil
}

func claimCalldata(claim *connect.SnPoolClaimResult) (common.Address, []byte, error) {
	if claim == nil || len(claim.NoId) == 0 || len(claim.NoId) > 32 || len(claim.Coldkey) != 32 || len(claim.PayoutRoot) != 32 || claim.ShareBps <= 0 || claim.ShareBps > 10_000 {
		return common.Address{}, nil, errors.New("claim response has malformed identity, epoch, root, or share")
	}
	// The v33 claim result carries the contract address only; the daemon's
	// vault is that address.
	vault := claim.ContractAddress
	if !common.IsHexAddress(vault) {
		return common.Address{}, nil, errors.New("claim response has inconsistent settlement vault address")
	}
	noID := new(big.Int).SetBytes(claim.NoId)
	var coldkey, root [32]byte
	copy(coldkey[:], claim.Coldkey)
	copy(root[:], claim.PayoutRoot)
	proof := make([][32]byte, len(claim.Proof))
	for index, node := range claim.Proof {
		if len(node) != 32 {
			return common.Address{}, nil, fmt.Errorf("claim proof node %d has %d bytes", index, len(node))
		}
		copy(proof[index][:], node)
	}
	share := new(big.Int).SetUint64(uint64(claim.ShareBps))
	if !merkle.Verify(root, merkle.PayoutLeaf(coldkey, share), proof) {
		return common.Address{}, nil, errors.New("claim proof does not reconstruct the advertised payout root")
	}
	calldata, err := onchain.BuildClaimCalldata(onchain.ClaimIntent{E: new(big.Int).SetUint64(claim.Epoch), NoID: noID, Coldkey: coldkey, ShareBps: share, Proof: proof})
	if err != nil {
		return common.Address{}, nil, err
	}
	return common.HexToAddress(vault), calldata, nil
}

func queryClaimedFinalized(ctx context.Context, cfg *ClaimDaemonConfig, claim *connect.SnPoolClaimResult) (bool, error) {
	return queryClaimedFinalizedObserved(ctx, cfg, claim, nil)
}

// The optional observer receives only a closed actual finalized observation.
func queryClaimedFinalizedObserved(ctx context.Context, cfg *ClaimDaemonConfig, claim *connect.SnPoolClaimResult, observe func(*protocol.ClaimObservation)) (bool, error) {
	if claim == nil {
		return false, errors.New("claim response is nil")
	}
	if claim.Error != nil {
		return false, errors.New(claim.Error.Message)
	}
	// The v33 claim result carries the contract address only; the daemon's
	// vault is that address.
	vault := claim.ContractAddress
	if !common.IsHexAddress(vault) {
		return false, fmt.Errorf("invalid settlement vault address %q", vault)
	}
	key, err := claimKey(claim.NoId, claim.Coldkey)
	if err != nil {
		return false, err
	}
	epoch := new(big.Int).SetUint64(claim.Epoch)
	noID := new(big.Int).SetBytes(claim.NoId)
	data := stSettlementVault.PackLeafClaimed(epoch, key)
	var advertisedRoot [32]byte
	if len(claim.PayoutRoot) != 32 {
		return false, errors.New("claim response has no 32-byte payout root")
	}
	copy(advertisedRoot[:], claim.PayoutRoot)
	var failures []error
	for _, endpoint := range cfg.RPC {
		client, dialErr := evmrpc.DialContext(ctx, endpoint)
		if dialErr != nil {
			failures = append(failures, fmt.Errorf("%s: %w", endpoint, dialErr))
			continue
		}
		chainID, idErr := client.ChainID(ctx)
		if idErr != nil || chainID == nil || !chainID.IsUint64() || chainID.Uint64() != claim.ChainId {
			client.Close()
			failures = append(failures, fmt.Errorf("%s: chain id %v, want %d: %v", endpoint, chainID, claim.ChainId, idErr))
			continue
		}
		finalized, finalErr := claimFinalizedEVM(ctx, client)
		if finalErr != nil {
			client.Close()
			failures = append(failures, fmt.Errorf("%s: finalized head: %w", endpoint, finalErr))
			continue
		}
		address := common.HexToAddress(vault)
		entitlementData := stSettlementVault.PackEntitlement(epoch, noID)
		entitlementOut, entitlementErr := client.CallContract(ctx, ethereum.CallMsg{To: &address, Data: entitlementData}, new(big.Int).SetUint64(finalized.Number))
		if entitlementErr != nil {
			client.Close()
			failures = append(failures, fmt.Errorf("%s: entitlement: %w", endpoint, entitlementErr))
			continue
		}
		entitlement, entitlementErr := stSettlementVault.UnpackEntitlement(entitlementOut)
		if entitlementErr != nil {
			client.Close()
			failures = append(failures, fmt.Errorf("%s: decode finalized entitlement: %w", endpoint, entitlementErr))
			continue
		}
		if entitlement.PayoutRoot != advertisedRoot {
			if err := closeClaimFinalizedEVM(ctx, client, finalized); err != nil {
				client.Close()
				failures = append(failures, err)
				continue
			}
			client.Close()
			failures = append(failures, &claimArtifactRootMismatchError{epoch: int64(claim.Epoch), advertised: advertisedRoot, finalized: entitlement.PayoutRoot})
			continue
		}
		out, callErr := client.CallContract(ctx, ethereum.CallMsg{To: &address, Data: data}, new(big.Int).SetUint64(finalized.Number))
		if callErr == nil {
			callErr = closeClaimFinalizedEVM(ctx, client, finalized)
		}
		client.Close()
		if callErr != nil {
			failures = append(failures, fmt.Errorf("%s: leafClaimed: %w", endpoint, callErr))
			continue
		}
		claimed, unpackErr := stSettlementVault.UnpackLeafClaimed(out)
		if unpackErr != nil {
			failures = append(failures, fmt.Errorf("%s: decode leafClaimed: %w", endpoint, unpackErr))
			continue
		}
		if observe != nil {
			if observation := finalizedClaimObservation(claim, entitlement, finalized, claimed); observation != nil {
				observe(observation)
			}
		}
		return claimed, nil
	}
	return false, errors.Join(failures...)
}

// The v1 and v2 queues each prove that exact bytes are in acknowledged custody.
type claimSignedCustody interface {
	requireSigned(context.Context, *types.Transaction) error
}

// The caller already authenticated the exact signed transaction. Replaying
// those bytes needs finalized chain authority, not a retained historical API.
func rebroadcastSignedClaim(ctx context.Context, cfg *ClaimDaemonConfig, tx *types.Transaction, from common.Address, store claimSignedCustody) (bool, error) {
	return replaySignedClaim(ctx, cfg, tx, from, store, true)
}

// Only the v2 daemon replays without the preflight, and only bytes whose claim
// its finalized watermark already covers, which can only revert in the vault.
func replaySignedClaim(ctx context.Context, cfg *ClaimDaemonConfig, tx *types.Transaction, from common.Address, store claimSignedCustody, preflight bool) (bool, error) {
	if err := store.requireSigned(ctx, tx); err != nil {
		return false, err
	}
	var failures []error
	for _, endpoint := range cfg.RPC {
		client, dialErr := evmrpc.DialContext(ctx, endpoint)
		if dialErr != nil {
			failures = append(failures, dialErr)
			continue
		}
		chainID, idErr := client.ChainID(ctx)
		if idErr != nil || chainID == nil || chainID.Cmp(tx.ChainId()) != 0 {
			client.Close()
			failures = append(failures, fmt.Errorf("%s chain identity mismatch", endpoint))
			continue
		}
		finalized, finalErr := claimFinalizedEVM(ctx, client)
		if finalErr != nil {
			client.Close()
			failures = append(failures, finalErr)
			continue
		}
		nonce, nonceErr := client.NonceAt(ctx, from, new(big.Int).SetUint64(finalized.Number))
		if nonceErr != nil {
			client.Close()
			failures = append(failures, nonceErr)
			continue
		}
		if nonce > tx.Nonce() {
			if err := closeClaimFinalizedEVM(ctx, client, finalized); err != nil {
				client.Close()
				failures = append(failures, err)
				continue
			}
			client.Close()
			return true, nil
		}
		// The vault checks the saved epoch, proof and claim status at the
		// finalized head. A now-invalid intent remains a signed liability;
		// it is never converted into a new transaction or erased.
		if preflight {
			_, preflightErr := client.CallContract(ctx, ethereum.CallMsg{From: from, To: tx.To(), Gas: tx.Gas(), GasPrice: tx.GasPrice(), Value: tx.Value(), Data: tx.Data()}, new(big.Int).SetUint64(finalized.Number))
			if preflightErr != nil {
				client.Close()
				failures = append(failures, fmt.Errorf("finalized exact-claim preflight: %w", preflightErr))
				continue
			}
		}
		if err := closeClaimFinalizedEVM(ctx, client, finalized); err != nil {
			client.Close()
			failures = append(failures, err)
			continue
		}
		if err := store.requireSigned(ctx, tx); err != nil {
			client.Close()
			return false, err
		}
		sendErr := client.SendTransaction(ctx, tx)
		client.Close()
		if sendErr == nil || knownClaimTransaction(sendErr) {
			return false, nil
		}
		failures = append(failures, sendErr)
	}
	if len(failures) == 0 {
		return false, errors.New("exact claim replay has no RPC endpoint")
	}
	return false, errors.Join(failures...)
}

func knownClaimTransaction(err error) bool {
	if err == nil {
		return true
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "known transaction") || strings.Contains(message, "already known") || strings.Contains(message, "already imported") || strings.Contains(message, "nonce too low") || strings.Contains(message, "replacement transaction underpriced")
}

// Claim discovery needs only the epoch clock. Optional chain settings in the
// shared API response have independent wire types and are not claim authority.
func readClaimEpoch(ctx context.Context, strategy *connect.ClientStrategy, apiURL, byJWT string) (int64, error) {
	return readClaimEpochWithRetry(ctx, strategy, apiURL, byJWT, claimReadRetryHooks{})
}

// One retained read owns its original URL and credentials through recovery.
func readClaimEpochWithRetry(ctx context.Context, strategy *connect.ClientStrategy, apiURL, byJWT string, hooks claimReadRetryHooks) (int64, error) {
	return retryClaimApiRead(ctx, hooks, func(readCtx context.Context) (int64, error) {
		return readClaimEpochAttempt(readCtx, strategy, apiURL, byJWT)
	})
}

// Parsing remains inside the attempt so a complete malformed response is hard.
func readClaimEpochAttempt(ctx context.Context, strategy *connect.ClientStrategy, apiURL, byJWT string) (int64, error) {
	body, err := connect.HttpGetWithStrategyRaw(ctx, strategy, apiURL+"/sn/epoch", byJWT)
	if err != nil {
		return 0, err
	}
	var summary struct {
		Epoch *int64 `json:"epoch"`
	}
	if err := json.Unmarshal(body, &summary); err != nil {
		return 0, err
	}
	if summary.Epoch == nil || *summary.Epoch < 0 {
		return 0, errors.New("claim epoch response has no nonnegative epoch")
	}
	return *summary.Epoch, nil
}

type claimAPI interface {
	SnPoolClaimSyncWithContext(context.Context, *connect.SnPoolClaimArgs) (*connect.SnPoolClaimResult, error)
}

// The daemon calls reconciliation and submission inside one shared admission.
// Its finite context also bounds every multi-call finalized observation.
func reconcileClaimEntry(ctx context.Context, cfg *ClaimDaemonConfig, api claimAPI, entry *ClaimQueueEntry, store *claimQueueStore) (string, error) {
	if err := store.requireOwner(); err != nil {
		return "", err
	}
	if entry == nil || cfg == nil {
		return "", errors.New("claim reconciliation is unavailable")
	}
	if entry.TxHash != "" || entry.RawTxHex != "" {
		return reconcileSignedClaim(ctx, cfg, entry, store)
	}
	if api == nil {
		return "", errors.New("claim API is unavailable")
	}
	claim, err := readClaimPoolWithRetry(ctx, api, entry.Epoch, claimReadRetryHooks{})
	if err != nil {
		return "", err
	}
	if claim == nil {
		return "", errors.New("claim response is nil")
	}
	if claim.Error != nil {
		return "", errors.New(claim.Error.Message)
	}
	if claim.Epoch != uint64(entry.Epoch) {
		return "", errors.New("claim artifact epoch differs from the queued epoch")
	}
	if len(claim.NoId) == 0 {
		// Only an unsigned entry can become a terminal API zero payout.
		// Signed liabilities need exact canonical receipt evidence instead.
		entry.PublicObservation = absentClaimObservation(entry.Epoch)
		return "no-claim", nil
	}
	claimed, err := queryClaimedFinalizedObserved(ctx, cfg, claim, func(observation *protocol.ClaimObservation) { entry.PublicObservation = observation })
	if err != nil {
		return "", err
	}
	if claimed {
		return "finalized", nil
	}
	return "", nil
}

// Admission already covers reconciliation through finality. Never wait for
// another nonce lock after recording the durable submitting state.
func submitClaimDirect(ctx context.Context, cfg *ClaimDaemonConfig, api claimAPI, entry *ClaimQueueEntry, store *claimQueueStore, queue *ClaimQueue, admission *claimAdmission) error {
	if err := store.requireWrite(ctx); err != nil {
		return err
	}
	claim, err := readClaimPoolWithRetry(ctx, api, entry.Epoch, claimReadRetryHooks{})
	if err != nil {
		return err
	}
	if claim == nil {
		return errors.New("claim response is nil")
	}
	if claim.Error != nil {
		return errors.New(claim.Error.Message)
	}
	if claim.Epoch != uint64(entry.Epoch) {
		return errors.New("claim artifact epoch differs from the queued epoch")
	}
	vault, calldata, err := claimCalldata(claim)
	if err != nil {
		return err
	}
	if err := store.requireWrite(ctx); err != nil {
		return err
	}
	key, err := onchain.LoadKeyFile(cfg.KeyFile)
	if err != nil {
		return err
	}
	if err := admission.checkChain(new(big.Int).SetUint64(claim.ChainId)); err != nil {
		return err
	}
	receipt, err := onchain.SubmitWithHooks(ctx, onchain.SubmitParams{
		Contract: vault, Rpcs: cfg.RPC, Key: key, Calldata: calldata, NonceFloor: admission.nonceMinimum(),
		ChainID: new(big.Int).SetUint64(claim.ChainId),
		RuntimeAdmission: func(request context.Context, _ *ethclient.Client, number *big.Int) error {
			if number != nil {
				return errors.Join(request.Err(), store.requireOwner())
			}
			return store.requireWrite(request)
		},
	}, onchain.SubmitHooks{
		Prepared: func(hash common.Hash, raw []byte) error {
			priorHash, priorRaw := entry.TxHash, entry.RawTxHex
			entry.TxHash = strings.ToLower(hash.Hex())
			entry.RawTxHex = "0x" + hex.EncodeToString(raw)
			entry.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
			if saveErr := store.save(queue); saveErr != nil {
				// An uncertain checkpoint may already contain these exact
				// signed bytes. Preserve their nonce floor before this worker
				// releases the shared relayer, even before joined reconciliation.
				if errors.Is(saveErr, durablehead.ErrUncertain) {
					saveErr = errors.Join(saveErr, admission.rememberSigned(cfg, entry))
				}
				entry.TxHash, entry.RawTxHex = priorHash, priorRaw
				return saveErr
			}
			return admission.rememberSigned(cfg, entry)
		},
		BeforeBroadcast: func(hash common.Hash) error {
			tx, _, _, err := authenticateSignedClaim(cfg, entry)
			if err != nil {
				return err
			}
			if tx.Hash() != hash {
				return errors.New("claim handoff differs from retained prepared bytes")
			}
			return store.requireSigned(ctx, tx)
		},
		Broadcast: func(hash common.Hash) error {
			if !strings.EqualFold(entry.TxHash, hash.Hex()) {
				return errors.New("broadcast transaction hash differs from the durable prepared hash")
			}
			entry.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
			return store.save(queue)
		},
	})
	if err != nil {
		return err
	}
	tx, intent, from, err := authenticateSignedClaim(cfg, entry)
	if err != nil {
		return err
	}
	reconciled, err := finalizedClaimReceipt(ctx, cfg, entry.TxHash, tx.ChainId())
	if err != nil {
		return err
	}
	if receipt == nil || receipt.BlockNumber == nil || receipt.TxHash != reconciled.TxHash || receipt.BlockHash != reconciled.BlockHash || receipt.BlockNumber.Cmp(reconciled.BlockNumber) != 0 || receipt.TransactionIndex != reconciled.TransactionIndex {
		return errors.New("claim finality reconciliation changed the original receipt identity")
	}
	if err := verifySignedClaimReceipt(tx, intent, from, reconciled); err != nil {
		return err
	}
	if err := recordFinalizedClaimReceipt(entry, reconciled); err != nil {
		return err
	}
	entry.PublicObservation = signedClaimObservation(tx, intent, from, reconciled)
	return nil
}

// recordFinalizedClaimReceipt binds a successful queue entry to the exact
// canonical receipt which caused it. Raw signed transaction bytes alone prove
// intent but not inclusion, so the simulator's immutable evidence capture
// retains the finalized block and a canonical hash of every receipt log.
func recordFinalizedClaimReceipt(entry *ClaimQueueEntry, receipt *types.Receipt) error {
	if entry == nil || receipt == nil || receipt.BlockNumber == nil || !receipt.BlockNumber.IsUint64() || receipt.BlockNumber.Sign() <= 0 || receipt.BlockHash == (common.Hash{}) || receipt.TxHash == (common.Hash{}) {
		return errors.New("claim returned an incomplete finalized receipt")
	}
	if receipt.Status != types.ReceiptStatusSuccessful {
		return errors.New("claim finalized with a failed receipt")
	}
	if entry.TxHash == "" || !strings.EqualFold(entry.TxHash, receipt.TxHash.Hex()) {
		return errors.New("claim receipt transaction differs from the durable prepared transaction")
	}
	logs, err := json.Marshal(receipt.Logs)
	if err != nil {
		return fmt.Errorf("encode claim receipt logs: %w", err)
	}
	digest := sha256.Sum256(logs)
	entry.FinalizedBlock = receipt.BlockNumber.Uint64()
	entry.FinalizedBlockHash = strings.ToLower(receipt.BlockHash.Hex())
	entry.ReceiptStatus = receipt.Status
	entry.ReceiptLogsHash = "sha256:" + hex.EncodeToString(digest[:])
	return nil
}

// finalizedClaimReceipt recovers inclusion evidence after a crash between
// chain finality and the queue fsync. An endpoint must provide a canonical
// finalized block before its recovered receipt is accepted.
func finalizedClaimReceipt(ctx context.Context, cfg *ClaimDaemonConfig, txHash string, chainId *big.Int) (*types.Receipt, error) {
	if cfg == nil || chainId == nil || chainId.Sign() <= 0 {
		return nil, errors.New("claim receipt chain configuration is unavailable")
	}
	raw, err := hex.DecodeString(strings.TrimPrefix(txHash, "0x"))
	if err != nil || len(raw) != common.HashLength {
		return nil, fmt.Errorf("invalid claim transaction hash %q", txHash)
	}
	hash := common.BytesToHash(raw)
	var failures []error
	for _, endpoint := range cfg.RPC {
		client, dialErr := evmrpc.DialContext(ctx, endpoint)
		if dialErr != nil {
			failures = append(failures, dialErr)
			continue
		}
		observedChainId, idErr := client.ChainID(ctx)
		if idErr != nil || observedChainId == nil || observedChainId.Cmp(chainId) != 0 {
			client.Close()
			failures = append(failures, fmt.Errorf("claim receipt RPC chain identity mismatch: %v", idErr))
			continue
		}
		receipt, receiptErr := client.TransactionReceipt(ctx, hash)
		if receiptErr != nil {
			client.Close()
			failures = append(failures, receiptErr)
			continue
		}
		if receipt == nil || receipt.TxHash != hash || receipt.BlockNumber == nil || !receipt.BlockNumber.IsUint64() || receipt.BlockNumber.Sign() <= 0 || receipt.BlockHash == (common.Hash{}) {
			client.Close()
			failures = append(failures, errors.New("claim receipt has an incomplete or mismatched identity"))
			continue
		}
		finalized, finalErr := claimFinalizedEVM(ctx, client)
		if finalErr != nil || finalized.Number < receipt.BlockNumber.Uint64() {
			client.Close()
			if finalErr != nil {
				failures = append(failures, finalErr)
			} else {
				failures = append(failures, errors.New("claim receipt is not finalized"))
			}
			continue
		}
		block, blockErr := onchain.ReadEVMBlockIdentity(ctx, client, receipt.BlockNumber)
		if blockErr != nil {
			client.Close()
			failures = append(failures, blockErr)
			continue
		}
		if block.Hash != receipt.BlockHash {
			client.Close()
			return nil, fmt.Errorf("claim transaction %s receipt is not canonical", txHash)
		}
		// Close both inclusion and the separate finalized witness. A later
		// head may advance, but neither original coordinate may be replaced.
		if err := checkClaimEVMCanonical(ctx, client, block); err != nil {
			client.Close()
			failures = append(failures, err)
			continue
		}
		if err := closeClaimFinalizedEVM(ctx, client, finalized); err != nil {
			client.Close()
			failures = append(failures, err)
			continue
		}
		client.Close()
		return receipt, nil
	}
	if len(failures) == 0 {
		return nil, errors.New("claim receipt has no RPC endpoint")
	}
	return nil, errors.Join(failures...)
}

// Missing provider credentials are held; legacy mode requires an explicit
// original coldkey and never changes the destination of retained signed work.
func readClaimDaemonJWT(cfg *ClaimDaemonConfig) (string, error) {
	token, _, err := readSnCredentials(snCredentialSelection{jwtFile: cfg.JWTFile, legacyNetwork: cfg.LegacyColdkey != ""})
	return token, err
}

func runClaimDaemonWithAdmission(ctx context.Context, configPath string, admission *claimAdmission, initialDelay time.Duration, onReady func()) (runErr error) {
	if ctx == nil || admission == nil {
		return errors.New("claim daemon context or admission is unavailable")
	}
	if err := durablepath.Require(ctx); err != nil {
		return err
	}
	cfg, err := LoadClaimDaemonConfig(configPath)
	if err != nil {
		return err
	}
	store, err := newClaimQueueStore(cfg.StateDir, ctx)
	if err != nil {
		return err
	}
	defer func() { runErr = errors.Join(runErr, store.close()) }()
	return runClaimOwner(ctx, cfg, store, claimOwnerHooks{run: func(owner *claimQueueStore) error {
		return runClaimDaemonWithStore(ctx, cfg, owner, admission, initialDelay, onReady)
	}})
}

// The caller retains the queue owner until this function has joined all local
// work. A swarm supplies its already loaded config and already locked store.
func runClaimDaemonWithStore(ctx context.Context, cfg *ClaimDaemonConfig, store *claimQueueStore, admission *claimAdmission, initialDelay time.Duration, onReady func()) (runErr error) {
	store.progress = cfg.progress
	defer admission.forget(cfg.StateDir)
	if err := admission.seedMember(cfg, store); err != nil {
		return err
	}
	queue, err := store.load()
	if err != nil {
		return err
	}
	if err := store.save(queue); err != nil {
		return err
	}
	strategy := connect.NewClientStrategy(ctx, defaultClaimReadStrategySettings())
	defer strategy.Close()
	api := newDaemonApi(ctx, strategy, cfg.APIURL)
	// The connect handle owns no background workers; sdk's CloseAndWait waited
	// for the sdk jwt refresh worker, which the v33 lane does not have.
	defer api.Close()
	claims := &claimCredentialApi{api: api, selection: snCredentialSelection{jwtFile: cfg.JWTFile, legacyNetwork: cfg.LegacyColdkey != ""}, legacyColdkey: cfg.LegacyColdkey, onReady: onReady}
	credentialErr := claims.reload()
	if credentialErr != nil {
		retainedSigned := false
		for _, entry := range queue.Entries {
			if entry != nil && (entry.TxHash != "" || entry.RawTxHex != "") {
				retainedSigned = true
				break
			}
		}
		if !retainedSigned {
			return credentialErr
		}
	}
	if initialDelay > 0 {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(initialDelay):
		}
	}
	period := time.Duration(cfg.PollSeconds) * time.Second
	epochCtx, epochCancel := context.WithCancel(ctx)
	epochs, epochDone := startClaimEpochReader(epochCtx, period, func(ctx context.Context) (int64, error) {
		return readClaimEpoch(ctx, strategy, cfg.APIURL, "")
	})
	defer func() { epochCancel(); <-epochDone }()
	ticker := time.NewTicker(period)
	defer ticker.Stop()
	saveCheckpoint := func(queue *ClaimQueue) error {
		if err := store.save(queue); err != nil {
			return err
		}
		if hooks, ok := ctx.Value(claimDaemonCheckpointHooksKey{}).(claimDaemonCheckpointHooks); ok && hooks.afterCheckpoint != nil {
			hooks.afterCheckpoint(queue)
		}
		return nil
	}
	return runClaimQueue(ctx, queue, cfg.LookbackEpochs, epochs, ticker.C, func() <-chan struct{} {
		return admission.ready(cfg.StateDir)
	}, claimQueuePollHooks{
		now: time.Now, save: saveCheckpoint, latestEpoch: admission.observeEpoch,
		begin: func(ctx context.Context, candidates []claimPollCandidate) (int, context.Context, func(), error) {
			return beginClaimOperation(ctx, admission, cfg.StateDir, candidates)
		},
		reconcile: func(ctx context.Context, entry *ClaimQueueEntry) (string, error) {
			return reconcileClaimEntry(ctx, cfg, claims, entry, store)
		},
		submit: func(ctx context.Context, entry *ClaimQueueEntry) error {
			return submitClaimDirect(ctx, cfg, claims, entry, store, queue, admission)
		},
	})
}

// RunClaimDaemon runs the production claim worker using direct package calls;
// it never shells out to the provider or snclaim CLIs.
func RunClaimDaemon(ctx context.Context, configPath string) error {
	return runClaimDaemonWithAdmission(ctx, configPath, &claimAdmission{}, 0, nil)
}

func runClaimDaemon(ctx context.Context, configPath string) error {
	event := connect.NewEventWithContext(ctx)
	event.SetOnSignals(syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM)
	return RunClaimDaemon(event.Ctx(), configPath)
}

// A physical writer alone cannot authorize unrelated transaction bytes. The
// exact original signed RLP and hash must still exist in the acknowledged queue.
func (self *claimQueueStore) requireSigned(ctx context.Context, tx *types.Transaction) error {
	if err := self.requireWrite(ctx); err != nil {
		return err
	}
	if tx == nil {
		return errors.New("claim signed transaction is absent")
	}
	raw, err := tx.MarshalBinary()
	if err != nil {
		return err
	}
	queue, err := self.load()
	if err != nil {
		return err
	}
	matched := false
	for _, entry := range queue.Entries {
		if entry == nil || !strings.EqualFold(entry.TxHash, tx.Hash().Hex()) {
			continue
		}
		retained, err := hex.DecodeString(strings.TrimPrefix(entry.RawTxHex, "0x"))
		if err == nil && bytes.Equal(retained, raw) {
			matched = true
			break
		}
	}
	if !matched {
		return errors.New("claim exact signed transaction is absent from acknowledged custody")
	}
	return self.requireWrite(ctx)
}
