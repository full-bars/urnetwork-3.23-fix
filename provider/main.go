package main

import (
	"bufio"
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	mathrand "math/rand"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/net/proxy"
	"golang.org/x/term"

	"github.com/docopt/docopt-go"

	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

const DefaultApiUrl = "https://api.bringyour.com"
const DefaultConnectUrl = "wss://connect.bringyour.com"

// proxyLaunchCount tracks how many proxy goroutines have passed the stagger
// delay and entered provideWithProxy. Used by paceMonitor for progress logging.
var proxyLaunchCount atomic.Int64

var provideStartTime time.Time

// proxyWarmupDone is set true once the initial file-proxy warmup phase
// completes. Hot-reloaded URL-sourced proxies and the URL fetcher wait
// for this before launching, so file proxies get an uncontested ramp.
var proxyWarmupDone atomic.Bool

// backoffPacer calculates the effective start delay for the n-th proxy goroutine.
// staggerMs is the base gap between consecutive proxy starts; ±50% random
// jitter spreads dials within each slot. File proxies typically pass 0 for
// immediate launch (the backend's own rate limiter handles bursts), while
// URL-sourced proxies use a non-zero stagger to avoid thundering-herd.
func backoffPacer(n int, staggerMs int, now time.Time, proxyCtx context.Context) bool {
	if staggerMs <= 0 {
		return true
	}

	jitter := mathrand.Intn(staggerMs + 1) // [0, staggerMs]
	if mathrand.Intn(2) == 0 {             // coinflip — add or subtract
		jitter = -jitter
	}

	wait := time.Duration(n)*time.Duration(staggerMs)*time.Millisecond + time.Duration(jitter)*time.Millisecond

	select {
	case <-proxyCtx.Done():
		return false
	case <-time.After(wait):
	}
	return true
}

// paceMonitor logs real-time warmup progress every 30s. It uses the health
// snapshot to show the user how the proxy fleet is coming online. This is a
// passive observer — the jittered stagger (backoffPacer) and per-transport
// retry (transport.go) handle rate-limiting without global cooldowns.
func paceMonitor(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		up, _, _, _, connecting := connect.ProxyHealthSnapshot()
		total := connect.ProxyHealthCount()
		if total < 5 {
			tlog("🔥 [pace] ✓ warmup: %d up, %d total (< 5) — done\n", up, total)
			proxyWarmupDone.Store(true)
			if reloadPath, err := proxyReloadPath(); err == nil {
				if err := writeReloadTrigger(reloadPath); err != nil {
					tlog("[proxy] warn: failed to signal proxy reload after warmup (write .reload): %v\n", err)
				}
			}
			return
		}
		pct := float64(up) * 100 / float64(total)
		connectingN := len(connecting)
		elapsed := time.Since(provideStartTime)
		if elapsed > 60*time.Minute {
			tlog("🔥 [pace] warmup: %d/%d up (%.0f%%), %d connecting — forced done after 60m\n",
				up, total, pct, connectingN)
			proxyWarmupDone.Store(true)
			if reloadPath, err := proxyReloadPath(); err == nil {
				if err := writeReloadTrigger(reloadPath); err != nil {
					tlog("[proxy] warn: failed to signal proxy reload after warmup (write .reload): %v\n", err)
				}
			}
			return
		}
		if pct < 50 && connectingN > 10 {
			tlog("🔥 [pace] ⚠ warmup: %d/%d up (%.0f%%), %d connecting, %d done\n",
				up, total, pct, connectingN, total-up-connectingN)
		} else if pct > 90 && connectingN < 5 {
			tlog("🔥 [pace] ✓ warmup: %d/%d up (%.0f%%), %d connecting — done\n",
				up, total, pct, connectingN)
			proxyWarmupDone.Store(true)
			if reloadPath, err := proxyReloadPath(); err == nil {
				if err := writeReloadTrigger(reloadPath); err != nil {
					tlog("[proxy] warn: failed to signal proxy reload after warmup (write .reload): %v\n", err)
				}
			}
			return // warmup complete — stop repeating
		} else {
			tlog("🔥 [pace] warmup: %d/%d up (%.0f%%), %d connecting\n",
				up, total, pct, connectingN)
		}
	}
}

// this value is set via the linker, e.g.
// -ldflags "-X main.Version=$WARP_VERSION-$WARP_VERSION_CODE"
var Version string

func init() {
	// debug.SetGCPercent(10)

	initGlog()

	// initPprof()
}

// isLongRunningSubcommand reports whether os.Args invokes the long-running
// provide (or auth-provide) command, as opposed to a one-shot CLI
// subcommand like `proxy remove-dead` or `proxy summary`. Used to decide
// whether stdout/stderr should be redirected into the ramlog: doing so for
// one-shot commands left them producing no visible output to the caller at
// all, since the redirect is process-wide, not scoped to `provide`.
//
// -h/--help/--version are excluded even on `provide`/`auth-provide`: this
// runs from init(), before docopt.ParseArgs gets a chance to handle those
// flags and exit, so a terminating invocation like `provide --help` would
// otherwise have its usage/version text redirected into the ramlog too.
func isLongRunningSubcommand() bool {
	if len(os.Args) < 2 {
		return false
	}
	if os.Args[1] != "provide" && os.Args[1] != "auth-provide" {
		return false
	}
	for _, arg := range os.Args[2:] {
		switch arg {
		case "-h", "--help", "--version":
			return false
		}
	}
	return true
}

func initGlog() {
	flag.Set("logtostderr", "true")
	flag.Set("stderrthreshold", "INFO")
	flag.Set("v", "0")
	// unlike unix, the android/ios standard is for diagnostics to go to stdout
	os.Stderr = os.Stdout

	profile := os.Getenv("URNETWORK_PROFILE")
	ramlogs := os.Getenv("URNETWORK_RAMLOGS") == "1"
	if (profile == "lowmem" || profile == "eco" || ramlogs) && isLongRunningSubcommand() {
		// If explicitly requested via profile or env, just start it.
		// Auto-detection handover is handled in main() with a countdown.
		// One-shot CLI subcommands print directly to the terminal instead.
		initSHMLogger()
	}
}

func initSHMLoggerWithHandover() {
	fmt.Printf("\n[audit] Slow disk detected. Moving all subsequent logs to RAM (/dev/shm) for performance.\n")
	tlog("[audit] >>> To view live logs, run: urnet-tools logs <<<\n")
	tlog("[audit] Redirecting in 3...")
	time.Sleep(1 * time.Second)
	fmt.Printf(" 2...")
	time.Sleep(1 * time.Second)
	fmt.Printf(" 1...\n")
	time.Sleep(1 * time.Second)
	initSHMLogger()
}

func RunStartupAudit() (slowDisk bool, lowSpace bool) {
	if os.Getenv("URNETWORK_SKIP_AUDIT") == "1" {
		tlog("[audit] System audit skipped (URNETWORK_SKIP_AUDIT=1)\n")
		return false, false
	}
	tlog("[audit] Running system checks...\n")
	profile := os.Getenv("URNETWORK_PROFILE")
	ramlogs := os.Getenv("URNETWORK_RAMLOGS")

	// If RAM logs are already ON (manually or via profile), skip disk benchmark
	skipDisk := (ramlogs == "1" || profile == "lowmem" || profile == "eco")

	return connect.RunSystemAudit(skipDisk)
}

func applyLowmodeSettings(clientSettings *connect.ClientSettings, localUserNatSettings *connect.LocalUserNatSettings) {
	if os.Getenv("URNETWORK_PROFILE") != "lowmem" {
		return
	}

	// 1. Initial Contract Size: 2 MiB -> 256 KiB
	clientSettings.ContractManagerSettings.InitialContractTransferByteCount = 256 * 1024

	// 2. IP Buffer Depth: 256 -> 16
	localUserNatSettings.SequenceBufferSize = 16
	localUserNatSettings.TcpBufferSettings.SequenceBufferSize = 16
	localUserNatSettings.UdpBufferSettings.SequenceBufferSize = 16

	// 3. TCP Accordion Window: 1MB -> 32KB
	localUserNatSettings.TcpBufferSettings.MaxWindowSize = 32 * 1024
}

// detectEffectiveRAMLimitBytes returns the effective RAM ceiling in bytes.
// Checks cgroup v2, then cgroup v1, then /proc/meminfo MemTotal.
func detectEffectiveRAMLimitBytes() int64 {
	// cgroup v2
	if data, err := os.ReadFile("/sys/fs/cgroup/memory.max"); err == nil {
		s := strings.TrimSpace(string(data))
		if s != "max" {
			if v, err := strconv.ParseInt(s, 10, 64); err == nil && v > 0 {
				return v
			}
		}
	}
	// cgroup v1 — sentinel for "no limit" is near max int64; filter anything >= 1 TiB
	const oneTiB = 1 << 40
	if data, err := os.ReadFile("/sys/fs/cgroup/memory/memory.limit_in_bytes"); err == nil {
		if v, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64); err == nil && v > 0 && v < oneTiB {
			return v
		}
	}
	// /proc/meminfo MemTotal (kB)
	if f, err := os.Open("/proc/meminfo"); err == nil {
		defer f.Close()
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "MemTotal:") {
				fields := strings.Fields(line)
				if len(fields) >= 2 {
					if v, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
						return v * 1024
					}
				}
			}
		}
	}
	return 850 * 1024 * 1024
}

func applyTurboSettings(clientSettings *connect.ClientSettings, localUserNatSettings *connect.LocalUserNatSettings) {
	profile := os.Getenv("URNETWORK_PROFILE")
	var windowSize uint32
	var queueBytes connect.ByteCount
	switch profile {
	case "turbo-v4":
		windowSize = 4 * 1024 * 1024
		queueBytes = 8 * 1024 * 1024
	case "turbo-v8":
		windowSize = 8 * 1024 * 1024
		queueBytes = 16 * 1024 * 1024
	default:
		return
	}

	// TCP Accordion window — primary per-connection throughput ceiling (window / RTT)
	localUserNatSettings.TcpBufferSettings.MaxWindowSize = windowSize
	localUserNatSettings.UdpBufferSettings.MaxWindowSize = windowSize

	// IP-layer packet queue depth
	localUserNatSettings.SequenceBufferSize = 512
	localUserNatSettings.TcpBufferSettings.SequenceBufferSize = 512
	localUserNatSettings.UdpBufferSettings.SequenceBufferSize = 512

	// Transfer-layer send/receive queues — must scale with window or they become the bottleneck
	clientSettings.SendBufferSettings.ResendQueueMaxByteCount = queueBytes
	clientSettings.ReceiveBufferSettings.ReceiveQueueMaxByteCount = queueBytes

	// Transfer-layer goroutine queue depth
	clientSettings.SendBufferSettings.SequenceBufferSize = 64
	clientSettings.ReceiveBufferSettings.SequenceBufferSize = 64

	// Retention telemetry: wire retained-item ack/drop events to the
	// persistent health event log so retention data survives restarts.
	clientSettings.SendBufferSettings.RetentionEventCallback = appendRetentionEvent

	// WebRTC per-peer DataChannel buffer
	clientSettings.WebRtcSettings.ReceiveBufferSize = connect.ByteCount(windowSize) * 2

	// Faster contract ramp: reach StandardContractTransferByteCount in 3 contracts instead of 4
	clientSettings.ContractManagerSettings.ContractTransferByteSeqScale = 3

	if os.Getenv("GOGC") == "" {
		debug.SetGCPercent(200)
	}
}

// applyPoolAutoSize scales the message pool free-list capacity to RAM/32 at
// startup. The pool default (1 MiB) is badly undersized for 4000+ proxies —
// almost every packet misses the pool and falls back to a GC allocation.
// Skipped when lowmem is active (it manages its own footprint) or when
// --max-memory was set (that path already resizes via maxMemory/8).
func applyPoolAutoSize(maxMemory connect.ByteCount) {
	if maxMemory > 0 {
		return
	}
	if os.Getenv("URNETWORK_PROFILE") == "lowmem" {
		return
	}
	ram := detectEffectiveRAMLimitBytes()
	poolBytes := connect.ByteCount(ram) / 32
	const floor = 8 * 1024 * 1024
	const ceiling = 256 * 1024 * 1024
	if poolBytes < floor {
		poolBytes = floor
	}
	if poolBytes > ceiling {
		poolBytes = ceiling
	}
	connect.ResizeMessagePoolsPerClass(poolBytes)
	tlog("📦 [pool] message pool %dMiB (RAM=%dMiB)\n", poolBytes/1024/1024, connect.ByteCount(ram)/1024/1024)
}

func applyEcoSettings(maxMemory connect.ByteCount) {
	if os.Getenv("URNETWORK_PROFILE") != "eco" {
		return
	}

	if os.Getenv("GOGC") == "" {
		debug.SetGCPercent(50)
	}

	// Only set GOMEMLIMIT if neither --max-memory nor the GOMEMLIMIT env var
	// were provided explicitly; those take precedence.
	if os.Getenv("GOMEMLIMIT") == "" && maxMemory == 0 {
		ramBytes := detectEffectiveRAMLimitBytes()
		ecoLimit := ramBytes * 75 / 100
		debug.SetMemoryLimit(ecoLimit)
	}
}

// ensureMemoryLimit guarantees every provider code path runs under a finite
// GOMEMLIMIT so heap growth is always bounded by the runtime (the ATL2
// outage class: turbo/Tier3/Tier4 and bare `provide` left the process with
// no limit, so nothing pushed back and the kernel chose swap). Call it once
// AFTER all profile/tier eco/turbo application so turbo and eco limits are
// already in place and win. Operator overrides always win.
//
// Order of precedence (first match wins):
//  1. GOMEMLIMIT set in the environment (operator explicit)
//  2. --max-memory flag (maxMemory > 0, applied earlier)
//  3. a finite limit already set by any tier/profile (eco, turbo)
//  4. this function's default: 80% of effective RAM, absolute headroom
//     capped at 1 GiB so a RAM-rich box does not claim RAM other tenants need.
func ensureMemoryLimit(maxMemory connect.ByteCount) {
	if os.Getenv("GOMEMLIMIT") != "" || maxMemory > 0 {
		return // operator precedence, already applied
	}
	// A finite limit from a tier/profile means the box is already protected.
	if cur := debug.SetMemoryLimit(-1); cur > 0 && cur < math.MaxInt64 {
		return
	}
	ram := detectEffectiveRAMLimitBytes()
	if ram <= 0 {
		tlog("[mem] memory limit: cannot detect effective RAM; leaving unset\n")
		return
	}
	// tighter of 80% and (ram - 1 GiB) reserves headroom for other tenants on
	// large boxes while still bounding runaway growth.
	const gib = int64(1) << 30
	const mib = int64(1) << 20
	// ram-gib is negative below 1 GiB; clamp so min() can never pick a negative
	// limit (the tiny-box guard below would correct it, but not by design).
	limit := min(ram*80/100, max(ram-gib, 0))
	if limit < 256*mib {
		// tiny-box guard: a sub-256MiB default would choke a small host.
		limit = ram * 80 / 100
	}
	debug.SetMemoryLimit(limit)
	tlog("[mem] memory limit: %.0fMiB (RAM=%.0fMiB, no explicit GOMEMLIMIT/--max-memory)\n",
		float64(limit)/float64(mib), float64(ram)/float64(mib))
}

func readMemAvailableMiB() int64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return -1
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "MemAvailable:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				if v, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
					return v / 1024
				}
			}
		}
	}
	return -1
}

// readCgroupAvailableMiB returns the free headroom within the active cgroup
// memory limit in MiB, or -1 if no cgroup limit is set.
// This is necessary for correct pressure detection inside Docker containers
// where /proc/meminfo MemAvailable reflects host RAM, not the container limit.
func readCgroupAvailableMiB() int64 {
	const oneTiB = int64(1) << 40

	// cgroup v2
	maxData, maxErr := os.ReadFile("/sys/fs/cgroup/memory.max")
	currData, currErr := os.ReadFile("/sys/fs/cgroup/memory.current")
	if maxErr == nil && currErr == nil {
		maxStr := strings.TrimSpace(string(maxData))
		if maxStr != "max" {
			limit, err1 := strconv.ParseInt(maxStr, 10, 64)
			curr, err2 := strconv.ParseInt(strings.TrimSpace(string(currData)), 10, 64)
			if err1 == nil && err2 == nil && limit > 0 && limit < oneTiB {
				if avail := (limit - curr) / 1024 / 1024; avail >= 0 {
					return avail
				}
				return 0
			}
		}
	}

	// cgroup v1
	limitData, limitErr := os.ReadFile("/sys/fs/cgroup/memory/memory.limit_in_bytes")
	usageData, usageErr := os.ReadFile("/sys/fs/cgroup/memory/memory.usage_in_bytes")
	if limitErr == nil && usageErr == nil {
		limit, err1 := strconv.ParseInt(strings.TrimSpace(string(limitData)), 10, 64)
		usage, err2 := strconv.ParseInt(strings.TrimSpace(string(usageData)), 10, 64)
		if err1 == nil && err2 == nil && limit > 0 && limit < oneTiB {
			if avail := (limit - usage) / 1024 / 1024; avail >= 0 {
				return avail
			}
			return 0
		}
	}

	return -1
}

// ErrTokenInvalid is returned when a token is invalid or expired.
var ErrTokenInvalid = errors.New("auth: token is invalid or expired")

// validateJWTExpiry parses the JWT locally to check the 'exp' claim.
// It returns ErrTokenInvalid if the token is definitely expired (with 30s leeway).
func validateJWTExpiry(byJwt string) error {
	expParser := gojwt.NewParser()
	if tok, _, parseErr := expParser.ParseUnverified(byJwt, gojwt.MapClaims{}); parseErr == nil {
		if claims, ok := tok.Claims.(gojwt.MapClaims); ok {
			if exp, ok := claims["exp"].(float64); ok && time.Now().Unix() > int64(exp)+30 {
				return ErrTokenInvalid
			}
		}
	}
	return nil
}

// parseJWTExpiryTime extracts the exp claim from a JWT without signature verification.
func parseJWTExpiryTime(byJwt string) *time.Time {
	parser := gojwt.NewParser()
	tok, _, err := parser.ParseUnverified(byJwt, gojwt.MapClaims{})
	if err != nil {
		return nil
	}
	claims, ok := tok.Claims.(gojwt.MapClaims)
	if !ok {
		return nil
	}
	exp, ok := claims["exp"].(float64)
	if !ok {
		return nil
	}
	t := time.Unix(int64(exp), 0)
	return &t
}

func jwtContainsClientId(byJwt string) bool {
	parser := gojwt.NewParser()
	tok, _, err := parser.ParseUnverified(byJwt, gojwt.MapClaims{})
	if err != nil {
		return false
	}
	claims, ok := tok.Claims.(gojwt.MapClaims)
	if !ok {
		return false
	}
	_, hasClientId := claims["client_id"]
	return hasClientId
}

// hotRestartEnabled reports whether persisted client JWTs should be reused
// across process restarts. On by default unless URNETWORK_HOT_RESTART=0.
func hotRestartEnabled() bool {
	return os.Getenv("URNETWORK_HOT_RESTART") != "0"
}

// jwtNetworkId extracts the network_id claim from byJwt, if present.
func jwtNetworkId(byJwt string) (string, bool) {
	parser := gojwt.NewParser()
	tok, _, err := parser.ParseUnverified(byJwt, gojwt.MapClaims{})
	if err != nil {
		return "", false
	}
	claims, ok := tok.Claims.(gojwt.MapClaims)
	if !ok {
		return "", false
	}
	networkId, ok := claims["network_id"].(string)
	return networkId, ok
}

// jwtClientId extracts the client_id claim from a client JWT, if present.
func jwtClientId(byJwt string) string {
	parser := gojwt.NewParser()
	tok, _, err := parser.ParseUnverified(byJwt, gojwt.MapClaims{})
	if err != nil {
		return ""
	}
	claims, ok := tok.Claims.(gojwt.MapClaims)
	if !ok {
		return ""
	}
	clientId, _ := claims["client_id"].(string)
	return clientId
}

// accountNetworkId extracts the network_id claim from an account JWT (may be
// empty for malformed tokens — the store treats a mismatch as mint-fresh).
func accountNetworkId(byJwt string) string {
	if nid, ok := jwtNetworkId(byJwt); ok {
		return nid
	}
	return ""
}

// readAccountJWT reads the account (network) JWT from disk. The account-JWT
// refresher may have rotated it, so renewal reads it fresh on every attempt.
func readAccountJWT() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(filepath.Join(home, ".urnetwork", "jwt"))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

func printNetworkIdCmd(opts docopt.Opts) {
	filePath, _ := opts.String("<file>")
	data, err := os.ReadFile(filePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: cannot read %s: %v\n", filePath, err)
		os.Exit(1)
	}
	byJwt := strings.TrimSpace(string(data))
	networkId, ok := jwtNetworkId(byJwt)
	if !ok {
		fmt.Fprintf(os.Stderr, "ERROR: could not extract network_id from %s\n", filePath)
		os.Exit(1)
	}
	fmt.Println(networkId)
}

func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// sessionFilesAllowlist mirrors internal/urnettools/session_cmds.go's
// sessionFiles. Only these files are promoted from staging to the live
// identity directory.
var sessionFilesAllowlist = map[string]bool{
	".client_jwts.json": true,
	"jwt":               true,
	"jwt_last_refresh":  true,
	".provider.key":     true,
	".provider.cert":    true,
	"proxy":             true,
	"proxy_url.json":    true,
	"proxy.state":       true,
}

func isSessionFile(name string) bool {
	return sessionFilesAllowlist[name]
}

// applyStagedSession atomically swaps in identity and proxy-list files
// from ~/.urnetwork/.session-staging/ if a .session-pending marker exists.
// This is the provider-side counterpart to `urnet-tools session load`:
// the shell wrapper writes files to staging, and the provider applies
// them on the next startup so the running process never sees a partial
// or torn set of files.
func applyStagedSession() {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	urNetworkDir := filepath.Join(home, ".urnetwork")
	stagingDir := filepath.Join(urNetworkDir, ".session-staging")
	pending := filepath.Join(urNetworkDir, ".session-pending")

	if _, err := os.Stat(pending); os.IsNotExist(err) {
		return
	}

	tlog("[session] applying staged session from %s\n", stagingDir)

	entries, err := os.ReadDir(stagingDir)
	if err != nil {
		tlog("[session] could not read staging dir: %v\n", err)
		return
	}
	for _, e := range entries {
		// G-M11 fix: only promote files in the session allowlist.
		// The writer side (session_cmds.go) carefully iterates this
		// list; the reader side must match to prevent stray files
		// (older tool versions, manual copies) from landing in the
		// live identity directory.
		if !isSessionFile(e.Name()) {
			continue
		}
		// CR: only promote REGULAR files. An allowlisted symlink or
		// directory in staging must not be promoted — later reads of
		// ~/.urnetwork/jwt could follow a symlink to an attacker-controlled
		// target. os.ReadDir's DirEntry.Type() reports the file type without
		// following symlinks.
		if info, err := e.Info(); err != nil || !info.Mode().IsRegular() {
			tlog("[session] skip staged %s: not a regular file\n", e.Name())
			continue
		}
		src := filepath.Join(stagingDir, e.Name())
		dst := filepath.Join(urNetworkDir, e.Name())
		if err := os.Rename(src, dst); err != nil {
			tlog("[session] rename %s -> %s failed: %v\n", src, dst, err)
		}
	}
	os.RemoveAll(stagingDir)
	os.Remove(pending)
	tlog("[session] staged session applied\n")
}

// sanitizeRootPath rewrites PATH when running as root. Instead of replacing
// PATH with a fixed FHS list (which drops reachable entries on NixOS and
// similar where systemctl lives under /run/current-system/sw/bin), it keeps
// only existing absolute directories that are not group- or world-writable —
// NOR any of their ancestor directories (M8: a root-owned leaf inside an
// attacker-writable parent can be swapped for a malicious binary after
// sanitization at process start; the leaf's permissions alone don't protect
// it) — then appends the safe FHS defaults. Missing/relative/attacker-
// writable entries are dropped.
func sanitizeRootPath() {
	seen := map[string]bool{}
	// Ancestor checks are memoized per sanitized run: PATH entries share
	// deep prefixes (e.g. /usr/lib/...), and stat-ing the whole chain for
	// every entry is wasteful. Cached verdicts stay valid for the lifetime
	// of the call, which is the same window the filter is meant to cover.
	ancestorSafe := map[string]bool{}
	isDirWritableByOthers := func(path string) bool {
		info, err := os.Stat(path)
		if err != nil {
			// Missing ancestor: treat as unsafe — it could be created
			// (and made writable) between now and exec time.
			return true
		}
		if !info.IsDir() {
			return true
		}
		return info.Mode().Perm()&0o022 != 0
	}
	ancestorWritable := func(abs string) bool {
		// Walk from root down to the entry's parent (the leaf itself is
		// checked separately by the caller). "/" (Clean's stop) is skipped.
		p := filepath.Clean(abs)
		var chain []string
		for {
			parent := filepath.Dir(p)
			if parent == p {
				break
			}
			chain = append(chain, p)
			p = parent
		}
		// chain is [leaf, ..., /]; check ancestors only (exclude leaf at 0).
		for i := len(chain) - 1; i >= 1; i-- {
			dir := chain[i]
			if v, ok := ancestorSafe[dir]; ok {
				if !v {
					return true
				}
				continue
			}
			unsafe := isDirWritableByOthers(dir)
			ancestorSafe[dir] = !unsafe
			if unsafe {
				return true
			}
		}
		return false
	}
	var filtered []string
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			continue
		}
		abs := dir
		if !filepath.IsAbs(dir) {
			continue // relative entries are ambiguous for a root child exec
		}
		info, err := os.Stat(abs)
		if err != nil || !info.IsDir() {
			continue // missing or not a dir
		}
		mode := info.Mode()
		// Drop group/world-writable dirs (attacker-jump targets), keep the rest.
		if mode.Perm()&0o022 != 0 {
			continue
		}
		if strings.Contains(abs, "..") {
			continue
		}
		// M8 TOCTOU fix: also drop entries whose ANY ancestor directory is
		// group/world-writable (or missing) — an unprivileged user who can
		// write to a parent can replace the safe leaf after this filter ran.
		if ancestorWritable(abs) {
			continue
		}
		key := filepath.Clean(abs)
		if !seen[key] {
			seen[key] = true
			filtered = append(filtered, key)
		}
	}
	// Append safe FHS defaults (dedup).
	for _, dir := range []string{"/usr/local/sbin", "/usr/local/bin", "/usr/sbin", "/usr/bin", "/sbin", "/bin"} {
		if !seen[filepath.Clean(dir)] {
			seen[filepath.Clean(dir)] = true
			filtered = append(filtered, dir)
		}
	}
	os.Setenv("PATH", strings.Join(filtered, string(os.PathListSeparator)))
}

