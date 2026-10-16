// Epoch reads have their own joined worker so a slow API cannot occupy a ready
// relayer ticket. Only the daemon loop owns and persists the claim queue.
package main

import (
	"context"
	"time"
)

// The single buffered value is the latest observed epoch, not a work queue.
// Cancellation joins every read before the strategy/API owner is closed.
// The v2 daemon reads its account snapshots through the same worker.
func startClaimEpochReader[T any](ctx context.Context, period time.Duration, read func(context.Context) (T, error)) (<-chan T, <-chan struct{}) {
	epochs := make(chan T, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ctx.Err() == nil {
			readCtx, cancel := context.WithTimeout(ctx, claimOperationTimeout)
			epoch, err := read(readCtx)
			cancel()
			if err == nil {
				select {
				case <-epochs:
				default:
				}
				select {
				case epochs <- epoch:
				case <-ctx.Done():
					return
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(period):
			}
		}
	}()
	return epochs, done
}

// Ready admission never waits for another epoch read. Discovery remains live
// while another member owns the nonce, and one daemon serializes its own writes.
func runClaimQueue(ctx context.Context, queue *ClaimQueue, lookback uint64, epochs <-chan int64, polls <-chan time.Time, ready func() <-chan struct{}, hooks claimQueuePollHooks) error {
	for {
		// A completed read outranks an immediate ready ticket. This is a
		// nonblocking mailbox read, never a wait on the epoch API itself.
		select {
		case current := <-epochs:
			discoverClaims(queue, current, lookback)
			if err := hooks.save(queue); err != nil {
				return err
			}
		default:
		}
		if err := pollClaimQueue(ctx, queue, hooks); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case current := <-epochs:
			discoverClaims(queue, current, lookback)
			if err := hooks.save(queue); err != nil {
				return err
			}
		case <-polls:
		case <-ready():
		}
	}
}
