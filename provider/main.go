package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	mathrand "math/rand"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
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

var webhookClient = &http.Client{Timeout: 5 * time.Second}

// this value is set via the linker, e.g.
// -ldflags "-X main.Version=$WARP_VERSION-$WARP_VERSION_CODE"
var Version string

func init() {
	// debug.SetGCPercent(10)

	initGlog()

	// initPprof()
}

func initGlog() {
	flag.Set("logtostderr", "true")
	flag.Set("stderrthreshold", "INFO")
	flag.Set("v", "0")
	// unlike unix, the android/ios standard is for diagnostics to go to stdout
	os.Stderr = os.Stdout

	profile := os.Getenv("URNETWORK_PROFILE")
	ramlogs := os.Getenv("URNETWORK_RAMLOGS") == "1"
	if profile == "lowmem" || profile == "eco" || ramlogs {
		// If explicitly requested via profile or env, just start it.
		// Auto-detection handover is handled in main() with a countdown.
		initSHMLogger()
	}
}

func initSHMLoggerWithHandover() {
	fmt.Printf("\n[audit] Slow disk detected. Moving all subsequent logs to RAM (/dev/shm) for performance.\n")
	fmt.Printf("[audit] >>> To view live logs, run: urnet-tools logs <<<\n")
	fmt.Printf("[audit] Redirecting in 3...")
	time.Sleep(1 * time.Second)
	fmt.Printf(" 2...")
	time.Sleep(1 * time.Second)
	fmt.Printf(" 1...\n")
	time.Sleep(1 * time.Second)
	initSHMLogger()
}

func RunStartupAudit() (slowDisk bool, lowSpace bool) {
	fmt.Printf("[audit] Running system checks...\n")
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

	// WebRTC per-peer DataChannel buffer
	clientSettings.WebRtcSettings.ReceiveBufferSize = connect.ByteCount(windowSize) * 2

	// Faster contract ramp: reach StandardContractTransferByteCount in 2 contracts instead of 4
	clientSettings.ContractManagerSettings.ContractTransferByteSeqScale = 2

	// Let the heap breathe; no GOMEMLIMIT on RAM-rich boxes
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
	connect.ResizeMessagePools(poolBytes)
	fmt.Printf("[pool] message pool %dMiB (RAM=%dMiB)\n", poolBytes/1024/1024, connect.ByteCount(ram)/1024/1024)
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

type ecoState int

const (
	ecoStateNormal   ecoState = iota
	ecoStatePressure
	ecoStateCritical
)

// runEcoMemoryMonitor watches system memory availability and dynamically tightens
// GC pressure when RAM is low. This complements the static GOMEMLIMIT ceiling set
// at startup by responding to actual OS memory conditions at runtime.
// The eco memory monitor is a single global watcher. It can be requested from
// multiple places (the top-level eco profile check and the per-proxy provide
// loop), so startEcoMonitorOnce guards it to a single instance per process.
// startEcoMonitor is a test seam.
var (
	ecoMonitorStarted atomic.Bool
	startEcoMonitor   = func(ctx context.Context) { go runEcoMemoryMonitor(ctx) }
)

func startEcoMonitorOnce(ctx context.Context) {
	if ecoMonitorStarted.CompareAndSwap(false, true) {
		startEcoMonitor(ctx)
	}
}

func runEcoMemoryMonitor(ctx context.Context) {
	const (
		criticalMiB int64 = 150
		pressureMiB int64 = 300
		recoveryMiB int64 = 450

		gcNormal   = 50
		gcPressure = 25
		gcCritical = 10
	)

	state := ecoStateNormal

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			avail := readMemAvailableMiB()
			if avail < 0 {
				continue
			}
			// Inside a Docker container, /proc/meminfo reflects host RAM.
			// Take the tighter of host available and cgroup headroom.
			if cgroupAvail := readCgroupAvailableMiB(); cgroupAvail >= 0 && cgroupAvail < avail {
				avail = cgroupAvail
			}

			var next ecoState
			switch {
			case avail <= criticalMiB:
				next = ecoStateCritical
			case avail <= pressureMiB:
				next = ecoStatePressure
			case avail >= recoveryMiB:
				next = ecoStateNormal
			default:
				// hysteresis zone (300-450 MiB): hold current state
				if state == ecoStateCritical {
					runtime.GC()
				}
				continue
			}

			if next == state {
				if state == ecoStateCritical {
					runtime.GC()
				}
				continue
			}

			state = next
			switch state {
			case ecoStateNormal:
				debug.SetGCPercent(gcNormal)
				fmt.Printf("[eco] memory pressure eased (available=%dMiB), GOGC=%d\n", avail, gcNormal)
			case ecoStatePressure:
				debug.SetGCPercent(gcPressure)
				fmt.Printf("[eco] memory pressure detected (available=%dMiB), GOGC=%d\n", avail, gcPressure)
			case ecoStateCritical:
				debug.SetGCPercent(gcCritical)
				runtime.GC()
				fmt.Printf("[eco] memory critical (available=%dMiB), GOGC=%d\n", avail, gcCritical)
			}
		}
	}
}

