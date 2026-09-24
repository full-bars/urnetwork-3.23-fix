package urnettools

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/urnetwork/connect"
)

// controlRequest is one line of the control socket protocol.
type controlRequest struct {
	Cmd     string `json:"cmd"` // "set", "clear", "get", "status", "history", "snapshot", "traffic", or "audit"
	Key     string `json:"key,omitempty"`
	Value   string `json:"value,omitempty"`
	Limit   int    `json:"limit,omitempty"`
	Cursor  string `json:"cursor,omitempty"`
	Action  string `json:"action,omitempty"`
	Address string `json:"address,omitempty"`
}

// AuditEntry mirrors the provider's CommandAudit for JSON wire format.
// The provider serialises Timestamp as an RFC3339 string (time.Time),
// so we accept it as a string here rather than int64.
type AuditEntry struct {
	Timestamp string `json:"timestamp"`
	Cmd       string `json:"cmd"`
	Key       string `json:"key"`
	Value     string `json:"value"`
	Error     string `json:"error,omitempty"`
	OK        bool   `json:"ok"`
}

// SettingInfo is the per-key detail returned by the "status" command.
type SettingInfo struct {
	Value  string `json:"value"`
	Source string `json:"source"`
	SetAt  string `json:"set_at,omitempty"`
}

// ProxyAuditStatus mirrors the provider's proxy audit snapshot in the control
// socket "audit" and "status" replies.
type ProxyAuditStatus struct {
	Acting          bool               `json:"acting"`
	NotActingReason string             `json:"not_acting_reason,omitempty"`
	Parked          []ProxyAuditParked `json:"parked,omitempty"`
	WouldPark       int                `json:"would_park"`
	Distrusted      bool               `json:"distrusted,omitempty"`
	Thin            bool               `json:"thin_pass,omitempty"`
	Parks24h        int                `json:"parks_24h"`
	Paused          bool               `json:"paused,omitempty"`
	PausedSince     time.Time          `json:"paused_since,omitempty"`
	UpdatedAt       time.Time          `json:"updated_at"`
}

// ProxyAuditParked is one proxy held out by the audit engine, with when its
// backoff probation ends.
type ProxyAuditParked struct {
	Addr  string    `json:"addr"`
	User  string    `json:"user,omitempty"` // obfuscated; tells accounts at one gateway apart
	Until time.Time `json:"until"`
}

// controlResponse is one line response from the control socket.
type controlResponse struct {
	OK           bool                   `json:"ok"`
	Value        string                 `json:"value,omitempty"`
	Found        bool                   `json:"found,omitempty"`
	Error        string                 `json:"error,omitempty"`
	NeedsRestart bool                   `json:"needs_restart,omitempty"`
	Entries      []AuditEntry           `json:"entries,omitempty"`
	NextCursor   string                 `json:"next_cursor,omitempty"`
	Settings     map[string]SettingInfo `json:"settings,omitempty"`
	// StartupValues maps restart-required keys to the values the running
	// provider actually started with (from env vars set at startup).
	// The dashboard compares these against current control-state values
	// to decide whether a restart banner is warranted.
	StartupValues map[string]string `json:"startup_values,omitempty"`
	// BuildVersion is the provider's own release version, answered by the
	// "version" command. Mirrors the provider-side field of the same name.
	BuildVersion string `json:"build_version,omitempty"`
	// MetricsAddrs are the addresses the provider's /metrics listens on,
	// answered by "status". Empty when metrics is off or the provider
	// predates the field.
	MetricsAddrs []string `json:"metrics_addrs,omitempty"`
	// Snapshot is the live node snapshot, answered by "snapshot". Absent
	// from providers that predate the command.
	Snapshot *NodeSnapshot `json:"snapshot,omitempty"`
	// Traffic is the light live-counter reply, answered by "traffic".
	Traffic *LiveTraffic `json:"traffic,omitempty"`
	// ProxyAudit is the provider's proxy audit status, answered by "status" and "audit".
	ProxyAudit *ProxyAuditStatus `json:"proxy_audit,omitempty"`
	Audit      *ProxyAuditStatus `json:"audit,omitempty"`
	Raw        []byte            `json:"-"`
}

