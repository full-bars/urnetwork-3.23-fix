// Claim polls batch retry diagnostics while keeping every transaction boundary
// durable. The queue has one daemon owner; hooks expose time and side effects.
package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"
)

// Historical repair cannot consume an entire poll or turn faster persistence
// into an unbounded API burst. Current epochs and uncertain outcomes are separate.
const claimHistoricalReconciliationsPerPoll = 2
const claimHistoricalOutcomesPerPoll = 2

// Each callback runs synchronously under the daemon's queue ownership. The
// production callbacks own reconciliation through broadcast and finality.
type claimQueuePollHooks struct {
	now         func() time.Time
	latestEpoch func(int64) int64
	save        func(*ClaimQueue) error
	begin       func(context.Context, []claimPollCandidate) (int, context.Context, func(), error)
	reconcile   func(context.Context, *ClaimQueueEntry) (string, error)
	submit      func(context.Context, *ClaimQueueEntry) error
}

type claimPollCandidate struct {
	entry  *ClaimQueueEntry
	recent bool
}

// A reconciliation failure has its own bounded backoff: failed readiness reads
// do not increment the count of actual transaction submission attempts.
func deferClaimReconciliation(entry *ClaimQueueEntry, now time.Time, failure error, recent bool) {
	if entry.ReconcileAttempts < 0 {
		entry.ReconcileAttempts = 0
	}
	if entry.ReconcileAttempts < 7 {
		entry.ReconcileAttempts++
	}
	delay := claimRetry(entry.ReconcileAttempts)
	if recent || entry.Status == "uncertain" {
		// Current payout roots and exact transaction outcomes must not wait
		// behind the longer backoff assigned to historical repair work.
		delay = min(delay, time.Minute)
	}
	if entry.Status == "uncertain" {
		entry.LastError = "uncertain transaction reconciliation: " + failure.Error()
	} else {
		entry.Status = "retry"
		entry.LastError = "claim is not ready for finalized reconciliation: " + failure.Error()
	}
	entry.NextRetryAt = now.Add(delay).UTC().Format(time.RFC3339Nano)
	entry.UpdatedAt = now.UTC().Format(time.RFC3339Nano)
}

