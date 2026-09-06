package urnettools

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
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
	Cmd   string `json:"cmd"` // "set", "clear", or "get"
	Key   string `json:"key"`
	Value string `json:"value,omitempty"`
}

// controlResponse is one line response from the control socket.
type controlResponse struct {
	OK    bool   `json:"ok"`
	Value string `json:"value,omitempty"`
	Found bool   `json:"found,omitempty"`
	Error string `json:"error,omitempty"`
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
	case "fast_auth", "proxy_self_heal":
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
		if value != "off" {
			if _, err := strconv.Atoi(value); err != nil {
				return fmt.Errorf("gogc: must be an integer percentage or 'off' (got %q)", value)
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
	conn, err := net.DialTimeout("unix", sockPath, 2*time.Second)
	if err != nil {
		return controlResponse{}, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return controlResponse{}, err
	}
	var resp controlResponse
	if err := json.NewDecoder(bufio.NewReader(conn)).Decode(&resp); err != nil {
		return controlResponse{}, err
	}
	return resp, nil
}

// queuePendingOverride appends one op to pending_overrides.json in stateDir
// using atomic temp-file-and-rename.
func queuePendingOverride(stateDir, op, key, value string) error {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	queueFile := filepath.Join(stateDir, "pending_overrides.json")

	var ops []pendingOp
	data, err := os.ReadFile(queueFile)
	if err == nil && len(data) > 0 {
		_ = json.Unmarshal(data, &ops)
	}

	ops = append(ops, pendingOp{Op: op, Key: key, Value: value})
	encoded, err := json.MarshalIndent(ops, "", "  ")
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp(stateDir, ".pending_overrides.json.tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.Write(append(encoded, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return err
	}
	_ = chownLikeStateOwner(stateDir, tmpName)
	if err := os.Rename(tmpName, queueFile); err != nil {
		return err
	}
	_ = chownLikeStateOwner(stateDir, queueFile)
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
// Returns (appliedLive, error).
func applyControlOverride(p Provider, op, key, value string, dryRun bool) (bool, error) {
	canonicalKey, ok := canonicalControlKey(key)
	if !ok {
		return false, fmt.Errorf("unknown key %q (see 'urnet-tools set help')", key)
	}
	if p.StateDir == "" {
		return false, fmt.Errorf("provider %s has no resolvable state dir", providerLabel(p))
	}
	if op == "set" {
		if err := validateControlValue(canonicalKey, value); err != nil {
			return false, err
		}
	}

	if dryRun {
		return false, nil
	}

	sockPath := filepath.Join(p.StateDir, "provider.sock")
	resp, dialErr := sendSocketRequest(sockPath, controlRequest{
		Cmd:   op,
		Key:   canonicalKey,
		Value: value,
	})

	if dialErr == nil {
		if !resp.OK {
			return false, fmt.Errorf("%s", resp.Error)
		}
		removeLegacyFile(p.StateDir, canonicalKey)
		return true, nil
	}

	if isSocketUnavailable(dialErr) {
		if err := queuePendingOverride(p.StateDir, op, canonicalKey, value); err != nil {
			return false, fmt.Errorf("queue pending override: %w", err)
		}
		removeLegacyFile(p.StateDir, canonicalKey)
		return false, nil
	}

	return false, fmt.Errorf("control socket %s: %w", sockPath, dialErr)
}

// queryControlOverride retrieves the current value for canonicalKey on provider p.
// Checks the socket first, then pending_overrides.json, then legacy files.
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