// pendingOp is an entry in ~/.urnetwork/pending_overrides.json.
type pendingOp struct {
	Op    string `json:"op"` // "set" or "clear"
	Key   string `json:"key"`
	Value string `json:"value,omitempty"`
}

// controlKeyCanonical maps user-facing CLI keys (both kebab-case and snake_case)
// to the canonical key name accepted by the provider's control socket.
var controlKeyCanonical = map[string]string{
	"node-name":                   "node_name",
	"node_name":                   "node_name",
	"report-url":                  "report_url",
	"report_url":                  "report_url",
	"report-interval":             "report_interval",
	"report_interval":             "report_interval",
	"fast-auth":                   "fast_auth",
	"fast_auth":                   "fast_auth",
	"self-heal":                   "proxy_self_heal",
	"proxy-self-heal":             "proxy_self_heal",
	"proxy_self_heal":             "proxy_self_heal",
	"proxy-audit":                 "proxy_audit",
	"proxy_audit":                 "proxy_audit",
	"proxy-url-max":               "proxy_url_max",
	"proxy_url_max":               "proxy_url_max",
	"proxy-url-refresh":           "proxy_url_refresh",
	"proxy_url_refresh":           "proxy_url_refresh",
	"cleanup-scope":               "proxy_dead_cleanup_scope",
	"proxy-dead-cleanup-scope":    "proxy_dead_cleanup_scope",
	"proxy_dead_cleanup_scope":    "proxy_dead_cleanup_scope",
	"cleanup-interval":            "proxy_dead_cleanup_interval",
	"proxy-dead-cleanup-interval": "proxy_dead_cleanup_interval",
	"proxy_dead_cleanup_interval": "proxy_dead_cleanup_interval",
	"hot-restart":                 "hot_restart",
	"hot_restart":                 "hot_restart",
	"gomemlimit":                  "gomemlimit",
	"go-memlimit":                 "gomemlimit",
	"gogc":                        "gogc",
	"go-gc":                       "gogc",
	"profile":                     "profile",
	"ramlogs":                     "ramlogs",
	"ram-logs":                    "ramlogs",
	"metrics":                     "metrics",
	"metrics_listen":              "metrics_listen",
	"metrics-listen":              "metrics_listen",
}

// canonicalControlKey resolves any user-supplied key name to the socket's
// canonical key. Returns false if the key is unrecognized.
func canonicalControlKey(key string) (string, bool) {
	c, ok := controlKeyCanonical[strings.ToLower(strings.TrimSpace(key))]
	return c, ok
}

