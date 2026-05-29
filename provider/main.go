package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	// "net"
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
    --proxy_file=<proxy_file>        A path to a file where each line contains on entry as host:port, host:port:user:pass, host:port::, or key@host:port`,
		DefaultApiUrl,
		DefaultConnectUrl,
	)

	opts, err := docopt.ParseArgs(usage, os.Args[1:], RequireVersion())

	if err != nil {
		panic(err)
	}

	// Support auth code via environment variable for Docker/dash-prefixed tokens
	if envAuthCode := os.Getenv("URNETWORK_AUTH_CODE"); envAuthCode != "" {
		opts["<auth_code>"] = envAuthCode
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
		}
	} else if auth_, _ := opts.Bool("auth"); auth_ {
		auth(opts)
	} else if provide_, _ := opts.Bool("provide"); provide_ {
		provide(opts)
	} else if authProvide, _ := opts.Bool("auth-provide"); authProvide {
		auth(opts)
		provide(opts)
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
		fmt.Printf("[health] uptime=%s profile=%s heap=%dMiB sys=%dMiB\n",
			uptime, profile, heapMiB, sysMiB)
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
				connect.TriggerPulse()
			}
		}
	}()

	if os.Getenv("URNETWORK_PROFILE") == "eco" {
		go runEcoMemoryMonitor(ctx)
	}

	nodeName := strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' {
			return -1
		}
		return r
	}, os.Getenv("URNETWORK_NODE_NAME"))
	if nodeName == "" {
		hostname, _ := os.Hostname()
		if _, err := os.Stat("/.dockerenv"); err == nil {
			nodeName = hostname + " (docker)"
		} else {
			nodeName = hostname + " (binary)"
		}
	}
	go runOutageWatcher(ctx, nodeName, os.Getenv("URNETWORK_ALERT_WEBHOOK"))
	go runHealthHeartbeat(ctx, provideStartTime, os.Getenv("URNETWORK_PROFILE"))

	provideWithProxy := func(proxySettings *connect.ProxySettings) {
		proxyCtx, proxyCancel := context.WithCancel(ctx)
		defer proxyCancel()

		clientStrategySettings := connect.DefaultClientStrategySettings()
		clientStrategySettings.ProxySettings = proxySettings
		clientSettings := connect.DefaultClientSettings()
		localUserNatSettings := connect.DefaultLocalUserNatSettings()

		autoEco := connect.ApplyAutoTuning(clientSettings, localUserNatSettings)
		applyLowmodeSettings(clientSettings, localUserNatSettings)
		applyTurboSettings(clientSettings, localUserNatSettings)

		profile := os.Getenv("URNETWORK_PROFILE")
		if profile == "eco" || autoEco {
			go runEcoMemoryMonitor(ctx)
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

		localUserNat := connect.NewLocalUserNat(proxyCtx, clientId.String(), localUserNatSettings)
		defer localUserNat.Close()
		remoteUserNatProvider := connect.NewRemoteUserNatProvider(connectClient, localUserNat, remoteUserNatProviderSettings)
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

	if allProxySettings := readProxySettings(); 0 < len(allProxySettings) {
		fmt.Printf("Using %d proxy servers:\n", len(allProxySettings))

		for i, proxySettings := range allProxySettings {
			var user string
			var password string
			if proxySettings.Auth != nil {
				user = proxySettings.Auth.User
				password = proxySettings.Auth.Password
			}
			fmt.Printf("  proxy[%d] %s (%s/%s)\n",
				i,
				proxySettings.Address,
				obfuscateUser(user),
				obfuscatePassword(password),
			)
		}
		for i, proxySettings := range allProxySettings {
			wg.Add(1)
			go connect.HandleError(func() {
				defer wg.Done()

				initialDelay := time.Duration(i) * 100 * time.Millisecond
				select {
				case <-ctx.Done():
				case <-time.After(initialDelay):
				}

				provideWithProxy(proxySettings)
			})
		}
	} else {
		wg.Add(1)
		go connect.HandleError(func() {
			defer wg.Done()
			provideWithProxy(nil)
		})
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

	hostname, _ := os.Hostname()
	displayName := nodeName
	if displayName == "" {
		displayName = hostname
	}

	// Detect if hostname is a container ID (12-character hex)
	isContainerID, _ := regexp.MatchString("^[0-9a-f]{12}$", hostname)

	// Build a compact label. If hostname is just a container ID, ignore it to save space.
	label := displayName
	if nodeName != "" && hostname != "" && nodeName != hostname && !isContainerID {
		label = fmt.Sprintf("%s (%s)", nodeName, hostname)
	}

	description := fmt.Sprintf("%s [%s]", label, RequireVersion())

	if publicIP := os.Getenv("URNETWORK_PUBLIC_IP"); publicIP != "" {
		// Redact the IP for privacy (keep first and last octets)
		parts := strings.Split(publicIP, ".")
		if len(parts) == 4 {
			redactedIP := fmt.Sprintf("%s.x.x.%s", parts[0], parts[3])
			description = fmt.Sprintf("%s @ %s [%s]", label, redactedIP, RequireVersion())
		}
	}

	authClientArgs := &connect.AuthNetworkClientArgs{
		Description: description,
		DeviceSpec:  "",
	}

	fmt.Printf("[INFO] Reporting to dashboard as: %s\n", description)

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