func main() {
	// G-M2: sanitize PATH when running as root to prevent hijacking
	// of exec.Command bare names (systemctl, docker, etc.) via attacker-writable
	// directories earlier in root's PATH. Filter rather than replace so
	// reachable entries (e.g. NixOS /run/current-system/sw/bin) survive.
	if os.Getuid() == 0 {
		sanitizeRootPath()
	}

	profile := os.Getenv("URNETWORK_PROFILE")
	ramlogs := os.Getenv("URNETWORK_RAMLOGS")

	// If in auto mode and RAM logs aren't already explicitly on, we audit the disk speed
	// BEFORE initializing the logger. This allows us to auto-enable it.
	autoRamLogTriggered := false
	if profile == "auto" && isLongRunningSubcommand() {
		manualRamLogs := (ramlogs == "1")
		slowDisk, _ := RunStartupAudit()
		if slowDisk && !manualRamLogs {
			tlog("[audit] Disk speed is suboptimal. Auto-enabling RAM logs for performance.\n")
			os.Setenv("URNETWORK_RAMLOGS", "1")
			autoRamLogTriggered = true
		}
	} else if isLongRunningSubcommand() {
		// Even if not in auto, run audit for visibility
		RunStartupAudit()
	}

	initGlog()

	// If auto-tuner enabled RAM logs, perform the countdown handover now.
	// Only for the long-running provide process — see isLongRunningSubcommand.
	if autoRamLogTriggered && isLongRunningSubcommand() {
		initSHMLoggerWithHandover()
	}

	usage := fmt.Sprintf(
		`Connect provider.

The default URLs are:
    api_url: %s
    connect_url: %s

Usage:
    provider auth ([<auth_code>] | --user_auth=<user_auth> [--password=<password>]) [-f]
    	[--api_url=<api_url>]
    	[--max-memory=<mem>]
    	[-v...]
    provider provide [--port=<port>]
        [--api_url=<api_url>]
        [--connect_url=<connect_url>]
        [--wallet=<coldkey_ss58>]
        [--max-memory=<mem>]
        [--proxy_file=<proxy_file>]
        [--proxy_url=<proxy_url>...]
        [--proxy_url_refresh=<proxy_url_refresh>]
        [--proxy_url_max=<proxy_url_max>]
        [--proxy_dead_cleanup_scope=<proxy_dead_cleanup_scope>]
        [--proxy_dead_cleanup_interval=<proxy_dead_cleanup_interval>]
        [-v...]
    provider auth-provide ([<auth_code>] | --user_auth=<user_auth> [--password=<password>]) [-f]
    	[--port=<port>]
        [--api_url=<api_url>]
        [--connect_url=<connect_url>]
        [--wallet=<coldkey_ss58>]
        [--max-memory=<mem>]
        [-v...]
    provider wallet set <coldkey_ss58>
        [--api_url=<api_url>]
        [-v...]
    provider claim [--epoch=<epoch>] [--rpc=<rpc_url>]... [--key_file=<key_file>] [--dry-run]
        [--api_url=<api_url>]
        [-v...]
    provider bind-head --hotkey=<hex> --registrant=<registrant> --contract=<contract> [--rpc=<rpc_url>]... [--key_file=<key_file>] [--dry-run]
        [-v...]
    provider unbind-head --hotkey=<hex> [--contract=<contract>] [--rpc=<rpc_url>]... [--key_file=<key_file>] [--dry-run]
        [-v...]
    provider proxy auth add [<key>] <proxy_user> <proxy_password> [-f]
    provider proxy auth remove [<key>] [--all]
    provider proxy add [<key_address>...] [--proxy_file=<proxy_file>] [-f]
    provider proxy remove [<key_address>...] [--all]
    provider proxy remove --match=<pattern> [--yes] [--preview]
    provider proxy remove-dead [--degraded[=<duration>]] [--auth-failures=<N>] [--source=<source>] [--yes] [--preview]
    provider proxy activity
    provider proxy refresh [--force]
    provider proxy add-source <url>
    provider proxy remove-source <url>
    provider proxy exclude [<pattern>] [--remove]
    provider proxy summary
    provider proxy trim <count> [--preview]
    provider direct <state>
    provider proxy paste [--file=<file>]
    provider logs [-n <lines>]
    provider print-network-id <file>
    provider choose_network <api_url> [<connect_url>]
    provider choose_network --reset

Options:
    -h --help                        Show this help and exit.
    --version                        Show version.
    -v...                            Enable verbose mode. -v implies verbose level 1,
    				                 -vv implies level 2... etc.
    -f                               Force overwrite the JWT token store file or proxy value, if exists.
                                     By default, existing values will not be overwritten.
    --api_url=<api_url>              Specify a custom API URL to use.
    --connect_url=<connect_url>      Specify a custom connect URL to use.
    <api_url>                        API URL to save as the chosen network (http:// or https://),
                                     or a preset: main | beta.
    <connect_url>                    Connect URL to save as the chosen network (ws:// or wss://).
    --reset                          With choose_network, clear the saved network and revert to the main network.
    --user_auth=<user_auth>	         Login with a username.
    --password=<password>            Login with a password. If --user_auth is used, you will be prompted for your
    				                 password anyways, if you don't specify it using this option.
    -p --port=<port>                 Status server port [default: 0].
    --max-memory=<mem>               Set the maximum amount of memory in bytes, or the suffixes b, kib, mib, gib may be used [This is a soft limit].
    --wallet=<coldkey_ss58>          Also set the subnet claim wallet at startup, same as provider wallet set.
                                     A failure is logged and does not block providing.
    <coldkey_ss58>                   Subnet claim wallet: an ss58 coldkey address (prefix 42).
    --epoch=<epoch>                  Epoch to fetch the subnet pool claim for. Defaults to the last
                                     finalized epoch, which is the epoch before the current one.
    --rpc=<rpc_url>                  EVM json-rpc endpoint used to check the payout root on-chain.
                                     May be repeated; endpoints are tried in order until one answers.
    --key_file=<key_file>            EVM private key file. When given, claim/bind-head/unbind-head sign
                                     and submit the transaction (via --rpc); without it, the ready-to-submit
                                     calldata is printed for the offline/air-gapped snclaim path.
    --dry-run                        Build and sign the extrinsic but do not submit.
    --hotkey=<hex>                   Head-tier miner hotkey as a 0x-optional 32-byte hex account id.
    --registrant=<registrant>        The EVM address that will submit bindHead via snclaim (0x, 20 bytes).
                                     The head-bind digest is bound to this address, so it MUST equal the
                                     snclaim sender, whose mirror must be the hotkey's on-chain coldkey.
    --contract=<contract>            STSubnet proxy contract address (0x, 20 bytes).
    <key>                            Authentication key
    <proxy_user>                     SOCKS5 user
    <proxy_password>                 SOCKS5 password
    <key_address>                    SOCKS5 server as host:port, host:port:user:pass, host:port::, or key@host:port
    --proxy_file=<proxy_file>        A path to a file where each line contains on entry as host:port, host:port:user:pass, host:port::, or key@host:port
    --proxy_url=<proxy_url>          A live proxy list URL. Repeatable. Additive with --proxy_file / internal config. Also settable via PROXY_URL (comma-separated for multiple).
    --proxy_url_refresh=<dur>        How often to re-fetch --proxy_url sources and add new entries. Also settable via PROXY_URL_REFRESH.
    --proxy_url_max=<n>              Cap on total proxies sourced from --proxy_url. 0 = unlimited, defaults to 500. Also settable via PROXY_URL_MAX.
    --proxy_dead_cleanup_scope=<s>   Automatic dead-proxy cleanup scope: none, url, or all. Defaults to url (URL-sourced only). Also settable via PROXY_DEAD_CLEANUP_SCOPE.
    --proxy_dead_cleanup_interval=<dur>  How often automatic cleanup runs, when scope isn't none. Also settable via PROXY_DEAD_CLEANUP_INTERVAL.
    <url>                            A proxy list URL.
    --match=<pattern>                Case-insensitive substring matched against proxy hosts (never port or
                                     credentials). Removes matches from the proxy list, proxy file, and URL
                                     cache, and excludes the pattern from future URL fetches. See 'proxy exclude'.
    <pattern>                        Host substring for 'proxy exclude' (add). With --remove, deletes the pattern.
                                     With no pattern, 'proxy exclude' lists active patterns.
    --file=<file>                    Read 'proxy paste' input from a file instead of stdin. Each line is a
                                     proxy (any common format) or an http(s) URL to fetch as a proxy source.
    <count>                          Max number of running proxies to keep. The A-F worst-graded above it are shed. 0/off clears the cap.
    --force                          Bypass the 8-hour warmup protection gate.
    -n <lines>                       Number of lines to show from the end of the log [default: 0].`,
		DefaultApiUrl,
		DefaultConnectUrl,
	)

	// Allow `provider help` as a friendlier alias for --help
	if len(os.Args) == 2 && os.Args[1] == "help" {
		os.Args[1] = "--help"
	}

	opts, err := docopt.ParseArgs(usage, os.Args[1:], RequireVersion())

	if err != nil {
		panic(err)
	}

	// Support auth code via environment variable for Docker/dash-prefixed tokens.
	// An explicit CLI positional argument takes precedence over the env var.
	if cur, _ := opts.String("<auth_code>"); cur == "" {
		if envAuthCode := os.Getenv("URNETWORK_AUTH_CODE"); envAuthCode != "" {
			opts["<auth_code>"] = envAuthCode
		}
	}

	if proxy, _ := opts.Bool("proxy"); proxy {
		if auth, _ := opts.Bool("auth"); auth {
			if add, _ := opts.Bool("add"); add {
				proxyAuthAdd(opts)
			} else if remove, _ := opts.Bool("remove"); remove {
				proxyAuthRemove(opts)
			}
		} else if addSource, _ := opts.Bool("add-source"); addSource {
			proxyAddSource(opts)
		} else if paste, _ := opts.Bool("paste"); paste {
			proxyPaste(opts)
		} else if removeSource, _ := opts.Bool("remove-source"); removeSource {
			proxyRemoveSource(opts)
		} else if exclude, _ := opts.Bool("exclude"); exclude {
			proxyExclude(opts)
		} else if add, _ := opts.Bool("add"); add {
			proxyAdd(opts)
		} else if removeDead, _ := opts.Bool("remove-dead"); removeDead {
			proxyRemoveDead(opts)
		} else if remove, _ := opts.Bool("remove"); remove {
			proxyRemove(opts)
		} else if refresh, _ := opts.Bool("refresh"); refresh {
			proxyRefresh(opts)
		} else if activity, _ := opts.Bool("activity"); activity {
			proxyActivity()
		} else if summary, _ := opts.Bool("summary"); summary {
			proxySummary()
		} else if trim, _ := opts.Bool("trim"); trim {
			proxyTrim(opts)
		}
	} else if wallet, _ := opts.Bool("wallet"); wallet {
		if set, _ := opts.Bool("set"); set {
			walletSet(opts)
		}
	} else if claim_, _ := opts.Bool("claim"); claim_ {
		claim(opts)
	} else if bindHead_, _ := opts.Bool("bind-head"); bindHead_ {
		bindHead(opts)
	} else if unbindHead_, _ := opts.Bool("unbind-head"); unbindHead_ {
		unbindHead(opts)
	} else if auth_, _ := opts.Bool("auth"); auth_ {
		auth(opts)
	} else if provide_, _ := opts.Bool("provide"); provide_ {
		provide(opts)
	} else if authProvide, _ := opts.Bool("auth-provide"); authProvide {
		auth(opts)
		provide(opts)
	} else if logs, _ := opts.Bool("logs"); logs {
		providerLogs(opts)
	} else if printNetworkId, _ := opts.Bool("print-network-id"); printNetworkId {
		printNetworkIdCmd(opts)
	} else if chooseNetwork, _ := opts.Bool("choose_network"); chooseNetwork {
		chooseNetworkCmd(opts)
	} else if direct_, _ := opts.Bool("direct"); direct_ {
		cmdDirect(opts)
	}
}

func auth(opts docopt.Opts) {
	home, err := os.UserHomeDir()
	if err != nil {
		shmLogFatal(10, "could not determine home directory: %v", err)
	}
	urNetworkDir := filepath.Join(home, ".urnetwork")
	jwtPath := filepath.Join(urNetworkDir, "jwt")

	if _, err := os.Stat(jwtPath); !errors.Is(err, os.ErrNotExist) {
		// jwt exists
		if force, _ := opts.Bool("-f"); !force {
			fmt.Printf("%s exists. Overwrite? [yN]\n", jwtPath)

			reader := bufio.NewReader(os.Stdin)
			confirm, _ := reader.ReadString('\n')
			if strings.ToLower(strings.TrimSpace(confirm)) != "y" {
				return
			}

		}
	}

	apiUrl, err := resolveApiUrl(opts)
	if err != nil {
		fmt.Printf("network config error: %s\n", err)
		os.Exit(1)
	}

	maxMemoryHumanReadable, err := opts.String("--max-memory")
	var maxMemory connect.ByteCount
	if err == nil {
		maxMemory, err = connect.ParseByteCount(maxMemoryHumanReadable)
		if err != nil {
			panic(fmt.Errorf("Bad mem argument: %s", maxMemoryHumanReadable))
		}
	}
	if 0 < maxMemory {
		connect.ResizeMessagePoolsPerClass(maxMemory / 8)
		debug.SetMemoryLimit(maxMemory)
	}

	event := connect.NewEventWithContext(context.Background())
	event.SetOnSignals(syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM)

	ctx, cancel := context.WithCancel(event.Ctx())
	defer cancel()

	clientStrategy := connect.NewClientStrategyWithDefaults(ctx)

	api := connect.NewBringYourApi(ctx, clientStrategy, apiUrl)

	var byJwt string
	if userAuth, err := opts.String("--user_auth"); err == nil {
		// user_auth and password

		var password string
		if password, err = opts.String("--password"); err == nil && password == "" {
			fmt.Print("Enter password: ")
			passwordBytes, err := term.ReadPassword(int(syscall.Stdin))
			if err != nil {
				panic(err)
			}
			password = string(passwordBytes)
			fmt.Printf("\n")
		}

		// fmt.Printf("userAuth='%s'; password='%s'\n", userAuth, password)

		loginCallback, loginChannel := connect.NewBlockingApiCallback[*connect.AuthLoginWithPasswordResult](ctx)

		loginArgs := &connect.AuthLoginWithPasswordArgs{
			UserAuth: userAuth,
			Password: password,
		}

		api.AuthLoginWithPassword(loginArgs, loginCallback)

		var loginResult connect.ApiCallbackResult[*connect.AuthLoginWithPasswordResult]
		select {
		case <-ctx.Done():
			tlog("[auth] exiting: signal received during login\n")
			os.Exit(0)
		case loginResult = <-loginChannel:
		}

		if loginResult.Error != nil {
			shmLogFatal(11, "authentication request failed: %v", loginResult.Error)
		}
		if loginResult.Result.Error != nil {
			shmLogFatal(12, "authentication failed: %s", loginResult.Result.Error.Message)
		}
		if loginResult.Result.VerificationRequired != nil {
			shmLogFatal(13, "verification required for %s — complete account setup via the app or web first", loginResult.Result.VerificationRequired.UserAuth)
		}

		byJwt = loginResult.Result.Network.ByJwt
	} else {
		// auth_code
		authCode, _ := opts.String("<auth_code>")
		if authCode == "" {
			fmt.Print("Enter auth code: ")
			authCodeBytes, err := term.ReadPassword(int(syscall.Stdin))
			if err != nil {
				panic(err)
			}
			authCode = strings.TrimSpace(string(authCodeBytes))
			fmt.Printf("\n")
		}

		authCodeLogin := &connect.AuthCodeLoginArgs{
			AuthCode: authCode,
		}

		authCodeLoginCallback, authCodeLoginChannel := connect.NewBlockingApiCallback[*connect.AuthCodeLoginResult](ctx)

		api.AuthCodeLogin(authCodeLogin, authCodeLoginCallback)

		var authCodeLoginResult connect.ApiCallbackResult[*connect.AuthCodeLoginResult]
		select {
		case <-ctx.Done():
			tlog("[auth] exiting: signal received during auth-code login\n")
			os.Exit(0)
		case authCodeLoginResult = <-authCodeLoginChannel:
		}

		if authCodeLoginResult.Error != nil {
			shmLogFatal(14, "authentication code request failed: %v", authCodeLoginResult.Error)
		}
		if authCodeLoginResult.Result.Error != nil {
			shmLogFatal(15, "authentication code rejected: %s — auth codes are single-use; if restarting, mount /root/.urnetwork as a persistent volume", authCodeLoginResult.Result.Error.Message)
		}

		byJwt = authCodeLoginResult.Result.ByJwt
	}

	if byJwt != "" {
		if err := os.MkdirAll(urNetworkDir, 0700); err != nil {
			shmLogFatal(16, "could not create %s: %v", urNetworkDir, err)
		}
		if err := atomicWriteFile(jwtPath, []byte(byJwt), 0700); err != nil {
			shmLogFatal(17, "could not write jwt to %s: %v", jwtPath, err)
		}
		fmt.Printf("Jwt written to %s\n", jwtPath)
	}
}

// metricBytesToMiB converts a runtime/metrics Value to MiB. Checks Kind before
// dispatching to avoid panics; logs a warning and returns 0 for unrecognised kinds
// so a wrong metric name surfaces in logs rather than silently reading 0.
func metricBytesToMiB(name string, v metrics.Value) uint64 {
	switch v.Kind() {
	case metrics.KindUint64:
		return v.Uint64() / 1024 / 1024
	case metrics.KindFloat64:
		return uint64(v.Float64()) / 1024 / 1024
	default:
		tlog("[health] warning: metric %q has unreadable kind %v — check metric name\n", name, v.Kind())
		return 0
	}
}

// trafficBytes holds per-proxy byte counters for one tick, used to compute deltas.
type trafficBytes struct {
	rx, tx uint64
}

// fmtRate formats a bytes-per-second rate as a human-readable string.
func fmtRate(bytesPerSec float64) string {
	switch {
	case bytesPerSec >= 1e9:
		return fmt.Sprintf("%.1f GB/s", bytesPerSec/1e9)
	case bytesPerSec >= 1e6:
		return fmt.Sprintf("%.1f MB/s", bytesPerSec/1e6)
	case bytesPerSec >= 1e3:
		return fmt.Sprintf("%.1f KB/s", bytesPerSec/1e3)
	default:
		return fmt.Sprintf("%.0f B/s", bytesPerSec)
	}
}

// fmtBytes formats a byte count as a human-readable string.
func fmtBytes(b uint64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.2f GB", float64(b)/float64(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(b)/float64(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(b)/float64(1<<10))
	default:
		return fmt.Sprintf("%d B", b)
	}
}

// nextMidnight returns the next local midnight after t.
func nextMidnight(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d+1, 0, 0, 0, 0, t.Location())
}

// runEarningWindows emits a per-minute [earn] line with rolling billable windows.
// Tracks cumulative billable across all proxies and reports per-minute deltas for
// the 1m, 5m, 15m, and 60m windows. Handles counter resets (proxy restart) by
// treating a backwards counter as a zero-delta tick. Silent when no proxies are
// registered (non-proxy mode).
// encryptionManagers is a thread-safe registry of the live per-proxy encryption
// session managers. Multiple providers (a native client plus each proxy's
// client) each own their own connect client -> encryption manager, so a single
// pointer was both a data race (written per provideWithProxy, read by the
// periodic [pqe] line) and dropped every manager but the last. The [pqe] line
// sums counts across every registered manager.
var encryptionManagers = struct {
	mu  sync.Mutex
	set []*connect.EncryptionSessionManager
}{}

func registerEncryptionManager(m *connect.EncryptionSessionManager) {
	if m == nil {
		return
	}
	encryptionManagers.mu.Lock()
	defer encryptionManagers.mu.Unlock()
	encryptionManagers.set = append(encryptionManagers.set, m)
}

func unregisterEncryptionManager(m *connect.EncryptionSessionManager) {
	if m == nil {
		return
	}
	encryptionManagers.mu.Lock()
	defer encryptionManagers.mu.Unlock()
	for i, x := range encryptionManagers.set {
		if x == m {
			encryptionManagers.set = append(encryptionManagers.set[:i], encryptionManagers.set[i+1:]...)
			return
		}
	}
}

// pqeTotalCounts sums PQECounts across all live encryption managers.
func pqeTotalCounts() connect.PQECounts {
	var total connect.PQECounts
	encryptionManagers.mu.Lock()
	n := len(encryptionManagers.set)
	snapshot := make([]*connect.EncryptionSessionManager, n)
	copy(snapshot, encryptionManagers.set)
	encryptionManagers.mu.Unlock()
	for _, m := range snapshot {
		c := m.PQECounts()
		total.ActivePQE += c.ActivePQE
		total.ActiveClas += c.ActiveClas
		total.PQEHour += c.PQEHour
		total.PQEDay += c.PQEDay
		total.PQEWeek += c.PQEWeek
		total.PQELifetime += c.PQELifetime
		total.ClasHour += c.ClasHour
		total.ClasDay += c.ClasDay
		total.ClasWeek += c.ClasWeek
		total.ClasLifetime += c.ClasLifetime
	}
	return total
}

// lifetimeStore persists all-time operator metrics across restarts
// (~/.urnetwork/lifetime_metrics.json). Collectors feed it guarded deltas;
// flushes are throttled internally.
var lifetimeStore = loadLifetimeMetrics(lifetimeMetricsPath())

// uDelta returns cur-prev guarding against counter resets (a reading below
// the previous sample means the source restarted; contribute 0, never wrap).
func uDelta(cur uint64, prev *uint64) uint64 {
	var d uint64
	if cur >= *prev {
		d = cur - *prev
	}
	*prev = cur
	return d
}

// u64At returns a pointer to a stable uint64 slot for key, creating the
// entry if absent. Map values are not addressable in Go, so the slot is
// held via *uint64 indirection.
func u64At(m map[string]*uint64, key string) *uint64 {
	if p, ok := m[key]; ok {
		return p
	}
	v := uint64(0)
	m[key] = &v
	return &v
}

// iDelta is uDelta's counterpart for signed counters (contract totals are
// int64 atomics): negative or decreasing readings contribute zero.
func iDelta(cur int64, prev *int64) uint64 {
	var d int64
	if cur >= *prev {
		d = cur - *prev
	}
	*prev = cur
	if d < 0 {
		return 0
	}
	return uint64(d)
}

// runLifetimeCollector samples the existing exported counters once per earn
// tick, converts them to reset-guarded deltas, and feeds the persistent
// store. Read-only over the network stack: nothing here can affect the hot
// path.
func runLifetimeCollector(ctx context.Context) {
	var prevPQE, prevClas uint64
	var prevUp, prevDeny int64
	prevBillable := map[string]uint64{}
	prevRxPerProxy := map[string]*uint64{}
	prevTxPerProxy := map[string]*uint64{}
	var prevTime time.Time

	// departedBaselines remembers the last billable counter reading of
	// proxies that left the health snapshot, so a proxy returning within
	// tombstoneProxyBaselineTTL resumes against its old baseline instead
	// of re-counting its whole history as fresh delta.
	type departedBaseline struct {
		baseline uint64
		left     time.Time
	}
	departedBaselines := map[string]departedBaseline{}
	const tombstoneProxyBaselineTTL = 24 * time.Hour

	ticker := time.NewTicker(earnCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			lifetimeStore.Flush()
			return
		case <-ticker.C:
		}

		pq := pqeTotalCounts()
		up, deny := globalContractMetrics.totals()

		// Billable + transit volumes from the read-only health snapshot.
		// Per-key baselines are TOMBSTONED when a proxy leaves the
		// snapshot (moved to departedBaselines with a timestamp) rather
		// than deleted: a proxy that returns within tombstoneTTL with an
		// intact counter resumes against its old baseline — no double-
		// counting of its history. If it returns
		// RESET below baseline, that gap contributes zero and the
		// baseline re-anchors. Tombstones expire after tombstoneTTL so
		// the map stays bounded; a proxy returning after expiry is
		// treated as new (its residual pre-gap bytes are counted once —
		// accepted worst case).
		var billableSum, rxSum, txSum, clients uint64
		serving := 0
		now := time.Now()
		_, _, _, bw, _ := connect.ProxyHealthSnapshot()
		for key, b := range bw {
			billable := b.BillableRx.Load() + b.BillableTx.Load()
			pb, seen := prevBillable[key]
			if !seen {
				if dep, was := departedBaselines[key]; was && now.Sub(dep.left) < tombstoneProxyBaselineTTL {
					pb, seen = dep.baseline, true
				}
			}
			delete(departedBaselines, key)
			if seen && billable >= pb {
				billableSum += billable - pb
			}
			prevBillable[key] = billable
			rxSum += b.TotalRx.Load()
			txSum += b.TotalTx.Load()
			if c := b.Clients.Load(); c > 0 {
				clients += uint64(c)
				serving++
			}
		}
		// Move baselines of departed proxies into the tombstone map.
		for key := range prevBillable {
			if _, live := bw[key]; !live {
				departedBaselines[key] = departedBaseline{baseline: prevBillable[key], left: now}
				delete(prevBillable, key)
			}
		}
		// Expire stale tombstones so the map stays bounded under churn.
		for key, dep := range departedBaselines {
			if now.Sub(dep.left) >= tombstoneProxyBaselineTTL {
				delete(departedBaselines, key)
			}
		}

		lifetimeStore.Add(
			uDelta(uint64(pq.PQELifetime), &prevPQE),
			uDelta(uint64(pq.ClasLifetime), &prevClas),
			iDelta(up, &prevUp),
			iDelta(deny, &prevDeny),
			0, 0, // recovered/lost are fed by the health-heartbeat owner
			billableSum,
		)
		lifetimeStore.MaybeFlush(time.Now())

		// All-time rollup line (only once there is something to show).
		a1, a2, a3, a4, a5, a6, a7 := lifetimeStore.Snapshot()
		if a1|a2|a3|a4|a5|a6|a7 != 0 {
			tlog("♾️ [lifetime] all-time: post-quantum=%d classical=%d contracts_acquired=%d denied=%d proxies_recovered=%d lost=%d billable_total=%s\n",
				a1, a2, a3, a4, a5, a6, fmtBytes(a7))
		}

		// Transit visibility: traffic forwarded THROUGH this node to other
		// peers (as opposed to 🔐 [pqe] which counts tunnels terminating HERE).
		if clients > 0 && !prevTime.IsZero() {
			elapsed := now.Sub(prevTime).Seconds()
			if elapsed < 1 {
				elapsed = 1
			}
			// Per-proxy reset guards on the rate sums:
			// an aggregate-only guard lets a churned proxy's return dump
			// its whole counter into one tick's rate.
			var rxDelta, txDelta uint64
			for key, b := range bw {
				rx := b.TotalRx.Load()
				tx := b.TotalTx.Load()
				rxDelta += uDelta(rx, u64At(prevRxPerProxy, key))
				txDelta += uDelta(tx, u64At(prevTxPerProxy, key))
			}
			// Drop rate baselines for proxies no longer in the snapshot
			// (bounded under churn; a returning proxy starts at zero and
			// its first-tick delta is suppressed by the reset guard).
			for key := range prevRxPerProxy {
				if _, live := bw[key]; !live {
					delete(prevRxPerProxy, key)
					delete(prevTxPerProxy, key)
				}
			}
			tlog("🛰️ [relay] as-hop: %d clients via %d proxies — %s in, %s out\n",
				clients, serving,
				fmtRate(float64(rxDelta)/elapsed),
				fmtRate(float64(txDelta)/elapsed))
		} else {
			// Idle tick: re-anchor per-proxy rate baselines so bytes moved
			// during the idle window are not smeared into the next active
			// tick's rate display. Use u64At to create the baseline slot (the
			// active branch does the same): on the FIRST tick prevRxPerProxy is
			// empty while bw is populated with every known proxy, so a bare
			// index dereference panics (nil *uint64) and — with the collector
			// launched as a bare `go` — crashes the whole provider process.
			for key, b := range bw {
				*u64At(prevRxPerProxy, key) = b.TotalRx.Load()
				*u64At(prevTxPerProxy, key) = b.TotalTx.Load()
			}
		}
		prevTime = now
	}
}

