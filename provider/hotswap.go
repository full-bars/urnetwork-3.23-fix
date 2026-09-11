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
	"runtime"
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
	HotSwapAckTimeout = 60 * time.Second

	// HotSwapDrainTimeout is how long the retiring parent maintains in-flight streams before exit.
	HotSwapDrainTimeout = 30 * time.Second
)

var (
	// ErrNoNotifySocket indicates that systemd's NOTIFY_SOCKET is not present.
	ErrNoNotifySocket = errors.New("NOTIFY_SOCKET not set")
)

var (
	// isHotSwapDraining tracks whether this process is currently draining in-flight streams.
	isHotSwapDraining atomic.Bool

	// hotSwapLock guards against concurrent hot-swap operations in the same process.
	hotSwapLock sync.Mutex

	// coordinatorClosersMu protects coordinatorClosersMap.
	coordinatorClosersMu  sync.Mutex
	coordinatorCloserSeq  uint64
	coordinatorClosersMap = make(map[uint64]func())

	// Test hook points
	getpidFunc         = os.Getpid
	execInPlaceFunc    = execInPlace
	spawnCandidateFunc = spawnHotSwapCandidate
	exitFunc           = os.Exit
	executableFunc     = os.Executable
	statFunc           = os.Stat
	syscallAccess      = checkExecAccess
)

// installPath is the executable path captured at process start, before any
// updater could move the running binary aside.
//
// os.Executable() reads /proc/self/exe, which follows the inode rather than the
// path. The update flow installs the new build at the provider's binary path
// and moves the running one to a backup, so from that moment os.Executable()
// returns the BACKUP path. Spawning that re-executes the old build: the handoff
// reports success, the operator sees a clean zero-downtime swap, and the
// provider keeps running the version it was already on.
//
// Capturing the path during init pins it while it still names the install
// location, so a handoff always launches whatever now lives there.
//
// On Linux, /proc/self/exe appends " (deleted)" when the running binary has
// been unlinked, yielding a path that does not exist on disk. We strip that
// suffix here so the captured install path always names the real file location.
var installPath = func() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return strings.TrimSuffix(exe, procDeletedSuffix)
}()

// procDeletedSuffix is what readlink("/proc/self/exe") appends once the
// running binary has been unlinked. os.Executable returns it verbatim, with a
// nil error, and the resulting path does not exist.
const procDeletedSuffix = " (deleted)"

// checkExecutable reports whether path can actually be handed to exec. A
// plain existence check is not enough: an updater that has written the new
// build but not yet chmod'd it, or an install path replaced by a directory,
// both pass os.Stat and then fail at spawn. There is an unavoidable TOCTOU
// window between this and the spawn, so this is a way to fail early with a
// useful reason, not a guarantee.
//
// On non-Windows platforms, when a regular file exists but lacks execute
// permission, checkExecutable attempts the same self-healing chmod 0755 that
// spawnHotSwapCandidate performs on a spawn failure. This avoids a hard abort
// that would bypass the repair path and permanently block handoff.
func checkExecutable(path string) error {
	info, err := statFunc(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", path)
	}
	if runtime.GOOS != "windows" {
		// syscall.Access checks whether the calling process actually has
		// permission to execute the file, according to its real UID/GID.
		// The mode-bit mask (&0o111) only checks whether ANY execute bit
		// is set, not whether the caller can use it.
		if err := syscallAccess(path); err != nil {
			// Self-healing: if the file is a regular file but lacks
			// execute permission, repair it rather than aborting the
			// handoff. spawnHotSwapCandidate already does this on a
			// spawn failure, but healing here means the handoff can
			// proceed immediately.
			if errors.Is(err, os.ErrPermission) {
				tlog("⚠️ [hotswap-heal] Install path %s missing execute permission; auto-healing chmod 0755...\n", path)
				if chmodErr := os.Chmod(path, 0o755); chmodErr != nil {
					return fmt.Errorf("%s is not executable and auto-heal chmod failed: %w", path, chmodErr)
				}
				return nil
			}
			return fmt.Errorf("%s is not executable: %w", path, err)
		}
	}
	return nil
}

