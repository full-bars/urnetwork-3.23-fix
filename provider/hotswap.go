package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/docopt/docopt-go"
)

// HotswapMsgType defines the message actions exchanged over the IPC channel.
type HotswapMsgType string

const (
	// HotswapMsgReady is sent by the candidate child once all pre-flight checks pass.
	HotswapMsgReady HotswapMsgType = "READY"

	// HotswapMsgTakeover is sent by the parent instructing the child to take over live traffic.
	HotswapMsgTakeover HotswapMsgType = "TAKEOVER"

	// HotswapMsgAck is sent by the child confirming active traffic takeover.
	HotswapMsgAck HotswapMsgType = "ACK"

	// HotswapMsgCanaryDone is sent by parent to canary child when running in Docker PID 1 mode
	// to signal that pre-flight succeeded and child should exit cleanly before parent execve.
	HotswapMsgCanaryDone HotswapMsgType = "CANARY_DONE"

	// HotswapMsgError is sent when either side encounters a fatal protocol or pre-flight error.
	HotswapMsgError HotswapMsgType = "ERROR"
)

// HotswapMessage represents the JSON payload framed across the hot-swap IPC channel.
type HotswapMessage struct {
	Type      HotswapMsgType `json:"type"`
	Version   string         `json:"version,omitempty"`
	PID       int            `json:"pid,omitempty"`
	Error     string         `json:"error,omitempty"`
	Timestamp time.Time      `json:"timestamp"`
}

const (
	// EnvHotSwap is set to "1" in candidate child processes to indicate hot-swap mode.
	EnvHotSwap = "URNETWORK_HOTSWAP"

	// HotSwapPreflightTimeout is the max duration the parent waits for the candidate's READY signal.
	HotSwapPreflightTimeout = 20 * time.Second

	// HotSwapAckTimeout is the max duration the parent waits for the candidate's ACK after TAKEOVER.
	HotSwapAckTimeout = 15 * time.Second

	// HotSwapDrainTimeout is how long the retiring parent maintains in-flight streams before exit.
	HotSwapDrainTimeout = 30 * time.Second
)

var (
	// isHotSwapDraining tracks whether this process is currently draining in-flight streams.
	isHotSwapDraining atomic.Bool

	// hotSwapLock guards against concurrent hot-swap operations in the same process.
	hotSwapLock sync.Mutex

	// coordinatorClosersMu protects coordinatorClosers.
	coordinatorClosersMu sync.Mutex
	// coordinatorClosers holds callbacks to close live coordinator connections
	// during a handoff without terminating in-flight client streams.
	coordinatorClosers []func()

	// Test hook points
	getpidFunc         = os.Getpid
	execInPlaceFunc    = execInPlace
	spawnCandidateFunc = spawnHotSwapCandidate
	exitFunc           = os.Exit
)

// RegisterCoordinatorCloser registers a callback invoked to yield coordinator sessions
// during a hot-swap handoff.
func RegisterCoordinatorCloser(closer func()) {
	if closer == nil {
		return
	}
	coordinatorClosersMu.Lock()
	defer coordinatorClosersMu.Unlock()
	coordinatorClosers = append(coordinatorClosers, closer)
}

// ClearCoordinatorClosers clears all registered coordinator closers (primarily for testing).
func ClearCoordinatorClosers() {
	coordinatorClosersMu.Lock()
	defer coordinatorClosersMu.Unlock()
	coordinatorClosers = nil
}

// yieldCoordinatorSession executes all registered callbacks to disconnect from the coordinator.
func yieldCoordinatorSession() {
	coordinatorClosersMu.Lock()
	closers := append([]func(){}, coordinatorClosers...)
	coordinatorClosersMu.Unlock()

	for _, closer := range closers {
		if closer != nil {
			closer()
		}
	}
}

// writeHotswapMessage serializes and writes a newline-delimited JSON message to w.
func writeHotswapMessage(w io.Writer, msg HotswapMessage) error {
	msg.Timestamp = time.Now()
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal hotswap message: %w", err)
	}
	_, err = fmt.Fprintf(w, "%s\n", data)
	return err
}