// validateControlValue validates values before sending to the socket or
// queueing to pending_overrides.json.
func validateControlValue(canonicalKey, value string) error {
	switch canonicalKey {
	case "report_interval", "proxy_url_refresh", "proxy_dead_cleanup_interval":
		d, err := time.ParseDuration(value)
		if err != nil {
			return fmt.Errorf("%s: invalid duration %q (use e.g. 30s, 5m, 1h)", canonicalKey, value)
		}
		min := 10 * time.Second
		if canonicalKey == "proxy_dead_cleanup_interval" {
			min = time.Minute
		}
		if d < min {
			return fmt.Errorf("%s: %s is below the minimum %s", canonicalKey, value, min)
		}
	case "proxy_url_max":
		n, err := strconv.Atoi(value)
		if err != nil || n < 0 {
			return fmt.Errorf("%s: must be a non-negative integer (got %q)", canonicalKey, value)
		}
	case "proxy_dead_cleanup_scope":
		switch value {
		case "none", "url", "all":
		default:
			return fmt.Errorf("%s: must be none, url, or all (got %q)", canonicalKey, value)
		}
	case "fast_auth", "proxy_self_heal", "proxy_audit":
		switch strings.ToLower(value) {
		case "on", "off":
		default:
			return fmt.Errorf("%s: must be on or off (got %q)", canonicalKey, value)
		}
	case "hot_restart":
		switch strings.ToLower(value) {
		case "on", "off", "1", "0", "true", "false", "yes", "no":
		default:
			return fmt.Errorf("hot_restart: must be on or off (got %q)", value)
		}
	case "metrics":
		switch strings.ToLower(value) {
		case "on", "off":
		default:
			return fmt.Errorf("metrics: must be on or off (got %q)", value)
		}
	case "metrics_listen":
		if strings.EqualFold(value, "auto") || strings.EqualFold(value, "off") {
			return nil
		}
		if ap, err := netip.ParseAddrPort(value); err != nil || ap.Port() == 0 {
			return fmt.Errorf("metrics_listen: must be auto or an IP address with a port, like 100.64.0.10:9100 or 0.0.0.0:9100 (got %q)", value)
		}
	case "ramlogs":
		switch strings.ToLower(value) {
		case "on", "off", "1", "0", "true", "false":
		default:
			return fmt.Errorf("ramlogs: must be on or off (got %q)", value)
		}
	case "profile":
		switch value {
		case "auto", "eco", "lowmem", "turbo-v4", "turbo-v8":
		default:
			return fmt.Errorf("profile: must be auto, eco, lowmem, turbo-v4, or turbo-v8 (got %q)", value)
		}
	case "gomemlimit":
		if _, err := connect.ParseByteCount(value); err != nil {
			return fmt.Errorf("gomemlimit: invalid byte count %q: %w", value, err)
		}
	case "gogc":
		// "off" clears the override, as it does for every tuning key.
		// "disabled" is the distinct value that turns collection off, so a
		// plain "off" can never hand an operator an unbounded heap.
		if !strings.EqualFold(value, "off") && !strings.EqualFold(value, "disabled") {
			n, err := strconv.Atoi(value)
			if err != nil {
				return fmt.Errorf("gogc: must be an integer percentage, 'off' to clear, or 'disabled' to turn collection off (got %q)", value)
			}
			if n < 0 {
				return fmt.Errorf("gogc: must be a non-negative percentage, 'off' to clear, or 'disabled' to turn collection off (got %q)", value)
			}
		}
	}
	return nil
}

// isSocketUnavailable returns true if the error indicates that the Unix domain
// socket does not exist or connection was refused (provider not running).
func isSocketUnavailable(err error) bool {
	if err == nil {
		return false
	}
	if os.IsNotExist(err) || errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	var netErr *net.OpError
	if errors.As(err, &netErr) {
		if errors.Is(netErr.Err, syscall.ENOENT) || errors.Is(netErr.Err, syscall.ECONNREFUSED) {
			return true
		}
		var sysErr *os.SyscallError
		if errors.As(netErr.Err, &sysErr) {
			if errors.Is(sysErr.Err, syscall.ENOENT) || errors.Is(sysErr.Err, syscall.ECONNREFUSED) {
				return true
			}
		}
	}
	// Windows (Winsock) errno compatibility: WSAECONNREFUSED (10061), WSAEINVAL (10022), WSAENOTSOCK (10038)
	// and descriptive socket error messages when dialing an offline Unix domain socket on Windows.
	errStr := strings.ToLower(err.Error())
	if strings.Contains(errStr, "actively refused") ||
		strings.Contains(errStr, "invalid argument") ||
		strings.Contains(errStr, "not a socket") ||
		strings.Contains(errStr, "not found") ||
		strings.Contains(errStr, "cannot find") {
		return true
	}
	return false
}

// sendSocketRequest transmits a single JSON line to the socket and reads back
// the JSON response.
func sendSocketRequest(sockPath string, req controlRequest) (controlResponse, error) {
	return sendSocketRequestTimeout(sockPath, req, 5*time.Second)
}

