package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// pendingOverridesPath returns ~/.urnetwork/pending_overrides.json — a
// one-shot queue urnet-tools writes when it can't reach the running
// provider's control socket (provider not started yet, or down). The
// provider applies every entry on its next startup, in order, then deletes
// the file. This is the ONLY file two processes ever both touch: urnet-tools
// writes it only when the socket is unreachable, the provider reads and
// deletes it only at its own startup, so the two never race.
func pendingOverridesPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".urnetwork", "pending_overrides.json"), nil
}

// pendingOp is one queued change: Op is "set" or "clear".
type pendingOp struct {
	Op    string `json:"op"`
	Key   string `json:"key"`
	Value string `json:"value,omitempty"`
}

// mergePendingOverrides applies every queued change from
// pending_overrides.json (in order) to state, persists the result, and
// deletes the queue file. A missing queue file is the common case and not
// an error. A malformed file, or an invalid entry within it, is logged and
// skipped rather than blocking startup — the provider must always be able
// to come up even if the queue is bad.
//
// The whole read-apply-persist-delete runs under the same cross-process
// lock urnet-tools' queuePendingOverride holds when it appends to this file
// (pending_overrides_lock_unix.go/_windows.go), so a queued op written after
// we read but before we delete can never be dropped: urnet-tools blocks on
// the flock until after we've renamed the file away, then writes a fresh
// queue that the next startup consumes.
func mergePendingOverrides(state *controlState) {
	path, err := pendingOverridesPath()
	if err != nil {
		return
	}

	// Ensure the state dir exists so the lock file can be created — mirrors
	// queuePendingOverride, which does os.MkdirAll before locking. On a fresh
	// box with no ~/.urnetwork yet, this makes the whole merge a clean no-op
	// instead of failing the lock acquire on a missing parent dir.
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		tlog("[control] failed to create %s dir, skipping merge: %s\n", filepath.Dir(path), err)
		return
	}

	release, err := acquirePendingOverridesLock(path)
	if err != nil {
		tlog("[control] failed to acquire pending_overrides.json lock, skipping merge: %s\n", err)
		return
	}
	defer release()

	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			tlog("[control] failed to read pending_overrides.json, leaving it in place: %s\n", err)
		}
		return
	}

	var ops []pendingOp
	if err := json.Unmarshal(data, &ops); err != nil {
		tlog("[control] pending_overrides.json is malformed, leaving it in place for inspection: %s\n", err)
		return
	}

	// Taking txMu across the whole apply+persist+delete makes this one logical
	// transaction with the socket's set/clear path (handleControlRequest also
	// holds txMu): a concurrent socket `set` cannot interleave between a queued
	// op's in-memory apply and its persist, so memory and disk stay in agreement
	// and last-writer-wins is preserved across both writer paths.
	state.txMu.Lock()
	defer state.txMu.Unlock()

	applied := 0
	for _, op := range ops {
		var applyErr error
		switch op.Op {
		case "set":
			applyErr = state.set(op.Key, op.Value)
		case "clear":
			applyErr = state.clear(op.Key)
		default:
			applyErr = fmt.Errorf("unknown op %q", op.Op)
		}
		if applyErr != nil {
			tlog("[control] skipping invalid pending override (op=%q key=%q): %s\n", op.Op, op.Key, applyErr)
			continue
		}
		applied++
	}

	if applied > 0 {
		if err := state.persist(); err != nil {
			tlog("[control] applied %d pending override(s) in memory but failed to persist; leaving pending_overrides.json in place to retry next start: %s\n", applied, err)
			return
		}
		tlog("[control] applied %d pending override(s) from pending_overrides.json\n", applied)
	}

	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		tlog("[control] applied pending overrides but failed to remove pending_overrides.json: %s\n", err)
	}
}
