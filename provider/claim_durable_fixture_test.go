//go:build linux || darwin

// Only synthetic tests provision their owned private roots. Runtime queue
// admission has no enrollment, absent-context or pathname-creation fallback.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/urnetwork/connect/internal/durablefixture"
	"github.com/urnetwork/connect/internal/durablehead"
)

// Existing checkpoint metadata is never replaced, including negative fixtures.
// Deliberately prewritten legacy-format fixtures are adopted only by this
// explicit test preflight, before an actual runtime owner is constructed.
func claimQueueTestContext(t testing.TB, ctx context.Context, paths ...string) context.Context {
	t.Helper()
	var roots []string
	for _, path := range paths {
		physical, err := canonicalClaimStateDirectory(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(physical, 0700); err != nil {
			t.Fatal(err)
		}
		roots = append(roots, physical)
		attribute := durablehead.Attribute("provider-claim-queue", "claim-queue.json")
		if _, err := unix.Getxattr(physical, attribute, nil); err == nil {
			continue
		}
		var stat unix.Stat_t
		if err := unix.Stat(physical, &stat); err != nil {
			t.Fatal(err)
		}
		checkpoint := durablehead.Checkpoint{Schema: durablehead.Schema, Kind: "provider-claim-queue", Name: "claim-queue.json", MaximumBytes: maximumClaimQueueBytes, DirectoryInode: stat.Ino}
		filePath := filepath.Join(physical, "claim-queue.json")
		err = unix.Lstat(filePath, &stat)
		if err == nil && stat.Mode&syscall.S_IFMT == syscall.S_IFREG && stat.Mode&0077 == 0 && stat.Size <= maximumClaimQueueBytes {
			raw, err := os.ReadFile(filePath)
			if err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256(raw)
			checkpoint.Committed = durablehead.Member{Present: true, Inode: stat.Ino, Size: int64(len(raw)), Sha256: hex.EncodeToString(digest[:])}
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		raw, err := json.Marshal(checkpoint)
		if err != nil {
			t.Fatal(err)
		}
		if err := unix.Setxattr(physical, attribute, raw, unix.XATTR_CREATE); err != nil {
			t.Fatal(err)
		}
		directory, err := os.Open(physical)
		if err != nil {
			t.Fatal(err)
		}
		if err := errors.Join(directory.Sync(), directory.Close()); err != nil {
			t.Fatal(err)
		}
	}
	return durablefixture.New(t, ctx, roots...).Context
}

// A swarm's explicit fixture census is independent of its runtime discovery.
func claimSwarmTestContext(t testing.TB, ctx context.Context, swarm *ClaimSwarm) context.Context {
	t.Helper()
	var roots []string
	for _, member := range swarm.config.Members {
		cfg, err := LoadClaimDaemonConfig(member.ConfigPath)
		if err != nil {
			t.Fatal(err)
		}
		roots = append(roots, cfg.StateDir)
	}
	return claimQueueTestContext(t, ctx, roots...)
}

// Earlier transport/semantic fixtures now hold actual declared custody during
// their replay calls. This helper never appears in a production source file.
func claimRetainedTestStore(t *testing.T, cfg *ClaimDaemonConfig, entry *ClaimQueueEntry) *claimQueueStore {
	t.Helper()
	path := cfg.StateDir
	if path == "" {
		path = filepath.Join(t.TempDir(), "claims")
	}
	store := newClaimQueueTestStore(t, path)
	queue := &ClaimQueue{Schema: "urnetwork-provider-claim-queue-v1", LastDiscovered: entry.Epoch, Entries: map[string]*ClaimQueueEntry{fmt.Sprint(entry.Epoch): entry}}
	if err := store.save(queue); err != nil {
		store.close()
		t.Fatal(err)
	}
	return store
}

func reconcileClaimEntryTest(t *testing.T, ctx context.Context, cfg *ClaimDaemonConfig, api claimAPI, entry *ClaimQueueEntry) (string, error) {
	t.Helper()
	store := claimRetainedTestStore(t, cfg, entry)
	defer store.close()
	return reconcileClaimEntry(ctx, cfg, api, entry, store)
}

func reconcileSignedClaimTest(t *testing.T, ctx context.Context, cfg *ClaimDaemonConfig, entry *ClaimQueueEntry) (string, error) {
	t.Helper()
	store := claimRetainedTestStore(t, cfg, entry)
	defer store.close()
	return reconcileSignedClaim(ctx, cfg, entry, store)
}

func rebroadcastSignedClaimTest(t *testing.T, ctx context.Context, cfg *ClaimDaemonConfig, tx *types.Transaction, from common.Address) (bool, error) {
	t.Helper()
	raw, err := tx.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	entry := &ClaimQueueEntry{Epoch: 0, Status: "uncertain", TxHash: tx.Hash().Hex(), RawTxHex: "0x" + hex.EncodeToString(raw)}
	store := claimRetainedTestStore(t, cfg, entry)
	defer store.close()
	return rebroadcastSignedClaim(ctx, cfg, tx, from, store)
}