// hotSwapExecutablePath returns the binary a handoff should launch: the path
// captured at startup, which is where an updater installs the new build.
//
// When that path is unusable the handoff aborts rather than falling back to
// the running image. A handoff exists to change version; re-executing the
// build we are trying to replace, while reporting a clean zero-downtime swap,
// is the exact failure this function was written to remove. Aborting leaves
// the live provider untouched so the updater's verify loop detects the
// mismatch and rolls back from backup rather than silently re-executing the
// same build.
//
// Known limitation: os.Executable resolves /proc/self/exe to the real inode,
// so a deployment that launches the provider through a rotating symlink pins
// the release directory it resolved to, not the symlink. Replacing the file at
// the install path (what the updater does) is handled; rotating a symlink
// above it is not.
func hotSwapExecutablePath() (string, error) {
	rawCurrent, currentErr := executableFunc()
	isDeleted := false
	var current string
	if currentErr == nil {
		isDeleted = strings.HasSuffix(rawCurrent, procDeletedSuffix)
		current = strings.TrimSuffix(rawCurrent, procDeletedSuffix)
	}

	if installPath == "" {
		if currentErr != nil {
			return "", fmt.Errorf("no executable path captured at startup and the running image is unresolvable: %w", currentErr)
		}
		if err := checkExecutable(current); err != nil {
			return "", fmt.Errorf("no executable path captured at startup and the running image %s is unusable: %w", current, err)
		}
		return current, nil
	}

	if err := checkExecutable(installPath); err != nil {
		return "", fmt.Errorf("install path %s is unusable, so this handoff could not change version: %w", installPath, err)
	}

	if currentErr == nil && (isDeleted || current != installPath) {
		if isDeleted {
			tlog("⚡ [hotswap] Running image was unlinked on disk; launching the install path %s instead.\n", installPath)
		} else {
			// The running binary was moved aside, which is exactly what an update
			// does. Launching the install path is the whole point of the handoff.
			tlog("⚡ [hotswap] Running image moved to %s; launching the install path %s instead.\n", current, installPath)
		}
	}
	return installPath, nil
}

// RegisterCoordinatorCloser registers a callback invoked to yield coordinator sessions
// during a hot-swap handoff. It returns a cleanup function that must be deferred to prevent leaks.
func RegisterCoordinatorCloser(closer func()) func() {
	if closer == nil {
		return func() {}
	}
	coordinatorClosersMu.Lock()
	defer coordinatorClosersMu.Unlock()
	coordinatorCloserSeq++
	id := coordinatorCloserSeq
	if coordinatorClosersMap == nil {
		coordinatorClosersMap = make(map[uint64]func())
	}
	coordinatorClosersMap[id] = closer

	var once sync.Once
	return func() {
		once.Do(func() {
			coordinatorClosersMu.Lock()
			delete(coordinatorClosersMap, id)
			coordinatorClosersMu.Unlock()
		})
	}
}

// ClearCoordinatorClosers clears all registered coordinator closers (primarily for testing).
func ClearCoordinatorClosers() {
	coordinatorClosersMu.Lock()
	defer coordinatorClosersMu.Unlock()
	coordinatorClosersMap = make(map[uint64]func())
}