func runEarningWindows(ctx context.Context) {
	const maxSamples = 60
	deltas := make([]uint64, 0, maxSamples)
	var prevCum uint64
	var prevSet bool

	ticker := time.NewTicker(earnCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		c := pqeTotalCounts()
		if c.ActivePQE != 0 || c.ActiveClas != 0 || c.PQEHour != 0 || c.PQEDay != 0 ||
			c.PQEWeek != 0 || c.PQELifetime != 0 || c.ClasHour != 0 || c.ClasDay != 0 ||
			c.ClasWeek != 0 || c.ClasLifetime != 0 {
			// "direct-e2e": per-peer TLS tunnels THIS node terminates. Client
			// traffic merely forwarded through us (transit hops) never opens
			// such a session here — those appear on 🛰️ [relay] / 📈 [traffic].
			tlog("🔐 [pqe] direct-e2e tunnels terminated: live pqe=%d classical=%d | since-start: pqe=%d classical=%d | 1h: pqe=%d classical=%d | 24h: pqe=%d classical=%d | 7d: pqe=%d classical=%d\n",
				c.ActivePQE, c.ActiveClas,
				c.PQELifetime, c.ClasLifetime,
				c.PQEHour, c.ClasHour, c.PQEDay, c.ClasDay, c.PQEWeek, c.ClasWeek)
			allPQE, allClas, _, _, _, _, _ := lifetimeStore.Snapshot()
			tlog("🔐 [pqe] all-time (persists across restarts): pqe=%d classical=%d\n", allPQE, allClas)
		}

		if connect.ProxyHealthCount() == 0 {
			prevSet = false
			// No live proxies: clear the per-address earn tracker too, so
			// an address that re-appears later cannot inherit stale
			// "earning" state that would wrongly suppress its first paid
			// probe (an empty snapshot prunes every address).
			globalPerProxyEarnTracker.Update(nil)
			continue
		}

		_, _, _, bw, _ := connect.ProxyHealthSnapshot()

		// Feed the per-address earn tracker so the paid grader's earn-skip
		// sees the same liveness signal the [earn] log reports in aggregate,
		// but keyed by proxy address (delta-based, never cumulative).
		globalPerProxyEarnTracker.Update(bw)

		var cum uint64
		for _, p := range bw {
			cum += p.BillableRx.Load() + p.BillableTx.Load()
		}

		if prevSet {
			if cum >= prevCum {
				deltas = append(deltas, cum-prevCum)
			} else {
				deltas = append(deltas, 0)
			}
			if len(deltas) > maxSamples {
				deltas = deltas[len(deltas)-maxSamples:]
			}

			billable1m := sumLastN(deltas, 1)
			billable5m := sumLastN(deltas, 5)
			billable15m := sumLastN(deltas, 15)
			billable60m := sumLastN(deltas, 60)

			active := "no"
			if billable1m > 0 {
				active = "yes"
			}

			tlog("💰 [earn] billable_1m=%s billable_5m=%s billable_15m=%s billable_60m=%s active=%s\n",
				fmtBytes(billable1m), fmtBytes(billable5m), fmtBytes(billable15m), fmtBytes(billable60m), active)
		}
		prevCum = cum
		prevSet = true
	}
}

// sumLastN sums the last n entries from a slice of uint64. If fewer than n
// entries exist, sums all available (partial window).
func sumLastN(deltas []uint64, n int) uint64 {
	if len(deltas) < n {
		n = len(deltas)
	}
	var total uint64
	for _, d := range deltas[len(deltas)-n:] {
		total += d
	}
	return total
}

// earningReason returns a short, greppable token for the [profit] line's
// reason= field explaining why billable traffic is or isn't moving, so an
// operator can distinguish "no demand" from "no proxies" from "still warming
// up" without cross-referencing other lines. Returns "-" while earning. The
// checks are ordered most-fundamental first: a healthy earning provider needs
// proxies up, clients matched to them, and bytes actually moving.
func earningReason(earning bool, proxiesUp int, clients int64, warmup bool) string {
	switch {
	case earning:
		return "-"
	case warmup:
		return "warmup"
	case proxiesUp == 0:
		return "no_proxies"
	case clients == 0:
		return "idle"
	default:
		return "no_traffic"
	}
}

// profitIdleLogInterval caps how often a non-earning [profit] line is
// printed, so quiet periods (warmup, no assigned clients) don't flood the
// log at the 15s tick rate.
const profitIdleLogInterval = 5 * time.Minute

// runProfitHeartbeat logs a [profit] line focused on whether billable traffic
// is moving right now, distinct from runEarningWindows' longer rolling
// trend. It ticks every 15s but only prints every tick while earning=yes;
// once earning drops to no it prints immediately (so the exact stop time is
// visible) and then throttles to profitIdleLogInterval until traffic resumes.
func runProfitHeartbeat(ctx context.Context) {
	const interval = 15 * time.Second

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var prevBillable uint64
	var prevSet bool
	prevTickTime := time.Now()
	var lastLogTime time.Time
	wasEarning := false
	var prevClients int64

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		if connect.ProxyHealthCount() == 0 {
			prevSet = false
			continue
		}

		proxiesUp, _, _, bw, connecting := connect.ProxyHealthSnapshot()

		var billable uint64
		var clients int64
		var serving int
		for _, p := range bw {
			billable += p.BillableRx.Load() + p.BillableTx.Load()
			pc := p.Clients.Load()
			clients += pc
			if pc > 0 {
				serving++
			}
		}

		now := time.Now()
		if !prevSet {
			prevBillable = billable
			prevTickTime = now
			prevSet = true
			continue
		}

		elapsed := now.Sub(prevTickTime).Seconds()
		if elapsed < 1 {
			elapsed = 1
		}
		var delta uint64
		if billable >= prevBillable {
			delta = billable - prevBillable
		}
		prevBillable = billable
		prevTickTime = now

		earning := delta > 0 && clients > 0
		justStopped := wasEarning && !earning

		// Traffic start/stop markers (✈️/🛬 on clients transition)
		if prevClients == 0 && clients > 0 {
			tlog("✈️ [traffic] started (clients=%d)\n", clients)
		} else if prevClients > 0 && clients == 0 {
			tlog("🛬 [traffic] stopped (was=%d clients)\n", prevClients)
		}
		prevClients = clients

		wasEarning = earning

		if earning || justStopped || lastLogTime.IsZero() || now.Sub(lastLogTime) >= profitIdleLogInterval {
			status := "no"
			if earning {
				status = "yes"
			}
			idle := proxiesUp - serving
			if idle < 0 {
				idle = 0
			}
			// Still actively connecting a meaningful batch == warmup, so a
			// quiet provider mid-ramp reports reason=warmup rather than a
			// false "idle"/"no_traffic". Mirrors paceMonitor's done threshold.
			warmup := len(connecting) >= 5
			reason := earningReason(earning, proxiesUp, clients, warmup)
			profitEmoji := ""
			if status == "yes" {
				profitEmoji = "💰 "
			}
			acquired, denied, utilSum := connect.ContractMetricsSnapshot()
			contractFields := ""
			if acquired+denied > 0 {
				avgUtil := uint64(0)
				if acquired > 0 {
					avgUtil = utilSum / acquired
				}
				contractFields = fmt.Sprintf(" contracts=%d denied=%d avg_util=%d%%", acquired, denied, avgUtil)
			}
			tlog("%s[profit] earning=%s reason=%s clients=%d rate=%s proxies_up=%d serving=%d idle=%d%s\n",
				profitEmoji, status, reason, clients, fmtRate(float64(delta)/elapsed), proxiesUp, serving, idle, contractFields)
			lastLogTime = now
		}
	}
}

// runBillableRateWriter writes the aggregate billable traffic rate (bytes/sec)
// to ~/.urnetwork/billable_rate every 10 seconds. Used by urnet-tools
// idle-update to detect traffic lulls before applying updates.
func runBillableRateWriter(ctx context.Context) {
	const interval = 10 * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	prevBillable := make(map[string]uint64)
	prevTickTime := time.Now()

	tlog("[billable_rate] writer started (interval=%s)\n", interval)

	for {
		select {
		case <-ctx.Done():
			tlog("[billable_rate] writer stopped (shutdown)\n")
			return
		case <-ticker.C:
		}

		if connect.ProxyHealthCount() == 0 {
			for k := range prevBillable {
				delete(prevBillable, k)
			}
			writeRate(0)
			continue
		}

		_, _, _, bw, _ := connect.ProxyHealthSnapshot()
		if len(bw) == 0 {
			for k := range prevBillable {
				delete(prevBillable, k)
			}
			writeRate(0)
			continue
		}

		var totalDelta uint64
		for key, p := range bw {
			cur := p.BillableRx.Load() + p.BillableTx.Load()
			if prev, ok := prevBillable[key]; ok {
				if cur >= prev {
					totalDelta += cur - prev
				}
			}
			prevBillable[key] = cur
		}
		for k := range prevBillable {
			if _, ok := bw[k]; !ok {
				delete(prevBillable, k)
			}
		}

		now := time.Now()
		elapsed := now.Sub(prevTickTime).Seconds()
		if elapsed < 1 {
			elapsed = 1
		}
		prevTickTime = now

		rate := uint64(float64(totalDelta) / elapsed)
		writeRate(rate)
	}
}

