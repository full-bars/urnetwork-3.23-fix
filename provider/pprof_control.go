package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/connect"
)

// Loopback pprof is on by default so a stuck or heavy provider can be inspected
// without a restart, which would destroy the evidence. The control key `pprof`
// turns it off, or moves it to another loopback address. An explicit
// URNETWORK_PPROF keeps its older meaning and decides the address on its own.

const (
	pprofModeOn      = "on"
	pprofModeOff     = "off"
	pprofDefaultAddr = "127.0.0.1:6060"
)

var (
	managedPprof  profileListener
	pprofApplyMu  sync.Mutex
	pprofRetrySeq uint64
	// the retry for a port the hotswap parent still holds
	pprofRetryEvery = 5 * time.Second
	pprofRetryFor   = 2 * time.Minute
)

// validPprofValue reports whether value is on, off or a loopback host:port.
func validPprofValue(value string) bool {
	switch v := strings.ToLower(strings.TrimSpace(value)); v {
	case pprofModeOn, pprofModeOff:
		return true
	default:
		host, port, err := net.SplitHostPort(strings.TrimSpace(value))
		if err != nil {
			return false
		}
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return false
		}
		n, err := strconv.Atoi(port)
		return err == nil && 0 <= n && n <= 65535
	}
}

// pprofWantedAddr turns a control value into the address to bind, or "" for off.
func pprofWantedAddr(value string) string {
	switch v := strings.ToLower(strings.TrimSpace(value)); v {
	case pprofModeOff:
		return ""
	case pprofModeOn, "":
		return pprofDefaultAddr
	default:
		return strings.TrimSpace(value)
	}
}

// applyPprofMode makes the managed listener match a value of the key `pprof`.
func applyPprofMode(value string) error {
	if !validPprofValue(value) {
		return fmt.Errorf("pprof: must be on, off or a loopback host:port (got %q)", value)
	}
	pprofApplyMu.Lock()
	defer pprofApplyMu.Unlock()
	if os.Getenv("URNETWORK_PPROF") != "" {
		tlog("[profile] URNETWORK_PPROF decides the diagnostics address, so the pprof setting %q is saved and applies when that variable is unset\n", value)
		return nil
	}
	pprofRetrySeq++
	want := pprofWantedAddr(value)
	if want == "" {
		if managedPprof.addr() != "" {
			tlog("[profile] diagnostics stopped\n")
		}
		return managedPprof.stop()
	}
	if current := managedPprof.addr(); current != "" {
		if wanted, fellBack := managedPprof.usingFallback(); wanted == want && !fellBack {
			return nil
		}
		if err := managedPprof.stop(); err != nil {
			return err
		}
	}
	bound, err := managedPprof.start(want)
	if err != nil {
		tlog("[profile] diagnostics could not start on %s: %v\n", want, err)
		return err
	}
	tlog("[profile] diagnostics on %s (loopback only): /debug/pprof/goroutine?debug=1, /debug/pprof/threadcreate?debug=1, /debug/pprof/heap\n", bound)
	if _, fellBack := managedPprof.usingFallback(); fellBack {
		tlog("[profile] %s was in use, so diagnostics use %s until it is free (a hotswap parent holds it while it drains)\n", want, bound)
		// the retry takes its values here, under the mutex; a later setting
		// change ends this retry through the sequence instead of retiming it
		seq := pprofRetrySeq
		retryEvery, retryFor := pprofRetryEvery, pprofRetryFor
		go connect.HandleError(func() { retryPprofPort(seq, want, retryEvery, retryFor) })
	}
	return nil
}

// retryPprofPort moves the listener onto its wanted port once the holder of it
// (usually the hotswap parent) has exited. It stops when the setting changes.
// The timing and the sequence are passed in because this goroutine can start
// after applyPprofMode has returned, so a read of the package variables here
// would not be ordered with a later write to them.
func retryPprofPort(seq uint64, want string, retryEvery, retryFor time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), retryFor)
	defer cancel()
	ticker := time.NewTicker(retryEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		pprofApplyMu.Lock()
		if seq != pprofRetrySeq {
			pprofApplyMu.Unlock()
			return
		}
		if _, fellBack := managedPprof.usingFallback(); !fellBack {
			pprofApplyMu.Unlock()
			return
		}
		_ = managedPprof.stop()
		bound, err := managedPprof.start(want)
		if err == nil {
			if _, stillFallback := managedPprof.usingFallback(); !stillFallback {
				tlog("[profile] diagnostics moved to %s\n", bound)
				pprofApplyMu.Unlock()
				return
			}
		}
		pprofApplyMu.Unlock()
	}
}

// startDefaultPprof starts the managed listener at provider start from the
// persisted value of the key, which defaults to on. It does nothing when
// URNETWORK_PPROF is set, because that variable keeps the older listener.
func startDefaultPprof() {
	if os.Getenv("URNETWORK_PPROF") != "" {
		return
	}
	value := pprofModeOn
	if saved, ok := globalControlState.get("pprof"); ok && saved != "" {
		value = saved
	}
	if err := applyPprofMode(value); err != nil {
		tlog("[profile] diagnostics are off: %v\n", err)
	}
}

// pprofStatusAddr is the address the diagnostics listener is bound to now, or
// "" when it is off.
func pprofStatusAddr() string { return managedPprof.addr() }
