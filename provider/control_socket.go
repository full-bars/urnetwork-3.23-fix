package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"syscall"

	"github.com/urnetwork/connect"
)

// controlSocketPath returns ~/.urnetwork/provider.sock — the Unix domain
// socket urnet-tools talks to instead of writing override files directly.
// The provider is the only writer of its own settings (see controlState);
// this socket is how another process (urnet-tools) asks it to change one.
func controlSocketPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".urnetwork", "provider.sock"), nil
}

// controlRequest is one line of the socket protocol: newline-delimited JSON,
// one request per line, one response per line, in order.
type controlRequest struct {
	Cmd   string `json:"cmd"` // "set", "clear", or "get"
	Key   string `json:"key"`
	Value string `json:"value,omitempty"`
}

type controlResponse struct {
	OK    bool   `json:"ok"`
	Value string `json:"value,omitempty"`
	Found bool   `json:"found,omitempty"`
	Error string `json:"error,omitempty"`
}

// startControlSocket opens the control socket and serves it until ctx is
// canceled. Returns once the listener is up and accepting; serving happens
// on a background goroutine. The returned cleanup func closes the listener
// and removes the socket file — call it (or just let ctx cancellation do
// the equivalent) on shutdown.
func startControlSocket(ctx context.Context, state *controlState) (func(), error) {
	path, err := controlSocketPath()
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}

	if err := removeStaleSocket(path); err != nil {
		return nil, err
	}

	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("control socket listen: %w", err)
	}
	// Unix sockets inherit umask at creation time rather than an explicit
	// mode argument to Listen, so lock it down explicitly: owner-only, no
	// group/other access. Anyone who can reach this socket can change
	// provider settings.
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, fmt.Errorf("control socket chmod: %w", err)
	}

	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				// Expected on shutdown (ctx.Done() closed ln above); nothing
				// else to do either way — the listener is gone.
				return
			}
			go handleControlConn(conn, state)
		}
	}()

	cleanup := func() {
		ln.Close()
		os.Remove(path)
	}
	return cleanup, nil
}

// removeStaleSocket removes path if nothing is actually listening on it —
// i.e. it's a leftover from a previous process that didn't shut down
// cleanly. If a live process IS listening (this provider is somehow already
// running), it leaves the file alone and returns an error instead of
// stealing the socket out from under a running instance.
func removeStaleSocket(path string) error {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil // nothing to clean up
		}
		return err
	}

	conn, err := net.Dial("unix", path)
	if err == nil {
		conn.Close()
		return fmt.Errorf("control socket %s already has a live listener; is another provider instance running?", path)
	}
	// ECONNREFUSED (or similar): the file exists but nothing is listening —
	// a previous process left it behind. Safe to remove and re-bind.
	return os.Remove(path)
}

// handleControlConn serves one client connection: one JSON request per
// line, one JSON response per line, until the client disconnects.
func handleControlConn(conn net.Conn, state *controlState) {
	defer conn.Close()

	scanner := bufio.NewScanner(conn)
	enc := json.NewEncoder(conn)
	for scanner.Scan() {
		var req controlRequest
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			enc.Encode(controlResponse{OK: false, Error: "invalid request: " + err.Error()})
			continue
		}
		enc.Encode(handleControlRequest(state, req))
	}
}

func handleControlRequest(state *controlState, req controlRequest) controlResponse {
	if req.Key == "" {
		return controlResponse{OK: false, Error: "key is required"}
	}

	switch req.Cmd {
	case "get":
		value, found := state.get(req.Key)
		if !controlKeys[req.Key] {
			return controlResponse{OK: false, Error: fmt.Sprintf("unknown control key %q", req.Key)}
		}
		return controlResponse{OK: true, Value: value, Found: found}

	case "set":
		// Persist-then-commit would be safer in the abstract, but persist()
		// needs the full snapshot including this change, so: apply, try to
		// persist, and roll back the in-memory change if persisting fails —
		// keeping memory and disk from disagreeing about what's "set".
		oldValue, hadOld := state.get(req.Key)
		if err := state.set(req.Key, req.Value); err != nil {
			return controlResponse{OK: false, Error: err.Error()}
		}
		if err := state.persist(); err != nil {
			if hadOld {
				state.set(req.Key, oldValue)
			} else {
				state.clear(req.Key)
			}
			return controlResponse{OK: false, Error: "set applied in memory but failed to persist: " + err.Error()}
		}
		if err := applyLiveSideEffect(req.Key, req.Value); err != nil {
			// Persisted fine — it'll take effect on the next restart — but
			// the immediate, no-restart-needed part of it failed. Surface
			// that distinction rather than claiming full success.
			return controlResponse{OK: false, Error: "persisted, but failed to apply live: " + err.Error()}
		}
		return controlResponse{OK: true}

	case "clear":
		oldValue, hadOld := state.get(req.Key)
		if err := state.clear(req.Key); err != nil {
			return controlResponse{OK: false, Error: err.Error()}
		}
		if err := state.persist(); err != nil {
			if hadOld {
				state.set(req.Key, oldValue)
			}
			return controlResponse{OK: false, Error: "clear applied in memory but failed to persist: " + err.Error()}
		}
		return controlResponse{OK: true}

	default:
		return controlResponse{OK: false, Error: fmt.Sprintf("unknown command %q", req.Cmd)}
	}
}

// dialControlSocket is a small client helper for urnet-tools (PR 3) and for
// tests here: send one request, read one response, close the connection.
// errNoProvider distinguishes "provider isn't running" (caller should fall
// back to the pending-queue file) from an actual protocol/application error.
var errNoProvider = errors.New("no provider listening on control socket")

func dialControlSocket(req controlRequest) (controlResponse, error) {
	path, err := controlSocketPath()
	if err != nil {
		return controlResponse{}, err
	}
	conn, err := net.Dial("unix", path)
	if err != nil {
		if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED) {
			return controlResponse{}, errNoProvider
		}
		return controlResponse{}, err
	}
	defer conn.Close()

	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return controlResponse{}, err
	}
	var resp controlResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return controlResponse{}, err
	}
	return resp, nil
}

// applyLiveSideEffect runs the immediate, no-restart-needed part of a
// socket `set`, for the handful of keys where the underlying knob is a Go
// runtime setting safely changeable at any time via runtime/debug. Every
// other key has no live side effect here: the persisted value alone is
// enough, because the resolve*/*Enabled function that consumes it re-reads
// controlState on its own next call (see bandwidth_reporter.go,
// auth_rate_limiter.go, proxy_url_source.go). Not called on `clear` — there
// is no well-defined "revert to" value to apply live, so clearing one of
// these two keys only affects the NEXT restart's baseline, same as before
// this feature existed.
func applyLiveSideEffect(key, value string) error {
	switch key {
	case "gomemlimit":
		limit, err := connect.ParseByteCount(value)
		if err != nil {
			return fmt.Errorf("gomemlimit: %w", err)
		}
		debug.SetMemoryLimit(limit)
	case "gogc":
		percent, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("gogc: %w", err)
		}
		debug.SetGCPercent(percent)
	}
	return nil
}