// readHotswapMessage reads and deserializes a single newline-delimited JSON message from r.
func readHotswapMessage(r *bufio.Reader) (HotswapMessage, error) {
	line, err := r.ReadBytes('\n')
	if err != nil {
		return HotswapMessage{}, err
	}
	var msg HotswapMessage
	if err := json.Unmarshal(line, &msg); err != nil {
		return HotswapMessage{}, fmt.Errorf("unmarshal hotswap message %q: %w", string(line), err)
	}
	return msg, nil
}

// runHotSwapChildPreflight validates tokens, configs, and API reachability before announcing READY.
func runHotSwapChildPreflight(opts docopt.Opts, apiUrl string) error {
	// 1. Verify account JWT exists and is readable with a valid expiry.
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home dir: %w", err)
	}
	jwtPath := filepath.Join(home, ".urnetwork", "jwt")
	jwtBytes, err := os.ReadFile(jwtPath)
	if err != nil {
		if os.IsPermission(err) {
			tlog("⚠️ [hotswap-heal] Repairing permissions on %s (0600)\n", jwtPath)
			_ = os.Chmod(jwtPath, 0600)
			jwtBytes, err = os.ReadFile(jwtPath)
		}
		if err != nil {
			return fmt.Errorf("read %s: %w", jwtPath, err)
		}
	}
	exp := parseJWTExpiryTime(string(jwtBytes))
	if exp == nil {
		return fmt.Errorf("account JWT at %s lacks a valid exp claim", jwtPath)
	}
	if time.Until(*exp) <= 0 {
		return fmt.Errorf("account JWT is expired (%s ago)", time.Since(*exp).Round(time.Second))
	}

	// 2. Verify API URL reachability (host:port dial) with dual-stack retry
	u, err := url.Parse(apiUrl)
	if err != nil {
		return fmt.Errorf("parse api_url %q: %w", apiUrl, err)
	}
	host := u.Host
	if !strings.Contains(host, ":") {
		if u.Scheme == "http" {
			host += ":80"
		} else {
			host += ":443"
		}
	}
	dialer := &net.Dialer{Timeout: 4 * time.Second}
	var lastDialErr error
	for attempt := 1; attempt <= 3; attempt++ {
		conn, err := dialer.Dial("tcp", host)
		if err == nil {
			_ = conn.Close()
			lastDialErr = nil
			break
		}
		lastDialErr = err

		// Dual-stack healing: If IPv6 route is blackholed, try explicit IPv4
		conn4, err4 := dialer.Dial("tcp4", host)
		if err4 == nil {
			_ = conn4.Close()
			tlog("⚡ [hotswap-heal] IPv4 fallback dial to %s succeeded on attempt %d\n", host, attempt)
			lastDialErr = nil
			break
		}
		lastDialErr = err4

		if attempt < 3 {
			time.Sleep(time.Duration(attempt*250) * time.Millisecond)
		}
	}
	if lastDialErr != nil {
		return fmt.Errorf("pre-flight dial to API %s failed: %w", host, lastDialErr)
	}

	return nil
}