func writeRate(rate uint64) {
	dir, ok := proxyHealthDir()
	if !ok {
		return
	}
	path := filepath.Join(dir, "billable_rate")
	tmp := path + ".tmp"
	content := strconv.FormatUint(rate, 10) + "\n"
	if err := os.WriteFile(tmp, []byte(content), 0644); err != nil {
		tlog("[billable_rate] warn: failed to write billable rate file (~/.urnetwork/health/billable_rate): %v\n", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		tlog("[billable_rate] warn: failed to finalize billable rate file (rename tmp -> billable_rate): %v\n", err)
	}
}

// runHealthHeartbeat logs a [health] line at a regular interval with runtime
// memory stats and uptime. Interval is configurable via URNETWORK_HEALTH_INTERVAL
// (e.g. "10m", "1h"); defaults to 5 minutes. Minimum 1 minute.
func runHealthHeartbeat(ctx context.Context, startTime time.Time, profile string) {
	interval := 5 * time.Minute
	if s := os.Getenv("URNETWORK_HEALTH_INTERVAL"); s != "" {
		if d, err := time.ParseDuration(s); err == nil && d >= time.Minute {
			interval = d
		}
	}
	if profile == "" {
		profile = "default"
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// deadConfirmDelay gates confirmed-dead event logging until one pulse cycle has
	// elapsed, so the startup ramp is not recorded as dead.
	const deadConfirmDelay = 65 * time.Minute

	// per-proxy byte counts from the previous tick, used to compute rates.
	prevTick := map[string]trafficBytes{}
	prevTickTime := time.Now()

	// per-proxy billable byte checkpoint at midnight, to show "today" totals.
	midnightCheckpoint := map[string]uint64{}
	nextMidnightReset := nextMidnight(time.Now())

	// velocity and peak tracking
	var prevTotalRx, prevTotalTx uint64
	var peakRx, peakTx uint64
	var peakRxElapsed, peakTxElapsed float64 = 1, 1
	velocityLogged := time.Now()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		samples := []metrics.Sample{
			{Name: "/memory/classes/heap/objects:bytes"},
			{Name: "/memory/classes/total:bytes"},
		}
		metrics.Read(samples)
		heapMiB := metricBytesToMiB("/memory/classes/heap/objects:bytes", samples[0].Value)
		sysMiB := metricBytesToMiB("/memory/classes/total:bytes", samples[1].Value)
		uptime := time.Since(startTime).Truncate(time.Second)
		dohFailures := connect.GetDohFailureCount()
		healthLine := fmt.Sprintf("❤️ [health] uptime=%s profile=%s heap=%dMiB sys=%dMiB goroutines=%d connections=%d proxies=%d",
			uptime, profile, heapMiB, sysMiB, runtime.NumGoroutine(), connect.ActiveConnectionCount(), connect.ActiveProxyConnections())
		if dohFailures > 0 {
			healthLine += fmt.Sprintf(" dns_failures=%d", dohFailures)
		}
		tlog("%s\n", healthLine)

		if connect.ProxyHealthCount() == 0 {
			continue // non-proxy mode: no [health][proxies] lines
		}

		now := time.Now()
		report := connect.ProxyHealthHeartbeat(uptime >= deadConfirmDelay)
		down := len(report.Dead) + len(report.Degraded)
		tlog("❤️ [health][proxies] up=%d down=%d dead=%d degraded=%d recovered=%d lost=%d lifetime_recovered=%d lifetime_lost=%d\n",
			report.Up, down, len(report.Dead), len(report.Degraded),
			len(report.Recovered), len(report.NewlyDegraded),
			report.LifetimeRecovered, report.LifetimeLost)
		// Feed the persistent all-time store: this loop is the exclusive
		// consumer of the heartbeat report, so transition deltas are
		// captured exactly once, here. Lost = up->down EVENTS only:
		// NewlyDegraded (was up, went down) plus NewlyDead (never-up,
		// newly confirmed dead). report.Dead is the COMPLETE currently-
		// dead list rebuilt every tick — counting it here would inflate
		// the persisted counter on every tick a proxy stays dead.
		lifetimeStore.Add(0, 0, 0, 0,
			uint64(len(report.Recovered)),
			uint64(len(report.NewlyDegraded))+uint64(len(report.NewlyDead)), 0)
		if len(report.Dead) > 0 {
			tlog("[health][proxies] unreachable (dead): %s\n", capProxyList(report.Dead, proxyHealthListCap))
		}
		if len(report.Degraded) > 0 {
			tlog("[health][proxies] failing (degraded): %s\n", capProxyList(report.Degraded, proxyHealthListCap))
		}

		// Reset midnight checkpoints when the day rolls over.
		if now.After(nextMidnightReset) {
			for k, bw := range report.Bandwidth {
				midnightCheckpoint[k] = bw.BillableRx.Load() + bw.BillableTx.Load()
			}
			nextMidnightReset = nextMidnight(now)
		}

		// Compute per-tick rates and emit [traffic] lines.
		elapsed := now.Sub(prevTickTime).Seconds()
		if elapsed < 1 {
			elapsed = 1
		}
		var totalRxDelta, totalTxDelta, totalBillable uint64
		var totalClients int64
		activeProxies := 0
		serving := 0
		for key, bw := range report.Bandwidth {
			rx := bw.TotalRx.Load()
			tx := bw.TotalTx.Load()
			clients := bw.Clients.Load()
			totalClients += clients
			if clients > 0 {
				serving++
			}
			prev := prevTick[key]
			// Guard against counter resets: a proxy goroutine restart / hot-reload
			// hands back a fresh zeroed ProxyBandwidth, so the current value can be
			// below the persisted previous one. An unguarded uint64 subtraction would
			// wrap to ~18 EB and print absurd Tbps rates. Treat a backwards counter as
			// a fresh baseline with zero delta for this tick.
			var rxDelta, txDelta uint64
			if rx >= prev.rx {
				rxDelta = rx - prev.rx
			}
			if tx >= prev.tx {
				txDelta = tx - prev.tx
			}
			totalRxDelta += rxDelta
			totalTxDelta += txDelta
			prevTick[key] = trafficBytes{rx: rx, tx: tx}

			if rxDelta == 0 && txDelta == 0 {
				continue
			}
			activeProxies++
			// Same reset guard for the midnight-anchored "today" total: if the live
			// counter dropped below the checkpoint (proxy restart), rebase the
			// checkpoint so billable_today starts from the current value instead of
			// underflowing. A missing checkpoint defaults to 0 (lifetime == today
			// until the first midnight rollover), preserving prior behavior.
			billableTotal := bw.BillableRx.Load() + bw.BillableTx.Load()
			cp := midnightCheckpoint[key]
			if billableTotal < cp {
				cp = billableTotal
				midnightCheckpoint[key] = billableTotal
			}
			billableToday := billableTotal - cp
			totalBillable += billableToday

			// Only emit a per-proxy line when the proxy is actually carrying client
			// sessions. Connected-but-idle proxies still move a few bytes per tick
			// (keepalive), so without this gate every proxy prints a line every tick
			// (thousands of lines), burying other log output. The total rollup below
			// still accounts for all proxies, active or idle.
			if clients == 0 {
				continue
			}
			ageStr := ""
			if age := bw.MaxAge(); age > 0 {
				ageStr = fmt.Sprintf(" age=%s", age.Round(time.Second))
			}
			tlog("📈 [traffic] %s rx=%s tx=%s clients=%d%s billable_today=%s\n",
				key,
				fmtRate(float64(rxDelta)/elapsed),
				fmtRate(float64(txDelta)/elapsed),
				clients,
				ageStr,
				fmtBytes(billableToday),
			)
		}
		prevTickTime = now

		// velocity alerts: detect dramatic rate changes
		totalRx := totalRxDelta
		totalTx := totalTxDelta
		if prevTotalRx > 0 || prevTotalTx > 0 {
			prevTotal := prevTotalRx + prevTotalTx
			currTotal := totalRx + totalTx
			if currTotal > 0 && prevTotal > 0 {
				if currTotal > prevTotal*3 && (currTotal-prevTotal) > 100*1024 {
					if time.Since(velocityLogged) > 5*time.Minute {
						tlog("📈 [traffic] velocity: %.1fx → rx=%s tx=%s (was rx=%s tx=%s)\n",
							float64(currTotal)/float64(prevTotal),
							fmtRate(float64(totalRx)/elapsed),
							fmtRate(float64(totalTx)/elapsed),
							fmtRate(float64(prevTotalRx)/elapsed),
							fmtRate(float64(prevTotalTx)/elapsed))
						velocityLogged = now
					}
				} else if prevTotal > currTotal*3 && (prevTotal-currTotal) > 100*1024 {
					if time.Since(velocityLogged) > 5*time.Minute {
						tlog("📈 [traffic] velocity: %.1fx → rx=%s tx=%s (was rx=%s tx=%s) — traffic dropping\n",
							float64(currTotal)/float64(prevTotal),
							fmtRate(float64(totalRx)/elapsed),
							fmtRate(float64(totalTx)/elapsed),
							fmtRate(float64(prevTotalRx)/elapsed),
							fmtRate(float64(prevTotalTx)/elapsed))
						velocityLogged = now
					}
				}
			}
		}

		// Prune per-proxy bookkeeping for addresses that left the health
		// snapshot (hot-reload removal, degraded reaper, URL-proxy churn under
		// AIMD shed, `proxy remove`). Without this, prevTick/midnightCheckpoint
		// each leak one entry per departed address for the life of the process
		// (finding #7) — the same prune runLifetimeCollector and
		// runBillableRateWriter already do. Live map keys are exactly the
		// report.Bandwidth set.
		live := make(map[string]struct{}, len(report.Bandwidth))
		for key := range report.Bandwidth {
			live[key] = struct{}{}
		}
		for key := range prevTick {
			if _, ok := live[key]; !ok {
				delete(prevTick, key)
				delete(midnightCheckpoint, key)
			}
		}

		// peak tracking: update high water marks and freeze elapsed at the time of the peak
		if totalRx > peakRx {
			peakRx = totalRx
			peakRxElapsed = elapsed
		}
		if totalTx > peakTx {
			peakTx = totalTx
			peakTxElapsed = elapsed
		}

		prevTotalRx = totalRx
		prevTotalTx = totalTx

		earning := "no"
		if totalBillable > 0 {
			earning = "yes"
		}
		tlog("📈 [traffic] total rx=%s tx=%s clients=%d active_proxies=%d billable_today=%s earning=%s peak_rx=%s peak_tx=%s\n",
			fmtRate(float64(totalRxDelta)/elapsed),
			fmtRate(float64(totalTxDelta)/elapsed),
			totalClients,
			activeProxies,
			fmtBytes(totalBillable),
			earning,
			fmtRate(float64(peakRx)/peakRxElapsed),
			fmtRate(float64(peakTx)/peakTxElapsed),
		)
		// [earn] surfaces utilization: how many up proxies are actually carrying
		// users (serving) vs sitting idle. Sustained high idle with up>0 means the
		// platform is not assigning users to this node — an earning signal distinct
		// from [traffic] (bytes) and [contract] (assignments).
		idle := report.Up - serving
		if idle < 0 {
			idle = 0
		}
		tlog("[earn] proxies_up=%d serving=%d idle=%d clients=%d\n",
			report.Up, serving, idle, totalClients)

		// See desiredAddressesForHistoryPruning for why this isn't just
		// currently-registered health entries.
		keepAddrs, pruneErr := desiredAddressesForHistoryPruning()
		if pruneErr != nil {
			tlog("[proxy] warning: could not determine desired proxy addresses for history pruning: %v\n", pruneErr)
			keepAddrs = make(map[string]bool, len(report.Bandwidth))
			for k := range report.Bandwidth {
				keepAddrs[k] = true
			}
		}
		globalProxyFailureHistory.Prune(keepAddrs)
		globalProvenProxies.Prune(keepAddrs)

		// Update proxy.state health snapshot for use by proxy refresh subcommand.
		// proxyStateMu serializes this with reload()'s state write to prevent
		// resurrection of proxies that were removed between our read and write.
		// Entries absent from liveHealth are removed (not marked dead) — they are
		// either deregistered via hot-reload or stale from a prior run.
		go func() {
			// Take the cross-process lock too, so a concurrent CLI run
			// (`provider proxy remove --all` in another process) can't clobber
			// this read-modify-write and silently resurrect proxies the CLI
			// just removed. proxyStateMu alone only serializes in-process
			// writers (finding #11).
			lockRelease, lockErr := acquireProxyLockWithRetry()
			if lockErr != nil {
				tlog("[proxy] warn: health snapshot could not acquire proxy lock, skipping this tick: %v\n", lockErr)
				return
			}
			defer lockRelease()

			proxyStateMu.Lock()
			defer proxyStateMu.Unlock()
			state, err := readProxyState()
			if err != nil {
				return
			}
			if state.StartedAt.IsZero() {
				state.StartedAt = startTime
			}
			liveHealth := connect.ProxyHealthByAddress()
			for addr, entry := range state.Proxies {
				if h, ok := liveHealth[addr]; ok {
					entry.Health = h.Health
					if h.DownSince.IsZero() {
						entry.DownSince = ""
					} else {
						entry.DownSince = h.DownSince.Format(time.RFC3339)
					}
					entry.AuthFailures = h.AuthFailures
					state.Proxies[addr] = entry
				}
			}
			if err := writeProxyState(state); err != nil {
				tlog("[proxy] warn: state write failed: %v\n", err)
			}
		}()

		if dir, ok := proxyHealthDir(); ok {
			writeProxyHealthState(dir, report, now)
			writeProxyHealthEvents(dir, report, now)
			writeProxyTrafficState(dir, report, now)
			writeUsageHistory(dir, report, now)
		}
	}
}

// refreshJWT renews the provider's network JWT (the same token species originally
// minted by the auth command and stored at ~/.urnetwork/jwt). It uses the
// same code-create → code-login flow as initial authentication so it returns
// a network JWT (no client_id claim) instead of a client JWT.
//
// Previous implementation used /network/auth-client which is a client-provisioning
// endpoint — every call minted a new throwaway client_id + device row and
// silently corrupted the on-disk token from network JWT to client JWT.
func refreshJWT(ctx context.Context, apiUrl, byJwt string) (string, error) {
	clientStrategy := connect.NewClientStrategyWithDefaults(ctx)
	api := connect.NewBringYourApi(ctx, clientStrategy, apiUrl)
	api.SetByJwt(byJwt)

	tlog("🔑 [jwt] refresh → step 1/3: requesting auth code...\n")

	ccCallback, ccChannel := connect.NewBlockingApiCallback[*connect.AuthCodeCreateResult](ctx)
	api.AuthCodeCreate(&connect.AuthCodeCreateArgs{}, ccCallback)

	var ccResult connect.ApiCallbackResult[*connect.AuthCodeCreateResult]
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case ccResult = <-ccChannel:
	}
	if ccResult.Error != nil {
		return "", fmt.Errorf("code-create api error: %w", ccResult.Error)
	}
	if ccResult.Result == nil {
		return "", fmt.Errorf("empty result from code-create API")
	}
	if ccResult.Result.Error != nil {
		if ccResult.Result.Error.AuthCodeLimitExceeded {
			return "", fmt.Errorf("auth code limit exceeded: %s", ccResult.Result.Error.Message)
		}
		return "", fmt.Errorf("code-create rejected: %s", ccResult.Result.Error.Message)
	}
	if ccResult.Result.AuthCode == "" {
		return "", fmt.Errorf("empty auth code in response")
	}

	tlog("🔑 [jwt] refresh → step 1/3 ok: auth code received (%d chars)\n", len(ccResult.Result.AuthCode))

	tlog("🔑 [jwt] refresh → step 2/3: exchanging auth code for network JWT...\n")

	clCallback, clChannel := connect.NewBlockingApiCallback[*connect.AuthCodeLoginResult](ctx)
	api.AuthCodeLogin(&connect.AuthCodeLoginArgs{
		AuthCode: ccResult.Result.AuthCode,
	}, clCallback)

	var clResult connect.ApiCallbackResult[*connect.AuthCodeLoginResult]
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case clResult = <-clChannel:
	}
	if clResult.Error != nil {
		return "", fmt.Errorf("code-login api error: %w", clResult.Error)
	}
	if clResult.Result == nil {
		return "", fmt.Errorf("empty result from code-login API")
	}
	if clResult.Result.Error != nil {
		return "", fmt.Errorf("code-login rejected: %s", clResult.Result.Error.Message)
	}
	if clResult.Result.ByJwt == "" {
		return "", fmt.Errorf("empty ByJwt in code-login response")
	}
	if jwtContainsClientId(clResult.Result.ByJwt) {
		return "", fmt.Errorf("regression guard: code-login returned a client JWT instead of a network JWT")
	}

	newJwt := clResult.Result.ByJwt

	tlog("🔑 [jwt] refresh → step 2/3 ok: network JWT received (%d chars)\n", len(newJwt))

	tlog("🔑 [jwt] refresh → step 3/3: verifying new token against %s/transfer/stats...\n", apiUrl)

	// Verify the fresh token works before returning it so the caller never
	// overwrites the on-disk JWT with a dead token. Uses a lightweight read-only
	// API endpoint to keep side effects zero (no auth codes, no client rows).
	req, err := http.NewRequestWithContext(ctx, "GET", apiUrl+"/transfer/stats", nil)
	if err != nil {
		return "", fmt.Errorf("could not build verification request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+newJwt)
	verifyClient := &http.Client{Timeout: 30 * time.Second}
	resp, err := verifyClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("fresh token failed verification: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("fresh token rejected by server (HTTP %d)", resp.StatusCode)
	}

	tlog("🔑 [jwt] refresh → step 3/3 ok: verification passed (HTTP %d)\n", resp.StatusCode)

	return newJwt, nil
}

// runJWTRefresher polls once per hour and refreshes the JWT under two
// independent conditions (OR logic — either one triggers a refresh):
//
//  1. Periodic refresh: it has been >= 7 days since the last successful
//     refresh, regardless of the token's actual expiry. This is the primary
//     mechanism — it guarantees the token is rotated on a fixed cadence so
//     it never gets anywhere near expiry under normal operation.
//  2. Expiry fallback: the token's exp claim is within 48 hours of expiring.
//     This is a safety net in case the periodic refresh above failed
//     repeatedly (e.g. multi-day API outage) — it gives a last-resort window
//     to recover before the provider would otherwise hit the exit-78 cycle.
//
// The last successful refresh time is persisted to disk (next to the JWT
// file) so the 7-day cadence survives provider restarts.
func runJWTRefresher(ctx context.Context, apiUrl string) {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	jwtPath := filepath.Join(home, ".urnetwork", "jwt")
	lastRefreshPath := filepath.Join(home, ".urnetwork", "jwt_last_refresh")

	const periodicInterval = 7 * 24 * time.Hour
	const expiryFallbackWindow = 48 * time.Hour

	// Startup jitter (0-9 minutes) to desynchronize refresh checks across the fleet.
	jitterMs := time.Duration(mathrand.Intn(10)) * time.Minute
	select {
	case <-time.After(jitterMs):
	case <-ctx.Done():
		return
	}

	readLastRefreshTime := func() time.Time {
		data, err := os.ReadFile(lastRefreshPath)
		if err != nil {
			// No record on disk — treat as never refreshed so the periodic
			// condition fires on the first check and establishes a baseline.
			return time.Time{}
		}
		unixSec, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
		if err != nil {
			return time.Time{}
		}
		return time.Unix(unixSec, 0)
	}

	writeLastRefreshTime := func(t time.Time) error {
		return atomicWriteFile(lastRefreshPath, []byte(strconv.FormatInt(t.Unix(), 10)), 0700)
	}

	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	for {
		byJwtBytes, err := os.ReadFile(jwtPath)
		if err != nil {
			if !os.IsNotExist(err) {
				// Log non-NotExist errors (permission denied, truncated file, etc.)
				// so operators can diagnose why the refresher is stuck.
				tlog("[auth] warn: could not read jwt file for refresh: %v\n", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				continue
			}
		}
		if err == nil {
			byJwt := strings.TrimSpace(string(byJwtBytes))

			lastRefreshTime := readLastRefreshTime()
			sinceLastRefresh := time.Since(lastRefreshTime)
			periodicDue := sinceLastRefresh >= periodicInterval

			// A zero-value lastRefreshTime (no on-disk record yet, e.g. a
			// fresh node) makes sinceLastRefresh ~2026 years, which
			// time.Duration silently saturates to its ~292-year int64 max
			// (2562047h47m16s) rather than overflowing. Report "never"
			// instead of that nonsensical ceiling value.
			sinceLastRefreshDesc := formatDuration(sinceLastRefresh)
			if lastRefreshTime.IsZero() {
				sinceLastRefreshDesc = "never"
			}

			exp := parseJWTExpiryTime(byJwt)
			expiryDue := exp != nil && time.Until(*exp) <= expiryFallbackWindow

			// Emit warning when expiry is within 48h
			if expiryDue && exp != nil {
				remaining := time.Until(*exp)
				tlog("🔑 [jwt] ⚠ expires in %s — refresh triggered\n", formatDuration(remaining))
			}

			if periodicDue || expiryDue {
				var reason string
				switch {
				case periodicDue && expiryDue:
					reason = fmt.Sprintf("7-day periodic refresh due (last refresh %s ago) and within %s of expiry",
						sinceLastRefreshDesc, formatDuration(expiryFallbackWindow))
				case periodicDue:
					reason = fmt.Sprintf("7-day periodic refresh due (last refresh %s ago)", sinceLastRefreshDesc)
				default:
					reason = fmt.Sprintf("expiry fallback triggered (expires in %s, within %s threshold)",
						formatDuration(time.Until(*exp)), formatDuration(expiryFallbackWindow))
				}
				tlog("🔑 [jwt] refreshing token — %s\n", reason)

				newJwt, err := refreshJWT(ctx, apiUrl, byJwt)
				if err != nil {
					tlog("🔑 [jwt] refresh FAILED: %v — keeping existing JWT (will retry in 1h)\n", err)
				} else if err := atomicWriteFile(jwtPath, []byte(newJwt), 0700); err != nil {
					tlog("🔑 [jwt] refresh FAILED on disk write: %v — keeping existing JWT in memory (will retry in 1h)\n", err)
				} else {
					now := time.Now()
					if err := writeLastRefreshTime(now); err != nil {
						tlog("🔑 [jwt] refresh OK but failed to persist last-refresh timestamp: %v\n", err)
					}
					tlog("🔑 [jwt] refresh OK — network JWT written to %s (%d bytes, next refresh in %s)\n",
						jwtPath, len(newJwt), formatDuration(periodicInterval))
				}
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// classifyAuthFailureCause derives a human-readable root cause for a proxy's
// final auth failure, so operators get an actionable message instead of a
// generic "JWT may be expired" that is wrong for the common cases below.
//
// "proxy unreachable" gets its own bucket, separate from real network errors
// reaching the API: it's synthesized by the local SOCKS5 reachability probe
// (probeProxySocks5) before any auth attempt is made, so it says nothing
// about api.bringyour.com's health — it just means this proxy's port is
// dead or not actually speaking SOCKS5. Bundling it with genuine API-side
// timeouts made dead entries in a public proxy list look identical to real
// API outages in the logs.
func classifyAuthFailureCause(err error) string {
	errMsg := err.Error()
	switch {
	case strings.Contains(errMsg, "proxy unreachable"):
		return "proxy itself is unreachable (dead/offline SOCKS endpoint — not an API issue)"
	case errors.Is(err, context.DeadlineExceeded),
		errors.Is(err, context.Canceled),
		strings.Contains(errMsg, "Timeout"),
		strings.Contains(errMsg, "timeout"),
		strings.Contains(errMsg, "deadline exceeded"),
		strings.Contains(errMsg, "connection refused"),
		strings.Contains(errMsg, "no such host"):
		return "network error reaching API (check connectivity to api.bringyour.com)"
	default:
		return "API rejected token (check JWT validity)"
	}
}

const (
	degradedReaperTicker      = 3 * time.Minute
	degradedReaperMinDownTime = 30 * time.Minute
	degradedReaperKeepPct     = 50
)

// degradedReaperKeepCount returns how many proxies to keep (ceil rounding).
// With keepPct=50: 0→1, 1→1, 2→1, 3→2, 4→2, 5→3, 10→5.
func degradedReaperKeepCount(total int) int {
	if total <= 0 {
		return 1
	}
	keep := (total*degradedReaperKeepPct + 99) / 100
	if keep < 1 {
		return 1
	}
	return keep
}

// scoredDegradedProxy pairs a degraded proxy with its lifetime-contribution
// score (traffic bytes plus a per-contract bonus). Exported field names kept
// lowercase-package-local since tests live in the same package.
type scoredDegradedProxy struct {
	entry connect.DegradedProxyEntry
	score uint64
}

// contractsAcquiredFunc returns the lifetime contracts-acquired count for a
// proxy index, or 0 if unknown. Lets tests inject a fake without touching the
// real globalContractMetrics registry.
type contractsAcquiredFunc func(index int) int64

// liveContractsAcquired adapts the real globalContractMetrics registry to
// contractsAcquiredFunc. This is what runDegradedProxyReaper uses in
// production; tests pass their own stub instead.
func liveContractsAcquired(index int) int64 {
	m := globalContractMetrics.get(index)
	if m == nil {
		return 0
	}
	acquired, _ := m.snapshot()
	return acquired
}

// scoreDegradedProxies ranks degraded proxies by lifetime contribution
// (traffic + contracts acquired), ascending — the worst contributors sort
// first. This is the single source of truth for the ranking: both the live
// reaper and its tests call this function, so a regression here (e.g. a
// reversed comparator) is caught by any test that exercises it, instead of
// silently passing against a second, hand-maintained copy.
func scoreDegradedProxies(entries []connect.DegradedProxyEntry, getContracts contractsAcquiredFunc) []scoredDegradedProxy {
	scored := make([]scoredDegradedProxy, len(entries))
	for i, d := range entries {
		score := d.TotalRxBytes + d.TotalTxBytes
		if getContracts != nil {
			if acquired := getContracts(d.Index); acquired > 0 {
				score += uint64(acquired) * 1024
			}
		}
		scored[i] = scoredDegradedProxy{entry: d, score: score}
	}
	sort.SliceStable(scored, func(i, j int) bool {
		return scored[i].score < scored[j].score
	})
	return scored
}

// selectProxiesToReap picks which degraded proxies to cancel: everyone
// outside the top `keep` contributors (by score, so the worst-contributing
// proxies are chosen first), further filtered to those that have been
// degraded at least minDownTime. scored must already be sorted ascending by
// score (as scoreDegradedProxies returns it).
func selectProxiesToReap(scored []scoredDegradedProxy, keep int, minDownTime time.Duration) []connect.DegradedProxyEntry {
	var toReap []connect.DegradedProxyEntry
	for i := 0; i < len(scored)-keep; i++ {
		p := scored[i].entry
		if p.DownFor < minDownTime {
			continue
		}
		toReap = append(toReap, p)
	}
	return toReap
}

// onlyCancellableProxies filters degraded proxies down to those the reaper
// can actually act on — i.e. present in proxyCancelMap. Native/"direct" mode
// is registered in the same health tracking as any proxy (so it can appear
// in connect.DegradedProxies()) but is deliberately never added to
// proxyCancelMap (it must be immune to hot-reload deletions) and must never
// be reaped. Filtering here, before scoring, keeps it from ever consuming a
// retention or reap slot — it previously could silently no-op after being
// selected, which both wastes a reap slot and skews the keep/reap split.
func onlyCancellableProxies(degraded []connect.DegradedProxyEntry, proxyCancelMap map[string]context.CancelFunc, proxyCancelMu *sync.Mutex) []connect.DegradedProxyEntry {
	proxyCancelMu.Lock()
	defer proxyCancelMu.Unlock()
	var out []connect.DegradedProxyEntry
	for _, d := range degraded {
		if _, ok := proxyCancelMap[d.Address]; ok {
			out = append(out, d)
		}
	}
	return out
}

// stillDegradedFunc reports whether a proxy address is degraded right now.
// Lets tests inject a fake without touching the real connect-package health
// registry.
type stillDegradedFunc func(address string) bool

// liveIsDegraded adapts connect.IsDegraded to stillDegradedFunc — what
// reapProxies uses in production.
func liveIsDegraded(address string) bool {
	return connect.IsDegraded(address)
}

// reapProxies cancels each candidate in toReap, but only after re-verifying
// — right before pulling the trigger — that it is still actually degraded.
// Scoring and sorting thousands of proxies takes real wall-clock time; in
// that window a candidate may have reconnected on its own, or hot-reload may
// have torn it down and respawned a fresh instance at the same address.
// Either way the toReap decision was made about an instance that no longer
// exists in that state, so cancelling now would kill something other than
// what was actually decided about. isStillDegraded is checked under the same
// lock as the cancel/delete so nothing can change between the check and the
// act.
func reapProxies(toReap []connect.DegradedProxyEntry, proxyCancelMap map[string]context.CancelFunc, proxyCancelMu *sync.Mutex, isStillDegraded stillDegradedFunc) int64 {
	var reaped int64
	for _, p := range toReap {
		proxyCancelMu.Lock()
		if !isStillDegraded(p.Address) {
			proxyCancelMu.Unlock()
			continue
		}
		cancel, ok := proxyCancelMap[p.Address]
		if ok {
			cancel()
			delete(proxyCancelMap, p.Address)
			reaped++
		}
		proxyCancelMu.Unlock()
	}
	return reaped
}

func runDegradedProxyReaper(ctx context.Context, proxyCancelMap map[string]context.CancelFunc, proxyCancelMu *sync.Mutex) {
	ticker := time.NewTicker(degradedReaperTicker)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		degraded := connect.DegradedProxies()
		if len(degraded) <= 1 {
			continue
		}

		cancellable := onlyCancellableProxies(degraded, proxyCancelMap, proxyCancelMu)
		if len(cancellable) <= 1 {
			continue
		}

		scored := scoreDegradedProxies(cancellable, liveContractsAcquired)
		keep := degradedReaperKeepCount(len(scored))
		toReap := selectProxiesToReap(scored, keep, degradedReaperMinDownTime)

		reaped := reapProxies(toReap, proxyCancelMap, proxyCancelMu, liveIsDegraded)

		if reaped > 0 {
			tlog("[proxy-quality] dropped %d under-performing proxies (kept best %d of %d)\n",
				reaped, keep, len(scored))
		}
	}
}

func provide(opts docopt.Opts) {
	connect.CritLogger = critLog

	port, _ := opts.Int("--port")

	apiUrl, err := resolveApiUrl(opts)
	if err != nil {
		fmt.Printf("network config error: %s\n", err)
		os.Exit(1)
	}

	connectUrl, err := resolveConnectUrl(opts)
	if err != nil {
		fmt.Printf("network config error: %s\n", err)
		os.Exit(1)
	}

	maxMemoryHumanReadable, err := opts.String("--max-memory")
	var maxMemory connect.ByteCount
	if err == nil {
		maxMemory, err = connect.ParseByteCount(maxMemoryHumanReadable)
		if err != nil {
			panic(fmt.Errorf("Bad mem argument: %s", maxMemoryHumanReadable))
		}
	}
	if 0 < maxMemory {
		connect.ResizeMessagePoolsPerClass(maxMemory / 8)
		debug.SetMemoryLimit(maxMemory)
	}
	applyPoolAutoSize(maxMemory)

	provideStartTime = time.Now()

	// Apply a staged session (from `urnet-tools session load`) before
	// loading any identity or starting transports. The staging dir and
	// marker file are written by the shell wrapper; the provider's job
	// is to atomically swap them in on the next startup.
	applyStagedSession()

	tlog("❤️ [startup] provider version=%s\n", RequireVersion())
	host, _ := os.Hostname()
	critLog("STARTUP: version=%s pid=%d host=%s", RequireVersion(), os.Getpid(), host)

	// Log JWT expiry status at startup
	home, _ := os.UserHomeDir()
	if home != "" {
		jwtPath := filepath.Join(home, ".urnetwork", "jwt")
		if _, err := os.Stat(jwtPath); err == nil {
			if jwtBytes, err := os.ReadFile(jwtPath); err == nil {
				if exp := parseJWTExpiryTime(string(jwtBytes)); exp != nil {
					remaining := time.Until(*exp)
					daysUntil := remaining.Hours() / 24
					if daysUntil >= 1 {
						tlog("🔑 [jwt] expires in %d days\n", int(daysUntil))
					} else if daysUntil >= 0 {
						tlog("🔑 [jwt] expires in %s\n", formatDuration(remaining))
					} else {
						tlog("🔑 [jwt] EXPIRED %s ago — refresh needed\n", formatDuration(-remaining))
					}
				}
			}
		}
	}

	event := connect.NewEventWithContext(context.Background())
	event.SetOnSignals(syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM)

	rawCtx, rawCancel := context.WithCancel(event.Ctx())
	ctx := rawCtx
	var cancelSource atomic.Value // stores string(debug.Stack()) of the first caller to cancel()
	cancel := func() {
		if cancelSource.Load() == nil {
			cancelSource.Store(string(debug.Stack()))
		}
		rawCancel()
	}
	defer cancel()
	// Drain buffered retention events before exit so a shutdown racing the
	// writer goroutine doesn't drop the tail of the log (proxy_health_log.go).
	defer flushRetentionEvents()

	// Exit-visibility: log what triggered the shutdown. The wrapped cancel
	// function captures a stack trace at the moment it is first invoked. If
	// cancel() was never called (e.g. the parent event.Ctx() was cancelled
	// by a signal or panic recovery in SetOnSignals), the shutdown goroutine
	// reports that instead.
	go func() {
		<-ctx.Done()
		source, _ := cancelSource.Load().(string)
		if source == "" {
			source = "context cancelled by parent (signal or event.Set())"
		}
		tlog("[provider] shutting down: main context cancelled\n")
		critLog("SIGNAL: context cancelled — draining goroutines\nsource:\n%s", source)
	}()

	// subnet claim wallet (sn/PLAN.md 7.3, decision D-2): validate the
	// ss58 coldkey locally and idempotently register it with the platform
	// before providing starts. A failure warns and does not block
	// providing — the wallet may already be set from a previous run, and
	// the call can be retried any time with `provider wallet set`.
	if coldkeySs58, walletErr := opts.String("--wallet"); walletErr == nil && coldkeySs58 != "" {
		walletClientStrategy := connect.NewClientStrategyWithDefaults(ctx)
		if err := snSetWallet(ctx, walletClientStrategy, apiUrl, coldkeySs58); err != nil {
			fmt.Printf("subnet wallet not set: %s\n", err)
			fmt.Printf("continuing to provide. Retry with: provider wallet set <coldkey_ss58>\n")
		}
	}

	// Hourly pulse: wakes all stalled transports and proxies so they retry
	// connections without needing a provider restart.
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(1 * time.Hour):
				if connect.ProxyHealthCount() > 0 {
					_, dead, degraded, _, connecting := connect.ProxyHealthSnapshot()
					down := len(dead) + len(degraded)
					tlog("[hourly-maintenance] reconnecting stalled transports: down=%d dead=%d degraded=%d connecting=%d\n",
						down, len(dead), len(degraded), len(connecting))
				}
				connect.TriggerPulse()
			}
		}
	}()

	nodeName := strings.TrimSpace(os.Getenv("URNETWORK_NODE_NAME"))

	// Determine a temporary display name for the outage watcher/heartbeat
	watcherName := nodeName
	if watcherName == "" {
		watcherName, _ = os.Hostname()
		if containerIDRe.MatchString(watcherName) {
			watcherName = "provider"
		}
	}

	bootstrapHubCA(ctx, os.Getenv("URNETWORK_REPORT_URL"), os.Getenv("URNETWORK_HUB_TOKEN"))

	go connect.HandleError(func() { runHealthHeartbeat(ctx, provideStartTime, os.Getenv("URNETWORK_PROFILE")) })
	go connect.HandleError(func() {
		runBandwidthReporter(ctx, watcherName, watcherName, os.Getenv("URNETWORK_REPORT_URL"), provideStartTime)
	})
	go connect.HandleError(func() {
		runHeartbeatReporter(ctx, watcherName, watcherName, os.Getenv("URNETWORK_REPORT_URL"), provideStartTime)
	})
	go connect.HandleError(func() { runJWTRefresher(ctx, apiUrl) })
	go connect.HandleError(func() { runEarningWindows(ctx) })
	go connect.HandleError(func() { runLifetimeCollector(ctx) })
	go connect.HandleError(func() { runProfitHeartbeat(ctx) })
	go connect.HandleError(func() { runBillableRateWriter(ctx) })

	proxyURLs := resolveProxyURLs(opts)
	proxyURLRefresh := resolveDuration(opts, "--proxy_url_refresh", "PROXY_URL_REFRESH", 1*time.Hour)
	proxyURLMax := resolveInt(opts, "--proxy_url_max", "PROXY_URL_MAX", 500)
	cleanupScope := resolveString(opts, "--proxy_dead_cleanup_scope", "PROXY_DEAD_CLEANUP_SCOPE", "url")
	cleanupInterval := resolveDuration(opts, "--proxy_dead_cleanup_interval", "PROXY_DEAD_CLEANUP_INTERVAL", 6*time.Hour)
	// Self-heal defaults OFF: the pressure-based load shedding system is opt-in
	// via URNETWORK_SELF_HEAL=1 or `urnet-tools self-heal on`.
	selfHealEnabled := os.Getenv("URNETWORK_SELF_HEAL") == "1"

	// Extract API host:port for the reachability probe (from the chosen
	// network's API URL, already resolved above via resolveApiUrl).
	apiProbeHost, apiProbePort := apiProbeHostPort(apiUrl)
	// Wrapped in connect.HandleError so a panic in paceMonitor degrades to
	// "that one loop dies" instead of taking the whole provider process down,
	// consistent with every other fleet background loop (#12 fault isolation).
	go connect.HandleError(func() { paceMonitor(ctx) })

	// Declared here (rather than next to the startup loop below) so
	// provideWithProxy can close over them directly: on a permanent give-up,
	// it needs to remove its own entry to make itself eligible for re-add by
	// the next reload() reconciliation pass. Both the initial startup loop
	// and ProxyReloader share this same map/mutex pair.
	var proxyCancelMu sync.Mutex
	proxyCancelMap := map[string]context.CancelFunc{}

	provideWithProxy := func(proxyCtx context.Context, proxySettings *connect.ProxySettings, isNative bool, isURLSourced bool) {
		clientStrategySettings := connect.DefaultClientStrategySettings()
		clientStrategySettings.ProxySettings = proxySettings
		clientSettings := connect.DefaultClientSettings()
		// Load previously-persisted long-lived identity material — the
		// Ed25519 client-key seed and the sequence-level TLS server
		// cert + private key. Missing or invalid files are silently
		// ignored; the client will then generate fresh values and we
		// save them back after construction.
		if seed, err := readProviderClientKeySeed(); err == nil && 0 < len(seed) {
			clientSettings.ClientKeySeed = seed
		}
		if certPem, keyPem, err := readProviderTlsCertAndKey(); err == nil && 0 < len(certPem) && 0 < len(keyPem) {
			if clientSettings.EncryptionSettings == nil {
				clientSettings.EncryptionSettings = connect.DefaultEncryptionSettings()
			}
			clientSettings.EncryptionSettings.ProvideTlsCertificatePem = certPem
			clientSettings.EncryptionSettings.ProvideTlsPrivateKeyPem = keyPem
		}
		enableProviderEncryption(clientSettings)
		localUserNatSettings := connect.DefaultLocalUserNatSettings()

		connect.ApplyAutoTuning(clientSettings, localUserNatSettings)
		applyLowmodeSettings(clientSettings, localUserNatSettings)
		applyTurboSettings(clientSettings, localUserNatSettings)

		profile := os.Getenv("URNETWORK_PROFILE")

		if (profile == "turbo-v4" || profile == "turbo-v8") &&
			os.Getenv("GOMEMLIMIT") == "" && maxMemory == 0 {
			ramBytes := detectEffectiveRAMLimitBytes()
			debug.SetMemoryLimit(ramBytes * 80 / 100)
		}
		applyEcoSettings(maxMemory)
		ensureMemoryLimit(maxMemory)
		localUserNatSettings.TcpBufferSettings.ConnectSettings = clientStrategySettings.ConnectSettings
		localUserNatSettings.UdpBufferSettings.ConnectSettings = clientStrategySettings.ConnectSettings
		remoteUserNatProviderSettings := connect.DefaultRemoteUserNatProviderSettings()

		clientStrategy := connect.NewClientStrategy(proxyCtx, clientStrategySettings)

		// Plumb the out-of-band peer-client-key fetcher so each
		// per-peer encryption session can cross-check the
		// contract-supplied public client key against the
		// canonical value served by the platform's unauthenticated
		// `/key/<peerId>` API. Today the session only logs on
		// mismatch; the contract value is still trusted, but
		// operators get an early-warning signal for a substitution
		// attack. Skipped if `EncryptionSettings` is nil
		// (encryption disabled).
		if clientSettings.EncryptionSettings != nil && clientSettings.EncryptionSettings.NewPeerClientPublicKeyFetcher == nil {
			clientSettings.EncryptionSettings.NewPeerClientPublicKeyFetcher = func(peerId connect.Id) func(context.Context) ([]byte, error) {
				return func(fetchCtx context.Context) ([]byte, error) {
					r, err := connect.HttpGetWithStrategy(
						fetchCtx,
						clientStrategy,
						fmt.Sprintf("%s/key/%s", apiUrl, peerId),
						"",
						&connect.GetClientKeyResult{},
						connect.NewNoopApiCallback[*connect.GetClientKeyResult](),
					)
					if err != nil {
						return nil, err
					}
					return r.PublicKey, nil
				}
			}
		}

		byClientJwt, clientId, reused, err := func() (string, connect.Id, bool, error) {
			// Consecutive auth failures (network errors, API timeouts, or token
			// rejection). After maxAuthFailures the proxy gives up and goes offline
			// until the next hourly pulse.
			// "Jwt does not exist" is a configuration issue, not a network/token
			// error — it retries indefinitely until the user runs 'urnetwork auth'.
			//
			// A proxy that has never once succeeded gets a much shorter leash
			// than one with a proven track record: on a free list that's mostly
			// open-port-but-broken entries, ten retries against something that's
			// never worked even once is ten auth-rate-limiter slots spent
			// re-confirming what's already very likely true, instead of being
			// spent discovering whether a fresh, untried candidate works.
			const provenMaxAuthFailures = 10
			const unprovenMaxAuthFailures = 3
			maxAuthFailures := provenMaxAuthFailures
			if proxySettings != nil && !globalProvenProxies.HasSucceeded(proxySettings.Address) {
				maxAuthFailures = unprovenMaxAuthFailures
			}
			authFailures := 0

			// Restart storm guard: if persisted slow-retry state shows
			// this proxy was recently attempted (or is dropped), skip the
			// fast-retry phase entirely. Without this, 356 dead proxies
			// would each run 10 fast auth attempts on restart — 3,560
			// unthrottled calls before any slow-retry code runs.
			if proxySettings != nil && !isURLSourced {
				if globalProxySlowRetryState.WasDropped(proxySettings.Address) || globalProxySlowRetryState.TimeUntilNextAttempt(proxySettings.Address) > 0 {
					authFailures = maxAuthFailures
				}
			}
			for {
				var err error
				var byClientJwt string
				var clientId connect.Id
				var reused bool

				// Only URL-sourced proxies get the pre-auth SOCKS5 reachability probe.
				// File/internal lists are operator-curated (paid) endpoints that should
				// always attempt auth; the probe exists to cheaply skip dead entries in
				// large free URL lists before spending a shared auth-rate-limiter slot.
				// URL-sourced proxies additionally carry a recorded stage-1 table score;
				// a below-bar score blocks auth even when the proxy is momentarily
				// reachable, so a quality-rejected entry cannot spend auth slots.
				if proxySettings != nil && isURLSourced && !urlProxyPassesAdmission(proxyCtx, proxySettings.Address) {
					// Either the proxy isn't speaking SOCKS5 right now (dead port,
					// broken service, captive portal — a dead local hop, not a signal
					// about the API's health), or its recorded stage-1 score is below
					// the quality bar. Both mean: skip the auth attempt (and the
					// shared rate limiter) entirely rather than spending a slot and
					// reporting a timeout that would falsely look like the API is
					// overloaded and throttle every other proxy's auth rate for no
					// reason.
					cfg := resolveProxyTableProbeConfig()
					if score, ok := cachedProxyURLScore(proxySettings.Address); ok && cfg.Enabled && score < cfg.PassBar {
						// QUALITY rejection: the recorded score is below
						// the bar AND the kill switch is ON. This is a filter, not a
						// failure of auth or reachability — it must not count in
						// authFailures, RecordFailure, or RecordGiveUp, and must never
						// trigger evictProxyURLAddress (a below-bar proxy is alive,
						// just not good enough; blacklisting it for 24h is wrong).
						// Exit the retry loop with a distinguishable error so the
						// give-up path below skips the accounting; the next fetch
						// cycle re-grades the entry (and the merge drops it from the
						// desired set) and a later score above the bar re-admits it.
						//
						// The cfg.Enabled guard is load-bearing: with the kill switch
						// OFF the gate never rejects on score (urlProxyPassesAdmission
						// skips it), so a gate failure here means the proxy is DEAD —
						// labeling it a quality rejection would suppress the give-up/
						// eviction/backoff machinery and the address would churn on
						// every reload forever.
						return "", connect.Id{}, false, fmt.Errorf("%w: %s (score %.2f)", errProxyURLBelowBar, proxySettings.Address, score)
					}
					err = fmt.Errorf("proxy unreachable: %s", proxySettings.Address)
				} else {
					// Weight this wait by the proxy's lifetime failure count
					// (persists across the 15-minute URL-source requeue, unlike
					// the local authFailures counter) so a chronically dead
					// address doesn't keep re-entering the lottery at full
					// "untried" priority every time it comes back.
					admitFailureCount := authFailures
					if proxySettings != nil {
						admitFailureCount = globalProxyFailureHistory.FailureCount(proxySettings.Address)
					}
					// Acquire slow-retry semaphore before admission gate so
					// at most slowRetryMaxConcurrent slow-retry proxies can
					// be in the auth pipeline at once. Select on proxyCtx
					// so a cancelled proxy doesn't hang on the semaphore.
					if !isURLSourced && authFailures >= maxAuthFailures {
						select {
						case slowRetrySemaphore <- struct{}{}:
						case <-proxyCtx.Done():
							return "", connect.Id{}, false, proxyCtx.Err()
						}
					}
					release, waitErr := globalProxyAdmissionGate.Admit(proxyCtx, admitFailureCount)
					if waitErr != nil {
						if !isURLSourced && authFailures >= maxAuthFailures {
							<-slowRetrySemaphore
						}
						return "", connect.Id{}, false, waitErr
					}
					identityKey := "direct"
					if proxySettings != nil {
						identityKey = proxySettings.Address
					}
					byClientJwt, clientId, reused, err = provideAuth(proxyCtx, clientStrategy, apiUrl, opts, nodeName, identityKey)
					release()
					// Limit concurrent slow-retry auth attempts to avoid
					// thundering-herd: a box with hundreds of dead proxies
					// would otherwise overwhelm the auth API on each daily
					// cycle, potentially causing genuine proxies to fail too.
					// Release immediately after auth — the slot must not be
					// held across the 24h sleep in the slow-retry block below.
					if !isURLSourced && authFailures >= maxAuthFailures {
						<-slowRetrySemaphore
					}
					if proxySettings != nil {
						if err == nil {
							globalProvenProxies.MarkSucceeded(proxySettings.Address)
							globalProxyFailureHistory.Reset(proxySettings.Address)
							// Clear any slow-retry state for a proxy that
							// just recovered — prevents stale "previously
							// dropped" log messages on next restart.
							globalProxySlowRetryState.ClearDropped(proxySettings.Address)
						}
						globalAuthRateLimiter.ReportResultForProxy(err, globalProvenProxies.HasSucceeded(proxySettings.Address))
					} else {
						globalAuthRateLimiter.ReportResult(err)
					}
					if err == nil {
						if reused {
							fmt.Printf("♻️ client_id: %s (reused)\n", clientId)
						} else {
							fmt.Printf("✨ client_id: %s (new)\n", clientId)
						}
						return byClientJwt, clientId, reused, nil
					}
				}

				if errors.Is(err, ErrTokenInvalid) {
					shmLogFatal(78, "token invalid or expired — exiting so the startup script can refresh it")
				}

				if strings.Contains(err.Error(), "Jwt does not exist") {
					authFailures = 0
					fmt.Printf("Authentication missing. Please run 'urnetwork auth' to configure your provider.\n")
					retryDelay := 30 * time.Second
					select {
					case <-proxyCtx.Done():
						return "", connect.Id{}, false, proxyCtx.Err()
					case <-time.After(retryDelay):
						continue
					}
				}

				authFailures++
				if proxySettings != nil {
					globalProxyFailureHistory.RecordFailure(proxySettings.Address)
				}
				if authFailures >= maxAuthFailures {
					cause := classifyAuthFailureCause(err)
					// URL-sourced (free lists) keep the short leash: give up and let
					// the requeue path bring them back later, so a huge mostly-dead
					// list does not pin a goroutine per entry.
					// Operator-curated proxies (file/internal) get a slow retry
					// with a 14-day ceiling: daily attempts give brief outages a
					// chance to recover, but truly dead proxies are dropped from
					// the active pool after two weeks. The proxy remains in the
					// config file and will be retried fresh on the next provider
					// restart. The native direct connection (proxySettings == nil)
					// is excluded — it is a single endpoint, not a paid proxy list.
					if isURLSourced {
						return "", connect.Id{}, false, fmt.Errorf("authentication failed after %d attempts — %s: %w", maxAuthFailures, cause, err)
					}
					// Persist slow-retry start time (survives reboots) and
					// check if this proxy has exceeded the 14-day drop window.
					if proxySettings != nil {
						startedAt := globalProxySlowRetryState.RecordSlowRetryStart(proxySettings.Address)
						if globalProxySlowRetryState.ShouldDrop(proxySettings.Address) {
							globalProxySlowRetryState.MarkDropped(proxySettings.Address)
							dropAge := time.Since(startedAt)
							tlog("[proxy][slow-retry] proxy[%d] (%s) dropped after %s of continuous failure (%d total attempts); removed from active pool\n",
								proxySettings.Index, proxySettings.Address, formatDuration(dropAge), authFailures)
							// Clean up proxyCancelMap so the reloader can
							// relaunch this proxy if the operator refreshes
							// the proxy list.
							proxyCancelMu.Lock()
							delete(proxyCancelMap, proxySettings.Address)
							proxyCancelMu.Unlock()
							return "", connect.Id{}, false, fmt.Errorf("proxy dropped after %s of continuous failure — %s", formatDuration(dropAge), cause)
						}
						// The 24h daily gate only applies after the first 3
						// slow retries (which use the 5m/10m/15m ramp via
						// proxyAuthSlowRetryDelay). Before that, fall through
						// to the ramp delay directly.
						slowRetryAttempt := authFailures - maxAuthFailures + 1
						if slowRetryAttempt > slowRetryRampAttempts && !globalProxySlowRetryState.RecordSlowRetryAttempt(proxySettings.Address) {
							// Not time yet — sleep precisely until the
							// daily interval elapses from the last attempt.
							waitTime := globalProxySlowRetryState.TimeUntilNextAttempt(proxySettings.Address)
							tlog("[proxy][slow-retry] proxy[%d] (%s) auth still failing after %d attempts (%s); already attempted recently, next check in %s\n",
								proxySettings.Index, proxySettings.Address, authFailures, cause, formatDuration(waitTime))
							dailyTimer := time.NewTimer(waitTime)
							select {
							case <-proxyCtx.Done():
								dailyTimer.Stop()
								return "", connect.Id{}, false, proxyCtx.Err()
							case <-dailyTimer.C:
								continue
							}
						}
					}
					slowDelay := proxyAuthSlowRetryDelay(authFailures - maxAuthFailures + 1)
					if proxySettings != nil {
						tlog("[proxy][init] proxy[%d] (%s) auth still failing after %d attempts (%s); retrying in %s\n",
							proxySettings.Index, proxySettings.Address, authFailures, cause, formatDuration(slowDelay))
					} else if isNative {
						tlog("[proxy][init] proxy[0] (direct) auth still failing after %d attempts (%s); retrying in %s\n",
							authFailures, cause, formatDuration(slowDelay))
					} else {
						tlog("[init] auth still failing after %d attempts (%s); retrying in %s\n",
							authFailures, cause, formatDuration(slowDelay))
					}
					select {
					case <-proxyCtx.Done():
						return "", connect.Id{}, false, proxyCtx.Err()
					case <-time.After(slowDelay):
						continue
					}
				}

				retryDelay := proxyAuthRetryDelay(err, authFailures)
				if proxySettings != nil {
					tlog("[proxy][init] proxy[%d] (%s) auth failed (attempt %d/%d): %v. Will retry in %.2fs\n",
						proxySettings.Index, proxySettings.Address, authFailures, maxAuthFailures, err, float64(retryDelay/time.Millisecond)/1000.0)
				} else if isNative {
					tlog("[proxy][init] proxy[0] (direct) auth failed (attempt %d/%d): %v. Will retry in %.2fs\n",
						authFailures, maxAuthFailures, err, float64(retryDelay/time.Millisecond)/1000.0)
				} else {
					tlog("[init] auth failed (attempt %d/%d): %v. Will retry in %.2fs\n", authFailures, maxAuthFailures, err, float64(retryDelay/time.Millisecond)/1000.0)
				}
				select {
				case <-proxyCtx.Done():
					return "", connect.Id{}, false, proxyCtx.Err()
				case <-time.After(retryDelay):
				}
			}
		}()
		if err != nil {
			if proxySettings != nil {
				if isURLSourced {
					proxyCancelMu.Lock()
					delete(proxyCancelMap, proxySettings.Address)
					proxyCancelMu.Unlock()

					if errors.Is(err, errProxyURLBelowBar) {
						// Quality rejection: the proxy was filtered
						// by its recorded stage-1 score, not by auth or
						// reachability failure. No give-up accounting, no
						// eviction, no backoff — the next fetch cycle re-grades
						// the entry and the merge re-admits it if it clears the
						// bar.
						tlog("[proxy][init] proxy[%d] (%s) rejected by stage-1 quality gate: %v. Not counted as a failure; re-graded next fetch cycle.\n",
							proxySettings.Index, proxySettings.Address, err)
					} else if errors.Is(err, context.Canceled) || proxyCtx.Err() != nil {
						// Context cancellation (trim, reaper, reload, drain) is NOT an
						// auth failure. Do not record give-up — it would permanently
						// evict a healthy proxy after enough operational cycles.
						tlog("[proxy][init] proxy[%d] (%s) cancelled (not a give-up): %v\n",
							proxySettings.Index, proxySettings.Address, err)
					} else {
						giveUpCount := globalProxyFailureHistory.RecordGiveUp(proxySettings.Address)
						if giveUpCount >= proxyURLGiveUpEvictAfterCycles {
							if evictErr := evictProxyURLAddress(proxySettings.Address); evictErr != nil {
								fmt.Fprintf(os.Stderr, "[proxy][init] proxy[%d] (%s) could not evict after %d give-ups: %v\n",
									proxySettings.Index, proxySettings.Address, giveUpCount, evictErr)
								delay := proxyURLGiveUpRetryDelay(giveUpCount)
								globalProxyFailureHistory.SetBackoffUntil(proxySettings.Address, time.Now().Add(delay))
							} else {
								fmt.Fprintf(os.Stderr, "[proxy][init] proxy[%d] (%s) authentication failed after retries: %v. Permanently removed after %d give-ups, will not be retried.\n",
									proxySettings.Index, proxySettings.Address, err, giveUpCount)
							}
						} else {
							delay := proxyURLGiveUpRetryDelay(giveUpCount)
							// Enforce the backoff at launch time, not just by
							// scheduling a one-shot reload: record the earliest
							// time this address may be relaunched so the reload
							// path skips it until the window elapses. Otherwise any
							// other reload (another proxy's give-up, a URL refresh)
							// would relaunch it immediately and defeat the backoff.
							globalProxyFailureHistory.SetBackoffUntil(proxySettings.Address, time.Now().Add(delay))
							if reloadPath, pathErr := proxyReloadPath(); pathErr == nil {
								time.AfterFunc(delay, func() {
									if err := writeReloadTrigger(reloadPath); err != nil {
										tlog("[proxy] warn: failed to signal proxy reload after warmup (write .reload): %v\n", err)
									}
								})
							}
							fmt.Fprintf(os.Stderr, "[proxy][init] proxy[%d] (%s) authentication failed after retries: %v. URL-sourced, give-up %d of %d before eviction, will retry automatically in %s.\n",
								proxySettings.Index, proxySettings.Address, err, giveUpCount, proxyURLGiveUpEvictAfterCycles, formatDuration(delay))
						}
					}
				} else {
					fmt.Fprintf(os.Stderr, "[proxy][init] proxy[%d] (%s) authentication failed after retries: %v (proxy will remain offline; run 'urnet-tools proxy refresh' after fixing the underlying issue)\n",
						proxySettings.Index, proxySettings.Address, err)
				}
			} else if isNative {
				fmt.Fprintf(os.Stderr, "[proxy][init] proxy[0] (direct) authentication failed after retries: %v (proxy will remain offline, retry on next hourly pulse)\n", err)
			} else {
				fmt.Fprintf(os.Stderr, "[init] authentication failed after retries: %v\n", err)
			}
			return
		}

		identityKey := "direct"
		proxyIndex := 0
		if proxySettings != nil {
			identityKey = proxySettings.Address
			proxyIndex = proxySettings.Index
		}

		instanceId := connect.NewId()

		clientOob := connect.NewApiOutOfBandControl(proxyCtx, clientStrategy, byClientJwt, apiUrl)
		connectClient := connect.NewClient(proxyCtx, clientId, clientOob, clientSettings)
		defer func() {
			unregisterEncryptionManager(connectClient.EncryptionSessionManager())
			connectClient.Close()
		}()
		registerEncryptionManager(connectClient.EncryptionSessionManager())

		// Persist the live identity material so the next process
		// start loads the same values. On a fresh install both
		// reads above returned empty and the connect.Client just
		// generated; on subsequent starts we're writing back the
		// same bytes (cheap no-op-equivalent).
		if keyManager := connectClient.ClientKeyManager(); keyManager != nil {
			if seed := keyManager.Seed(); 0 < len(seed) {
				if err := writeProviderClientKeySeed(seed); err != nil {
					fmt.Printf("provider client key save failed: %s\n", err)
				}
			}
		}
		if encManager := connectClient.EncryptionSessionManager(); encManager != nil {
			certPem := encManager.ProvideTlsCertificatePem()
			keyPem := encManager.ProvideTlsPrivateKeyPem()
			if 0 < len(certPem) && 0 < len(keyPem) {
				if err := writeProviderTlsCertAndKey(certPem, keyPem); err != nil {
					fmt.Printf("provider tls cert/key save failed: %s\n", err)
				}
			}
		}

		// routeManager := connect.NewRouteManager(connectClient)
		// contractManager := connect.NewContractManagerWithDefaults(connectClient)
		// connectClient.Setup(routeManager, contractManager)
		// go connectClient.Run()

		fmt.Printf("instance_id: %s\n", instanceId)

		auth := &connect.ClientAuth{
			ByJwt: byClientJwt,
			// ClientId: clientId,
			InstanceId: instanceId,
			AppVersion: RequireVersion(),
		}
		platformTransport := connect.NewPlatformTransportWithDefaults(proxyCtx, clientStrategy, connectClient.RouteManager(), connectUrl, auth)
		// go platformTransport.Run(connectClient.RouteManager())

		// The renewal watcher closes revocationDone on a successful renewal:
		// the identity is then demonstrably alive, and the revocation watcher
		// must stop before it evicts the fresh entry while the transport is
		// still reconnecting.
		revocationDone := make(chan struct{})

		if reused {
			// The reuse path in provideAuth never contacts the server, so a
			// client_id that was revoked server-side for a reason other than
			// local JWT expiry would otherwise never be noticed — transport
			// auth would just keep retrying the same rejected identity
			// forever. Watch for that specific signature (transport never
			// authenticates even once, auth failures keep piling up) and
			// evict the persisted entry so the next mint attempt starts
			// fresh instead of repeating a dead identity indefinitely.
			go watchReusedIdentityForRevocation(proxyCtx, identityKey, proxyIndex, revocationDone)
		}

		// In-process client-JWT renewal: the beta backend (and mainnet's new
		// token format) now mints 24h client JWTs, and nothing upstream renews
		// them for long-lived providers. Without this, every proxy's client
		// JWT expires ~24h after start and the provider becomes a black hole
		// (audit/contract OOB 401s, no new contracts). The watcher renews each
		// proxy's JWT 12h before expiry (hourly retry), immediately on a 401,
		// and at startup if the reused token is already expired, preserving
		// the client_id identity. On backends still issuing long-lived tokens
		// the 12h threshold never fires — a no-op.
		renewNow := make(chan struct{}, 1)
		go runProxyJWTWatcher(proxyCtx, proxyJWTWatcherConfig{
			IdentityKey:    identityKey,
			ClientID:       clientId,
			CurrentJWT:     byClientJwt,
			Description:    providerDescription(nodeName),
			ApiURL:         apiUrl,
			ClientStrategy: clientStrategy,
			OOB:            clientOob,
			Transport:      platformTransport,
			RenewNow:       renewNow,
			ProxyIndex:     proxyIndex,
			InstanceId:     instanceId,
			RevocationDone: revocationDone,
		})

		var bw *connect.ProxyBandwidth
		if proxySettings != nil {
			bw = connect.RegisterProxyBandwidth(proxySettings.Index)
		} else if isNative {
			bw = connect.RegisterProxyBandwidth(0)
		}

		localUserNat := connect.NewLocalUserNat(proxyCtx, clientId.String(), bw, localUserNatSettings)
		defer localUserNat.Close()
		remoteUserNatProvider := connect.NewRemoteUserNatProvider(connectClient, localUserNat, bw, remoteUserNatProviderSettings)
		defer remoteUserNatProvider.Close()
		if proxySettings != nil {
			startProxyBenchmarks(proxyCtx, bw, proxySettings)
		}

		provideModes := map[protocol.ProvideMode]bool{
			protocol.ProvideMode_Public:  true,
			protocol.ProvideMode_Network: true,
		}
		connectClient.ContractManager().SetProvideModes(provideModes)

		if proxySettings != nil {
			retireMetrics := registerContractCallback(proxySettings.Index, connectClient)
			defer retireMetrics()
		}

		select {
		case <-proxyCtx.Done():
		}
	}

	var wg sync.WaitGroup

	// Sentinel goroutine to prevent wg.Wait() from unblocking
	// if the hot-reloader drops active proxies to zero.
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-ctx.Done()
	}()

	if profile := os.Getenv("URNETWORK_PROFILE"); profile == "turbo-v4" || profile == "turbo-v8" {
		var windowMiB, queueMiB uint32
		switch profile {
		case "turbo-v4":
			windowMiB, queueMiB = 4, 8
		case "turbo-v8":
			windowMiB, queueMiB = 8, 16
		}
		tlog("[turbo] profile=%s window=%dMiB resendQueue=%dMiB\n", profile, windowMiB, queueMiB)
	}

	// Load proxy.state to assign address-stable IDs. Known addresses keep their ID
	// across restarts/reloads; new addresses get the next monotonic counter value.
	proxyState, stateErr := readProxyState()
	if stateErr != nil {
		tlog("[proxy] warning: could not read proxy.state: %v\n", stateErr)
		proxyState = &ProxyState{Proxies: map[string]ProxyEntry{}}
	}
	proxyState.StartedAt = provideStartTime

	// Advance the ID counter above any IDs already in state so they are never reused.
	highestID := -1
	for _, e := range proxyState.Proxies {
		if e.ID > highestID {
			highestID = e.ID
		}
	}
	initProxyIDCounter(highestID)

	// Select the proxy source: external file (Workflow A) or internal config (Workflow B).
	proxyFile, _ := opts.String("--proxy_file")
	// Also accept PROXY_FILE env var (for Docker/Pelican where CLI flags
	// cannot be set by the user — the env var is the only knob).
	if proxyFile == "" {
		proxyFile = os.Getenv("PROXY_FILE")
	}
	var allProxySettings []*connect.ProxySettings
	if proxyFile != "" {
		settings, err := readProxySettingsFromFile(proxyFile)
		if err != nil {
			shmLogFatal(20, "[proxy] could not read proxy file: %v", err)
		}
		if len(settings) == 0 {
			shmLogFatal(21, "[proxy] proxy file %s contained no valid proxies (expected one ip:port:user:pass per line)", proxyFile)
		}
		allProxySettings = settings
		proxyState.Source = proxyFile
	} else {
		allProxySettings = readProxySettings()
		proxyState.Source = ""
	}

	// Merge in any already-cached URL-sourced proxies (Workflow A/B + URL
	// source are additive, not mutually exclusive). proxySourceOf records
	// each address's provenance for tagProxySourceIfUnset below.
	primarySource := "internal"
	if proxyFile != "" {
		primarySource = "file"
	}
	proxyDesiredSet := make(map[string]*connect.ProxySettings, len(allProxySettings))
	proxySourceOf := make(map[string]string, len(allProxySettings))
	for _, s := range allProxySettings {
		proxyDesiredSet[s.Address] = s
		proxySourceOf[s.Address] = primarySource
	}
	if urlState, err := readProxyURLState(); err != nil {
		tlog("[proxy][url] warning: could not read proxy_url.json: %v\n", err)
	} else {
		mergeProxyURLCache(proxyDesiredSet, proxySourceOf, urlState)
	}
	allProxySettings = allProxySettings[:0]
	for _, s := range proxyDesiredSet {
		allProxySettings = append(allProxySettings, s)
	}
	// Sort so file-sourced (or internal-config) proxies launch before
	// URL-sourced ones. backoffPacer uses the index in this slice to
	// determine initial delay, so file proxies get a head start of
	// ~len(file_proxies) * staggerMs before URL proxies begin connecting.
	sort.SliceStable(allProxySettings, func(i, j int) bool {
		si := proxySourceOf[allProxySettings[i].Address]
		sj := proxySourceOf[allProxySettings[j].Address]
		if si == "url" && sj != "url" {
			return false
		}
		if si != "url" && sj == "url" {
			return true
		}
		return false
	})

	// Start the native [direct] connection as proxy[0] unless the operator
	// has explicitly disabled it. Precedence (highest to lowest):
	//   1. Runtime toggle file ~/.urnetwork/direct (set via `provider direct off|on`)
	//   2. DISABLE_DIRECT_IP=1 env var (startup-only default)
	// The toggle file always wins — `provider direct on` re-enables direct
	// even if the env var was set, and vice versa.
	noDirect := !isDirectEnabled()
	if !noDirect {
		// ALWAYS start the native [direct] connection as proxy[0].
		// We run this exactly like a proxy so it registers in telemetry and earns bandwidth.
		wg.Add(1)
		nativeCtx, nativeCancel := context.WithCancel(ctx)
		// Register nativeCancel in the reloader's cancelMap under the special
		// key "direct" so reload() can hot-toggle it via `provider direct off|on`.
		proxyCancelMu.Lock()
		proxyCancelMap[directProxyKey] = nativeCancel
		proxyCancelMu.Unlock()
		go connect.HandleError(func() {
			defer wg.Done()
			defer nativeCancel()
			// Clean up cancelMap entry AFTER UnregisterProxy so stale-unregister
			// can't nuke a fresh direct's registration, and so the reloader's
			// running[] snapshot doesn't leave direct permanently "running" after
			// the goroutine exits.
			defer func() {
				proxyCancelMu.Lock()
				if cur, ok := proxyCancelMap[directProxyKey]; ok && reflect.ValueOf(cur).Pointer() == reflect.ValueOf(nativeCancel).Pointer() {
					delete(proxyCancelMap, directProxyKey)
				}
				proxyCancelMu.Unlock()
			}()
			defer connect.UnregisterProxy(0)

			// Register it early so it shows up in health reports immediately as [direct]
			connect.RegisterProxy(0, "direct")
			provideWithProxy(nativeCtx, nil, true, false)
		})
	} else {
		tlog("[no-direct] providing on direct/local IP is disabled; using proxy list only\n")
	}

	// Persist the initial state snapshot now that all IDs are resolved.
	proxyState.NextID = currentProxyIDCounter()
	if err := writeProxyState(proxyState); err != nil {
		tlog("[proxy] warning: could not write proxy.state: %v\n", err)
	}

	// Load persisted DoH server scores and start a warm-up probe so the
	// server-scorer (P2-2) has signal before real DNS traffic arrives.
	// Scores are saved periodically and on shutdown; the cache is closed
	// via deferred cleanup.
	_, closeDohCache := initPersistentDohCache(ctx)
	defer closeDohCache()

	// Load persisted slow-retry state so that a restart does not reset
	// the 14-day drop clock for proxies that were already failing.
	globalProxySlowRetryState = LoadProxySlowRetryState()

	if 0 < len(allProxySettings) {
		fmt.Printf("Using %d proxy servers:\n", len(allProxySettings))

		for _, proxySettings := range allProxySettings {
			stableID := resolveProxyID(proxyState, proxySettings.Address)
			proxySettings.Index = stableID
			tagProxySourceIfUnset(proxyState, proxySettings.Address, proxySourceOf[proxySettings.Address])
			connect.RegisterProxy(stableID, proxySettings.Address)
			var user string
			var password string
			if proxySettings.Auth != nil {
				user = proxySettings.Auth.User
				password = proxySettings.Auth.Password
			}
			fmt.Printf("  proxy[%d] %s (%s/%s)\n",
				stableID,
				proxySettings.Address,
				obfuscateUser(user),
				obfuscatePassword(password),
			)
		}

		for i, proxySettings := range allProxySettings {
			proxyCtx, proxyCancel := context.WithCancel(ctx)
			proxyCancelMu.Lock()
			proxyCancelMap[proxySettings.Address] = proxyCancel
			proxyCancelMu.Unlock()

			stableID := proxySettings.Index
			proxyIdx := i
			isURLSourced := proxySourceOf[proxySettings.Address] == "url"
			wg.Add(1)
			go connect.HandleError(func() {
				defer wg.Done()
				defer connect.UnregisterProxy(stableID)
				defer proxyCancel()

				staggerMs := 150
				if isURLSourced {
					staggerMs = 500
				}
				now := time.Now()
				if !backoffPacer(proxyIdx, staggerMs, now, proxyCtx) {
					return
				}
				// Log previously-dropped proxies on restart so the
				// operator gets visibility into persistent failures.
				// The continuous drop clock means this proxy will likely
				// be re-dropped on the first slow auth attempt.
				if !isURLSourced && proxySettings != nil && globalProxySlowRetryState.WasDropped(proxySettings.Address) {
					dropAge := time.Since(globalProxySlowRetryState.DropTime(proxySettings.Address))
					tlog("[proxy][slow-retry] proxy[%d] (%s) previously dropped %s ago; re-entering slow retry (will likely re-drop)\n",
						proxySettings.Index, proxySettings.Address, formatDuration(dropAge))
				}
				proxyLaunchCount.Add(1)

				provideWithProxy(proxyCtx, proxySettings, false, isURLSourced)
			})
		}
	}

	// Start the hot-reload watcher: it polls ~/.urnetwork/proxy.reload and applies
	// add/remove diffs to the running proxy set without restarting the provider.
	reloader := &ProxyReloader{
		cancelMap:       proxyCancelMap,
		cancelMapMu:     &proxyCancelMu,
		state:           proxyState,
		sourcePath:      proxyFile,
		parentCtx:       ctx,
		wg:              &wg,
		spawnProxy:      provideWithProxy,
		drainingProxies: make(map[string]context.CancelFunc),
	}
	reloader.StartWatcher(ctx)
	// Enforce an operator trim cap immediately at startup. The initial launch
	// loop spawns every entry in the source, so without this the first reload
	// reconciler tick (up to an hour later) would be the first time the cap
	// binds.
	reloader.reload()

	go connect.HandleError(func() {
		runProxyURLFetcher(ctx, proxyURLs, proxyURLRefresh, proxyURLMax, apiProbeHost, apiProbePort, selfHealEnabled)
	})
	go connect.HandleError(func() { runURLProxyReaper(ctx, apiProbeHost, apiProbePort) })
	// Paid/file-list proxy grading: rides the reaper ticker cadence, grades
	// non-URL proxies read-only on the 1-3h stale sweep (design note).
	go connect.HandleError(func() { runPaidProxyGrader(ctx, apiProbeHost, apiProbePort) })
	// Periodic A-F grade summary of the RUNNING proxy set (design 2026-08-09):
	// running/per-source/changes/scores lines (important + disk + grades.log)
	// and a ramlog-only next-probe countdown. Pure-read, never probes.
	go connect.HandleError(func() { runProxyGradeSummary(ctx) })
	go connect.HandleError(func() { pruneURLProxyBlacklist(ctx) })
	go connect.HandleError(func() { runProxyURLCleanup(ctx, cleanupScope, cleanupInterval, selfHealEnabled) })
	go connect.HandleError(func() { runPressureMonitor(ctx, selfHealEnabled) })
	go connect.HandleError(func() { runPoolController(ctx, proxyURLMax, selfHealEnabled) })
	go connect.HandleError(func() { runDegradedProxyReaper(ctx, proxyCancelMap, &proxyCancelMu) })
	go connect.HandleError(func() { runReloadReconciler(ctx) })

	if profileAddr := os.Getenv("URNETWORK_PPROF"); profileAddr != "" {
		tlog("[profile] enabling diagnostics on %s (loopback only): /debug/pprof/*, /metrics/pool, /metrics/errors\n", profileAddr)
		if err := connect.EnableProfiling(profileAddr); err != nil {
			tlog("[profile] failed to enable diagnostics: %v\n", err)
		}
	}
	if 0 < port {
		fmt.Printf(
			"Provider %s started. Status on *:%d\n",
			RequireVersion(),
			port,
		)
		statusServer := &http.Server{
			Addr:    fmt.Sprintf(":%d", port),
			Handler: &Status{},
			// Matches hub/main.go: guards against Slowloris-style connection
			// exhaustion (dribbled headers, opened-and-idle connections).
			ReadHeaderTimeout: 10 * time.Second,
			IdleTimeout:       120 * time.Second,
		}

		go func() {
			for {
				err := statusServer.ListenAndServe()
				if errors.Is(err, http.ErrServerClosed) {
					return
				}
				if err != nil {
					tlog("[status] error: %v — retrying in 30s\n", err)
				}
				select {
				case <-time.After(30 * time.Second):
					continue
				case <-ctx.Done():
					return
				}
			}
		}()
	} else {
		fmt.Printf(
			"Provider %s started\n",
			RequireVersion(),
		)
	}

	wg.Wait()

	// All goroutines have finished. Log final status before exit.
	tlog("[provider] exiting\n")
	critLog("PROVIDER EXIT: normal shutdown (code=0)")
	// Explicitly close the DoH cache before os.Exit(0) since defers do
	// not run on os.Exit. idempotent — safe if already called by defer.
	closeDohCache()
	os.Exit(0)
}

// containerIDRe matches a default Docker container hostname (12-char hex),
// so we can omit it from the dashboard label when it carries no useful meaning.
var containerIDRe = regexp.MustCompile("^[0-9a-f]{12}$")

// providerStatePath returns the absolute filesystem path of a named
// provider state file under ~/.urnetwork (alongside `jwt`). Does not
// create the directory.
func providerStatePath(name string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".urnetwork", name), nil
}