// Retry admission precedes any API/RPC call. Retry-only changes are coalesced
// into one checkpoint; submitting, signed bytes and final receipts never wait
// for that batch. Recent claims are considered before historical repair work.
func pollClaimQueue(ctx context.Context, queue *ClaimQueue, hooks claimQueuePollHooks) error {
	dirty := false
	historicalReconciliations := 0
	historicalOutcomes := 0
	flush := func() error {
		if !dirty {
			return nil
		}
		if err := hooks.save(queue); err != nil {
			return err
		}
		dirty = false
		return nil
	}
	latestEpoch := queue.LastDiscovered
	if hooks.latestEpoch != nil {
		latestEpoch = hooks.latestEpoch(latestEpoch)
	}
	pending := []claimPollCandidate{}
	for epoch := queue.LastDiscovered; epoch >= 0; epoch-- {
		entry := queue.Entries[fmt.Sprint(epoch)]
		if entry == nil || entry.Status == "finalized" || entry.Status == "no-claim" {
			continue
		}
		if entry.NextRetryAt != "" {
			when, err := time.Parse(time.RFC3339Nano, entry.NextRetryAt)
			if err != nil {
				return fmt.Errorf("claim epoch %d has an invalid retry deadline: %w", epoch, err)
			}
			if hooks.now().Before(when) {
				continue
			}
		}
		pending = append(pending, claimPollCandidate{entry: entry, recent: epoch >= latestEpoch-1})
	}
	// Old outcomes rotate by their durable retry deadline. A single slow
	// reconciliation cannot become due again and forever hide older entries.
	sort.SliceStable(pending, func(i, j int) bool {
		if pending[i].recent != pending[j].recent {
			return pending[i].recent
		}
		if pending[i].recent {
			return false
		}
		left, _ := time.Parse(time.RFC3339Nano, pending[i].entry.NextRetryAt)
		right, _ := time.Parse(time.RFC3339Nano, pending[j].entry.NextRetryAt)
		return left.Before(right)
	})
	for ctx.Err() == nil {
		candidates := pending[:0]
		for _, candidate := range pending {
			if !candidate.recent {
				if candidate.entry.Status == "uncertain" && historicalOutcomes >= claimHistoricalOutcomesPerPoll || candidate.entry.Status != "uncertain" && historicalReconciliations >= claimHistoricalReconciliationsPerPoll {
					continue
				}
			}
			candidates = append(candidates, candidate)
		}
		pending = candidates
		index, operationCtx, done := 0, ctx, func() {}
		if hooks.begin != nil {
			var err error
			index, operationCtx, done, err = hooks.begin(ctx, candidates)
			if err != nil {
				if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
					return flush()
				}
				return errors.Join(err, flush())
			}
			if index < 0 {
				return flush()
			}
		}
		if len(candidates) == 0 {
			break
		}
		candidate := candidates[index]
		entry, recent := candidate.entry, candidate.recent
		pending = append(pending[:index], pending[index+1:]...)
		if !recent {
			if entry.Status == "uncertain" {
				historicalOutcomes++
			} else {
				historicalReconciliations++
			}
		}
		err := func(ctx context.Context) error {
			reconciled, reconcileErr := hooks.reconcile(ctx, entry)
			if (entry.TxHash != "" || entry.RawTxHex != "") && reconciled != "finalized" {
				entry.Status = "uncertain"
				if reconciled != "" {
					reconcileErr = &claimSignedOutcomeError{reason: "reconciliation cannot replace retained signed bytes with " + reconciled, cause: reconcileErr}
					reconciled = ""
				}
			}
			if reconciled != "" {
				entry.Status = reconciled
				entry.UpdatedAt = hooks.now().UTC().Format(time.RFC3339Nano)
				entry.LastError, entry.NextRetryAt = "", ""
				entry.ReconcileAttempts = 0
				dirty = true
				if err := flush(); err != nil {
					return err
				}
				if reconciled == "finalized" || reconciled == "no-claim" {
					return nil
				}
			}
			if reconcileErr != nil {
				deferClaimReconciliation(entry, hooks.now(), reconcileErr, recent)
				dirty = true
				return nil
			}
			if entry.Status == "uncertain" {
				// Pending exact outcomes still need a bounded, durable revisit.
				// They cannot bypass all historical limits on every daemon poll.
				entry.NextRetryAt = hooks.now().Add(time.Minute).UTC().Format(time.RFC3339Nano)
				entry.UpdatedAt = hooks.now().UTC().Format(time.RFC3339Nano)
				dirty = true
				return nil
			}
			if ctx.Err() != nil {
				return nil
			}
			entry.ReconcileAttempts = 0
			entry.Status = "submitting"
			entry.Attempts++
			entry.TxHash, entry.RawTxHex = "", ""
			entry.UpdatedAt = hooks.now().UTC().Format(time.RFC3339Nano)
			entry.LastError, entry.NextRetryAt = "", ""
			dirty = true
			if err := flush(); err != nil {
				return err
			}
			claimErr := hooks.submit(ctx, entry)
			entry.UpdatedAt = hooks.now().UTC().Format(time.RFC3339Nano)
			if claimErr == nil {
				entry.Status = "finalized"
			} else if entry.TxHash != "" || entry.RawTxHex != "" {
				entry.Status = "uncertain"
				entry.LastError = "exact transaction was prepared but finality was not confirmed: " + claimErr.Error()
			} else {
				entry.Status = "retry"
				entry.LastError = claimErr.Error()
				entry.NextRetryAt = hooks.now().Add(claimRetry(entry.Attempts)).UTC().Format(time.RFC3339Nano)
			}
			dirty = true
			if err := flush(); err != nil {
				return err
			}
			var custodyErr *claimNonceSafetyError
			if errors.As(claimErr, &custodyErr) {
				return custodyErr
			}
			return nil
		}(operationCtx)
		done()
		if err != nil {
			return err
		}
		if hooks.begin != nil {
			// Yield queue ownership after each admitted operation so discovery
			// and cancellation are observed before any further network work.
			return flush()
		}
	}
	return flush()
}