// runHotSwapChildHandshake executes the child protocol: pre-flight checks -> announce READY -> wait TAKEOVER.
func runHotSwapChildHandshake(ipcConn io.ReadWriter, opts docopt.Opts, apiUrl string) error {
	tlog("⚡ [hotswap] Candidate process (PID %d, version %s) running pre-flight checks...\n", os.Getpid(), RequireVersion())

	if err := runHotSwapChildPreflight(opts, apiUrl); err != nil {
		_ = writeHotswapMessage(ipcConn, HotswapMessage{
			Type:    HotswapMsgError,
			PID:     os.Getpid(),
			Version: RequireVersion(),
			Error:   err.Error(),
		})
		return err
	}

	// Announce READY to parent
	if err := writeHotswapMessage(ipcConn, HotswapMessage{
		Type:    HotswapMsgReady,
		PID:     os.Getpid(),
		Version: RequireVersion(),
	}); err != nil {
		return fmt.Errorf("send READY: %w", err)
	}

	tlog("⚡ [hotswap] Candidate PID %d passed pre-flight -> announced READY to parent\n", os.Getpid())

	// Wait for response from parent: TAKEOVER (standard) or CANARY_DONE (Docker PID 1)
	reader := bufio.NewReader(ipcConn)
	msg, err := readHotswapMessage(reader)
	if err != nil {
		return fmt.Errorf("wait for parent handoff signal: %w", err)
	}
	switch msg.Type {
	case HotswapMsgTakeover:
		tlog("⚡ [hotswap] TAKEOVER received from parent (PID %d) -> candidate assuming live traffic\n", msg.PID)
		return nil
	case HotswapMsgCanaryDone:
		tlog("⚡ [hotswap] Canary verification confirmed by parent (PID %d) -> exiting cleanly for PID 1 takeover\n", msg.PID)
		exitFunc(0)
		return nil
	default:
		return fmt.Errorf("unexpected message from parent (wanted TAKEOVER or CANARY_DONE, got %s: %s)", msg.Type, msg.Error)
	}
}

// runHotSwapChildAck informs the parent that the child has assumed live traffic.
func runHotSwapChildAck(ipcConn io.Writer) error {
	return writeHotswapMessage(ipcConn, HotswapMessage{
		Type:    HotswapMsgAck,
		PID:     os.Getpid(),
		Version: RequireVersion(),
	})
}