func main() {
	profile := os.Getenv("URNETWORK_PROFILE")
	ramlogs := os.Getenv("URNETWORK_RAMLOGS")

	// If in auto mode and RAM logs aren't already explicitly on, we audit the disk speed
	// BEFORE initializing the logger. This allows us to auto-enable it.
	autoRamLogTriggered := false
	if profile == "auto" {
		manualRamLogs := (ramlogs == "1")
		slowDisk, _ := RunStartupAudit()
		if slowDisk && !manualRamLogs {
			fmt.Printf("[audit] Disk speed is suboptimal. Auto-enabling RAM logs for performance.\n")
			os.Setenv("URNETWORK_RAMLOGS", "1")
			autoRamLogTriggered = true
		}
	} else if len(os.Args) > 1 && (os.Args[1] == "provide" || os.Args[1] == "auth-provide") {
		// Even if not in auto, run audit for visibility
		RunStartupAudit()
	}

	initGlog()

	// If auto-tuner enabled RAM logs, perform the countdown handover now
	if autoRamLogTriggered {
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
        [--max-memory=<mem>]
        [--proxy_file=<proxy_file>]
        [-v...]
    provider auth-provide ([<auth_code>] | --user_auth=<user_auth> [--password=<password>]) [-f]
    	[--port=<port>]
        [--api_url=<api_url>]
        [--connect_url=<connect_url>]
        [--max-memory=<mem>]
        [-v...]
    provider proxy auth add [<key>] <proxy_user> <proxy_password> [-f]
    provider proxy auth remove [<key>] [--all]
    provider proxy add [<key_address>...] [--proxy_file=<proxy_file>] [-f]
    provider proxy remove [<key_address>...] [--all]
    provider proxy refresh [--force]
    provider logs [-n <lines>]

Options:
    -h --help                        Show this help and exit.
    --version                        Show version.
    -v...                            Enable verbose mode. -v implies verbose level 1,
    				                 -vv implies level 2... etc.
    -f                               Force overwrite the JWT token store file or proxy value, if exists.
                                     By default, existing values will not be overwritten.
    --api_url=<api_url>              Specify a custom API URL to use.
    --connect_url=<connect_url>      Specify a custom connect URL to use.
    --user_auth=<user_auth>	         Login with a username.
    --password=<password>            Login with a password. If --user_auth is used, you will be prompted for your
    				                 password anyways, if you don't specify it using this option.
    -p --port=<port>                 Status server port [default: 0].
    --max-memory=<mem>               Set the maximum amount of memory in bytes, or the suffixes b, kib, mib, gib may be used [This is a soft limit].
    <key>                            Authentication key
    <proxy_user>                     SOCKS5 user
    <proxy_password>                 SOCKS5 password
    <key_address>                    SOCKS5 server as host:port, host:port:user:pass, host:port::, or key@host:port
    --proxy_file=<proxy_file>        A path to a file where each line contains on entry as host:port, host:port:user:pass, host:port::, or key@host:port
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
		} else if add, _ := opts.Bool("add"); add {
			proxyAdd(opts)
		} else if remove, _ := opts.Bool("remove"); remove {
			proxyRemove(opts)
		} else if refresh, _ := opts.Bool("refresh"); refresh {
			proxyRefresh(opts)
		}
	} else if auth_, _ := opts.Bool("auth"); auth_ {
		auth(opts)
	} else if provide_, _ := opts.Bool("provide"); provide_ {
		provide(opts)
	} else if authProvide, _ := opts.Bool("auth-provide"); authProvide {
		auth(opts)
		provide(opts)
	} else if logs, _ := opts.Bool("logs"); logs {
		providerLogs(opts)
	}
}