// readProviderClientKeySeed loads the Ed25519 seed for the provider
// client's long-lived identity key from `~/.urnetwork/.provider.key`.
// Returns (nil, nil) when the file does not exist — a fresh install.
// The file is the raw 32-byte seed; no encoding.
func readProviderClientKeySeed() ([]byte, error) {
	p, err := providerStatePath(".provider.key")
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return b, err
}

// writeProviderClientKeySeed persists the Ed25519 seed to
// `~/.urnetwork/.provider.key` with 0600 permissions (sensitive
// material — anyone with this file can impersonate the provider
// against the platform identity layer).
func writeProviderClientKeySeed(seed []byte) error {
	p, err := providerStatePath(".provider.key")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return err
	}
	return atomicWriteFile(p, seed, 0600)
}

// enableProviderEncryption turns on the per-peer e2e encryption sessions
// (post-quantum key exchange) on a provider's serving client. The provider
// serves plaintext and encrypted peers seamlessly: a session only forms when
// an initiator starts the handshake, and every enabled provider increases
// the set of peers that post-quantum initiators can reach. Opportunistic
// (not Required) so older consumers that cannot establish a session are
// still served.
func enableProviderEncryption(clientSettings *connect.ClientSettings) {
	if clientSettings.EncryptionSettings == nil {
		clientSettings.EncryptionSettings = connect.DefaultEncryptionSettings()
	}
	clientSettings.EncryptionSettings.Mode = connect.EncryptionModeOpportunistic
}

