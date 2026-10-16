// Read-only EVM recovery owns one operation budget and retains hard failures.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"syscall"
	"time"
)

const (
	ethRpcOperationTimeout = 300 * time.Second
	ethRpcRetryDelay       = 2 * time.Second
)

// Per-operation hooks allow deadline and cancellation tests without clock
// races. Production supplies neither hook; there is no mutable global state.
type ethRpcRetryHooks struct {
	withTimeout func(context.Context, time.Duration) (context.Context, context.CancelFunc)
	wait        func(context.Context, time.Duration) error
}

// HTTP status carries structured retry eligibility; diagnostic words do not.
type ethRpcStatusError struct {
	method string
	status int
}

// Retain the method and status for callers without inspecting response bodies.
func (self *ethRpcStatusError) Error() string {
	return fmt.Sprintf("%s: http %d", self.method, self.status)
}

// Only the request transport and bounded body reader may assign this origin.
type ethRpcTransportError struct {
	cause error
}

// Preserve the original diagnostic and structured cause.
func (self *ethRpcTransportError) Error() string { return self.cause.Error() }

// Expose timeout and cancellation while retaining transport provenance.
func (self *ethRpcTransportError) Unwrap() error { return self.cause }

// The operation has time to recover a minute-long outage, while each attempt
// retains its shorter request timeout. Cancellation always defeats recovery.
func retryEthRpcRead(ctx context.Context, hooks ethRpcRetryHooks, read func(context.Context) (string, error)) (string, error) {
	withTimeout := hooks.withTimeout
	if withTimeout == nil {
		withTimeout = context.WithTimeout
	}
	ctx, cancel := withTimeout(ctx, ethRpcOperationTimeout)
	defer cancel()
	var lastErr error
	for {
		if err := ctx.Err(); err != nil {
			return "", errors.Join(lastErr, err)
		}
		value, err := read(ctx)
		if ownerErr := ctx.Err(); ownerErr != nil {
			return "", errors.Join(err, ownerErr)
		}
		if err == nil {
			return value, nil
		}
		lastErr = err
		if !retryableEthRpcError(err, false) {
			return "", err
		}
		delay := ethRpcRetryDelay + time.Duration(rand.Int64N(int64(ethRpcRetryDelay/2)))
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
			return "", errors.Join(lastErr, err)
		}
	}
}

// Every joined cause must permit retry. A timeout cannot hide malformed JSON,
// an identity conflict, a contract refusal, or a response-close integrity error.
func retryableEthRpcError(err error, transportOrigin bool) bool {
	budget := &minerReadCauseBudget{remaining: minerReadCauseMaximumNodes}
	return retryableEthRpcCause(err, transportOrigin, 0, budget)
}

// A delegated claim transport subtree consumes the same traversal allowance.
func retryableEthRpcCause(err error, transportOrigin bool, depth int, budget *minerReadCauseBudget) bool {
	if !budget.admit(err, depth) || err == context.Canceled {
		return false
	}
	switch err.(type) {
	case *os.PathError, *os.LinkError:
		return false
	}
	if err == context.DeadlineExceeded || err == net.ErrClosed || err == syscall.ECONNRESET || err == syscall.ECONNREFUSED || err == syscall.EPIPE || err == syscall.ETIMEDOUT || err == syscall.ENETUNREACH || err == syscall.EHOSTUNREACH {
		return true
	}
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		return transportOrigin
	}
	switch cause := err.(type) {
	case *ethRpcStatusError:
		return cause.status == http.StatusTooManyRequests || cause.status >= 500 && cause.status <= 599
	case *ethRpcTransportError:
		return retryableEthRpcCause(cause.cause, true, depth+1, budget)
	case *url.Error:
		return retryableEthRpcCause(cause.Err, true, depth+1, budget)
	case *net.OpError:
		return retryableEthRpcCause(cause.Err, true, depth+1, budget)
	case *net.DNSError:
		// Explicit absence stays hard. A wrapped cause retains its own
		// verdict; availability flags decide only a leaf without a child.
		if cause.IsNotFound {
			return false
		}
		if cause.UnwrapErr != nil {
			return retryableEthRpcCause(cause.UnwrapErr, transportOrigin, depth+1, budget)
		}
		return cause.IsTimeout || cause.IsTemporary
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		causes := joined.Unwrap()
		if len(causes) == 0 || len(causes) > budget.remaining {
			return false
		}
		for _, cause := range causes {
			if !retryableEthRpcCause(cause, transportOrigin, depth+1, budget) {
				return false
			}
		}
		return true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return retryableEthRpcCause(wrapped.Unwrap(), transportOrigin, depth+1, budget)
	}
	if networkErr, ok := err.(net.Error); ok {
		return networkErr.Timeout() || networkErr.Temporary()
	}
	return false
}