// sendSocketRequestTimeout is sendSocketRequest with the whole exchange bounded
// by timeout, and the dial by the smaller of that and two seconds. The cheap
// polling commands use a short one so a struggling provider is not also left
// holding their sockets and goroutines.
func sendSocketRequestTimeout(sockPath string, req controlRequest, timeout time.Duration) (controlResponse, error) {
	conn, err := net.DialTimeout("unix", sockPath, min(timeout, 2*time.Second))
	if err != nil {
		return controlResponse{}, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(timeout))

	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return controlResponse{}, err
	}
	var resp controlResponse
	// Cap the response read: provider.sock can be attacker-replaced (it
	// lives in a user-owned dir), so an unbounded read lets a hostile socket
	// stream gigabytes into memory within the 5s deadline. The
	// provider's real replies are short (a few KB at most); reading through
	// an io.LimitReader bounds the allocation AND the newline scan, and a
	// response that fills the budget without a newline is a protocol error.
	const maxSocketResponse = 1 << 20 // 1 MiB
	reader := bufio.NewReader(io.LimitReader(conn, maxSocketResponse+1))
	line, err := reader.ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return controlResponse{}, err
	}
	if len(line) > maxSocketResponse {
		return controlResponse{}, fmt.Errorf("control socket response from %s exceeds %d bytes — refusing (possible hostile socket)", sockPath, maxSocketResponse)
	}
	if err := json.Unmarshal(line, &resp); err != nil {
		return controlResponse{}, err
	}
	resp.Raw = line
	return resp, nil
}

// controlSocketPath returns ~/.urnetwork/provider.sock — the default Unix
// domain socket path for the provider control plane.
func controlSocketPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".urnetwork", "provider.sock"), nil
}

// dialControlSocket connects to the provider's control socket
// (~/.urnetwork/provider.sock), sends the given request, and decodes the response.
func dialControlSocket(req controlRequest) (controlResponse, error) {
	sockPath, err := controlSocketPath()
	if err != nil {
		return controlResponse{}, err
	}
	return sendSocketRequest(sockPath, req)
}

