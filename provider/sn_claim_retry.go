package main

// Only idempotent claim epoch and pool reads enter this owner.
// Signing, submission and wallet mutations never share its retry loop.

import (
	"context"
	"errors"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/urnetwork/connect"
)

const (
	claimReadOperationTimeout = 300 * time.Second
	claimReadRetryDelay       = 2 * time.Second
	claimReadMaximumAttempts  = 256
)

// Instance-only clock/wait seams preserve real SDK requests and parsing. The
// executable has no setting that bypasses the finite production budget.
type claimReadRetryHooks struct {
	withTimeout func(context.Context, time.Duration) (context.Context, context.CancelFunc)
	wait        func(context.Context, time.Duration) error
}

// A test can select a hermetic real strategy while exercising the whole flow.
// The public CLI always uses defaults and the ordinary retry clock.
type finiteClaimHooks struct {
	strategySettings *connect.ClientStrategySettings
	retry            claimReadRetryHooks
}

// One read retains its original deadline across transient transport/status
// failures. Every attempt finishes before the next begins or the owner returns.
func retryClaimApiRead[T any](ctx context.Context, hooks claimReadRetryHooks, read func(context.Context) (T, error)) (T, error) {
	var zero T
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	withTimeout := hooks.withTimeout
	if withTimeout == nil {
		withTimeout = context.WithTimeout
	}
	ctx, cancel := withTimeout(ctx, claimReadOperationTimeout)
	defer cancel()
	var lastErr error
	for attempt := 1; attempt <= claimReadMaximumAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return zero, errors.Join(lastErr, err)
		}
		result, err := read(ctx)
		if ownerErr := ctx.Err(); ownerErr != nil {
			return zero, errors.Join(err, ownerErr)
		}
		if err == nil {
			return result, nil
		}
		lastErr = err
		if !retryableClaimApiRead(err) || attempt == claimReadMaximumAttempts {
			return zero, err
		}
		delay := claimReadRetryDelay + time.Duration(rand.Int64N(int64(claimReadRetryDelay/2)))
		wait := hooks.wait
		if wait == nil {
			wait = func(ctx context.Context, delay time.Duration) error {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(delay):
					return nil
				}
			}
		}
		if err := wait(ctx, delay); err != nil {
			return zero, errors.Join(lastErr, err)
		}
	}
	return zero, lastErr
}

// All joined causes must permit retry: an auth, parser or integrity failure
// cannot be hidden by a simultaneous timeout or gateway status.
func retryableClaimApiRead(err error) bool {
	budget := &minerReadCauseBudget{remaining: minerReadCauseMaximumNodes}
	return retryableClaimApiReadCause(err, 0, budget)
}

// SDK status authority does not extend inside a physical transport subtree.
func retryableClaimApiReadCause(err error, depth int, budget *minerReadCauseBudget) bool {
	switch cause := err.(type) {
	case *connect.HttpStatusError:
		if !budget.admit(err, depth) {
			return false
		}
		return cause.StatusCode == http.StatusRequestTimeout || cause.StatusCode == http.StatusTooEarly || cause.StatusCode == http.StatusTooManyRequests || cause.StatusCode >= 500 && cause.StatusCode <= 599
	case *os.PathError, *os.LinkError, *url.Error, *net.OpError, *net.DNSError:
		return retryableEthRpcCause(err, false, depth, budget)
	case interface{ Unwrap() []error }:
		if !budget.admit(err, depth) {
			return false
		}
		causes := cause.Unwrap()
		if len(causes) == 0 || len(causes) > budget.remaining {
			return false
		}
		for _, child := range causes {
			if !retryableClaimApiReadCause(child, depth+1, budget) {
				return false
			}
		}
		return true
	case interface{ Unwrap() error }:
		if !budget.admit(err, depth) {
			return false
		}
		return retryableClaimApiReadCause(cause.Unwrap(), depth+1, budget)
	default:
		return retryableEthRpcCause(err, false, depth, budget)
	}
}
