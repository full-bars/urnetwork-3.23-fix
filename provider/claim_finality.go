package main

import (
	"context"
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"

	"github.com/urfoundation/sn/miner/onchain"
)

// Claims execute in the EVM block domain. Native finalized heights cannot
// select contract state, nonce state, or establish EVM receipt finality.
func claimFinalizedEVM(ctx context.Context, client *ethclient.Client) (onchain.EVMBlockIdentity, error) {
	empty := onchain.EVMBlockIdentity{}
	if ctx == nil || client == nil {
		return empty, errors.New("claim EVM finality reader is unavailable")
	}
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	head, err := onchain.ReadEVMBlockIdentity(ctx, client, big.NewInt(int64(rpc.FinalizedBlockNumber)))
	if err != nil {
		return empty, fmt.Errorf("read claim EVM finalized head: %w", err)
	}
	if err := checkClaimEVMCanonical(ctx, client, head); err != nil {
		return empty, err
	}
	return head, nil
}

// Finality itself must still hold after dependent reads. A canonical block
// alone is not a finalized block. Normal advancement retains the original
// state snapshot; regression or an unavailable finalized tag cannot publish it.
func closeClaimFinalizedEVM(ctx context.Context, client *ethclient.Client, original onchain.EVMBlockIdentity) error {
	current, err := claimFinalizedEVM(ctx, client)
	if err != nil {
		return err
	}
	if current.Number < original.Number {
		return errors.New("claim EVM finalized head regressed below the observed state")
	}
	if current == original {
		return ctx.Err()
	}
	return checkClaimEVMCanonical(ctx, client, original)
}

// Open and close the same endpoint's exact finalized or receipt identity.
// A failed read stays a transport/decoder error, not evidence of a reorganization.
func checkClaimEVMCanonical(ctx context.Context, client *ethclient.Client, block onchain.EVMBlockIdentity) error {
	if ctx == nil || client == nil || block.Number == 0 || block.Hash == (common.Hash{}) {
		return errors.New("claim EVM canonical identity is incomplete")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	canonical, err := onchain.ReadEVMBlockIdentity(ctx, client, new(big.Int).SetUint64(block.Number))
	if err != nil {
		return fmt.Errorf("read claim EVM canonical block: %w", err)
	}
	if canonical != block {
		return errors.New("claim EVM block is not canonical at its original height")
	}
	return ctx.Err()
}