// controlSocketReachable reports whether the provider's control socket
// (~/.urnetwork/provider.sock, derived from StateDir) is accepting now. This
// is the strongest liveness signal for the control-plane plane this feature
// introduced: a provider can be running (pid alive) yet not have its control
// socket bound (startup failed, or a same-user collision). Untouched by the
// set/clear path — a pure reachability probe, so it never mutates state.
func controlSocketReachable(p Provider) bool {
	if p.StateDir == "" {
		return false
	}
	sockPath := filepath.Join(p.StateDir, "provider.sock")
	conn, err := net.DialTimeout("unix", sockPath, 500*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// pendingOverridesLockWait bounds how long a queue update waits for the lock.
// The critical section is a small JSON read-modify-write, so a wait this long
// means the holder is stuck or hostile. The lock lives in a directory the
// provider user owns, so that user can flock it and never let go; without a
// bound a root-run `urnet-tools set` would hang forever.
const pendingOverridesLockWait = 30 * time.Second

// pendingOverridesFile and pendingOverridesTmp name the queue and its temp
// file inside the state dir. The temp name is fixed (not CreateTemp): the
// cross-process lock serializes writers, and a fixed name lets a stale or
// planted entry be removed by name before each write.
const (
	pendingOverridesFile = "pending_overrides.json"
	pendingOverridesTmp  = ".pending_overrides.json.tmp"
)

// queuePendingOverride appends one op to pending_overrides.json in stateDir
// using atomic temp-file-and-rename. The whole read-modify-write is held
// under a cross-process lock: without it, two concurrent `urnet-tools`
// invocations (the provider socket unavailable for both) can each read the
// same queue, append their own op in memory, and let the last rename win
// — both commands report success, but only one op survives to be applied
// at the provider's next startup.
//
// This runs as root against a directory the provider user owns, so the lock,
// the read, the temp write (with its ownership) and the rename all go through
// one descriptor-pinned handle on the state dir instead of re-resolving the
// pathname for each step.
func queuePendingOverride(stateDir, op, key, value string) error {
	return queuePendingOverrideIn("", stateDir, op, key, value)
}

// queuePendingOverrideIn is queuePendingOverride for a state dir known to lie
// beneath the trusted home root (Provider.StateHome), which is then walked
// without following symlinks.
func queuePendingOverrideIn(root, stateDir, op, key, value string) error {
	h, err := openStateDirInCreate(root, stateDir)
	if err != nil {
		return err
	}
	defer h.Close()

	release, err := h.lockFile(pendingOverridesFile+".lock", pendingOverridesLockWait)
	if err != nil {
		return fmt.Errorf("acquire pending-overrides lock: %w", err)
	}
	defer release()

	var ops []pendingOp
	data, err := h.readFile(pendingOverridesFile, maxStateFileBytes)
	switch {
	case err == nil:
		if len(data) > 0 {
			if err := json.Unmarshal(data, &ops); err != nil {
				// Do NOT discard a malformed queue by rewriting it with only the
				// new op: every previously-queued override would be lost and the
				// operator would get a success message. Surface the parse error
				// and leave the file untouched for inspection/fix instead.
				return fmt.Errorf("parse %s: %w (fix or remove the file, then retry)", filepath.Join(stateDir, pendingOverridesFile), err)
			}
		}
	case errors.Is(err, fs.ErrNotExist):
		// no queue yet
	default:
		// A symlink, FIFO or oversize queue is not silently treated as empty
		// and overwritten.
		return fmt.Errorf("read %s: %w", filepath.Join(stateDir, pendingOverridesFile), err)
	}

	ops = append(ops, pendingOp{Op: op, Key: key, Value: value})
	encoded, err := json.MarshalIndent(ops, "", "  ")
	if err != nil {
		return err
	}

	// Clear any stale or planted entry at the temp name first (unlinkat never
	// follows a symlink), so a leftover cannot wedge every later call.
	if err := h.removeAll(pendingOverridesTmp); err != nil {
		return err
	}
	// writeOwned sets the mode and the state-dir owner on the open
	// descriptor, before the file is visible under its final name.
	if err := h.writeOwned(pendingOverridesTmp, append(encoded, '\n'), 0o644); err != nil {
		h.removeAll(pendingOverridesTmp)
		return err
	}
	// rename the already-owned file into place
	if err := h.rename(pendingOverridesTmp, pendingOverridesFile); err != nil {
		h.removeAll(pendingOverridesTmp)
		return err
	}
	return nil
}

// removeLegacyFile removes the legacy per-key override file if one exists,
// so old files don't linger after a socket or queue write.
func removeLegacyFile(stateDir, canonicalKey string) {
	legacyFiles := map[string]string{
		"node_name":                   "node_name",
		"report_url":                  "report_url",
		"report_interval":             "report_interval",
		"fast_auth":                   "fast_auth",
		"proxy_self_heal":             "proxy_self_heal",
		"proxy_url_max":               "proxy_url_max",
		"proxy_url_refresh":           "proxy_url_refresh",
		"proxy_dead_cleanup_scope":    "proxy_dead_cleanup_scope",
		"proxy_dead_cleanup_interval": "proxy_dead_cleanup_interval",
	}
	if name, ok := legacyFiles[canonicalKey]; ok {
		_ = os.Remove(filepath.Join(stateDir, name))
	}
}

// applyControlOverride applies op ("set" or "clear") for key on provider p.
// Returns (appliedLive, needsRestart, error).
func applyControlOverride(p Provider, op, key, value string, dryRun bool) (bool, bool, error) {
	canonicalKey, ok := canonicalControlKey(key)
	if !ok {
		return false, false, fmt.Errorf("unknown key %q (see 'urnet-tools set help')", key)
	}
	if p.StateDir == "" {
		return false, false, fmt.Errorf("provider %s has no resolvable state dir", providerLabel(p))
	}
	if op == "set" {
		if err := validateControlValue(canonicalKey, value); err != nil {
			return false, false, err
		}
	}

	if dryRun {
		return false, false, nil
	}

	sockPath := filepath.Join(p.StateDir, "provider.sock")
	resp, dialErr := sendSocketRequest(sockPath, controlRequest{
		Cmd:   op,
		Key:   canonicalKey,
		Value: value,
	})

	if dialErr == nil {
		if !resp.OK {
			return false, false, fmt.Errorf("%s", resp.Error)
		}
		removeLegacyFile(p.StateDir, canonicalKey)
		return true, resp.NeedsRestart, nil
	}

	if isSocketUnavailable(dialErr) {
		// A provider the tool believes is RUNNING must have a live control
		// socket; an unreachable one means the record is a ghost (e.g. a
		// container process misattributed to the host — see the discovery
		// namespace filter) or the provider is mid-crash. Queuing to a
		// guessed host state-dir in that case both fabricates a state dir
		// (os.MkdirAll below creates e.g. /root/.urnetwork) and prints a
		// fake "queued" success for a config that will never apply.
		// Only STOPPED providers are legitimately queued-to.
		if p.Running {
			return false, false, fmt.Errorf(
				"provider %s is running (pid %d) but its control socket %s is unreachable — not queuing a change that would never apply; check whether this provider is actually running on this host (docker container misattribution?) or restart it",
				providerLabel(p), p.PID, sockPath)
		}
		if err := queuePendingOverrideIn(p.StateHome, p.StateDir, op, canonicalKey, value); err != nil {
			return false, false, fmt.Errorf("queue pending override: %w", err)
		}
		removeLegacyFile(p.StateDir, canonicalKey)
		return false, false, nil
	}

	return false, false, fmt.Errorf("control socket %s: %w", sockPath, dialErr)
}

// queryControlOverride retrieves the current value for canonicalKey on provider p. Checks the socket first, then pending_overrides.json, then legacy files.
func queryControlOverride(p Provider, canonicalKey string) (value string, source string, found bool, err error) {
	if p.StateDir == "" {
		return "", "", false, fmt.Errorf("provider %s has no resolvable state dir", providerLabel(p))
	}

	sockPath := filepath.Join(p.StateDir, "provider.sock")
	resp, dialErr := sendSocketRequest(sockPath, controlRequest{
		Cmd: "get",
		Key: canonicalKey,
	})
	if dialErr == nil && resp.OK && resp.Found {
		return resp.Value, "socket", true, nil
	}

	// Check pending_overrides.json
	pendingPath := filepath.Join(p.StateDir, "pending_overrides.json")
	if data, err := os.ReadFile(pendingPath); err == nil {
		var ops []pendingOp
		if json.Unmarshal(data, &ops) == nil {
			for i := len(ops) - 1; i >= 0; i-- {
				if ops[i].Key == canonicalKey {
					if ops[i].Op == "clear" {
						return "", "pending", false, nil
					}
					return ops[i].Value, "pending", true, nil
				}
			}
		}
	}

	// Check legacy files
	legacyFiles := map[string]string{
		"node_name":                   "node_name",
		"report_url":                  "report_url",
		"report_interval":             "report_interval",
		"fast_auth":                   "fast_auth",
		"proxy_self_heal":             "proxy_self_heal",
		"proxy_url_max":               "proxy_url_max",
		"proxy_url_refresh":           "proxy_url_refresh",
		"proxy_dead_cleanup_scope":    "proxy_dead_cleanup_scope",
		"proxy_dead_cleanup_interval": "proxy_dead_cleanup_interval",
	}
	if filename, ok := legacyFiles[canonicalKey]; ok {
		if b, err := os.ReadFile(filepath.Join(p.StateDir, filename)); err == nil {
			if canonicalKey == "fast_auth" {
				return "on", "legacy", true, nil
			}
			return strings.TrimSpace(string(b)), "legacy", true, nil
		}
	}

	return "", "", false, nil
}

// providerVersionFromSocket asks a running provider what version it is, over
// its own control socket.
//
// This is the only source that answers the question directly. Every other one
// infers it from the filesystem and can be wrong in a way the caller cannot
// detect: buildinfo is empty on -trimpath release builds, and reading or
// exec'ing a path is answering "what is on disk under this name?" — which an
// update's binary swap changes underneath a running process, and which a
// local user can point somewhere else. The socket is bound by the process
// itself inside its own state dir, so a reply can only have come from the
// provider that owns it.
//
// Returns ok=false when the provider is too old to know the command, when the
// socket is not bound, or on any transport error. Callers fall back rather
// than treat a miss as "no version".
func providerVersionFromSocket(p Provider) (string, bool) {
	if p.StateDir == "" {
		return "", false
	}
	sockPath := filepath.Join(p.StateDir, "provider.sock")
	resp, err := sendSocketRequest(sockPath, controlRequest{Cmd: "version"})
	if err != nil || !resp.OK || resp.BuildVersion == "" {
		return "", false
	}
	return resp.BuildVersion, true
}
