package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

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
	Cmd    string `json:"cmd"` // "set", "clear", "get", "status", or "history"
	Key    string `json:"key"`
	Value  string `json:"value,omitempty"`
	Limit  int    `json:"limit,omitempty"`  // for "history" command
	Cursor string `json:"cursor,omitempty"` // for "history" command
}

// settingInfo is the per-key detail returned by the "status" command.
type settingInfo struct {
	Value  string     `json:"value"`
	Source string     `json:"source"`
	SetAt  *time.Time `json:"set_at,omitempty"`
}

type controlResponse struct {
	OK           bool                   `json:"ok"`
	Value        string                 `json:"value,omitempty"`
	Found        bool                   `json:"found,omitempty"`
	Error        string                 `json:"error,omitempty"`
	NeedsRestart bool                   `json:"needs_restart,omitempty"`
	Entries      []CommandAudit         `json:"entries,omitempty"`
	NextCursor   string                 `json:"next_cursor,omitempty"`
	Settings     map[string]settingInfo `json:"settings,omitempty"`
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

// formerValue renders the previous value of a control key for the log line,
// distinguishing "was empty" from "was never set".
func formerValue(old string, had bool) string {
	if !had {
		return "unset"
	}
	if old == "" {
		return `""`
	}
	return old
}

// liveEffectKeys tracks which control keys can be applied at runtime
// without a restart. When a new key gets a live-apply case (either in
// applyLiveSideEffect or via a resolve* function that reads
// globalControlState on every call), it should be added here.
//
// Keys NOT in this set: profile (initGlog/SHMLogger is one-shot at startup),
// ramlogs (stdout/stderr redirect is irreversible mid-process).
var liveEffectKeys = map[string]bool{
	// Runtime tuning via debug.Set* — immediate effect.
	"gomemlimit": true,
	"gogc":       true,
	// resolve* functions read globalControlState on every call — already live.
	"fast_auth":                   true,
	"proxy_self_heal":             true,
	"report_url":                  true,
	"report_interval":             true,
	"proxy_url_refresh":           true,
	"proxy_url_max":               true,
	"proxy_dead_cleanup_scope":    true,
	"proxy_dead_cleanup_interval": true,
	"node_name":                   true,
	"hot_restart":                 true,
	"metrics":                     true,
}

// needsRestart returns true if setting this key requires a provider restart
// to take effect. Any key NOT in liveEffectKeys needs a restart.
func needsRestart(key string) bool {
	return !liveEffectKeys[key]
}

// validateControlValue validates a value server-side before persisting.
// Mirrors the client-side validation in internal/urnettools/control_client.go
// so raw socket clients can't persist invalid values that silently break
// on next restart.
func validateControlValue(key, value string) error {
	// Normalize to lowercase so ON/TRUE/YES are accepted — matches
	// client-side behavior in internal/urnettools/control_client.go.
	valLower := strings.ToLower(value)
	switch key {
	case "report_interval", "proxy_url_refresh", "proxy_dead_cleanup_interval":
		d, err := time.ParseDuration(value)
		if err != nil {
			return fmt.Errorf("%s: invalid duration %q (use e.g. 30s, 5m, 1h)", key, value)
		}
		min := 10 * time.Second
		if key == "proxy_dead_cleanup_interval" {
			min = time.Minute
		}
		if d < min {
			return fmt.Errorf("%s: %s is below the minimum %s", key, value, min)
		}
	case "proxy_url_max":
		n, err := strconv.Atoi(value)
		if err != nil || n < 0 {
			return fmt.Errorf("%s: must be a non-negative integer (got %q)", key, value)
		}
	case "proxy_dead_cleanup_scope":
		switch value {
		case "none", "url", "all":
		default:
			return fmt.Errorf("%s: must be none, url, or all (got %q)", key, value)
		}
	case "fast_auth", "proxy_self_heal":
		switch valLower {
		case "on", "off":
		default:
			return fmt.Errorf("%s: must be on or off (got %q)", key, value)
		}
	case "hot_restart":
		switch valLower {
		case "on", "off", "1", "0", "true", "false", "yes", "no":
		default:
			return fmt.Errorf("hot_restart: must be on or off (got %q)", value)
		}
	case "ramlogs":
		switch valLower {
		case "on", "off", "1", "0", "true", "false":
		default:
			return fmt.Errorf("ramlogs: must be on or off (got %q)", value)
		}
	case "profile":
		switch valLower {
		case "auto", "eco", "lowmem", "turbo-v4", "turbo-v8":
		default:
			return fmt.Errorf("profile: must be auto, eco, lowmem, turbo-v4, or turbo-v8 (got %q)", value)
		}
	case "gomemlimit":
		if _, err := connect.ParseByteCount(value); err != nil {
			return fmt.Errorf("gomemlimit: invalid byte count %q: %w", value, err)
		}
	case "gogc":
		if valLower != "off" {
			n, err := strconv.Atoi(value)
			if err != nil {
				return fmt.Errorf("gogc: must be an integer percentage or 'off' (got %q)", value)
			}
			if n < 0 {
				return fmt.Errorf("gogc: must be a non-negative percentage or 'off' (got %q)", value)
			}
		}
	case "metrics":
		switch valLower {
		case "on", "off":
		default:
			return fmt.Errorf("metrics: must be on or off (got %q)", value)
		}
	case "node_name":
		if value == "" {
			return fmt.Errorf("node_name: must not be empty")
		}
	case "report_url":
		if value != "" {
			// basic URL sanity — reject values with spaces or newlines
			for _, c := range value {
				if c == ' ' || c == '\n' || c == '\r' || c == '\t' {
					return fmt.Errorf("report_url: must not contain whitespace (got %q)", value)
				}
			}
		}
	}
	return nil
}

// liveDefaults maps live-applied keys to their Go runtime defaults.
// Used when clearing a key to reapply the default immediately.
var liveDefaults = map[string]string{
	"gomemlimit": "0", // Go's default: unlimited
	"gogc":       "100",
}

// applyLiveDefault reapplies the runtime default for a live-applied key.
func applyLiveDefault(key string) error {
	def, ok := liveDefaults[key]
	if !ok {
		return nil
	}
	return applyLiveSideEffect(key, def)
}

func handleControlRequest(state *controlState, req controlRequest) controlResponse {
	// Key check moved to individual cases — status and history don't need a key.

	IncrControlCmd(req.Cmd)

	switch req.Cmd {
	case "get":
		if req.Key == "" {
			return controlResponse{OK: false, Error: "key is required"}
		}
		value, found := state.get(req.Key)
		if !controlKeys[req.Key] {
			return controlResponse{OK: false, Error: fmt.Sprintf("unknown control key %q", req.Key)}
		}
		return controlResponse{OK: true, Value: value, Found: found}

	case "set":
		if req.Key == "" {
			return controlResponse{OK: false, Error: "key is required"}
		}
		// Validate before touching state — reject bad values at the
		// socket so the operator sees the error immediately, rather
		// than persisting garbage that breaks on next restart.
		if err := validateControlValue(req.Key, req.Value); err != nil {
			return controlResponse{OK: false, Error: err.Error()}
		}
		// Persist-then-commit would be safer in the abstract, but persist()
		// needs the full snapshot including this change, so: apply, try to
		// persist, and roll back the in-memory change if persisting fails —
		// keeping memory and disk from disagreeing about what's "set". Hold
		// txMu across the whole get-old -> set -> persist -> rollback unit so
		// concurrent connections can't interleave (see controlState.txMu).
		state.txMu.Lock()
		defer state.txMu.Unlock()
		oldValue, oldMeta, hadOld := state.getWithMeta(req.Key)
		if err := state.set(req.Key, req.Value); err != nil {
			tlog("❌ [control] set %s=%s rejected: %s\n", req.Key, req.Value, err)
			return controlResponse{OK: false, Error: err.Error()}
		}
		if err := state.persist(); err != nil {
			// Restore both value and meta to preserve provenance.
			state.mu.Lock()
			if hadOld {
				state.values[req.Key] = oldValue
				state.meta[req.Key] = oldMeta
			} else {
				delete(state.values, req.Key)
				delete(state.meta, req.Key)
			}
			state.mu.Unlock()
			tlog("❌ [control] set %s=%s failed to persist, rolled back: %s\n", req.Key, req.Value, err)
			return controlResponse{OK: false, Error: "set applied in memory but failed to persist: " + err.Error()}
		}
		if err := applyLiveSideEffect(req.Key, req.Value); err != nil {
			// Persisted fine — it'll take effect on the next restart — but
			// the immediate, no-restart-needed part of it failed. Surface
			// that distinction rather than claiming full success.
			tlog("⚠️ [control] set %s=%s (was %s) persisted but live apply failed, takes effect on restart: %s\n",
				req.Key, req.Value, formerValue(oldValue, hadOld), err)
			return controlResponse{OK: false, Error: "persisted, but failed to apply live: " + err.Error()}
		}
		// The operator's confirmation that the setting actually reached the
		// running provider: without this, `urnet-tools set` succeeding is
		// only visible in the CLI, and nothing in the provider's own log
		// shows the change was registered.
		tlog("⚙️ [control] set %s=%s (was %s)\n", req.Key, req.Value, formerValue(oldValue, hadOld))
		// Record audit entry
		recordAndPersist(CommandAudit{
			Timestamp: time.Now(),
			Cmd:       req.Cmd,
			Key:       req.Key,
			Value:     req.Value,
			OK:        true,
		})
		return controlResponse{OK: true, NeedsRestart: needsRestart(req.Key)}

	case "clear":
		if req.Key == "" {
			return controlResponse{OK: false, Error: "key is required"}
		}
		state.txMu.Lock()
		defer state.txMu.Unlock()
		oldValue, oldMeta, hadOld := state.getWithMeta(req.Key)
		if err := state.clear(req.Key); err != nil {
			tlog("❌ [control] clear %s rejected: %s\n", req.Key, err)
			return controlResponse{OK: false, Error: err.Error()}
		}
		if err := state.persist(); err != nil {
			// Restore both value and meta to preserve provenance.
			if hadOld {
				state.mu.Lock()
				state.values[req.Key] = oldValue
				state.meta[req.Key] = oldMeta
				state.mu.Unlock()
			}
			tlog("❌ [control] clear %s failed to persist, rolled back: %s\n", req.Key, err)
			return controlResponse{OK: false, Error: "clear applied in memory but failed to persist: " + err.Error()}
		}
		// For live-applied keys, reapply the runtime default immediately
		// instead of requiring a restart. For non-live keys, the cleared
		// value takes effect on next restart.
		liveCleared := liveEffectKeys[req.Key]
		if liveCleared {
			if err := applyLiveDefault(req.Key); err != nil {
				tlog("⚠️ [control] clear %s persisted but live default apply failed: %s\n", req.Key, err)
				return controlResponse{OK: true, Error: "cleared, but failed to reapply live default: " + err.Error()}
			}
		}
		tlog("⚙️ [control] cleared %s (was %s)\n", req.Key, formerValue(oldValue, hadOld))
		// Record audit entry
		recordAndPersist(CommandAudit{
			Timestamp: time.Now(),
			Cmd:       req.Cmd,
			Key:       req.Key,
			Value:     oldValue,
			OK:        true,
		})
		return controlResponse{OK: true, NeedsRestart: !liveCleared}

	case "status":
		raw := state.statusSnapshot()
		settings := make(map[string]settingInfo, len(raw))
		for k, v := range raw {
			si := settingInfo{Value: v.Value, Source: string(v.Meta.Source)}
			if !v.Meta.SetAt.IsZero() {
				si.SetAt = &v.Meta.SetAt
			}
			settings[k] = si
		}
		return controlResponse{OK: true, Settings: settings}

	case "history":
		limit := req.Limit
		if limit <= 0 {
			limit = 50
		}
		if limit > 100 {
			limit = 100
		}
		entries, nextCursor := globalAuditRing.Entries(limit, req.Cursor)
		return controlResponse{OK: true, Entries: entries, NextCursor: nextCursor}

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
		if strings.EqualFold(value, "off") {
			debug.SetGCPercent(-1)
		} else {
			percent, err := strconv.Atoi(value)
			if err != nil {
				return fmt.Errorf("gogc: %w", err)
			}
			if percent < 0 {
				return fmt.Errorf("gogc: must be a non-negative percentage (got %d)", percent)
			}
			debug.SetGCPercent(percent)
		}
	case "metrics":
		return applyMetricsLive(value)
	}
	return nil
}

// applyMetricsLive starts or stops the Prometheus /metrics listener.
func applyMetricsLive(value string) error {
	enabled := strings.EqualFold(value, "on")
	if enabled && metricsServer == nil {
		metricsAddr := os.Getenv("URNETWORK_METRICS")
		if metricsAddr == "" {
			return fmt.Errorf("metrics on: URNETWORK_METRICS env var not set")
		}
		connect.SetExtraMetricsProvider(providerExtraMetrics)
		connect.SetPersistentErrorFunc(IncrPersistentError)
		metricsServer = &http.Server{
			Addr:              metricsAddr,
			Handler:           connect.PrometheusHandler(),
			ReadHeaderTimeout: 10 * time.Second,
			IdleTimeout:       30 * time.Second,
		}
		go func() {
			if err := metricsServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				tlog("[metrics] listener failed: %v\n", err)
			}
		}()
		tlog("[metrics] started Prometheus /metrics on %s\n", metricsAddr)
	} else if !enabled && metricsServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := metricsServer.Shutdown(ctx); err != nil {
			return fmt.Errorf("metrics off: %w", err)
		}
		metricsServer = nil
		tlog("[metrics] stopped Prometheus /metrics\n")
	}
	return nil
}