// readProviderTlsCertAndKey loads the sequence-level TLS server cert
// chain and matching private key from `~/.urnetwork/.provider.cert`
// (PEM, leaf first, possibly chained) and the private key from the
// same file (the PEM blocks are concatenated: cert blocks first,
// then a single `PRIVATE KEY` block). Returns (nil, nil, nil) when
// the file does not exist.
func readProviderTlsCertAndKey() (certPem []byte, keyPem []byte, returnErr error) {
	p, err := providerStatePath(".provider.cert")
	if err != nil {
		return nil, nil, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	// Split into cert blocks and the private key block.
	rest := b
	for {
		block, next := pem.Decode(rest)
		if block == nil {
			break
		}
		// re-encode the block so the output is canonical PEM (one
		// trailing newline per block).
		blockPem := pem.EncodeToMemory(block)
		if block.Type == "CERTIFICATE" {
			certPem = append(certPem, blockPem...)
		} else {
			// First non-cert block (typically `PRIVATE KEY` or
			// `EC PRIVATE KEY`) is treated as the key. Stop after
			// the first key block.
			keyPem = blockPem
			break
		}
		rest = next
	}
	return certPem, keyPem, nil
}

// writeProviderTlsCertAndKey persists the sequence-level TLS server
// cert and private key to `~/.urnetwork/.provider.cert` with 0600
// permissions. The cert blocks are written first, then the private
// key block, so the on-disk file is a self-contained PEM bundle.
func writeProviderTlsCertAndKey(certPem, keyPem []byte) error {
	p, err := providerStatePath(".provider.cert")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return err
	}
	out := make([]byte, 0, len(certPem)+len(keyPem))
	out = append(out, certPem...)
	out = append(out, keyPem...)
	return atomicWriteFile(p, out, 0600)
}

// proxyAuthRetryDelay picks the wait before the next auth retry. A 429 means
// the API is explicitly asking us to slow down, unlike a timeout (which is
// just as likely transient on our end), so it scales more aggressively with
// the attempt count than every other error — otherwise a batch of proxies
// that all hit 429s keeps hammering the API at the same rate.
//
// Non-429 errors still scale with attempt, just gentler (1s/attempt instead
// of 5s/attempt, capped at 15s instead of 60s): a proven proxy's 9th retry
// got the exact same 0.5-10.5s jitter as its 1st, giving a chronically
// flaky proxy no extra breathing room as its failure streak grew.
// proxyURLGiveUpRetryBase is the starting delay before a URL-sourced proxy
// that exhausted its auth attempts is automatically retried via a delayed
// reload trigger — a lightweight time.AfterFunc, not a per-proxy goroutine.
// proxyURLGiveUpRetryDelay doubles this on every subsequent give-up cycle
// for the same address, up to proxyURLGiveUpRetryCap, so a chronically dead
// address stops competing for an auth-rate-limiter slot as often as a fresh
// one.
const (
	proxyURLGiveUpRetryBase = 15 * time.Minute
	proxyURLGiveUpRetryCap  = 24 * time.Hour

	// proxyURLGiveUpEvictAfterCycles is the lifetime give-up count at which a
	// URL-sourced address is permanently evicted (see evictProxyURLAddress)
	// instead of requeued again. At 4 cycles the doubling backoff reaches 2h
	// (15min → 30min → 1h → 2h), so a dead proxy is evicted after roughly
	// 4 hours of wall time. The 24h blacklist cooldown then prevents re-entry;
	// the blacklist pruner removes it after 24h, and the next URL fetch cycle
	// re-probes it from scratch (must pass the dual-stage probe to re-enter).
	proxyURLGiveUpEvictAfterCycles = 4
)

// proxyURLGiveUpRetryDelay computes the requeue delay for a URL-sourced
// proxy's Nth give-up (giveUpCount is 1-indexed: this call is for the
// giveUpCount'th give-up, so giveUpCount=1 is the very first time this
// address gave up). Doubles proxyURLGiveUpRetryBase each cycle, capped at
// proxyURLGiveUpRetryCap, with up to 20% jitter so many addresses that gave
// up around the same time don't all retry in the same instant.
func proxyURLGiveUpRetryDelay(giveUpCount int) time.Duration {
	if giveUpCount < 1 {
		giveUpCount = 1
	}
	delay := proxyURLGiveUpRetryBase
	for i := 1; i < giveUpCount; i++ {
		delay *= 2
		if delay >= proxyURLGiveUpRetryCap {
			delay = proxyURLGiveUpRetryCap
			break
		}
	}
	jitter := time.Duration(mathrand.Int63n(int64(delay)/5 + 1)) // up to 20%
	return delay + jitter
}

// proxyAuthSlowRetryDelay is the backoff for an operator-curated proxy
// (file/internal/direct) that has exhausted its fast retries. Rather than give
// up, it keeps trying on a slow schedule:
//
//   - Attempts 1-3: 5m, 10m, 15m (original ramp — catches brief flapping)
//   - Attempt 4+:    24h (daily — one attempt per day until 14-day drop)
//
// Jitter spreads a large batch so they do not all re-hit the API on the
// same tick.
func proxyAuthSlowRetryDelay(slowAttempt int) time.Duration {
	if slowAttempt < 1 {
		slowAttempt = 1
	}
	var base time.Duration
	if slowAttempt <= slowRetryRampAttempts {
		base = time.Duration(slowAttempt) * 5 * time.Minute
	} else {
		base = slowRetryDailyInterval
	}
	return base + time.Duration(mathrand.Intn(30000))*time.Millisecond
}

func proxyAuthRetryDelay(err error, attempt int) time.Duration {
	if isRateLimitedError(err) {
		delay := time.Duration(attempt)*5*time.Second + time.Duration(mathrand.Intn(5000))*time.Millisecond
		if delay > 60*time.Second {
			delay = 60 * time.Second
		}
		return delay
	}
	delay := time.Duration(500+mathrand.Intn(3000)) * time.Millisecond
	if attempt > 1 {
		delay += time.Duration(attempt-1) * time.Second
	}
	if delay > 15*time.Second {
		delay = 15 * time.Second
	}
	return delay
}

// providerDescription builds the display-name string sent as the client
// description at mint AND renewal time: "Identity [Version]", where Identity
// is the node name (URNETWORK_NODE_NAME, else HOST_HOSTNAME, else hostname),
// optionally "Name @ RedactedIP" when URNETWORK_PUBLIC_IP is set, or just the
// redacted IP for container-id gibberish names. Kept as ONE helper so mint
// (provideAuth) and in-process renewal (runProxyJWTWatcher) always agree —
// the server UPDATEs the row's description on renewal, so divergence would
// silently rename the device in the dashboard.
func providerDescription(nodeName string) string {
	displayName := nodeName
	hostname, _ := os.Hostname()
	if displayName == "" {
		if hostHostname := strings.TrimSpace(os.Getenv("HOST_HOSTNAME")); hostHostname != "" {
			displayName = hostHostname
		} else {
			displayName = hostname
		}
	}
	isContainerID := containerIDRe.MatchString(displayName)
	publicIP := strings.TrimSpace(os.Getenv("URNETWORK_PUBLIC_IP"))

	var dashboardLabel string
	if ip4 := net.ParseIP(publicIP).To4(); ip4 != nil {
		parts := strings.Split(ip4.String(), ".")
		redactedIP := fmt.Sprintf("%s.x.x.%s", parts[0], parts[3])
		if displayName == "" || isContainerID {
			dashboardLabel = redactedIP
		} else {
			dashboardLabel = fmt.Sprintf("%s @ %s", displayName, redactedIP)
		}
	} else {
		if displayName == "" || isContainerID {
			dashboardLabel = "provider"
		} else {
			dashboardLabel = displayName
		}
	}
	return fmt.Sprintf("%s [%s]", dashboardLabel, RequireVersion())
}

// newProviderAuthClientArgsForRenewal builds the AuthNetworkClientArgs used to
// RENEW an existing per-proxy client identity. Unlike the mint path
// (newProviderAuthClientArgs), it carries ClientId so the server updates the
// existing network_client row and signs a fresh JWT for the SAME
// client_id/device_id — preserving server-side reliability reputation.
// SourceClientId stays nil: proxies remain independent top-level clients.
func newProviderAuthClientArgsForRenewal(description string, clientId connect.Id) *connect.AuthNetworkClientArgs {
	return &connect.AuthNetworkClientArgs{
		ClientId:    &clientId,
		Description: description,
		DeviceSpec:  "",
	}
}

// renewClientJWT renews the per-proxy client JWT for an existing client
// identity, using the account JWT as the Bearer credential. Returns the fresh
// client JWT (same client_id claim), or an error. It mirrors the auth-client
// call in provideAuth but with ClientId set — the renewal path.
//
// clientStrategy MUST be the proxy's own strategy (the one carrying
// ProxySettings): the mint path dials the API through the proxy, and renewal
// must egress the same way or it fails on any box that reaches the API only
// via its proxy, and would correlate the whole fleet to one host IP.
func renewClientJWT(ctx context.Context, apiUrl, byJwt string, clientId connect.Id, description string, clientStrategy *connect.ClientStrategy) (string, error) {
	if clientStrategy == nil {
		clientStrategy = connect.NewClientStrategyWithDefaults(ctx)
	}
	api := connect.NewBringYourApi(ctx, clientStrategy, apiUrl)
	api.SetByJwt(byJwt)

	callback, channel := connect.NewBlockingApiCallback[*connect.AuthNetworkClientResult](ctx)
	api.AuthNetworkClient(newProviderAuthClientArgsForRenewal(description, clientId), callback)

	var result connect.ApiCallbackResult[*connect.AuthNetworkClientResult]
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case result = <-channel:
	}
	if result.Error != nil {
		return "", fmt.Errorf("auth-client renewal api error: %w", result.Error)
	}
	if result.Result == nil {
		return "", fmt.Errorf("empty result from auth-client renewal API")
	}
	if result.Result.Error != nil {
		return "", fmt.Errorf("auth-client renewal rejected: %s", result.Result.Error.Message)
	}
	if result.Result.ByClientJwt == "" {
		return "", fmt.Errorf("empty by_client_jwt in renewal response")
	}
	if !jwtContainsClientId(result.Result.ByClientJwt) {
		return "", fmt.Errorf("regression guard: renewal returned a JWT without client_id claim")
	}
	// The whole "true renewal" premise rests on the server honoring the
	// supplied ClientId. If it returns a valid JWT for a DIFFERENT client
	// (row deleted, revoked, future server mints new), hot-swapping it would
	// split the running client's identity and poison the store forever.
	if got := jwtClientId(result.Result.ByClientJwt); got != clientId.String() {
		return "", fmt.Errorf("renewal returned client_id %q, want %q — refusing to swap", got, clientId.String())
	}
	return result.Result.ByClientJwt, nil
}

// renewClientJWTFn is the injectable renewal entry point. It defaults to the
// real network-backed renewClientJWT but can be overridden in tests to exercise
// the renew-on-expiry branch of provideAuth deterministically.
var renewClientJWTFn = renewClientJWT

func provideAuth(ctx context.Context, clientStrategy *connect.ClientStrategy, apiUrl string, opts docopt.Opts, nodeName string, identityKey string) (byClientJwt string, clientId connect.Id, reused bool, returnErr error) {
	home, err := os.UserHomeDir()
	if err != nil {
		// No HOME: no persisted JWT possible. Surface as a normal error, not
		// a panic (the old panic killed --version and one-shot commands in
		// bare environments; shakedown finding 2026-08-15).
		returnErr = fmt.Errorf("could not determine home directory: %w", err)
		return
	}
	jwtPath := filepath.Join(home, ".urnetwork", "jwt")

	if _, err := os.Stat(jwtPath); errors.Is(err, os.ErrNotExist) {
		// jwt does not exist
		returnErr = fmt.Errorf("Jwt does not exist at %s", jwtPath)
		return
	}

	byJwtBytes, err := os.ReadFile(jwtPath)
	if err != nil {
		returnErr = err
		return
	}
	byJwt := strings.TrimSpace(string(byJwtBytes))

	// Layer 1: local pre-validation — avoids a network round-trip for an already-expired token.
	if err := validateJWTExpiry(byJwt); err != nil {
		returnErr = err
		return
	}

	// A stored client JWT is only safe to reuse under the network identity
	// that minted it — otherwise switching accounts (USER_AUTH change, new
	// ~/.urnetwork/jwt) would silently keep providing under the previous
	// account. An unscoped network_id claim on either side is treated as a
	// mismatch (mint fresh) rather than a match, since that's the safer
	// default for credential reuse.
	//
	// Hot restart (reuse of persisted client JWTs across process restarts)
	// has been ON by default since v3.23.0-fix.26 (URNETWORK_HOT_RESTART
	// != "0"). Explicitly set URNETWORK_HOT_RESTART=0 to disable.
	//
	// Legacy entries (stored with NetworkID="") may lack the network_id
	// field because they predate the field being added, or because the
	// account JWT at the time of storage had no network_id claim. These
	// are treated as compatible when the current account JWT DOES carry
	// a network_id (assumed same network, minted under the same account).
	// If the current JWT has no network_id claim, we always mint fresh
	// since we can't verify safe reuse. The first successful reuse or
	// renewal stamps the current NetworkID into the store, so subsequent
	// restarts get a clean match and any later account/network swap is
	// detected by the mismatch guard above rather than silently reusing
	// a stale identity.
	// Compute description early — needed for both the renewal fallback path
	// (reuse an expired stored identity) and the fresh-mint path below.
	description := providerDescription(nodeName)

	currentNetworkId, haveCurrentNetworkId := jwtNetworkId(byJwt)
	if hotRestartEnabled() {
		if entry, ok := globalClientJWTStore.Get(identityKey); ok {
			if !haveCurrentNetworkId || (entry.NetworkID != "" && entry.NetworkID != currentNetworkId) {
				tlog("🔥 [hot-restart] %s: network_id mismatch (stored=%q current=%q have_current=%v), minting fresh\n", identityKey, entry.NetworkID, currentNetworkId, haveCurrentNetworkId)
			} else if reuseErr := validateJWTExpiry(entry.ByClientJWT); reuseErr != nil {
				// Stored client JWT expired: try renewal-with-same-client_id
				// before falling through to fresh mint, so the operator's
				// identities (and their server-side reliability reputation)
				// survive even when the JWT has aged out of its 24h window.
				if parsedId, parseErr := connect.ParseId(entry.ClientID); parseErr == nil && jwtContainsClientId(entry.ByClientJWT) {
					renewedJwt, renewErr := renewClientJWTFn(ctx, apiUrl, byJwt, parsedId, description, clientStrategy)
					if renewErr == nil {
						tlog("🔥 [hot-restart] %s: stored client JWT expired, renewed identity %s\n", identityKey, parsedId)
						if putErr := globalClientJWTStore.Put(identityKey, clientJWTEntry{
							ByClientJWT: renewedJwt,
							ClientID:    entry.ClientID,
							NetworkID:   currentNetworkId,
							MintedAt:    time.Now(),
						}); putErr != nil {
							tlog("⚠️ [jwt-store] failed to persist renewed client JWT for %s: %v\n", identityKey, putErr)
						}
						return renewedJwt, parsedId, true, nil
					}
					tlog("🔥 [hot-restart] %s: stored client JWT expired, renewal attempt failed (%v), minting fresh\n", identityKey, renewErr)
				} else {
					tlog("🔥 [hot-restart] %s: stored client JWT expired, minting fresh\n", identityKey)
				}
			} else if !jwtContainsClientId(entry.ByClientJWT) {
				tlog("🔥 [hot-restart] %s: stored client JWT missing client_id claim, minting fresh\n", identityKey)
			} else if parsedId, parseErr := connect.ParseId(entry.ClientID); parseErr != nil {
				tlog("🔥 [hot-restart] %s: stored client_id %q failed to parse (%v), minting fresh\n", identityKey, entry.ClientID, parseErr)
			} else {
				reused = true
				// Self-heal: stamp the current network_id onto legacy
				// entries (stored with NetworkID="") so a future
				// account/network swap is detected by the mismatch guard
				// above instead of silently reusing a stale identity. Only
				// fires for entries whose NetworkID differs from the
				// current one — after the first reuse they match, so this
				// is a one-time write per proxy at startup, not per-auth.
				if entry.NetworkID != currentNetworkId {
					if putErr := globalClientJWTStore.Put(identityKey, clientJWTEntry{
						ByClientJWT: entry.ByClientJWT,
						ClientID:    entry.ClientID,
						NetworkID:   currentNetworkId,
						MintedAt:    entry.MintedAt,
					}); putErr != nil {
						tlog("⚠️ [jwt-store] failed to self-heal network_id for %s: %v\n", identityKey, putErr)
					}
				}
				return entry.ByClientJWT, parsedId, true, nil
			}
		}
	}

	api := connect.NewBringYourApi(ctx, clientStrategy, apiUrl)

	api.SetByJwt(byJwt)

	authClientCallback, authClientChannel := connect.NewBlockingApiCallback[*connect.AuthNetworkClientResult](ctx)

	// Final Description: "Identity [Version]" — computed by the shared helper
	// so mint (here) and in-process renewal (runProxyJWTWatcher) always send
	// the same string; the server UPDATEs the row's description on renewal.

	authClientArgs := &connect.AuthNetworkClientArgs{
		Description: description,
		DeviceSpec:  "",
	}

	api.AuthNetworkClient(authClientArgs, authClientCallback)

	var authClientResult connect.ApiCallbackResult[*connect.AuthNetworkClientResult]
	select {
	case <-ctx.Done():
		tlog("[auth] exiting: signal received during auth-client request\n")
		returnErr = ctx.Err()
		return
	case authClientResult = <-authClientChannel:
	}

	if authClientResult.Error != nil {
		returnErr = authClientResult.Error
		return
	}
	if authClientResult.Result == nil {
		returnErr = fmt.Errorf("auth response missing result")
		return
	}
	if authClientResult.Result.Error != nil {
		if authClientResult.Result.Error.ClientLimitExceeded {
			returnErr = fmt.Errorf("client limit exceeded: %s", authClientResult.Result.Error.Message)
			return
		}
		// Do NOT wrap in ErrTokenInvalid: that sentinel triggers shmLogFatal(78)
		// which kills the entire provider process. Auth-client rejections (bad
		// device spec, unrecognized description, temporary server error) should
		// retry via the normal backoff path, not crash all 120 nodes.
		returnErr = fmt.Errorf("auth-client rejected: %s", authClientResult.Result.Error.Message)
		return
	}

	byClientJwt = authClientResult.Result.ByClientJwt

	// parse the clientId
	parser := gojwt.NewParser()
	token, _, err := parser.ParseUnverified(byClientJwt, gojwt.MapClaims{})
	if err != nil {
		returnErr = fmt.Errorf("failed to parse client JWT from API response: %w", err)
		return
	}

	claims, ok := token.Claims.(gojwt.MapClaims)
	if !ok {
		returnErr = fmt.Errorf("unexpected claims type in client JWT")
		return
	}

	clientIdStr, ok := claims["client_id"].(string)
	if !ok {
		returnErr = fmt.Errorf("client_id claim missing or not a string in client JWT")
		return
	}

	clientId, err = connect.ParseId(clientIdStr)
	if err != nil {
		returnErr = fmt.Errorf("invalid client_id in JWT claims: %w", err)
		return
	}

	// Always persist client JWTs so the store is ready the moment hot-restart
	// is enabled — no warmup or re-auth cycle needed. The read/reuse path
	// remains gated on URNETWORK_HOT_RESTART=1.
	if putErr := globalClientJWTStore.Put(identityKey, clientJWTEntry{
		ByClientJWT: byClientJwt,
		ClientID:    clientIdStr,
		NetworkID:   currentNetworkId,
		MintedAt:    time.Now(),
	}); putErr != nil {
		tlog("⚠️ [jwt-store] failed to persist client JWT for %s: %v\n", identityKey, putErr)
	}

	return
}