func auth(opts docopt.Opts) {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: could not determine home directory: %v\n", err)
		os.Exit(1)
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

	apiUrl, err := opts.String("--api_url")
	if err != nil {
		apiUrl = DefaultApiUrl
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
		connect.ResizeMessagePools(maxMemory / 8)
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
			os.Exit(0)
		case loginResult = <-loginChannel:
		}

		if loginResult.Error != nil {
			fmt.Fprintf(os.Stderr, "Error: authentication request failed: %v\n", loginResult.Error)
			os.Exit(1)
		}
		if loginResult.Result.Error != nil {
			fmt.Fprintf(os.Stderr, "Error: authentication failed: %s\n", loginResult.Result.Error.Message)
			os.Exit(1)
		}
		if loginResult.Result.VerificationRequired != nil {
			fmt.Fprintf(os.Stderr, "Error: verification required for %s — complete account setup via the app or web first.\n", loginResult.Result.VerificationRequired.UserAuth)
			os.Exit(1)
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
			os.Exit(0)
		case authCodeLoginResult = <-authCodeLoginChannel:
		}

		if authCodeLoginResult.Error != nil {
			fmt.Fprintf(os.Stderr, "Error: authentication request failed: %v\n", authCodeLoginResult.Error)
			os.Exit(1)
		}
		if authCodeLoginResult.Result.Error != nil {
			fmt.Fprintf(os.Stderr, "Error: authentication failed: %s\n", authCodeLoginResult.Result.Error.Message)
			fmt.Fprintf(os.Stderr, "Hint: auth codes are single-use. If this container was restarted, mount a persistent volume at /root/.urnetwork so the JWT survives restarts.\n")
			os.Exit(1)
		}

		byJwt = authCodeLoginResult.Result.ByJwt
	}

	if byJwt != "" {
		if err := os.MkdirAll(urNetworkDir, 0700); err != nil {
			fmt.Fprintf(os.Stderr, "Error: could not create %s: %v\n", urNetworkDir, err)
			os.Exit(1)
		}
		os.WriteFile(jwtPath, []byte(byJwt), 0700)
		fmt.Printf("Jwt written to %s\n", jwtPath)
	}
}

// runOutageWatcher polls IsBackendDegraded every 30 seconds and logs a line on
// state transitions. If URNETWORK_ALERT_WEBHOOK is set it also POSTs a JSON
// payload so operators can receive push notifications.
// "Start" requires startConfirm consecutive degraded polls (5 minutes at the
// 30s poll interval) before firing, so a brief blip never raises a false alarm —
// the backend must fail continuously with zero successful connects or OOB calls
// for the whole window. "Clear" requires two consecutive healthy polls to avoid
// premature all-clears during brief lulls mid-outage. A 5-minute per-event
// cooldown prevents webhook spam if the backend flickers at a boundary.
func runOutageWatcher(ctx context.Context, nodeName, webhookURL string) {
	const pollInterval = 30 * time.Second
	const cooldown = 5 * time.Minute
	const clearConfirm = 2
	const startConfirm = 10 // 10 * 30s = 5 minutes of continuous degradation

	degraded := false
	degradedCount := 0
	clearCount := 0
	var lastStartFire, lastClearFire time.Time

	if webhookURL != "" {
		fmt.Printf("[outage] watcher active node=%s webhook=configured\n", nodeName)
	} else {
		fmt.Printf("[outage] watcher active node=%s\n", nodeName)
	}

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		if connect.IsBackendDegraded() {
			clearCount = 0
			if !degraded {
				degradedCount++
				if degradedCount >= startConfirm {
					degraded = true
					fmt.Printf("[outage] backend degraded — holding existing connections, not accepting new ones\n")
					if webhookURL != "" && time.Since(lastStartFire) >= cooldown {
						lastStartFire = time.Now()
						go fireWebhook(webhookURL, nodeName, "outage_start",
							"Backend unreachable — provider holding existing connections but not accepting new ones.")
					}
				}
			}
		} else {
			degradedCount = 0
			if degraded {
				clearCount++
				if clearCount >= clearConfirm {
					degraded = false
					clearCount = 0
					fmt.Printf("[outage] backend recovered\n")
					if webhookURL != "" && time.Since(lastClearFire) >= cooldown {
						lastClearFire = time.Now()
						go fireWebhook(webhookURL, nodeName, "outage_clear", "Backend connectivity restored.")
					}
				}
			}
		}
	}
}