// applyPersistedRuntimeTuning re-applies gomemlimit/gogc from state via
// applyLiveSideEffect. Unlike every other control key, these two have no
// other startup-time consumer (profile/ramlogs are seeded into env vars by
// seedEnvFromControlState/this package's init(), before main() even runs;
// everything else is read live by its own resolve*/*Enabled function on
// each call) — a value that reached state via mergePendingOverrides, or a
// reload, does not take effect until something calls applyLiveSideEffect
// for it. Called once after ordinary startup's mergePendingOverrides, and
// again after a HotSwap candidate's post-takeover reload+merge — the
// parent's own gomemlimit/gogc runtime.debug calls apply only to the
// parent's process, not the newly promoted candidate's.
func applyPersistedRuntimeTuning(state *controlState) {
	if v, ok := state.get("gomemlimit"); ok && v != "" && v != "off" {
		if err := applyLiveSideEffect("gomemlimit", v); err != nil {
			tlog("[control] failed to apply persisted gomemlimit=%s: %s\n", v, err)
		}
	}
	if v, ok := state.get("gogc"); ok && v != "" && v != "off" {
		if err := applyLiveSideEffect("gogc", v); err != nil {
			tlog("[control] failed to apply persisted gogc=%s: %s\n", v, err)
		}
	}
}

