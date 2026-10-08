// Package main implements a minimal fake provider for CI lifecycle testing.
//
// It stands in for the real provider wherever the test needs a supervised,
// control-socket-serving process without network access or an auth code. The
// OpenRC smoke test installs it as bin/urnetwork, so it must satisfy the same
// contract the real binary does:
//
//   - version reporting through BOTH paths the tool uses: --version (the
//     exec fallback in providerVersion) and the control socket's "version"
//     reply. The socket path is the authoritative one — discovery treats the
//     socket as "what the process IS" (discover_unix.go) — but the exec path
//     is what urnet-tools falls back to, and a test that only covers the
//     socket would miss a regression in the other.
//   - a URL_VERSION_STAMP embedded via -ldflags, matching the real provider's
//     -X main.VersionStamp=URNET_VERSION_STAMP=<tag>, so providerVersionFromStamp
//     resolves the tag out of the raw bytes.
//   - a long-running `provide` that binds $HOME/.urnetwork/provider.sock, so a
//     supervised child is observable and the socket is usable. HOME is what
//     supervise-daemon sets via command_user, so this also proves state lands
//     in the service user's home rather than root's.
//   - `auth <code>` writing $HOME/.urnetwork/jwt with a payload the tool can
//     decode, so network name/id show up in `urnet-tools providers`.
//   - clean SIGTERM exit, so OpenRC stop and supervise-daemon respawn behave.
//
// The real provider cannot be used for these assertions: with no JWT it exits,
// supervise-daemon respawns it every respawn_delay, the PID churns, and the
// update verification loop can never see a stable process.
package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

// Version is the tag reported by --version and the control socket, set via
// -ldflags "-X main.Version=<tag>" like the real provider.
var Version = "fake-provider-dev"

// VersionStamp is an alternative version marker embedded as program data
// (not buildinfo). Unlike -ldflags main.Version, which -trimpath can strip,
// VersionStamp survives as a Go string literal in the binary's data segment.
// urnet-tools reads it with a file grep when buildinfo is empty (the -trimpath
// case). The stub needs the same behavior so update verification resolves the
// running tag through the production path instead of a test-only shortcut.
// Set via: -ldflags "-X main.VersionStamp=URNET_VERSION_STAMP=<tag>"
var VersionStamp string

// VersionStampRef keeps VersionStamp alive in the linked image. Assigning it to
// a package-level variable (rather than only blank-reading it in init) is what
// makes the Go linker retain the string in the data segment, which is where
// providerVersionFromStamp scans for it. Without a real reference the linker
// drops the never-read variable even with -X, and the stamp silently vanishes
// from a -trimpath build — the exact case the stamp exists to cover.
var VersionStampRef = &VersionStamp

var (
	ln       net.Listener
	sockPath string
)

// controlRequest mirrors the wire format from the real provider.
type controlRequest struct {
	Cmd   string `json:"cmd"`
	Key   string `json:"key,omitempty"`
	Value string `json:"value,omitempty"`
}

// controlResponse is the fake provider's reply.
type controlResponse struct {
	OK           bool   `json:"ok"`
	Value        string `json:"value,omitempty"`
	BuildVersion string `json:"build_version,omitempty"`
	Error        string `json:"error,omitempty"`
}

// jwtPayload matches internal/urnettools.jwtPayload so the CLI can decode what
// this stub writes.
type jwtPayload struct {
	NetworkName string `json:"network_name"`
	NetworkID   string `json:"network_id"`
	Exp         int64  `json:"exp"`
}

func main() {
	args := os.Args[1:]
	switch {
	case len(args) == 1 && args[0] == "--version":
		fmt.Println(Version)
		return
	case len(args) >= 1 && args[0] == "auth":
		if err := writeJWT(); err != nil {
			fmt.Fprintf(os.Stderr, "fake-provider: auth: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("Jwt written to " + jwtPath())
		return
	case len(args) == 0 || args[0] == "provide":
		serve()
	default:
		fmt.Fprintf(os.Stderr, "fake-provider: unsupported subcommand %q\n", args[0])
		os.Exit(2)
	}
}

func stateDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine home directory: %w", err)
	}
	return filepath.Join(home, ".urnetwork"), nil
}

func jwtPath() string {
	dir, err := stateDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "jwt")
}

// writeJWT stores a self-contained token the CLI can decode without any
// verification. Not a real credential: the network id is a fixed placeholder
// and the expiry is far future so a long CI run never trips it.
func writeJWT() error {
	dir, err := stateDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("cannot create state dir: %w", err)
	}
	payload, err := json.Marshal(jwtPayload{
		NetworkName: "ci-net",
		NetworkID:   "00000000-0000-0000-0000-000000000001",
		Exp:         4102444800, // 2100-01-01
	})
	if err != nil {
		return err
	}
	// h.<base64url payload>.<base64url signature> — the CLI only reads the
	// payload, so the signature segment is a placeholder.
	token := "h." + base64.RawURLEncoding.EncodeToString(payload) + ".x"
	if err := os.WriteFile(jwtPath(), []byte(token), 0o600); err != nil {
		return fmt.Errorf("cannot write jwt: %w", err)
	}
	return nil
}

func serve() {
	dir, err := stateDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "fake-provider: %v\n", err)
		os.Exit(1)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "fake-provider: cannot create state dir: %v\n", err)
		os.Exit(1)
	}

	sockPath = filepath.Join(dir, "provider.sock")
	_ = os.Remove(sockPath)

	ln, err = net.Listen("unix", sockPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fake-provider: cannot bind %s: %v\n", sockPath, err)
		os.Exit(1)
	}
	defer ln.Close()
	_ = os.Chmod(sockPath, 0o600)

	// OpenRC stop (and `supervise-daemon`'s own TERM on respawn limits) must
	// produce a clean exit, not a killed process, so the test can assert on
	// the child being gone.
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		s := <-sigc
		fmt.Fprintf(os.Stderr, "fake-provider: %v received, exiting\n", s)
		_ = ln.Close()
		_ = os.Remove(sockPath)
		os.Exit(0)
	}()

	fmt.Fprintf(os.Stderr, "fake-provider: listening on %s (pid %d, version %s)\n", sockPath, os.Getpid(), Version)

	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			fmt.Fprintf(os.Stderr, "fake-provider: accept: %v\n", err)
			continue
		}
		go handleConn(conn)
	}
}

func handleConn(conn net.Conn) {
	defer conn.Close()

	reader := bufio.NewReader(conn)
	line, err := reader.ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return
	}

	var req controlRequest
	if err := json.Unmarshal(line, &req); err != nil {
		resp := controlResponse{OK: false, Error: fmt.Sprintf("bad request: %v", err)}
		json.NewEncoder(conn).Encode(resp)
		return
	}

	resp := controlResponse{OK: true}

	switch req.Cmd {
	case "shutdown":
		json.NewEncoder(conn).Encode(resp)
		fmt.Fprintf(os.Stderr, "fake-provider: shutdown requested, exiting\n")
		_ = ln.Close()
		_ = os.Remove(sockPath)
		go func() {
			time.Sleep(50 * time.Millisecond)
			os.Exit(0)
		}()
		return
	case "version":
		resp.BuildVersion = Version
	case "hotswap":
		// Matches the real provider's OpenRC decline so the smoke can assert
		// the reason rather than a generic failure.
		resp = controlResponse{OK: false, Error: "hotswap unavailable: OpenRC has no sd_notify handoff"}
	default:
		resp = controlResponse{OK: true}
	}

	json.NewEncoder(conn).Encode(resp)
}
