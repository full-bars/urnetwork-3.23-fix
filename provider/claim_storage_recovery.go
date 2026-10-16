// Storage recovery belongs to one joined claim worker. It does not replace
// shared nonce admission, reset claim counters, or restart sibling workers.
package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/urnetwork/connect/durablevolume"
	"github.com/urnetwork/connect/internal/durablehead"
)

const claimStorageReconciliations = 3

// A run callback must join its API and epoch workers before returning. The
// optional wait observes a real owner transition; it supplies no stored bytes.
type claimOwnerHooks struct {
	run   func(*claimQueueStore) error
	wait  func(context.Context, time.Duration) bool
	state func(error)
}

func claimStoragePending(err error) bool {
	return !errors.Is(err, durablevolume.ErrIdentity) &&
		(errors.Is(err, durablehead.ErrUncertain) || errors.Is(err, durablevolume.ErrUnavailable) || errors.Is(err, durablevolume.ErrBusy))
}

// Takes the initial store and closes every acquired generation. Resource
// pressure pauses the same owner; uncertain publication first joins and closes
// that owner, then permits at most three exact pending-byte reconciliations.
func runClaimOwner(ctx context.Context, cfg *ClaimDaemonConfig, store *claimQueueStore, hooks claimOwnerHooks) (runErr error) {
	if ctx == nil || cfg == nil || store == nil || hooks.run == nil {
		return errors.New("claim storage recovery requires context, config, owner and joined worker")
	}
	defer func() { runErr = errors.Join(runErr, store.close()) }()
	wait := func() bool {
		period := max(time.Second, time.Duration(cfg.PollSeconds)*time.Second)
		if hooks.wait != nil {
			return hooks.wait(ctx, period)
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(period):
			return true
		}
	}
	state := func(err error) {
		if hooks.state != nil {
			hooks.state(err)
		}
	}
	reconciliations := 0
	for ctx.Err() == nil {
		err := hooks.run(store)
		if err == nil || ctx.Err() != nil {
			return nil
		}
		state(err)
		if !claimStoragePending(err) {
			return err
		}
		if errors.Is(err, durablehead.ErrUncertain) {
			if closeErr := store.close(); closeErr != nil {
				return errors.Join(err, closeErr)
			}
			store = nil
			if reconciliations == claimStorageReconciliations {
				return fmt.Errorf("claim storage reconciliation budget exhausted: %w", err)
			}
			reconciliations++
		}
		for {
			if !wait() || ctx.Err() != nil {
				return nil
			}
			if store == nil {
				store, err = newClaimQueueStore(cfg.StateDir, ctx)
			} else {
				err = store.requireWrite(ctx)
			}
			if err == nil {
				break
			}
			state(err)
			// A reopened pending head that cannot reconcile exact actual bytes
			// remains stopped. No repeated scan can manufacture missing history.
			if !claimStoragePending(err) || errors.Is(err, durablehead.ErrUncertain) {
				return err
			}
		}
	}
	return nil
}