func fireWebhook(url, nodeName, event, message string) {
	// Format the body per service. Discord requires "content" and Slack requires
	// "text"; a generic {event,node,...} body is rejected by both (HTTP 400). Any
	// other endpoint (ntfy, custom) gets the structured JSON it can parse.
	var payload []byte
	var err error
	switch {
	case strings.Contains(url, "discord.com"), strings.Contains(url, "discordapp.com"):
		line := fmt.Sprintf("URnetwork [%s] node=%s: %s", event, nodeName, message)
		payload, err = json.Marshal(map[string]string{"content": line})
	case strings.Contains(url, "hooks.slack.com"):
		line := fmt.Sprintf("URnetwork [%s] node=%s: %s", event, nodeName, message)
		payload, err = json.Marshal(map[string]string{"text": line})
	default:
		payload, err = json.Marshal(map[string]string{
			"event":     event,
			"node":      nodeName,
			"timestamp": time.Now().UTC().Format(time.RFC3339),
			"message":   message,
		})
	}
	if err != nil {
		fmt.Printf("[webhook] marshal failed: %v\n", err)
		return
	}
	resp, err := webhookClient.Post(url, "application/json", bytes.NewReader(payload))
	if err != nil {
		fmt.Printf("[webhook] delivery failed (%s): %v\n", event, err)
		return
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		fmt.Printf("[webhook] non-2xx response (%s): %d\n", event, resp.StatusCode)
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
		fmt.Printf("[health] warning: metric %q has unreadable kind %v — check metric name\n", name, v.Kind())
		return 0
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
		fmt.Printf("[health] uptime=%s profile=%s heap=%dMiB sys=%dMiB connections=%d proxies=%d\n",
			uptime, profile, heapMiB, sysMiB, connect.ActiveConnectionCount(), connect.ActiveProxyConnections())

		if connect.ProxyHealthCount() == 0 {
			continue // non-proxy mode: no [health][proxies] lines
		}

		now := time.Now()
		report := connect.ProxyHealthHeartbeat(uptime >= deadConfirmDelay)
		down := len(report.Dead) + len(report.Degraded)
		fmt.Printf("[health][proxies] up=%d down=%d dead=%d degraded=%d recovered=%d lost=%d lifetime_recovered=%d lifetime_lost=%d\n",
			report.Up, down, len(report.Dead), len(report.Degraded),
			len(report.Recovered), len(report.NewlyDegraded),
			report.LifetimeRecovered, report.LifetimeLost)
		if len(report.Dead) > 0 {
			fmt.Printf("[health][proxies] dead: %s\n", capProxyList(report.Dead, proxyHealthListCap))
		}
		if len(report.Degraded) > 0 {
			fmt.Printf("[health][proxies] degraded: %s\n", capProxyList(report.Degraded, proxyHealthListCap))
		}

		if dir, ok := proxyHealthDir(); ok {
			writeProxyHealthState(dir, report, now)
			writeProxyHealthEvents(dir, report, now)
			writeProxyTrafficState(dir, report, now)
		}
	}
}