// runHotSwapParentHandoff coordinates spawning the candidate, validating its readiness,
// yielding the coordinator session, and entering graceful stream drain (or in-place execve for PID 1).
func runHotSwapParentHandoff(ctx context.Context, cancel context.CancelFunc, opts docopt.Opts) error {
	if !hotSwapLock.TryLock() {
		tlog("⚠️ [hotswap] Hot-swap already in progress; ignoring duplicate trigger\n")
		return nil
	}
	defer hotSwapLock.Unlock()

	if isHotSwapDraining.Load() {
		tlog("⚠️ [hotswap] Process is already draining; ignoring trigger\n")
		return nil
	}

	exe, err := os.Executable()
	if err != nil {
		exe = os.Args[0]
	}

	parentPID := getpidFunc()
	tlog("⚡ [hotswap] Initiating zero-downtime handoff (live parent PID %d, binary %s)...\n", parentPID, exe)

	session, err := spawnCandidateFunc(exe, os.Args[1:])
	if err != nil {
		tlog("❌ [hotswap] Failed to spawn candidate: %v. Live provider retained untouched.\n", err)
		return err
	}
	defer session.Close()

	// Wait for READY from candidate with timeout
	type readyResult struct {
		msg HotswapMessage
		err error
	}
	readyCh := make(chan readyResult, 1)
	go func() {
		msg, err := readHotswapMessage(session.Reader)
		readyCh <- readyResult{msg, err}
	}()

	var childPID int
	select {
	case res := <-readyCh:
		if res.err != nil {
			tlog("❌ [hotswap] Candidate failed pre-flight or disconnected: %v. Aborting handoff; live provider retained.\n", res.err)
			session.Kill()
			return res.err
		}
		if res.msg.Type != HotswapMsgReady {
			tlog("❌ [hotswap] Candidate pre-flight failed (%s: %s). Aborting handoff; live provider retained.\n", res.msg.Type, res.msg.Error)
			session.Kill()
			return fmt.Errorf("candidate pre-flight failed: %s", res.msg.Error)
		}
		childPID = res.msg.PID
		tlog("⚡ [hotswap] Candidate PID %d reported READY (version=%s)\n", childPID, res.msg.Version)
	case <-time.After(HotSwapPreflightTimeout):
		tlog("❌ [hotswap] Candidate timed out during pre-flight (>%s). Aborting handoff; live provider retained.\n", HotSwapPreflightTimeout)
		session.Kill()
		return errors.New("candidate pre-flight timeout")
	case <-ctx.Done():
		session.Kill()
		return ctx.Err()
	}

	// Branch: Docker PID 1 Container Entrypoint
	if getpidFunc() == 1 {
		tlog("⚡ [hotswap] Docker PID 1 detected: candidate pre-flight verified -> preparing in-place execve\n")

		// 1. Tell canary child it's done so it exits cleanly
		_ = writeHotswapMessage(session.Writer, HotswapMessage{
			Type:    HotswapMsgCanaryDone,
			PID:     parentPID,
			Version: RequireVersion(),
		})
		_ = session.Wait()
		session.Close()

		// 2. Yield live coordinator session cleanly
		yieldCoordinatorSession()

		// 3. Flush retention logs and lifetime metrics before replacing process memory
		flushRetentionEvents()
		lifetimeStore.Flush()

		tlog("⚡ [hotswap] Executing in-place syscall.Exec for PID 1 (container stays up)...\n")

		// 4. In-place execve: replaces process memory image without altering PID 1 or closing stdout/stderr
		var cleanEnv []string
		for _, e := range os.Environ() {
			if !strings.HasPrefix(e, EnvHotSwap+"=") {
				cleanEnv = append(cleanEnv, e)
			}
		}

		args := os.Args
		if len(args) == 0 {
			args = []string{exe}
		}

		if err := execInPlaceFunc(exe, args, cleanEnv); err != nil {
			tlog("❌ [hotswap] syscall.Exec failed: %v. Live provider retained.\n", err)
			return fmt.Errorf("syscall.Exec: %w", err)
		}
		return nil
	}

	// Standard Unix Branch (PID != 1): Baton handoff to child
	// 1. Yield live coordinator session so candidate can connect without collision
	yieldCoordinatorSession()

	// 2. Send TAKEOVER to candidate
	if err := writeHotswapMessage(session.Writer, HotswapMessage{
		Type:    HotswapMsgTakeover,
		PID:     parentPID,
		Version: RequireVersion(),
	}); err != nil {
		tlog("❌ [hotswap] Failed to send TAKEOVER: %v. Aborting handoff.\n", err)
		session.Kill()
		return err
	}

	// 3. Wait for ACK from candidate
	ackCh := make(chan readyResult, 1)
	go func() {
		msg, err := readHotswapMessage(session.Reader)
		ackCh <- readyResult{msg, err}
	}()

	select {
	case res := <-ackCh:
		if res.err != nil || res.msg.Type != HotswapMsgAck {
			tlog("⚠️ [hotswap] Candidate takeover ACK unconfirmed (%v). Proceeding with drain.\n", res.err)
		} else {
			tlog("⚡ [hotswap] Candidate PID %d confirmed active takeover (ACK received)!\n", childPID)
		}
	case <-time.After(HotSwapAckTimeout):
		tlog("⚠️ [hotswap] Candidate takeover ACK timed out (>%s). Proceeding with drain.\n", HotSwapAckTimeout)
	}

	// 4. Update systemd service manager with new MainPID
	if err := notifySystemdMainPID(childPID); err != nil {
		tlog("⚠️ [hotswap] systemd notify: %v\n", err)
	} else {
		tlog("⚡ [hotswap] systemd updated: MAINPID=%d\n", childPID)
	}

	// 5. Enter graceful stream drain mode
	isHotSwapDraining.Store(true)
	tlog("⚡ [hotswap] Parent PID %d entering graceful stream drain (max %s)...\n", parentPID, HotSwapDrainTimeout)

	go func() {
		// Wait for active streams to wind down, capped at HotSwapDrainTimeout
		time.Sleep(HotSwapDrainTimeout)
		flushRetentionEvents()
		lifetimeStore.Flush()
		tlog("⚡ [hotswap] Graceful drain complete -> parent PID %d exiting cleanly.\n", parentPID)
		cancel()
		exitFunc(0)
	}()

	return nil
}

