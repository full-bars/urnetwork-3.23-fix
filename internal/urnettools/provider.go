// Package urnettools implements a provider-aware manager for URnetwork
// providers. Its core principle: a provider's identity comes from its JWT
// network name, never from a hardcoded path or the caller's $HOME. The
// package discovers every provider running on the box (across all users),
// lets the operator target one explicitly, and refuses ambiguous operations.
package urnettools

import (
	"context"
	"debug/buildinfo"
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

	// BinaryDeleted is true when the running process's on-disk binary was
	// deleted (detected via /proc/<pid>/exe "(deleted)" suffix). Captured
	// before the suffix is stripped from Binary, so the update path can
	// detect a stale process whose binary has been swapped out by a prior
	// partial update.
	BinaryDeleted bool

	// IdentityRestricted is true when the provider's JWT could not be read
	// because the invoking user lacks permission on the state dir (another
	// account's provider seen via unit/process discovery). Network and
	// NetworkID are then empty NOT because the provider has no identity
	// but because it is not readable — the inventory must say so instead
	// of printing a blank net= field that masquerades as valid data
	// (LA1 defect 6c, 2026-08-24).
	IdentityRestricted bool

	// Network is the JWT network_name claim — the identity of the account
	// this provider serves (e.g. "tacogonzalez3000"). This is the ground
	// truth used for targeting; paths are only used to locate state.
	Network string
	// NetworkID is the JWT network_id claim.
	NetworkID string
	// JWTExpires is the JWT exp claim (zero when absent/unparseable).
	JWTExpires time.Time
}

// netLabel returns the provider's network identity for display, or
// "(restricted)" when the JWT could not be read (IdentityRestricted) so no
// display ever shows a blank/leading dashes that hides the reason the network
// is unknown. This is display-only — targeting still
// matches on the raw Network field.
func (p Provider) netLabel() string {
	if p.IdentityRestricted {
		return "(restricted)"
	}
	return p.Network
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
//
// A package-level var (not func) so tests can override it to simulate the
// cross-user case — reading another user's environ fails under an
// unprivileged caller and there is no way to provoke that for real in a
// single-user test sandbox.
var readEnviron = func(pid int) map[string]string {
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

// providerVersion returns the version string from a provider binary.
//
// Primary path: parse the embedded Go build info. The version is normally
// recorded under the "-ldflags" setting as "-X main.Version=<ver>", which
// buildinfo exposes without executing the file.
//
// IMPORTANT: when the binary is built with -trimpath (this repo's Makefile
// does so), Go strips -ldflags from buildinfo.Settings and Main.Version is
// empty. The buildinfo-only path returns "" in that case, which silently
// breaks update-skip and post-restart verification. We fall back to
// running the binary with --version under a tight timeout, gated behind
// isRecognizedExecutable (ELF/Mach-O/PE magic check) to prevent executing
// arbitrary attacker-chosen binaries.
func providerVersion(binary string) string {
	if binary == "" {
		return ""
	}
	if v := providerVersionFromBuildinfo(binary); v != "" {
		return v
	}
	return providerVersionFromExec(binary)
}

// providerVersionFromBuildinfo extracts the version from Go build info
// without executing the binary. Returns "" when no version is recorded
// (e.g. -trimpath builds).
func providerVersionFromBuildinfo(binary string) string {
	info, err := buildinfo.ReadFile(binary)
	if err != nil {
		return ""
	}
	for _, s := range info.Settings {
		if s.Key != "-ldflags" {
			continue
		}
		const prefix = "main.Version="
		idx := strings.Index(s.Value, prefix)
		if idx < 0 {
			continue
		}
		v := s.Value[idx+len(prefix):]
		if sp := strings.IndexByte(v, ' '); sp >= 0 {
			v = v[:sp]
		}
		v = strings.TrimSpace(v)
		if v != "" {
			return v
		}
	}
	if info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return ""
}

// providerVersionFromExec runs the binary with --version under a 3-second
// timeout and returns the first non-empty line of stdout. Used as a fallback
// when build info is unavailable (e.g. -trimpath builds). The 3s timeout
// mirrors the original pre-buildinfo implementation.
//
// SECURITY: gated behind isRecognizedExecutable to prevent executing
// arbitrary attacker-chosen binaries via exec -a argv[0] trick. Without
// this check, any local user can escalate to root on fleet nodes where
// the operator runs sudo urnet-tools.
func providerVersionFromExec(binary string) string {
	// Defense-in-depth: refuse to execute anything that does not look like a
	// provider binary. Discovery already gates this via isProviderArg before
	// setting p.Binary, but re-checking here makes the exec safe even if it is
	// ever called with an unvetted path.
	if !isRecognizedExecutable(binary) {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, binary, "--version").Output()
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(string(out))
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	return line
}

// unitStateDir returns the state directory implied by a systemd unit's
// User= setting (used for stopped units where no process exists).
func unitStateDir(user string) string {
	if user == "" {
		return ""
	}
	// Resolve the real home via getent (matches homeForUser used by the
	// update/reinstall paths); fall back to the
	// /root and /home conventions only when getent fails (e.g. an
	// ephemeral container without passwd entries).
	if home := homeForUser(user); home != "" {
		return filepath.Join(home, ".urnetwork")
	}
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
