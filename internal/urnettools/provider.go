// Package urnettools implements a provider-aware manager for URnetwork
// providers. Its core principle: a provider's identity comes from its JWT
// network name, never from a hardcoded path or the caller's $HOME. The
// package discovers every provider running on the box (across all users),
// lets the operator target one explicitly, and refuses ambiguous operations.
package urnettools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Provider describes one URnetwork provider discovered on the box.
//
// Identity fields (Network, NetworkID, JWTExpires) are decoded from the
// provider's JWT in its state directory; location fields (User, StateDir,
// Binary, Unit, PID, Version, Running) describe how and where it runs.
type Provider struct {
	// User is the OS account the provider runs under (from the process
	// environ or the systemd unit's User=).
	User string
	// StateDir is the provider's data directory (usually <home>/.urnetwork).
	StateDir string
	// Binary is the absolute path of the provider executable.
	Binary string
	// Unit is the systemd unit name owning this provider ("" if none /
	// not a systemd-managed process).
	Unit string
	// PID is the running process ID (0 when the unit exists but is stopped).
	PID int
	// Running reports whether a live process was observed.
	Running bool
	// Version is the provider binary version ("" if undetermined).
	Version string

	// Network is the JWT network_name claim — the identity of the account
	// this provider serves (e.g. "tacogonzalez3000"). This is the ground
	// truth used for targeting; paths are only used to locate state.
	Network string
	// NetworkID is the JWT network_id claim.
	NetworkID string
	// JWTExpires is the JWT exp claim (zero when absent/unparseable).
	JWTExpires time.Time
}

// jwtPayload is the subset of JWT claims the tool needs.
type jwtPayload struct {
	NetworkName string `json:"network_name"`
	NetworkID   string `json:"network_id"`
	Exp         int64  `json:"exp"`
}

// decodeJWT parses a provider JWT file and returns the identity claims.
//
// A JWT is three base64url segments; only the payload (middle) is needed.
// A missing or unparseable JWT is not fatal — the provider is still listed
// with an empty Network so the operator can see it exists, but targeting
// by network will not match it.
func decodeJWT(path string) (netName, netID string, exp time.Time, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", "", time.Time{}, err
	}
	parts := strings.Split(strings.TrimSpace(string(raw)), ".")
	if len(parts) != 3 {
		return "", "", time.Time{}, fmt.Errorf("not a JWT (%d segments)", len(parts))
	}
	payload := parts[1]
	// base64url decoding tolerates missing padding; add it if needed.
	if m := len(payload) % 4; m != 0 {
		payload += strings.Repeat("=", 4-m)
	}
	dec, err := base64.URLEncoding.DecodeString(payload)
	if err != nil {
		return "", "", time.Time{}, fmt.Errorf("payload decode: %w", err)
	}
	var p jwtPayload
	if err := json.Unmarshal(dec, &p); err != nil {
		return "", "", time.Time{}, fmt.Errorf("payload json: %w", err)
	}
	if p.Exp > 0 {
		exp = time.Unix(p.Exp, 0)
	}
	return p.NetworkName, p.NetworkID, exp, nil
}

// readEnviron parses a /proc/<pid>/environ blob (NUL-separated KEY=VALUE)
// into a map. Returns nil on any read error.
func readEnviron(pid int) map[string]string {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
	if err != nil {
		return nil
	}
	env := make(map[string]string)
	for _, kv := range strings.Split(string(raw), "\x00") {
		if i := strings.IndexByte(kv, '='); i > 0 {
			env[kv[:i]] = kv[i+1:]
		}
	}
	return env
}

// stateDirFor returns the provider state directory for a process: the
// process's HOME + "/.urnetwork", falling back to the well-known path
// convention if HOME is unavailable.
func stateDirFor(env map[string]string) string {
	if home := env["HOME"]; home != "" {
		return filepath.Join(home, ".urnetwork")
	}
	return ""
}

// providerVersion returns the version string from a provider binary by
// running "<binary> --version" with a short timeout. Errors yield "".
//
// The timeout is essential: Discover() calls this for every matched process
// synchronously, so a single hung provider binary would otherwise wedge
// every command including read-only `providers` (review finding H1).
func providerVersion(binary string) string {
	if binary == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "--version")
	cmd.Env = append(os.Environ(), "URNETWORK_NO_DOWNLOAD_TARBALL=1")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// unitStateDir returns the state directory implied by a systemd unit's
// User= setting (used for stopped units where no process exists).
func unitStateDir(user string) string {
	if user == "" {
		return ""
	}
	// Best effort: the provider's install convention is
	// ~/.local/share/urnetwork-provider with state in ~/.urnetwork.
	if user == "root" {
		return filepath.Join("/root", ".urnetwork")
	}
	return filepath.Join("/home", user, ".urnetwork")
}

// parsePID parses a decimal PID string, returning 0 on failure.
func parsePID(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}