func provide(opts docopt.Opts) {
	port, _ := opts.Int("--port")

	apiUrl, err := opts.String("--api_url")
	if err != nil {
		apiUrl = DefaultApiUrl
	}

	connectUrl, err := opts.String("--connect_url")
	if err != nil {
		connectUrl = DefaultConnectUrl
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
		connect.ResizeMessagePools(maxMemory / 8)
		debug.SetMemoryLimit(maxMemory)
	}
	applyPoolAutoSize(maxMemory)

	provideStartTime := time.Now()

	event := connect.NewEventWithContext(context.Background())
	event.SetOnSignals(syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM)

	ctx, cancel := context.WithCancel(event.Ctx())
	defer cancel()

	// Hourly pulse: wakes all stalled transports and proxies so they retry
	// connections without needing a provider restart.
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(1 * time.Hour):
				if connect.ProxyHealthCount() > 0 {
					up, dead, degraded, _ := connect.ProxyHealthSnapshot()
					down := len(dead) + len(degraded)
					_ = up
					fmt.Printf("[pulse] waking stalled transports: down=%d dead=%d degraded=%d\n",
						down, len(dead), len(degraded))
				}
				connect.TriggerPulse()
			}
		}
	}()

	if os.Getenv("URNETWORK_PROFILE") == "eco" {
		startEcoMonitorOnce(ctx)
	}

	nodeName := strings.TrimSpace(os.Getenv("URNETWORK_NODE_NAME"))
	
	// Determine a temporary display name for the outage watcher/heartbeat
	watcherName := nodeName
	if watcherName == "" {
		watcherName, _ = os.Hostname()
		if containerIDRe.MatchString(watcherName) {
			watcherName = "provider"
		}
	}

	go runOutageWatcher(ctx, watcherName, os.Getenv("URNETWORK_ALERT_WEBHOOK"))
	go runHealthHeartbeat(ctx, provideStartTime, os.Getenv("URNETWORK_PROFILE"))

	provideWithProxy := func(proxyCtx context.Context, proxySettings *connect.ProxySettings) {
		clientStrategySettings := connect.DefaultClientStrategySettings()
		clientStrategySettings.ProxySettings = proxySettings
		clientSettings := connect.DefaultClientSettings()
		localUserNatSettings := connect.DefaultLocalUserNatSettings()

		autoEco := connect.ApplyAutoTuning(clientSettings, localUserNatSettings)
		applyLowmodeSettings(clientSettings, localUserNatSettings)
		applyTurboSettings(clientSettings, localUserNatSettings)

		profile := os.Getenv("URNETWORK_PROFILE")
		if profile == "eco" || autoEco {
			startEcoMonitorOnce(ctx)
		}

		applyEcoSettings(maxMemory)
		localUserNatSettings.TcpBufferSettings.ConnectSettings = clientStrategySettings.ConnectSettings
		localUserNatSettings.UdpBufferSettings.ConnectSettings = clientStrategySettings.ConnectSettings
		remoteUserNatProviderSettings := connect.DefaultRemoteUserNatProviderSettings()

		clientStrategy := connect.NewClientStrategy(proxyCtx, clientStrategySettings)

		byClientJwt, clientId, err := func() (string, connect.Id, error) {
			// Consecutive failures where the JWT file exists but the API rejects it
			// (expired, revoked, or bad token). After maxAuthFailures the binary
			// exits so the shell restart loop can delete the JWT and re-authenticate.
			// "Jwt does not exist" is a configuration issue, not a bad token — it
			// retries indefinitely until the user runs 'urnetwork auth'.
			const maxAuthFailures = 10
			authFailures := 0
			for {
				byClientJwt, clientId, err := provideAuth(proxyCtx, clientStrategy, apiUrl, opts, nodeName)
				if err == nil {
					return byClientJwt, clientId, nil
				}

				if strings.Contains(err.Error(), "Jwt does not exist") {
					authFailures = 0
					fmt.Printf("Authentication missing. Please run 'urnetwork auth' to configure your provider.\n")
					retryDelay := 30 * time.Second
					select {
					case <-proxyCtx.Done():
						return "", connect.Id{}, proxyCtx.Err()
					case <-time.After(retryDelay):
						continue
					}
				}

				authFailures++
				if authFailures >= maxAuthFailures {
					return "", connect.Id{}, fmt.Errorf("authentication failed after %d attempts, JWT may be expired or revoked: %w", maxAuthFailures, err)
				}

				retryDelay := time.Duration(500+mathrand.Intn(10000)) * time.Millisecond
				fmt.Printf("init proxy auth failed: %v. Will retry in %.2fs\n", err, float64(retryDelay/time.Millisecond)/1000.0)
				select {
				case <-proxyCtx.Done():
					return "", connect.Id{}, proxyCtx.Err()
				case <-time.After(retryDelay):
				}
			}
		}()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}

		instanceId := connect.NewId()

		clientOob := connect.NewApiOutOfBandControl(proxyCtx, clientStrategy, byClientJwt, apiUrl)
		connectClient := connect.NewClient(proxyCtx, clientId, clientOob, clientSettings)
		defer connectClient.Close()

		// routeManager := connect.NewRouteManager(connectClient)
		// contractManager := connect.NewContractManagerWithDefaults(connectClient)
		// connectClient.Setup(routeManager, contractManager)
		// go connectClient.Run()

		fmt.Printf("client_id: %s\n", clientId)
		fmt.Printf("instance_id: %s\n", instanceId)

		auth := &connect.ClientAuth{
			ByJwt: byClientJwt,
			// ClientId: clientId,
			InstanceId: instanceId,
			AppVersion: RequireVersion(),
		}
		connect.NewPlatformTransportWithDefaults(proxyCtx, clientStrategy, connectClient.RouteManager(), connectUrl, auth)
		// go platformTransport.Run(connectClient.RouteManager())
		var bw *connect.ProxyBandwidth
		if proxySettings != nil {
			bw = connect.RegisterProxyBandwidth(proxySettings.Index)
		}

		localUserNat := connect.NewLocalUserNat(proxyCtx, clientId.String(), bw, localUserNatSettings)
		defer localUserNat.Close()
		remoteUserNatProvider := connect.NewRemoteUserNatProvider(connectClient, localUserNat, bw, remoteUserNatProviderSettings)
		defer remoteUserNatProvider.Close()

		provideModes := map[protocol.ProvideMode]bool{
			protocol.ProvideMode_Public:  true,
			protocol.ProvideMode_Network: true,
		}
		connectClient.ContractManager().SetProvideModes(provideModes)

		select {
		case <-proxyCtx.Done():
		}
	}

	var wg sync.WaitGroup

	if profile := os.Getenv("URNETWORK_PROFILE"); profile == "turbo-v4" || profile == "turbo-v8" {
		var windowMiB, queueMiB uint32
		switch profile {
		case "turbo-v4":
			windowMiB, queueMiB = 4, 8
		case "turbo-v8":
			windowMiB, queueMiB = 8, 16
		}
		fmt.Printf("[turbo] profile=%s window=%dMiB resendQueue=%dMiB\n", profile, windowMiB, queueMiB)
	}

	// Load proxy.state to assign address-stable IDs. Known addresses keep their ID
	// across restarts/reloads; new addresses get the next monotonic counter value.
	proxyState, stateErr := readProxyState()
	if stateErr != nil {
		fmt.Printf("[proxy] warning: could not read proxy.state: %v\n", stateErr)
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
	var allProxySettings []*connect.ProxySettings
	if proxyFile != "" {
		settings, err := readProxySettingsFromFile(proxyFile)
		if err != nil {
			// Fatal: the operator explicitly requested an external proxy file.
			// Falling back to no-proxy mode would silently route traffic directly
			// and leak the operator's real IP.
			fmt.Printf("[proxy] fatal: %v\n", err)
			os.Exit(1)
		}
		if len(settings) == 0 {
			fmt.Printf("[proxy] fatal: proxy file %s contained no valid proxies (expected ip:port:user:pass per line)\n", proxyFile)
			os.Exit(1)
		}
		allProxySettings = settings
		proxyState.Source = proxyFile
	} else {
		allProxySettings = readProxySettings()
		proxyState.Source = ""
	}

	// Per-proxy cancellation map keyed by address, so hot-reload can cancel
	// individual proxy goroutines without disturbing the others.
	var proxyCancelMu sync.Mutex
	proxyCancelMap := map[string]context.CancelFunc{}

	if 0 < len(allProxySettings) {
		fmt.Printf("Using %d proxy servers:\n", len(allProxySettings))

		for _, proxySettings := range allProxySettings {
			stableID := resolveProxyID(proxyState, proxySettings.Address)
			proxySettings.Index = stableID
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

		// Persist the initial state snapshot now that all IDs are resolved.
		proxyState.NextID = currentProxyIDCounter()
		if err := writeProxyState(proxyState); err != nil {
			fmt.Printf("[proxy] warning: could not write proxy.state: %v\n", err)
		}

		for i, proxySettings := range allProxySettings {
			proxyCtx, proxyCancel := context.WithCancel(ctx)
			proxyCancelMu.Lock()
			proxyCancelMap[proxySettings.Address] = proxyCancel
			proxyCancelMu.Unlock()

			stableID := proxySettings.Index
			staggerPos := i
			wg.Add(1)
			go connect.HandleError(func() {
				defer wg.Done()
				defer connect.UnregisterProxy(stableID)
				defer proxyCancel()

				initialDelay := time.Duration(staggerPos) * 100 * time.Millisecond
				select {
				case <-proxyCtx.Done():
				case <-time.After(initialDelay):
				}

				provideWithProxy(proxyCtx, proxySettings)
			})
		}
	} else {
		// No entry in proxyCancelMap — hot-reload only applies in proxy mode.
		wg.Add(1)
		proxyCtx, proxyCancel := context.WithCancel(ctx)
		go connect.HandleError(func() {
			defer wg.Done()
			defer proxyCancel()
			provideWithProxy(proxyCtx, nil)
		})
	}

	// Start the hot-reload watcher: it polls ~/.urnetwork/proxy.reload and applies
	// add/remove diffs to the running proxy set without restarting the provider.
	reloader := &ProxyReloader{
		cancelMap:   proxyCancelMap,
		cancelMapMu: &proxyCancelMu,
		state:       proxyState,
		sourcePath:  proxyFile,
		parentCtx:   ctx,
		wg:          &wg,
		spawnProxy:  provideWithProxy,
	}
	reloader.StartWatcher(ctx)

	if 0 < port {
		fmt.Printf(
			"Provider %s started. Status on *:%d\n",
			RequireVersion(),
			port,
		)
		statusServer := &http.Server{
			Addr:    fmt.Sprintf(":%d", port),
			Handler: &Status{},
		}
		defer statusServer.Shutdown(ctx)

		go connect.HandleError(func() {
			defer cancel()
			err := statusServer.ListenAndServe()
			if err != nil {
				fmt.Printf("status error: %s\n", err)
			}
		}, cancel)
	} else {
		fmt.Printf(
			"Provider %s started\n",
			RequireVersion(),
		)
	}

	wg.Wait()

	// exit
	os.Exit(0)
}

// containerIDRe matches a default Docker container hostname (12-char hex),
// so we can omit it from the dashboard label when it carries no useful meaning.
var containerIDRe = regexp.MustCompile("^[0-9a-f]{12}$")

func provideAuth(ctx context.Context, clientStrategy *connect.ClientStrategy, apiUrl string, opts docopt.Opts, nodeName string) (byClientJwt string, clientId connect.Id, returnErr error) {
	home, err := os.UserHomeDir()
	if err != nil {
		panic(err)
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

	api := connect.NewBringYourApi(ctx, clientStrategy, apiUrl)

	api.SetByJwt(byJwt)

	authClientCallback, authClientChannel := connect.NewBlockingApiCallback[*connect.AuthNetworkClientResult](ctx)

	// 1. Determine Display Name
	displayName := nodeName
	hostname, _ := os.Hostname()

	// 2. Allow override via HOST_HOSTNAME (for Docker users passing host $(hostname))
	if displayName == "" {
		if hostHostname := strings.TrimSpace(os.Getenv("HOST_HOSTNAME")); hostHostname != "" {
			displayName = hostHostname
		} else {
			displayName = hostname
		}
	}

	// 3. Filter Gibberish (12-char hex container IDs)
	isContainerID := containerIDRe.MatchString(displayName)
	publicIP := strings.TrimSpace(os.Getenv("URNETWORK_PUBLIC_IP"))

	// 4. Build Compact Dashboard Label
	var dashboardLabel string
	
	if ip4 := net.ParseIP(publicIP).To4(); ip4 != nil {
		parts := strings.Split(ip4.String(), ".")
		redactedIP := fmt.Sprintf("%s.x.x.%s", parts[0], parts[3])

		if displayName == "" || isContainerID {
			// Scenario: No useful name. Identity is just the Redacted IP.
			dashboardLabel = redactedIP
		} else {
			// Scenario: We have a useful name. Identity is "Name @ RedactedIP".
			dashboardLabel = fmt.Sprintf("%s @ %s", displayName, redactedIP)
		}
	} else {
		// Fallback for no IP connectivity
		if displayName == "" || isContainerID {
			dashboardLabel = "provider"
		} else {
			dashboardLabel = displayName
		}
	}

	// 5. Final Description: "Identity [Version]"
	description := fmt.Sprintf("%s [%s]", dashboardLabel, RequireVersion())

	authClientArgs := &connect.AuthNetworkClientArgs{
		Description: description,
		DeviceSpec:  "",
	}

	api.AuthNetworkClient(authClientArgs, authClientCallback)

	var authClientResult connect.ApiCallbackResult[*connect.AuthNetworkClientResult]
	select {
	case <-ctx.Done():
		os.Exit(0)
	case authClientResult = <-authClientChannel:
	}

	if authClientResult.Error != nil {
		panic(authClientResult.Error)
	}
	if authClientResult.Result.Error != nil {
		panic(fmt.Errorf("%s", authClientResult.Result.Error.Message))
	}

	byClientJwt = authClientResult.Result.ByClientJwt

	// parse the clientId
	parser := gojwt.NewParser()
	token, _, err := parser.ParseUnverified(byClientJwt, gojwt.MapClaims{})
	if err != nil {
		panic(err)
	}

	claims := token.Claims.(gojwt.MapClaims)

	clientId, err = connect.ParseId(claims["client_id"].(string))
	if err != nil {
		panic(err)
	}

	return
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
	if version := os.Getenv("WARP_VERSION"); version != "" {
		return version
	}
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
	proxyConfig := readProxyConfig()

	if all, _ := opts.Bool("--all"); all {
		clear(proxyConfig.Servers)
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
			fmt.Printf("[proxy] error: proxy %q missing credentials — required format ip:port:user:pass; skipping\n", line)
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
		fmt.Printf("error: no ramlogs found at %s — is URNETWORK_RAMLOGS=1 set?\n", shmLogPath)
		os.Exit(1)
	}
	fmt.Print(out)

	// Tail: follow the file from current position
	f, err := os.Open(shmLogPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: could not open log for tailing: %v\n", err)
		return
	}
	defer f.Close()
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		fmt.Fprintf(os.Stderr, "error: seek failed: %v\n", err)
		return
	}

	buf := make([]byte, 4096)
	for {
		nr, err := f.Read(buf)
		if nr > 0 {
			os.Stdout.Write(buf[:nr])
		}
		if err != nil && err != io.EOF {
			fmt.Fprintf(os.Stderr, "error: read failed: %v\n", err)
			return
		}
		if err == io.EOF {
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

func proxyRefresh(opts docopt.Opts) {
	force, _ := opts.Bool("--force")

	state, err := readProxyState()
	if err != nil {
		fmt.Printf("error: could not read proxy.state — is the provider running?\n")
		fmt.Printf("  To edit the proxy list for next startup, use: provider proxy add/remove\n")
		os.Exit(1)
	}

	// Check if provider is running by seeing if state is fresh
	if state.StartedAt.IsZero() {
		fmt.Printf("error: provider does not appear to be running (no proxy.state found).\n")
		fmt.Printf("  To edit the proxy list for next startup, use: provider proxy add/remove\n")
		os.Exit(1)
	}

	uptime := time.Since(state.StartedAt)

	// Warmup protection
	const warmupThreshold = 8 * time.Hour
	if uptime < warmupThreshold && !force {
		fmt.Printf("Provider has been running for %s. Proxies need 8-12h to warm up —\n", formatDuration(uptime))
		fmt.Printf("a proxy that looks dead now may still be establishing contracts.\n")
		fmt.Printf("Run again after 8h uptime, or use --force to override.\n")
		os.Exit(1)
	}

	// Load desired (from source file or internal config)
	var desired []*connect.ProxySettings
	if state.Source != "" {
		settings, err := readProxySettingsFromFile(state.Source)
		if err != nil {
			fmt.Printf("error: could not read proxy file %s: %v\n", state.Source, err)
			os.Exit(1)
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

	var removed []ProxyEntry
	for addr, e := range currentSet {
		if !desiredSet[addr] {
			e.Health = classifyHealth(e)
			removed = append(removed, e)
		}
	}

	if len(added) == 0 && len(removed) == 0 {
		fmt.Println("proxy list is already up to date. Nothing to do.")
		return
	}

	// Warn if all proxies would be removed
	if len(removed) == len(currentSet) && len(added) == 0 {
		fmt.Printf("WARNING: This will remove ALL proxies — unique IPs drive earning scale;\n")
		fmt.Printf("removing all proxies significantly reduces capacity.\n\n")
	}

	// Print diff
	fmt.Printf("proxy refresh: %d proxies will be removed, %d will be added.\n\n", len(removed), len(added))
	if len(removed) > 0 {
		fmt.Println("  Removing:")
		for _, e := range removed {
			fmt.Printf("    proxy[%d]  %s   — %s\n", e.ID, e.DownSince, e.Health)
		}
	}
	if len(added) > 0 {
		fmt.Println("\n  Adding:")
		for _, addr := range added {
			fmt.Printf("    %s\n", addr)
		}
	}

	// Check for high-risk removals
	highRisk := false
	for _, e := range removed {
		if e.Health == "up" || e.Health == "recently_offline" {
			highRisk = true
			break
		}
	}

	if highRisk {
		fmt.Printf("\nWARNING: One or more proxies being removed are currently online or recently online.\n")
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

	// Acquire lock and write trigger
	release, err := acquireProxyLock()
	if err != nil {
		fmt.Printf("error: %v\n", err)
		os.Exit(1)
	}
	defer release()

	reloadPath, err := proxyReloadPath()
	if err != nil {
		fmt.Printf("error: could not determine reload path: %v\n", err)
		os.Exit(1)
	}

	if err := writeReloadTrigger(reloadPath); err != nil {
		fmt.Printf("error: could not write reload trigger: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("Reload triggered. Provider will apply changes within 2 seconds.")
}

func confirm(prompt string) bool {
	fmt.Printf("%s [y/N] ", prompt)
	var resp string
	fmt.Scanln(&resp)
	return strings.ToLower(strings.TrimSpace(resp)) == "y"
}

func formatDuration(d time.Duration) string {
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	return fmt.Sprintf("%dh %dm", h, m)
}

func classifyHealth(e ProxyEntry) string {
	if e.Health != "" {
		return e.Health
	}
	return "unknown"
}

func readProxyConfig() *ProxyConfig {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Printf("[proxy] Error: could not find user home directory: %v\n", err)
		return &ProxyConfig{}
	}
	urNetworkDir := filepath.Join(home, ".urnetwork")
	proxyPath := filepath.Join(urNetworkDir, "proxy")

	if _, err := os.Stat(proxyPath); errors.Is(err, os.ErrNotExist) {
		return &ProxyConfig{}
	}

	b, err := os.ReadFile(proxyPath)
	if err != nil {
		fmt.Printf("[proxy] Error: could not read proxy config at %s: %v\n", proxyPath, err)
		return &ProxyConfig{}
	}

	var proxyConfig ProxyConfig
	err = json.Unmarshal(b, &proxyConfig)
	if err != nil {
		fmt.Printf("[proxy] Error: could not parse proxy config at %s: %v\n", proxyPath, err)
		return &ProxyConfig{}
	}
	return &proxyConfig
}

func writeProxyConfig(proxyConfig *ProxyConfig) {
	home, err := os.UserHomeDir()
	if err != nil {
		panic(err)
	}
	urNetworkDir := filepath.Join(home, ".urnetwork")
	proxyPath := filepath.Join(urNetworkDir, "proxy")

	if _, err := os.Stat(urNetworkDir); os.IsNotExist(err) {
		err = os.MkdirAll(urNetworkDir, 0700)
		if err != nil {
			panic(err)
		}
	}

	b, err := json.Marshal(proxyConfig)
	if err != nil {
		panic(err)
	}

	err = os.WriteFile(proxyPath, b, 0700)
	if err != nil {
		panic(err)
	}
}