// revokedIdentityAuthFailureThreshold is how many transport auth failures a
// reused identity is allowed before it's treated as revoked rather than
// merely unlucky (network blip, transient API error).
const revokedIdentityAuthFailureThreshold = 5

// watchReusedIdentityForRevocation polls proxyIndex's transport health and
// evicts identityKey from the client JWT store if the reused identity keeps
// failing to authenticate and never once comes up. See the call site for
// why this can't be detected any other way: the reuse path is intentionally
// server-round-trip-free, so nothing else observes a server-side rejection.
//
// revocationDone, when non-nil, is closed by the renewal watcher on a
// successful renewal: the identity is then demonstrably alive (the server
// just re-signed it), so this watcher stops before it evicts the fresh entry
// while the transport is still reconnecting.
func watchReusedIdentityForRevocation(ctx context.Context, identityKey string, proxyIndex int, revocationDone <-chan struct{}) {
	ticker := time.NewTicker(2 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-revocationDone:
			tlog("🛑 [jwt-store] identity for %s renewed successfully — revocation watcher standing down\n", identityKey)
			return
		case <-ticker.C:
		}
		if connect.ProxyEverUp(proxyIndex) {
			// Authenticated successfully at least once — the identity is good.
			return
		}
		if connect.ProxyAuthFailureCount(proxyIndex) >= revokedIdentityAuthFailureThreshold {
			// Re-check the renewal signal AFTER the failure-count check: the
			// renewal watcher may have closed revocationDone while this
			// goroutine was between its select and this eviction decision
			// (renewal writes the store then closes the channel synchronously,
			// but we could have already passed the select). Without this
			// double-check, a successfully renewed identity could be evicted
			// out from under the reconnecting transport.
			select {
			case <-revocationDone:
				tlog("🛑 [jwt-store] identity for %s renewed successfully — revocation watcher standing down\n", identityKey)
				return
			default:
			}
			if delErr := globalClientJWTStore.Delete(identityKey); delErr != nil {
				tlog("⚠️ [jwt-store] failed to evict possibly-revoked identity for %s: %v\n", identityKey, delErr)
			} else {
				tlog("⚠️ [jwt-store] reused client identity for %s never authenticated after %d transport auth failures — evicted, will mint fresh on next retry/restart\n",
					identityKey, revokedIdentityAuthFailureThreshold)
			}
			return
		}
	}
}

type Status struct {
}

func (self *Status) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	type WarpStatusResult struct {
		Version       string `json:"version,omitempty"`
		ConfigVersion string `json:"config_version,omitempty"`
		Status        string `json:"status"`
		ClientAddress string `json:"client_address,omitempty"`
		Host          string `json:"host"`
	}

	result := &WarpStatusResult{
		Version: RequireVersion(),
		// ConfigVersion: RequireConfigVersion(),
		Status: "ok",
		Host:   RequireHost(),
	}

	responseJson, err := json.Marshal(result)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(responseJson)
}

func Host() (string, error) {
	host := os.Getenv("WARP_HOST")
	if host != "" {
		return host, nil
	}
	host, err := os.Hostname()
	if err == nil {
		return host, nil
	}
	return "", errors.New("WARP_HOST not set")
}

func RequireHost() string {
	host, err := Host()
	if err != nil {
		panic(err)
	}
	return host
}

func RequireVersion() string {
	return Version
}

func proxyAuthAdd(opts docopt.Opts) {
	proxyConfig := readProxyConfig()

	key, _ := opts.String("key")
	user, _ := opts.String("proxy_user")
	password, _ := opts.String("proxy_password")

	if proxyConfig.Auths == nil {
		proxyConfig.Auths = map[string]*ProxyAuth{}
	}

	if _, ok := proxyConfig.Auths[key]; ok {
		if force, _ := opts.Bool("-f"); !force {
			fmt.Printf("auth key \"%s\" exists. Overwrite? [yN]\n", key)

			reader := bufio.NewReader(os.Stdin)
			confirm, _ := reader.ReadString('\n')
			if strings.ToLower(strings.TrimSpace(confirm)) != "y" {
				return
			}
		}
	}

	proxyConfig.Auths[key] = &ProxyAuth{
		User:     user,
		Password: password,
	}

	writeProxyConfig(proxyConfig)
}

func proxyAuthRemove(opts docopt.Opts) {
	proxyConfig := readProxyConfig()

	if all, _ := opts.Bool("--all"); all {
		clear(proxyConfig.Auths)
	} else {

		key, _ := opts.String("key")

		if proxyConfig.Auths == nil {
			proxyConfig.Auths = map[string]*ProxyAuth{}
		}

		delete(proxyConfig.Auths, key)
	}

	writeProxyConfig(proxyConfig)
}

func proxyAdd(opts docopt.Opts) {
	proxyConfig := readProxyConfig()

	allKeyAddress := []string{}
	if allKeyAddressAny, ok := opts["<key_address>"]; ok {
		allKeyAddress = append(allKeyAddress, allKeyAddressAny.([]string)...)
	}
	if proxyPath, _ := opts.String("--proxy_file"); proxyPath != "" {
		b, err := os.ReadFile(proxyPath)
		if err != nil {
			panic(err)
		}
		for _, line := range strings.Split(string(b), "\n") {
			line = strings.TrimSpace(line)
			if line != "" && line[0] != '#' {
				allKeyAddress = append(allKeyAddress, line)
			}
		}
	}

	if proxyConfig.Servers == nil {
		proxyConfig.Servers = map[string]string{}
	}

	for _, keyAddress := range allKeyAddress {
		var key string
		var proxyAddress string
		i := strings.Index(keyAddress, "@")
		if 0 <= i {
			key = keyAddress[:i]
			proxyAddress = keyAddress[i+1:]
		} else {
			key = ""
			proxyAddress = keyAddress
		}

		address, user, password := parseProxyAddress(proxyAddress)
		if proxyConfig.Auths != nil {
			proxyAuth, ok := proxyConfig.Auths[key]
			if ok {
				user = proxyAuth.User
				password = proxyAuth.Password
			}
		}

		if currentKey, ok := proxyConfig.Servers[proxyAddress]; ok && currentKey != key {
			if force, _ := opts.Bool("-f"); !force {
				fmt.Printf(
					"server %s (%s/%s) exists with different key. Change key? [yN]\n",
					address,
					obfuscateUser(user),
					obfuscatePassword(password),
				)

				reader := bufio.NewReader(os.Stdin)
				confirm, _ := reader.ReadString('\n')
				if strings.ToLower(strings.TrimSpace(confirm)) != "y" {
					return
				}
			}
		}

		fmt.Printf(
			"added server %s (%s/%s)\n",
			address,
			obfuscateUser(user),
			obfuscatePassword(password),
		)

		proxyConfig.Servers[proxyAddress] = key
	}

	writeProxyConfig(proxyConfig)
}

func proxyRemove(opts docopt.Opts) {
	if pattern, _ := opts.String("--match"); pattern != "" {
		proxyRemoveMatch(pattern, opts)
		return
	}

	proxyConfig := readProxyConfig()

	if all, _ := opts.Bool("--all"); all {
		clear(proxyConfig.Servers)
		// Take the cross-process lock so a concurrent daemon health-snapshot
		// write can't clobber this reset and silently revive the removed
		// proxies (finding #11). Proxy config write above is process-local;
		// this serializes the proxy.state reset against the daemon.
		stateLockRelease, stateLockErr := acquireProxyLockWithRetry()
		if stateLockErr != nil {
			tlog("[proxy] warning: could not acquire proxy lock for state reset: %v\n", stateLockErr)
			writeProxyConfig(proxyConfig)
			return
		}
		// Reset proxy.state so the next run starts ID assignment from 0.
		// Without this, the monotonic counter resumes above whatever IDs were
		// saved from previous runs, producing confusingly high and mixed IDs
		// even when the same proxies are re-added.
		if state, err := readProxyState(); err == nil {
			state.Proxies = map[string]ProxyEntry{}
			state.NextID = 0
			if err := writeProxyState(state); err != nil {
				tlog("[proxy] warning: could not reset proxy.state: %v\n", err)
			}
		}
		stateLockRelease()
		// Also clear the URL cache and source URLs so previously-fetched free
		// proxies don't reappear after a restart. The user wants only the
		// proxies they explicitly added; URL sources must be re-added if
		// desired.
		if urlState, err := readProxyURLState(); err == nil {
			urlState.Cache = map[string]ProxyURLEntry{}
			urlState.Sources = nil
			if err := writeProxyURLState(urlState); err != nil {
				tlog("[proxy] warning: could not clear proxy_url.json cache: %v\n", err)
			}
		}
	} else {

		allKeyAddress := []string{}
		if allKeyAddressAny, ok := opts["<key_address>"]; ok {
			allKeyAddress = append(allKeyAddress, allKeyAddressAny.([]string)...)
		}

		if proxyConfig.Servers == nil {
			proxyConfig.Servers = map[string]string{}
		}

		for _, keyAddress := range allKeyAddress {
			var key string
			var address string
			i := strings.Index(keyAddress, "@")
			if 0 <= i {
				key = keyAddress[:i]
				address = keyAddress[i+1:]
			} else {
				key = ""
				address = keyAddress
			}

			if key == "" || proxyConfig.Servers[address] == key {
				delete(proxyConfig.Servers, address)
			}
		}
	}

	writeProxyConfig(proxyConfig)
}

func proxyRemoveMatch(pattern string, opts docopt.Opts) {
	autoYes, _ := opts.Bool("--yes")
	preview, _ := opts.Bool("--preview")

	proxyConfig := readProxyConfig()

	// state and urlState are optional: the provider may never have run,
	// or there may be no URL sources. Missing stores just mean fewer
	// places to search.
	var stateProxies map[string]ProxyEntry
	var stateSource string
	state, stateErr := readProxyState()
	if stateErr == nil {
		stateProxies = state.Proxies
		stateSource = state.Source
	} else {
		state = &ProxyState{}
	}
	urlState, urlErr := readProxyURLState()
	if urlErr != nil {
		urlState = &ProxyURLState{}
	}

	addrsBySource, display := collectMatchingProxies(
		pattern, proxyConfig.Servers, stateProxies, stateSource, urlState.Cache)

	if len(display) == 0 {
		fmt.Printf("no proxies matched %q — nothing to do\n", pattern)
		return
	}

	const sampleMax = 10
	fmt.Printf("%d proxies match %q:\n", len(display), pattern)
	for i, d := range display {
		if i == sampleMax {
			fmt.Printf("    ... and %d more\n", len(display)-sampleMax)
			break
		}
		fmt.Printf("    %s\n", d)
	}

	if preview {
		fmt.Println("=== PREVIEW (no changes will be made) ===")
		return
	}

	if !autoYes && !confirm(fmt.Sprintf("Remove %d proxies and exclude %q from future URL fetches?", len(display), pattern)) {
		fmt.Println("Aborted.")
		return
	}

	if err := removeDeadProxies(state, addrsBySource); err != nil {
		fmt.Printf("removal failed: %v\n", err)
		return
	}

	// Persist the exclude pattern so URL source refreshes cannot re-add
	// matching proxies. Re-read to avoid clobbering the cache changes
	// removeDeadProxies just wrote.
	if urlState, err := readProxyURLState(); err == nil {
		if addExcludePattern(urlState, pattern) {
			if err := writeProxyURLState(urlState); err != nil {
				fmt.Printf("warning: could not persist exclude pattern: %v\n", err)
			}
		}
	}

	fmt.Printf("Removed %d proxies matching %q. Pattern excluded from future URL fetches.\n", len(display), pattern)
	fmt.Println("The running provider will apply the change via hot reload (no restart).")
}

// proxyExclude manages the URL-fetch exclude patterns:
//
//	proxy exclude                    list active patterns
//	proxy exclude <pattern>          add a pattern
//	proxy exclude <pattern> --remove delete a pattern
func proxyExclude(opts docopt.Opts) {
	pattern, _ := opts.String("<pattern>")
	removeFlag, _ := opts.Bool("--remove")

	urlState, err := readProxyURLState()
	if err != nil {
		fmt.Printf("could not read proxy_url.json: %v\n", err)
		return
	}

	if pattern == "" {
		if removeFlag {
			fmt.Println("usage: proxy exclude <pattern> --remove")
			return
		}
		if len(urlState.ExcludePatterns) == 0 {
			fmt.Println("no exclude patterns set")
			return
		}
		fmt.Printf("%d exclude patterns (URL fetches skip matching hosts):\n", len(urlState.ExcludePatterns))
		for _, p := range urlState.ExcludePatterns {
			fmt.Printf("    %s\n", p)
		}
		return
	}

	if removeFlag {
		if !removeExcludePattern(urlState, pattern) {
			fmt.Printf("pattern %q is not in the exclude list\n", pattern)
			if len(urlState.ExcludePatterns) > 0 {
				fmt.Printf("current patterns: %s\n", strings.Join(urlState.ExcludePatterns, ", "))
			}
			return
		}
		if err := writeProxyURLState(urlState); err != nil {
			fmt.Printf("could not write proxy_url.json: %v\n", err)
			return
		}
		fmt.Printf("removed exclude pattern %q — matching proxies may return on the next URL fetch\n", pattern)
		return
	}

	if !addExcludePattern(urlState, pattern) {
		fmt.Printf("pattern %q is already excluded\n", pattern)
		return
	}
	if err := writeProxyURLState(urlState); err != nil {
		fmt.Printf("could not write proxy_url.json: %v\n", err)
		return
	}
	fmt.Printf("added exclude pattern %q — future URL fetches will skip matching hosts\n", pattern)
	fmt.Println("note: already-cached/running proxies are not removed; use 'proxy remove --match' for that")
}

type ProxyConfig struct {
	Auths map[string]*ProxyAuth `json:"auths"`
	// TODO is there a use case for multiple keys to the same address?
	// address -> key
	Servers map[string]string `json:"servers"`
}

type ProxyAuth struct {
	User     string `json:"user"`
	Password string `json:"password"`
}

func readProxySettings() []*connect.ProxySettings {
	proxyConfig := readProxyConfig()

	if proxyConfig.Servers == nil {
		return nil
	}

	var allProxySettings []*connect.ProxySettings
	for proxyAddress, key := range proxyConfig.Servers {
		address, user, password := parseProxyAddress(proxyAddress)
		proxySettings := &connect.ProxySettings{
			Network: "tcp",
			Address: address,
		}
		if user != "" || password != "" {
			proxySettings.Auth = &proxy.Auth{
				User:     user,
				Password: password,
			}
		}
		if proxyConfig.Auths != nil {
			proxyAuth, ok := proxyConfig.Auths[key]
			if ok {
				proxySettings.Auth = &proxy.Auth{
					User:     proxyAuth.User,
					Password: proxyAuth.Password,
				}
			}
		}
		allProxySettings = append(allProxySettings, proxySettings)
	}

	return allProxySettings
}

// readProxySettingsFromFile reads proxy settings directly from an external file
// where each non-blank, non-comment line is ip:port:user:pass. Entries missing
// credentials are rejected (Workflow A requires authenticated proxies).
// Returns an error only if the file itself cannot be read; malformed individual
// lines are skipped with a warning.
func readProxySettingsFromFile(path string) ([]*connect.ProxySettings, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("could not read proxy file %s: %w", path, err)
	}
	var all []*connect.ProxySettings
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' {
			continue
		}
		address, user, password := parseProxyAddress(line)
		if user == "" || password == "" {
			tlog("[proxy] error: proxy %q missing credentials — required format ip:port:user:pass; skipping\n", line)
			continue
		}
		all = append(all, &connect.ProxySettings{
			Network: "tcp",
			Address: address,
			Auth:    &proxy.Auth{User: user, Password: password},
		})
	}
	return all, nil
}

// readSHMLog reads the RAM log file. If n > 0, returns only the last n lines.
// Returns an error if the file does not exist.
func readSHMLog(path string, n int) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if n <= 0 {
		return string(b), nil
	}
	s := strings.TrimRight(string(b), "\n")
	lines := strings.Split(s, "\n")
	if n > len(lines) {
		n = len(lines)
	}
	return strings.Join(lines[len(lines)-n:], "\n") + "\n", nil
}

func providerLogs(opts docopt.Opts) {
	n, _ := opts.Int("-n")
	out, err := readSHMLog(shmLogPath, n)
	if err != nil {
		shmLogFatal(40, "no ramlogs found at %s — is URNETWORK_RAMLOGS=1 set?", shmLogPath)
	}
	fmt.Print(out)

	// Tail: follow the file from current position.
	f, err := os.Open(shmLogPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: could not open log for tailing: %v\n", err)
		return
	}
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		f.Close()
		fmt.Fprintf(os.Stderr, "error: seek failed: %v\n", err)
		return
	}

	buf := make([]byte, 4096)
	for {
		nr, readErr := f.Read(buf)
		if nr > 0 {
			os.Stdout.Write(buf[:nr])
		}
		if readErr != nil && readErr != io.EOF {
			f.Close()
			fmt.Fprintf(os.Stderr, "error: read failed: %v\n", readErr)
			return
		}
		if readErr == io.EOF {
			// Detect ramlogs wrap: if the file shrunk behind our position, reopen from start.
			if pos, _ := f.Seek(0, io.SeekCurrent); pos > 0 {
				if fi, statErr := f.Stat(); statErr == nil && fi.Size() < pos {
					f.Close()
					newF, openErr := os.Open(shmLogPath)
					if openErr != nil {
						fmt.Fprintf(os.Stderr, "error: could not reopen log after wrap: %v\n", openErr)
						return
					}
					f = newF
				}
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
}

func parseProxyAddress(proxyAddress string) (address string, user string, password string) {
	r := regexp.MustCompile("^(.*:\\d*):([^:]*):([^:]*)$")
	groups := r.FindStringSubmatch(proxyAddress)
	if groups != nil {
		address = groups[1]
		user = groups[2]
		password = groups[3]
		return
	}
	// assume host:port
	address = proxyAddress
	return
}

func obfuscateUser(user string) string {
	if user == "" {
		return "<no user>"
	} else if len(user) < 6 {
		return "***"
	} else {
		return fmt.Sprintf("%s***%s", user[:2], user[len(user)-2:])
	}
}

func obfuscatePassword(password string) string {
	if password == "" {
		return "<no password>"
	} else if len(password) < 6 {
		return "***"
	} else {
		return fmt.Sprintf("%s***%s", password[:2], password[len(password)-2:])
	}
}

// resolveDuration returns the --flag value if set and parseable, else the
// env var if set and parseable, else def. Used for settings that must work
// identically whether passed as a provide() flag or a Docker env var.
func resolveDuration(opts docopt.Opts, flag, envVar string, def time.Duration) time.Duration {
	if v, _ := opts.String(flag); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
		tlog("[proxy][url] warning: invalid duration %q for %s; using default %s\n", v, flag, def)
		return def
	}
	if v := os.Getenv(envVar); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
		tlog("[proxy][url] warning: invalid duration %q for %s; using default %s\n", v, envVar, def)
	}
	return def
}

// resolveInt is resolveDuration's integer counterpart.
func resolveInt(opts docopt.Opts, flag, envVar string, def int) int {
	if v, _ := opts.String(flag); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
		tlog("[proxy][url] warning: invalid integer %q for %s; using default %d\n", v, flag, def)
		return def
	}
	if v := os.Getenv(envVar); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
		tlog("[proxy][url] warning: invalid integer %q for %s; using default %d\n", v, envVar, def)
	}
	return def
}

// resolveString is resolveDuration's plain-string counterpart.
func resolveString(opts docopt.Opts, flag, envVar, def string) string {
	if v, _ := opts.String(flag); v != "" {
		return v
	}
	if v := os.Getenv(envVar); v != "" {
		return v
	}
	return def
}

// resolveProxyURLs collects --proxy_url flag values, PROXY_URL env var
// values (comma-separated), and persisted sources from proxy_url.json
// (added via `proxy add-source`), deduplicated, in that priority order.
func resolveProxyURLs(opts docopt.Opts) []string {
	var urls []string

	if v, ok := opts["--proxy_url"]; ok && v != nil {
		switch vv := v.(type) {
		case []string:
			urls = append(urls, vv...)
		case string:
			if vv != "" {
				urls = append(urls, vv)
			}
		}
	}

	if envURLs := os.Getenv("PROXY_URL"); envURLs != "" {
		for _, u := range strings.Split(envURLs, ",") {
			if u = strings.TrimSpace(u); u != "" {
				urls = append(urls, u)
			}
		}
	}

	if urlState, err := readProxyURLState(); err != nil {
		tlog("[proxy][url] warning: could not read proxy_url.json: %v\n", err)
	} else {
		urls = append(urls, urlState.Sources...)
	}

	seen := map[string]bool{}
	deduped := make([]string, 0, len(urls))
	for _, u := range urls {
		if !seen[u] {
			seen[u] = true
			deduped = append(deduped, u)
		}
	}
	return deduped
}

func proxyRefresh(opts docopt.Opts) {
	force, _ := opts.Bool("--force")

	state, err := readProxyState()
	if err != nil {
		shmLogFatal(50, "could not read proxy.state (use 'provider proxy add/remove' to edit the proxy list for next startup)")
	}

	if state.StartedAt.IsZero() {
		shmLogFatal(51, "provider does not appear to be running (use 'provider proxy add/remove' to edit the proxy list for next startup)")
	}

	uptime := time.Since(state.StartedAt)

	const warmupThreshold = 8 * time.Hour
	if uptime < warmupThreshold && !force {
		shmLogFatal(52, "provider has only been running %s — proxies need 8-12h to warm up; use --force to override", formatDuration(uptime))
	}

	release, err := acquireProxyLock()
	if err != nil {
		shmLogFatal(53, "could not acquire proxy lock: %v", err)
	}
	defer release()

	var desired []*connect.ProxySettings
	if state.Source != "" {
		settings, err := readProxySettingsFromFile(state.Source)
		if err != nil {
			shmLogFatal(54, "could not read proxy file %s: %v", state.Source, err)
		}
		desired = settings
	} else {
		desired = readProxySettings()
	}

	// Diff
	desiredSet := map[string]bool{}
	for _, s := range desired {
		desiredSet[s.Address] = true
	}

	currentSet := map[string]ProxyEntry{}
	for addr, e := range state.Proxies {
		currentSet[addr] = e
	}

	var added []string
	for _, s := range desired {
		if _, ok := currentSet[s.Address]; !ok {
			added = append(added, s.Address)
		}
	}

	type removedProxy struct {
		addr  string
		entry ProxyEntry
	}
	var removed []removedProxy
	for addr, e := range currentSet {
		if !desiredSet[addr] {
			e.Health = classifyHealth(e)
			removed = append(removed, removedProxy{addr: addr, entry: e})
		}
	}

	if len(added) == 0 && len(removed) == 0 {
		fmt.Println("proxy list is already up to date. Nothing to do.")
		return
	}

	// Warn if all proxies would be removed — the provider exits when the last proxy goroutine stops.
	if len(removed) == len(currentSet) && len(added) == 0 {
		fmt.Printf("WARNING: This will remove ALL proxies. The provider process will exit once the\n")
		fmt.Printf("last proxy goroutine stops. Restart with a proxy list to resume providing.\n\n")
	}

	// Print diff
	fmt.Printf("proxy refresh: %d proxies will be removed, %d will be added.\n\n", len(removed), len(added))
	if len(removed) > 0 {
		fmt.Println("  Removing:")
		for _, rp := range removed {
			fmt.Printf("    proxy[%d]  %s   — %s\n", rp.entry.ID, rp.addr, rp.entry.Health)
		}
	}
	if len(added) > 0 {
		fmt.Println("\n  Adding:")
		for _, addr := range added {
			fmt.Printf("    %s\n", addr)
		}
	}

	// Check for high-risk removals: up, recently_offline, offline, long_offline all have
	// significant warm state. dead and inactive are low-risk (single confirmation).
	highRisk := false
	for _, rp := range removed {
		switch rp.entry.Health {
		case "up", "recently_offline", "offline", "long_offline":
			highRisk = true
		}
		if highRisk {
			break
		}
	}

	if highRisk {
		fmt.Printf("\nWARNING: One or more proxies being removed are online or have recent warm state.\n")
		if !confirm("Remove them anyway?") {
			fmt.Println("Aborted.")
			return
		}
		if !confirm("Are you sure? This may interrupt live traffic.") {
			fmt.Println("Aborted.")
			return
		}
	} else {
		if !confirm("Proceed?") {
			fmt.Println("Aborted.")
			return
		}
	}

	reloadPath, err := proxyReloadPath()
	if err != nil {
		shmLogFatal(55, "could not determine reload path: %v", err)
	}

	if err := writeReloadTrigger(reloadPath); err != nil {
		shmLogFatal(56, "could not write reload trigger: %v", err)
	}

	fmt.Println("Reload triggered. Provider will apply changes within 2 seconds.")
}