// persistedRuntimeTuningActive reports whether the operator has an explicit,
// persisted control-socket value for key ("gomemlimit" or "gogc") — i.e. a
// value other than "" (never set) or "off" (explicitly cleared).
//
// This exists because applyPersistedRuntimeTuning above only runs once at
// startup (and once more after a HotSwap takeover), but provideWithProxy /
// applyTurboSettings / applyEcoSettings run again on EVERY proxy add, and
// each only checked the GOMEMLIMIT/GOGC environment variables before
// overwriting the runtime setting with the profile default — so a persisted
// value applied at startup got silently clobbered back to the turbo/eco
// default the next time a proxy connected.
//
// Precedence for gomemlimit/gogc (highest wins), enforced by checking each
// gate in this order at every site that would otherwise apply a default:
//  1. GOMEMLIMIT/GOGC environment variable — operator-explicit, checked
//     first by every call site already.
//  2. A persisted control-socket value (`urnet-tools set gomemlimit|gogc`)
//     — also operator-explicit; this is what call sites were missing.
//  3. The active profile's default (turbo/eco).
//  4. ensureMemoryLimit's generic RAM-percentage fallback (gomemlimit only;
//     gogc has no generic fallback below the profile default).
func persistedRuntimeTuningActive(key string) bool {
	v, ok := globalControlState.get(key)
	return ok && v != "" && v != "off"
}

// waitForControlSocketRelease blocks until the running provider's control
// socket at ~/.urnetwork/provider.sock is no longer accepting connections — the
// parent has released it — or timeoutMillis elapses, whichever comes first.
// Used by a HotSwap candidate right after it ACKs takeover: the candidate must
// not reload provider_state.json or bind its own socket until the parent has
// stopped listening, otherwise a `set` that lands on the not-yet-closed parent
// socket after the candidate's snapshot is dropped, and removeStaleSocket would
// (correctly) refuse to steal a still-live listener — leaving the promoted
// candidate with no control socket. A false "still listening" (parent between
// CLOSE and unlink) only delays briefly; the caller treats a timeout as
// proceed-anyway (the merge below is best-effort like all control reloads).
func waitForControlSocketRelease(timeout time.Duration) {
	path, err := controlSocketPath()
	if err != nil {
		return
	}
	deadline := time.Now().Add(timeout)
	for {
		conn, err := net.Dial("unix", path)
		if err != nil {
			// ENOENT / EINVAL / ECONNREFUSED: not listening anymore, or gone.
			return
		}
		conn.Close()
		if time.Now().After(deadline) {
			tlog("[control] timed out waiting for parent to release control socket after takeover; proceeding anyway\n")
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}