// yieldCoordinatorSession executes all registered callbacks to disconnect from the coordinator.
func yieldCoordinatorSession() {
	coordinatorClosersMu.Lock()
	closers := make([]func(), 0, len(coordinatorClosersMap))
	for _, closer := range coordinatorClosersMap {
		if closer != nil {
			closers = append(closers, closer)
		}
	}
	coordinatorClosersMu.Unlock()

	for _, closer := range closers {
		closer()
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
	dialer := &net.Dialer{Timeout: 2 * time.Second}
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

	exe, err := hotSwapExecutablePath()
	if err != nil {
		tlog("❌ [hotswap] Failed to resolve executable path: %v. Aborting handoff.\n", err)
		return fmt.Errorf("resolve executable: %w", err)
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

	// Branch: Docker Container (PID 1 or container environment) -> In-Place execve
	isDocker := false
	if _, err := os.Stat("/.dockerenv"); err == nil {
		isDocker = true
	}
	if getpidFunc() == 1 || isDocker {
		tlog("⚡ [hotswap] Docker container detected: candidate pre-flight verified -> preparing in-place execve\n")

		// 1. Tell canary child it's done so it exits cleanly (with bounded timeout)
		if err := writeHotswapMessage(session.Writer, HotswapMessage{
			Type:    HotswapMsgCanaryDone,
			PID:     parentPID,
			Version: RequireVersion(),
		}); err != nil {
			tlog("⚠️ [hotswap] Failed to send CANARY_DONE: %v\n", err)
		}

		waitCh := make(chan error, 1)
		go func() {
			waitCh <- session.Wait()
		}()
		select {
		case <-waitCh:
		case <-time.After(5 * time.Second):
			tlog("⚠️ [hotswap] Canary did not exit within 5s; terminating canary\n")
			session.Kill()
		}
		session.Close()

		// 2. Yield live coordinator session and flush metrics immediately before execve
		yieldCoordinatorSession()
		flushRetentionEvents()
		lifetimeStore.Flush()

		tlog("⚡ [hotswap] Executing in-place syscall.Exec (container stays up)...\n")

		// 3. In-place execve: replaces process memory image without altering PID or closing stdout/stderr
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
			tlog("CRITICAL [hotswap] syscall.Exec failed: %v\n", err)
			critLog("FATAL: syscall.Exec failed during hotswap: %v", err)
			exitFunc(1)
			return fmt.Errorf("syscall.Exec: %w", err)
		}
		return nil
	}

	// Standard Unix Branch (Host / systemd): Baton handoff to child
	// Pre-check systemd notify capability: if running under systemd, NOTIFY_SOCKET is required to update MainPID (F-2)
	if os.Getenv("INVOCATION_ID") != "" && os.Getenv("NOTIFY_SOCKET") == "" {
		tlog("❌ [hotswap] Running under systemd without Type=notify (NOTIFY_SOCKET unset). Cannot safely transfer MainPID without service manager terminating unit. Aborting handoff; use standard restart.\n")
		session.Kill()
		return ErrNoNotifySocket
	}

	// 1. Send TAKEOVER to candidate FIRST before yielding
	if err := writeHotswapMessage(session.Writer, HotswapMessage{
		Type:    HotswapMsgTakeover,
		PID:     parentPID,
		Version: RequireVersion(),
	}); err != nil {
		tlog("❌ [hotswap] Failed to send TAKEOVER: %v. Aborting handoff; live provider retained.\n", err)
		session.Kill()
		return err
	}

	// 2. Wait for mandatory ACK from candidate before yielding our own coordinator
	// session. The candidate has not taken over traffic yet, so if the ACK fails
	// or times out we can still abort with the parent's transports untouched.
	ackCh := make(chan readyResult, 1)
	go func() {
		msg, err := readHotswapMessage(session.Reader)
		ackCh <- readyResult{msg, err}
	}()

	select {
	case res := <-ackCh:
		if res.err != nil || res.msg.Type != HotswapMsgAck {
			tlog("❌ [hotswap] Candidate failed active takeover (%v). Aborting handoff; live provider retained.\n", res.err)
			session.Kill()
			return fmt.Errorf("candidate takeover unconfirmed: %v", res.err)
		}
		tlog("⚡ [hotswap] Candidate PID %d confirmed active takeover (ACK received)!\n", childPID)
	case <-time.After(HotSwapAckTimeout):
		tlog("❌ [hotswap] Candidate takeover ACK timed out (>%s). Aborting handoff; live provider retained.\n", HotSwapAckTimeout)
		session.Kill()
		return fmt.Errorf("candidate takeover ACK timed out (>%s)", HotSwapAckTimeout)
	}

	// 3. Only now yield the live coordinator session: the candidate has confirmed
	// active takeover, so there is no window where neither process holds it.
	yieldCoordinatorSession()

	// 4. Update systemd service manager with new MainPID (if running under systemd)
	if os.Getenv("INVOCATION_ID") != "" || os.Getenv("NOTIFY_SOCKET") != "" {
		if err := notifySystemdMainPID(childPID); err != nil {
			if errors.Is(err, ErrNoNotifySocket) {
				tlog("⚠️ [hotswap] Running under systemd without Type=notify (NOTIFY_SOCKET unset). MainPID update skipped.\n")
			} else {
				tlog("⚠️ [hotswap] systemd notify: %v\n", err)
			}
		} else {
			tlog("⚡ [hotswap] systemd updated: MAINPID=%d\n", childPID)
		}
	}

	// 5. Enter graceful stream drain mode
	isHotSwapDraining.Store(true)
	tlog("⚡ [hotswap] Parent PID %d entering graceful stream drain (max %s)...\n", parentPID, HotSwapDrainTimeout)

	go func() {
		// Monitor candidate child liveness during drain.
		//
		// Only when a real child process backs the session: Wait returns
		// immediately otherwise, which makes this case ready at once and
		// leaves the select below picking at random between it and
		// ctx.Done() — reporting a healthy handoff as a dead candidate and
		// exiting non-zero. A nil channel blocks forever, so the select then
		// resolves on the drain timeout or ctx.Done() alone, which is the
		// intended behaviour when there is no candidate process to outlive.
		var childWaitCh <-chan error
		if session.hasChildProcess() {
			ch := make(chan error, 1)
			go func() {
				ch <- session.Wait()
			}()
			childWaitCh = ch
		}

		candidateDied := false
		select {
		case <-time.After(HotSwapDrainTimeout):
			// Normal drain duration elapsed
		case <-childWaitCh:
			tlog("⚠️ [hotswap] Candidate process exited unexpectedly during parent drain!\n")
			candidateDied = true
		case <-ctx.Done():
		}

		flushRetentionEvents()
		lifetimeStore.Flush()
		isHotSwapDraining.Store(false)
		cancel()
		if candidateDied {
			// The candidate already holds MainPID/traffic ownership; it died before
			// taking over, so no process is left serving. Exit non-zero so systemd
			// Restart=on-failure (or the Docker supervision loop) restarts the unit
			// instead of leaving the node silently offline after a clean exit(0).
			critLog("FATAL: hotswap candidate died during parent drain; exiting non-zero for supervisor restart")
			exitFunc(1)
			return
		}
		tlog("⚡ [hotswap] Graceful drain complete -> parent PID %d exiting cleanly.\n", parentPID)
		exitFunc(0)
	}()

	return nil
}

// ResetHotSwapStateForTest resets draining state and closers for test isolation.
func ResetHotSwapStateForTest() {
	isHotSwapDraining.Store(false)
	ClearCoordinatorClosers()
}