func proxyAddSource(opts docopt.Opts) {
	url, _ := opts.String("<url>")
	url = strings.TrimSpace(url)
	if url == "" {
		shmLogFatal(70, "no URL provided")
	}

	release, err := acquireProxyLock()
	if err != nil {
		shmLogFatal(71, "could not acquire proxy lock: %v", err)
	}

	state, err := readProxyURLState()
	if err != nil {
		release()
		shmLogFatal(72, "could not read proxy_url.json: %v", err)
	}
	for _, existing := range state.Sources {
		if existing == url {
			release()
			fmt.Printf("source already added: %s\n", url)
			return
		}
	}
	state.Sources = append(state.Sources, url)
	if err := writeProxyURLState(state); err != nil {
		release()
		shmLogFatal(73, "could not write proxy_url.json: %v", err)
	}
	release()

	fmt.Printf("added source: %s\nfetching now...\n", url)
	// maxTotal=0 here: the cap configured for the running provide() process
	// (--proxy_url_max) applies to its own background fetcher, not to this
	// one-shot CLI fetch. The next scheduled fetch will resume honoring it.
	// M6: probe the chosen network's API, not a hardcoded main-network host.
	probeHost, probePort := resolveAPIProbeHostPort()
	fetchAndMergeProxyURLs(context.Background(), []string{url}, 0, probeHost, probePort)
	fmt.Println("done.")
}

func proxyRemoveSource(opts docopt.Opts) {
	url, _ := opts.String("<url>")
	url = strings.TrimSpace(url)

	release, err := acquireProxyLock()
	if err != nil {
		shmLogFatal(75, "could not acquire proxy lock: %v", err)
	}
	defer release()

	state, err := readProxyURLState()
	if err != nil {
		shmLogFatal(76, "could not read proxy_url.json: %v", err)
	}

	kept := make([]string, 0, len(state.Sources))
	found := false
	for _, existing := range state.Sources {
		if existing == url {
			found = true
			continue
		}
		kept = append(kept, existing)
	}
	if !found {
		fmt.Printf("source not found: %s\n", url)
		return
	}

	state.Sources = kept
	if err := writeProxyURLState(state); err != nil {
		shmLogFatal(74, "could not write proxy_url.json: %v", err)
	}
	fmt.Printf("removed source: %s\n", url)
	fmt.Println("note: previously fetched proxies from this source remain running; use 'proxy remove-dead' to prune any that go dead.")
}

func proxyActivity() {
	shmLog := os.Getenv("URNETWORK_SHM_LOG")
	if shmLog == "" {
		shmLog = "/dev/shm/urnetwork.log"
	}

	statePath, err := proxyStatePath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: could not determine state path: %v\n", err)
		return
	}

	_ = statePath
	_ = shmLog

	fmt.Println("Proxy Activity Monitor")
	fmt.Println("Press Ctrl+C to exit")
	fmt.Println()

	// Use terminal directly for live updates
	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		// Non-interactive: just take one snapshot
		activitySnapshot(shmLog)
		return
	}
	defer term.Restore(int(os.Stdin.Fd()), oldState)

	reader := bufio.NewReader(os.Stdin)
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	done := make(chan struct{})
	go func() {
		for {
			b, _ := reader.ReadByte()
			if b == 'q' || b == 0x03 {
				close(done)
				return
			}
		}
	}()

	fmt.Print("\033[?25l")       // hide cursor
	defer fmt.Print("\033[?25h") // show cursor

	// Scroll window tracking recent contract events
	const maxEvents = 20
	var recentContracts []string
	var contractMu sync.Mutex

	// Goroutine to tail the log for contract events
	go func() {
		f, err := os.Open(shmLog)
		if err != nil {
			return
		}
		defer f.Close()

		// Seek to end
		_, _ = f.Seek(0, io.SeekEnd)

		buf := make([]byte, 4096)
		for {
			select {
			case <-done:
				return
			default:
			}
			n, err := f.Read(buf)
			if err != nil || n == 0 {
				time.Sleep(200 * time.Millisecond)
				continue
			}
			lines := strings.Split(string(buf[:n]), "\n")
			for _, line := range lines {
				if strings.Contains(line, "[contract] acquired") || strings.Contains(line, "[contract] denied") {
					// Extract timestamp and message
					if idx := strings.Index(line, "[contract]"); idx >= 0 {
						ts := ""
						if len(line) > 19 {
							ts = strings.TrimSpace(line[:19])
						}
						contractMu.Lock()
						recentContracts = append(recentContracts, fmt.Sprintf("%s %s", ts, line[idx:]))
						if len(recentContracts) > maxEvents {
							recentContracts = recentContracts[1:]
						}
						contractMu.Unlock()
					}
				}
			}
		}
	}()

	for {
		select {
		case <-done:
			return
		case <-ticker.C:
		}

		// Read health state for up/down counts
		up, connecting, degraded := 0, 0, 0
		healthDir, ok := proxyHealthDir()
		if ok {
			if data, err := os.ReadFile(filepath.Join(healthDir, "proxy_health.state")); err == nil {
				for _, line := range strings.Split(string(data), "\n") {
					if strings.HasPrefix(line, " Up:") {
						var down, dead int
						fmt.Sscanf(line, " Up: %d | Down: %d | Dead: %d | Degraded: %d", &up, &down, &dead, &degraded)
					}
				}
			}
		}

		// Parse traffic state for active proxies
		type activeProxy struct {
			id      string
			addr    string
			rx      string
			tx      string
			clients int
			age     string
			bill    string
		}
		var active []activeProxy
		if ok {
			if data, err := os.ReadFile(filepath.Join(healthDir, "proxy_traffic.state")); err == nil {
				lines := strings.Split(string(data), "\n")
				for _, line := range lines {
					if !strings.Contains(line, "proxy[") || strings.Contains(line, "PROXY ID") {
						continue
					}
					// Parse: | proxy[42] | 1.2.3.4:1080 | 2 | 5m | 10 MB / 20 MB | 100 MB / 200 MB |
					parts := strings.Split(line, "|")
					if len(parts) >= 6 {
						id := strings.TrimSpace(parts[1])
						addr := strings.TrimSpace(parts[2])
						clientsStr := strings.TrimSpace(parts[3])
						age := strings.TrimSpace(parts[4])
						bill := strings.TrimSpace(parts[5])
						total := strings.TrimSpace(parts[6])

						var cl int
						fmt.Sscanf(clientsStr, "%d", &cl)
						if cl > 0 {
							active = append(active, activeProxy{
								id: id, addr: addr, clients: cl,
								age: age, bill: bill, rx: total, tx: total,
							})
						}
					}
				}
			}
		}

		// Build output
		var out strings.Builder

		// Header
		out.WriteString("\033[H\033[J") // clear screen
		now := time.Now()
		out.WriteString(fmt.Sprintf("Proxy Activity — %s (refreshing every 1s)\n", now.Format(time.RFC3339)))
		out.WriteString(fmt.Sprintf("Up: %d | Degraded: %d | Connecting: %d\n", up, degraded, connecting))
		out.WriteString(fmt.Sprintf("Active proxies with clients: %d\n", len(active)))
		out.WriteString("\n")

		// Active proxies table
		if len(active) > 0 {
			out.WriteString(fmt.Sprintf("%-18s %-22s %9s %9s %5s  %s\n", "PROXY", "ADDRESS", "RX", "TX", "CLI", "AGE"))
			out.WriteString(strings.Repeat("-", 70) + "\n")
			for _, p := range active {
				out.WriteString(fmt.Sprintf("%-18s %-22s %9s %9s %5d  %s\n",
					p.id, p.addr, p.rx, p.tx, p.clients, p.age))
			}
		} else {
			out.WriteString("No proxies with active clients.\n")
		}

		// Recent contracts
		contractMu.Lock()
		if len(recentContracts) > 0 {
			out.WriteString(fmt.Sprintf("\nRecent Contracts:\n"))
			start := len(recentContracts) - 10
			if start < 0 {
				start = 0
			}
			for _, c := range recentContracts[start:] {
				out.WriteString("  " + c + "\n")
			}
		}
		contractMu.Unlock()

		out.WriteString("\n[q] quit\n")
		fmt.Print(out.String())
	}
}

func activitySnapshot(shmLog string) {
	// Single snapshot for non-interactive use
	healthDir, ok := proxyHealthDir()
	if !ok {
		return
	}

	up, degraded := 0, 0
	if data, err := os.ReadFile(filepath.Join(healthDir, "proxy_health.state")); err == nil {
		var down, dead int
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, " Up:") {
				fmt.Sscanf(line, " Up: %d | Down: %d | Dead: %d | Degraded: %d", &up, &down, &dead, &degraded)
			}
		}
	}
	fmt.Printf("Up: %d | Degraded: %d\n", up, degraded)

	if data, err := os.ReadFile(filepath.Join(healthDir, "proxy_traffic.state")); err == nil {
		fmt.Println(string(data))
	}
}

func proxySummary() {
	state, _ := readProxyState()

	up, dead, degraded, connecting := 0, 0, 0, 0
	if healthDir, ok := proxyHealthDir(); ok {
		if data, err := os.ReadFile(filepath.Join(healthDir, "proxy_health.state")); err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				if strings.HasPrefix(line, " Up:") {
					var down int
					fmt.Sscanf(line, " Up: %d | Down: %d | Dead: %d | Degraded: %d", &up, &down, &dead, &degraded)
				}
			}
		}
	}
	fileCount := 0
	urlCount := 0
	internalCount := 0
	total := 0
	if state != nil {
		total = len(state.Proxies)
		for _, e := range state.Proxies {
			switch e.Source {
			case "url":
				urlCount++
			case "file":
				fileCount++
			case "internal":
				internalCount++
			default:
				if state.Source != "" {
					fileCount++
				} else {
					internalCount++
				}
			}
		}
		connecting = total - up - dead - degraded
		if connecting < 0 {
			connecting = 0
		}
	}

	urlState, _ := readProxyURLState()
	urlSources := 0
	urlCached := 0
	urlBlacklisted := 0
	if urlState != nil {
		urlSources = len(urlState.Sources)
		urlCached = len(urlState.Cache)
		urlBlacklisted = len(urlState.Blacklist)
	}

	healthDir, _ := proxyHealthDir()

	fmt.Println("=========================================================================")
	fmt.Println(" PROXY SUMMARY")
	fmt.Printf(" Updated: %s\n", time.Now().UTC().Format(time.RFC3339))
	fmt.Println("=========================================================================")
	fmt.Println()
	fmt.Printf("  Total proxies:      %d\n", total)
	fmt.Printf("  Up:                 %d\n", up)
	fmt.Printf("  Connecting:         %d\n", connecting)
	fmt.Printf("  Degraded:           %d\n", degraded)
	fmt.Printf("  Dead:               %d\n", dead)
	fmt.Println()
	fmt.Println(" --- Sources ---")
	fileSource := "(internal)"
	if state != nil && state.Source != "" {
		fileSource = state.Source
	}
	fmt.Printf("  File proxies:       %d  (%s)\n", fileCount, fileSource)
	fmt.Printf("  URL proxies:        %d\n", urlCount)
	fmt.Printf("  Internal proxies:   %d\n", internalCount)
	fmt.Println()
	fmt.Println(" --- URL Sources ---")
	fmt.Printf("  Source URLs:        %d\n", urlSources)
	fmt.Printf("  Cached addresses:   %d\n", urlCached)
	fmt.Printf("  Blacklisted:        %d\n", urlBlacklisted)
	if len(urlState.ExcludePatterns) > 0 {
		fmt.Printf("  Exclude patterns:   %s\n", strings.Join(urlState.ExcludePatterns, ", "))
	}
	if urlSources > 0 {
		fmt.Println()
		for _, s := range urlState.Sources {
			fmt.Printf("    %s\n", s)
		}
	}
	fmt.Println()
	if state != nil {
		fmt.Printf("  Provider started:   %s\n", state.StartedAt.Format(time.RFC3339))
	}
	if state != nil {
		if p, err := proxyStatePath(); err == nil {
			fmt.Printf("  Proxy state file:   %s\n", p)
		}
	}
	fmt.Printf("  Health state:       %s/proxy_health.state\n", healthDir)
	if p, err := proxyURLStatePath(); err == nil {
		fmt.Printf("  URL state:          %s\n", p)
	}

	totalAcquired, totalDenied := globalContractMetrics.totals()
	a15, d15 := globalContractMetrics.windowTotals(15 * time.Minute)
	a60, d60 := globalContractMetrics.windowTotals(60 * time.Minute)
	a1440, d1440 := globalContractMetrics.windowTotals(1440 * time.Minute)

	fmt.Println()
	fmt.Println(" --- Contract Stats ---")
	cTotal := totalAcquired + totalDenied
	winRate := 0.0
	if cTotal > 0 {
		winRate = float64(totalAcquired) / float64(cTotal) * 100
	}
	fmt.Printf("  Acquired:           %d\n", totalAcquired)
	fmt.Printf("  Denied:             %d\n", totalDenied)
	fmt.Printf("  Win rate:           %.1f%%\n", winRate)
	fmt.Printf("  15m:  %d acquired / %d denied\n", a15, d15)
	fmt.Printf("  1h:   %d acquired / %d denied\n", a60, d60)
	fmt.Printf("  24h:  %d acquired / %d denied\n", a1440, d1440)
	fmt.Println("=========================================================================")
}

// removedProxy is one proxy the remove-dead command selected as a removal
// candidate, carrying its address and state entry.
type removedProxy struct {
	addr  string
	entry ProxyEntry
}

// removeDeadOptions captures the parsed remove-dead flags that drive which
// proxies are selected for removal: the source filter ("" = all), the degraded
// offline threshold (0 = disabled), and the minimum auth-failures/day (0 =
// disabled) that qualifies a proxy as auth-failing.
type removeDeadOptions struct {
	sourceFilter string
	degradedDur  time.Duration
	authFailMin  int64
}

// collectRemoveDeadCandidates is the pure decision core of the `proxy
// remove-dead` command: given the provider state, the parsed options, and the
// provider uptime, it returns the proxies to be removed, split into categories
// (dead / inactive / degraded / authFailing). It is extracted from
// proxyRemoveDead so this removal-selection logic — historically the site of
// the most regressions — can be unit-tested without docopt, interactive
// prompts, or a live provider that has been running past the 65-minute uptime
// gate (a proxy is only "confirmed dead" after the health system has settled;
// tests feed a state with a backdated StartedAt + explicit Health verdicts).
func collectRemoveDeadCandidates(state *ProxyState, o removeDeadOptions, uptime time.Duration) (dead, inactive, degraded, authFailing []removedProxy) {
	for addr, e := range state.Proxies {
		// Resolve the effective source for untagged entries (pre-source-tagging).
		effectiveSource := e.Source
		if effectiveSource == "" {
			if state.Source != "" {
				effectiveSource = "file"
			} else {
				effectiveSource = "internal"
			}
		}
		if o.sourceFilter != "" && effectiveSource != o.sourceFilter {
			continue
		}

		switch e.Health {
		case "dead":
			dead = append(dead, removedProxy{addr: addr, entry: e})
		case "inactive":
			inactive = append(inactive, removedProxy{addr: addr, entry: e})
		case "recently_offline", "offline", "long_offline":
			// Degraded removal requires an explicit --degraded threshold:
			// without it (degradedDur==0) offline proxies are NOT collected
			// here (matches runProxyURLCleanupOnce), so a plain `proxy
			// remove-dead` does not silently prune every offline proxy.
			// This block uses `break` (exits the switch only), NOT `continue`
			// (which inside a switch targets the ENCLOSING for loop and would
			// skip the auth-fail check below). Deliberate: offline proxies
			// remain auth-fail eligible regardless of degraded status/age —
			// auth failures are an independent, actionable signal and
			// dropping it would hide removals.
			if o.degradedDur > 0 {
				ds, err := time.Parse(time.RFC3339, e.DownSince)
				if err != nil || time.Since(ds) < o.degradedDur {
					break
				}
				degraded = append(degraded, removedProxy{addr: addr, entry: e})
			}
		}
		// A proxy can land in BOTH a category (dead/inactive/degraded) AND
		// authFailing — a deliberate, pre-existing overlap. At removal
		// removeDeadProxies de-dupes by source bucket and each per-source
		// removal is idempotent, so the overlap has no removal-correctness
		// impact (it only double-prints in the confirmation prompt).
		if o.authFailMin > 0 && e.Health != "up" {
			days := int64(max(1, int(uptime.Hours())/24))
			if e.AuthFailures >= o.authFailMin*days {
				authFailing = append(authFailing, removedProxy{addr: addr, entry: e})
			}
		}
	}
	return dead, inactive, degraded, authFailing
}

func proxyRemoveDead(opts docopt.Opts) {
	state, err := readProxyState()
	if err != nil || state.StartedAt.IsZero() {
		shmLogFatal(60, "provider does not appear to be running")
	}

	uptime := time.Since(state.StartedAt)
	const deadConfirmDelay = 65 * time.Minute
	if uptime < deadConfirmDelay {
		shmLogFatal(61, "provider has only been running %s — need %s uptime before dead status is confirmed", formatDuration(uptime), formatDuration(deadConfirmDelay))
	}

	// Parse options
	autoYes, _ := opts.Bool("--yes")
	preview, _ := opts.Bool("--preview")

	degradedDur := time.Duration(0)
	degradedFlag, _ := opts.Bool("--degraded")
	degVal, _ := opts.String("--degraded")
	if degradedFlag || degVal != "" {
		if degVal == "" || degVal == "true" {
			degradedDur = 24 * time.Hour // default: remove degraded > 24h
		} else {
			if d, err := time.ParseDuration(degVal); err == nil {
				degradedDur = d
			} else {
				fmt.Printf("invalid duration %q for --degraded (e.g. --degraded=24h)\n", degVal)
				return
			}
		}
	}

	var sourceFilter string
	if s, _ := opts.String("--source"); s != "" {
		if s != "url" && s != "file" && s != "internal" {
			fmt.Printf("invalid source %q (use 'url', 'file', or 'internal')\n", s)
			return
		}
		sourceFilter = s
	}

	authFailMin := int64(0)
	if af, _ := opts.Int("--auth-failures"); af > 0 {
		authFailMin = int64(af)
	} else if degradedDur > 0 {
		authFailMin = 250
	}

	// Collect candidates by category. The pure decision core lives in
	// collectRemoveDeadCandidates so the removal-selection logic (the area
	// that has seen the most regressions) is unit-testable.
	dead, inactive, degraded, authFailing := collectRemoveDeadCandidates(state, removeDeadOptions{
		sourceFilter: sourceFilter,
		degradedDur:  degradedDur,
		authFailMin:  authFailMin,
	}, uptime)

	if len(dead) == 0 && len(inactive) == 0 && len(degraded) == 0 && len(authFailing) == 0 {
		fmt.Println("Nothing to remove.")
		return
	}

	printCategory := func(label string, items []removedProxy) {
		if len(items) == 0 {
			return
		}
		sourceStr := ""
		if sourceFilter != "" {
			sourceStr = fmt.Sprintf(" [source=%s]", sourceFilter)
		}
		fmt.Printf("  %d %s%s:\n", len(items), label, sourceStr)
		for _, rp := range items {
			ts := ""
			if rp.entry.DownSince != "" {
				if t, err := time.Parse(time.RFC3339, rp.entry.DownSince); err == nil {
					ts = fmt.Sprintf(" down_since=%s", formatDuration(time.Since(t).Truncate(time.Second)))
				}
			}
			af := ""
			if rp.entry.AuthFailures > 0 {
				af = fmt.Sprintf(" auth_errors=%d", rp.entry.AuthFailures)
			}
			fmt.Printf("    proxy[%d]  %s%s%s\n", rp.entry.ID, rp.addr, ts, af)
		}
		fmt.Println()
	}

	if preview {
		fmt.Println("=== PREVIEW (no changes will be made) ===")
		printCategory("dead", dead)
		printCategory("inactive", inactive)
		printCategory(fmt.Sprintf("degraded (offline > %s)", formatDuration(degradedDur)), degraded)
		if authFailMin > 0 {
			printCategory(fmt.Sprintf("auth-failing (>= %d/day)", authFailMin), authFailing)
		}
		total := len(dead) + len(inactive) + len(degraded) + len(authFailing)
		fmt.Printf("Would remove %d proxies total.\n", total)
		return
	}

	var toRemove []removedProxy

	if len(dead) > 0 {
		printCategory("dead", dead)
		if autoYes || confirm(fmt.Sprintf("Remove %d dead proxies?", len(dead))) {
			toRemove = append(toRemove, dead...)
		}
	}

	if len(inactive) > 0 {
		printCategory("inactive", inactive)
		if autoYes || confirm(fmt.Sprintf("Remove %d inactive proxies?", len(inactive))) {
			toRemove = append(toRemove, inactive...)
		}
	}

	if len(degraded) > 0 {
		printCategory(fmt.Sprintf("degraded (offline > %s)", formatDuration(degradedDur)), degraded)
		if autoYes || confirm(fmt.Sprintf("Remove %d degraded proxies?", len(degraded))) {
			toRemove = append(toRemove, degraded...)
		}
	}

	if len(authFailing) > 0 {
		printCategory(fmt.Sprintf("auth-failing (>= %d/day)", authFailMin), authFailing)
		if autoYes || confirm(fmt.Sprintf("Remove %d auth-failing proxies?", len(authFailing))) {
			toRemove = append(toRemove, authFailing...)
		}
	}

	if len(toRemove) == 0 {
		fmt.Println("Nothing to remove.")
		return
	}

	addrsBySource := map[string][]string{}
	for _, rp := range toRemove {
		source := rp.entry.Source
		if source == "" {
			if state.Source != "" {
				source = "file"
			} else {
				source = "internal"
			}
		}
		addrsBySource[source] = append(addrsBySource[source], rp.addr)
	}

	if err := removeDeadProxies(state, addrsBySource); err != nil {
		shmLogFatal(62, "%v", err)
	}

	fmt.Printf("Removed %d proxies. Reload triggered.\n", len(toRemove))
}

func removeAddressesFromFile(path string, addresses []string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	removeSet := map[string]bool{}
	for _, a := range addresses {
		removeSet[a] = true
	}
	var kept []string
	for _, line := range strings.Split(string(b), "\n") {
		trimmed := strings.TrimSpace(line)
		addr, _, _ := parseProxyAddress(trimmed)
		if !removeSet[addr] {
			kept = append(kept, line)
		}
	}
	content := strings.Join(kept, "\n")
	if len(b) > 0 && b[len(b)-1] == '\n' && (len(content) == 0 || content[len(content)-1] != '\n') {
		content += "\n"
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func confirm(prompt string) bool {
	fmt.Printf("%s [y/N] ", prompt)
	scanner := bufio.NewScanner(os.Stdin)
	if scanner.Scan() {
		resp := scanner.Text()
		return strings.ToLower(strings.TrimSpace(resp)) == "y"
	}
	return false
}

func formatDuration(d time.Duration) string {
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if h == 0 {
		return fmt.Sprintf("%dm", m)
	}
	return fmt.Sprintf("%dh %dm", h, m)
}

func classifyHealth(e ProxyEntry) string {
	if e.Health != "" {
		return e.Health
	}
	return "starting"
}

func readProxyConfig() *ProxyConfig {
	home, err := os.UserHomeDir()
	if err != nil {
		tlog("[proxy] Error: could not find user home directory: %v\n", err)
		return &ProxyConfig{}
	}
	urNetworkDir := filepath.Join(home, ".urnetwork")
	proxyPath := filepath.Join(urNetworkDir, "proxy")

	if _, err := os.Stat(proxyPath); errors.Is(err, os.ErrNotExist) {
		return &ProxyConfig{}
	}

	b, err := os.ReadFile(proxyPath)
	if err != nil {
		tlog("[proxy] Error: could not read proxy config at %s: %v\n", proxyPath, err)
		return &ProxyConfig{}
	}

	var proxyConfig ProxyConfig
	err = json.Unmarshal(b, &proxyConfig)
	if err != nil {
		tlog("[proxy] Error: could not parse proxy config at %s: %v\n", proxyPath, err)
		return &ProxyConfig{}
	}
	return &proxyConfig
}

func writeProxyConfig(proxyConfig *ProxyConfig) {
	home, err := os.UserHomeDir()
	if err != nil {
		// No HOME: nowhere to persist the proxy config. Log and skip, matching
		// readProxyConfig's graceful handling (the old panic killed one-shot
		// commands in bare environments; shakedown finding 2026-08-15).
		tlog("[proxy] Error: could not find user home directory: %v\n", err)
		return
	}
	urNetworkDir := filepath.Join(home, ".urnetwork")
	proxyPath := filepath.Join(urNetworkDir, "proxy")

	if _, err := os.Stat(urNetworkDir); os.IsNotExist(err) {
		err = os.MkdirAll(urNetworkDir, 0700)
		if err != nil {
			// Same graceful handling as the HOME path above: never panic on
			// a one-shot command.
			tlog("[proxy] Error: could not create %s: %v\n", urNetworkDir, err)
			return
		}
	}

	b, err := json.Marshal(proxyConfig)
	if err != nil {
		tlog("[proxy] Error: could not marshal proxy config: %v\n", err)
		return
	}

	err = atomicWriteFile(proxyPath, b, 0700)
	if err != nil {
		tlog("[proxy] Error: could not write proxy config at %s: %v\n", proxyPath, err)
		return
	}

	// Automatically trigger a hot-reload so running providers pick up the changes
	if reloadPath, err := proxyReloadPath(); err == nil {
		if err := writeReloadTrigger(reloadPath); err != nil {
			tlog("[proxy] warn: failed to signal proxy reload after warmup (write .reload): %v\n", err)
		}
	}
}
