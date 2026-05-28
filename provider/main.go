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
	// initGlog is now called explicitly in main to allow audit-based tuning
}

func initGlog() {
	flag.Set("logtostderr", "true")
	flag.Set("stderrthreshold", "INFO")
	flag.Set("v", "0")
	// unlike unix, the android/ios standard is for diagnostics to go to stdout
	os.Stderr = os.Stdout

	profile := os.Getenv("URNETWORK_PROFILE")
	if profile == "lowmem" || profile == "eco" || os.Getenv("URNETWORK_RAMLOGS") == "1" {
		initSHMLogger()
	}
}

func RunStartupAudit() (slowDisk bool, lowSpace bool) {
	fmt.Printf("[audit] Running system checks...\n")
	return connect.RunSystemAudit()
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

	// Transfer-layer send/receive queues — must scale with window or they become the bottleneck
	clientSettings.SendBufferSettings.ResendQueueMaxByteCount = queueBytes
	clientSettings.ReceiveBufferSettings.ReceiveQueueMaxByteCount = queueBytes

	// Sequence buffer depths (IP layer and Transfer layer)
	localUserNatSettings.SequenceBufferSize = 512
	localUserNatSettings.TcpBufferSettings.SequenceBufferSize = 512
	localUserNatSettings.UdpBufferSettings.SequenceBufferSize = 512
	clientSettings.StreamManagerSettings.SequenceBufferSize = 64

	// WebRTC DataChannel buffer (approx 2x TCP window)
	clientSettings.WebRtcSettings.ReceiveBufferSize = connect.ByteCount(windowSize * 2)

	// Contract ramp — accelerate scaling: 4 -> 2 (reach full speed in 2 contracts)
	clientSettings.ContractManagerSettings.ContractTransferByteSeqScale = 2

	// GOGC/Memory Tuning — Turbo users usually have plenty of RAM
	debug.SetGCPercent(200)
	debug.SetMemoryLimit(-1)
}

func applyPoolAutoSize() {
	if os.Getenv("URNETWORK_PROFILE") == "lowmem" {
		return
	}
	ram := connect.DetectEffectiveRAMLimitBytes()
	poolBytes := connect.ByteCount(ram) / 32
	const floor = 8 * 1024 * 1024
	const ceiling = 256 * 1024 * 1024
	if poolBytes < floor {
		poolBytes = floor
	}
	if poolBytes > ceiling {
		poolBytes = ceiling
	}

	fmt.Printf("[pool] message pool %dMiB (RAM=%dMiB)\n",
		poolBytes/1024/1024, ram/1024/1024)

	connect.ResizeMessagePools(poolBytes)
}

func applyEcoSettings(maxMemory connect.ByteCount) {
	if os.Getenv("URNETWORK_PROFILE") != "eco" && os.Getenv("URNETWORK_PROFILE") != "lowmem" {
		return
	}

	// Only set GOMEMLIMIT if neither --max-memory nor the GOMEMLIMIT env var
	// were provided explicitly; those take precedence.
	if os.Getenv("GOMEMLIMIT") == "" && maxMemory == 0 {
		ramBytes := connect.DetectEffectiveRAMLimitBytes()
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

// readCgroupAvailableMiB calculates headroom available under a cgroup limit.
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

	// If in auto mode, we audit the disk speed BEFORE initializing the logger.
	// This allows us to automatically enable RAM logging on slow disks.
	if os.Getenv("URNETWORK_PROFILE") == "auto" {
		slowDisk, _ := RunStartupAudit()
		if slowDisk {
			fmt.Printf("[audit] Auto-enabling RAM logs due to slow disk I/O.\n")
			os.Setenv("URNETWORK_RAMLOGS", "1")
		}
	} else if os.Args[1] == "provide" || os.Args[1] == "auth-provide" {
		// Even if not in auto, run audit for visibility
		connect.RunSystemAudit()
	}

	initGlog()

	opts, err := docopt.ParseArgs(usage, os.Args[1:], RequireVersion())

	if err != nil {
		panic(err)
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
			confirm = strings.TrimSpace(confirm)
			if confirm != "y" && confirm != "Y" {
				os.Exit(0)
			}
		}
	}

	maxMemoryHumanReadable, err := opts.String("--max-memory")
	var maxMemory connect.ByteCount
	if err == nil {
		maxMemory, err = connect.ParseByteCount(maxMemoryHumanReadable)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: invalid max-memory value: %v\n", err)
			os.Exit(1)
		}
	}

	if maxMemory != 0 {
		connect.ResizeMessagePools(maxMemory / 8)
	}

	apiUrl, err := opts.String("--api_url")
	if err != nil {
		apiUrl = DefaultApiUrl
	}

	clientStrategySettings := connect.DefaultClientStrategySettings()
	clientStrategy := connect.NewClientStrategy(clientStrategySettings)

	var byJwt string
	if userAuth, _ := opts.String("--user_auth"); userAuth != "" {
		password, _ := opts.String("--password")
		if password == "" {
			fmt.Printf("Password for %s: ", userAuth)
			passwordBytes, err := term.ReadPassword(int(syscall.Stdin))
			if err != nil {
				panic(err)
			}
			password = string(passwordBytes)
			fmt.Printf("\n")
		}

		loginResult, err := clientStrategy.UserAuthLogin(
			context.Background(),
			apiUrl,
			userAuth,
			password,
		)
		if err != nil {
			panic(err)
		}

		if loginResult.Result.Error != nil {
			fmt.Fprintf(os.Stderr, "Error: %s\n", loginResult.Result.Error.Message)
			os.Exit(1)
		}

		if loginResult.Result.VerificationRequired != nil {
			fmt.Fprintf(os.Stderr, "Error: verification required for %s — complete account setup via the app or web first.\n", loginResult.Result.VerificationRequired.UserAuth)
			os.Exit(1)
		}

		byJwt = loginResult.Result.ByJwt
	} else {
		authCode, _ := opts.String("<auth_code>")

		if authCode == "" {
			fmt.Printf("Auth code (found at https://ur.io): ")

			reader := bufio.NewReader(os.Stdin)
			authCode, _ = reader.ReadString('\n')
			authCode = strings.TrimSpace(authCode)
		}

		authCodeLoginResult, err := clientStrategy.AuthCodeLogin(
			context.Background(),
			apiUrl,
			authCode,
		)
		if err != nil {
			panic(err)
		}

		if authCodeLoginResult.Result.Error != nil {
			fmt.Fprintf(os.Stderr, "Error: %s\n", authCodeLoginResult.Result.Error.Message)
			os.Exit(1)
		}

		byJwt = authCodeLoginResult.Result.ByJwt
	}

	if byJwt != "" {
		if err := os.MkdirAll(urNetworkDir, 0700); err != nil {
			panic(err)
		}
		os.WriteFile(jwtPath, []byte(byJwt), 0700)
		fmt.Printf("Jwt written to %s\n", jwtPath)
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
			fmt.Fprintf(os.Stderr, "Error: invalid max-memory value: %v\n", err)
			os.Exit(1)
		}
	}

	applyPoolAutoSize()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	nodeName := os.Getenv("URNETWORK_NODE_NAME")
	if nodeName == "" {
		hostname, _ := os.Hostname()
		if hostname == "" {
			hostname = "unknown"
		}
		suffix := "(binary)"
		if _, err := os.Stat("/.dockerenv"); err == nil {
			suffix = "(docker)"
		}
		nodeName = fmt.Sprintf("%s %s", hostname, suffix)
	}
	// Sanitize nodeName (remove newlines)
	nodeName = strings.ReplaceAll(nodeName, "\n", "")
	nodeName = strings.ReplaceAll(nodeName, "\r", "")

	webhookURL := os.Getenv("URNETWORK_ALERT_WEBHOOK")
	go runOutageWatcher(ctx, nodeName, webhookURL)

	startTime := time.Now()
	go runHealthHeartbeat(ctx, startTime, os.Getenv("URNETWORK_PROFILE"))

	provideWithProxy := func(proxySettings *connect.ProxySettings) {
		proxyCtx, cancel := context.WithCancel(ctx)
		defer cancel()

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

		clientStrategy := connect.NewClientStrategy(clientStrategySettings)
		byClientJwt, clientId, err := provideAuth(proxyCtx, clientStrategy, apiUrl, opts)
		if err != nil {
			fmt.Fprintf(os.Stderr, "auth error: %v\n", err)
			// return
			os.Exit(1)
		}

		connectClientSettings := connect.DefaultConnectClientSettings()
		connectClientSettings.ClientSettings = *clientSettings
		connectClientSettings.LocalUserNatSettings = *localUserNatSettings
		connectClientSettings.RemoteUserNatProviderSettings = *remoteUserNatProviderSettings

		connectClient := connect.NewClient(
			proxyCtx,
			connectClientSettings,
			clientStrategy,
			apiUrl,
			connectUrl,
			byClientJwt,
			clientId,
		)

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

func provideAuth(ctx context.Context, clientStrategy *connect.ClientStrategy, apiUrl string, opts docopt.Opts) (byClientJwt string, clientId connect.Id, returnErr error) {
	home, err := os.UserHomeDir()
	if err != nil {
		panic(err)
	}
	jwtPath := filepath.Join(home, ".urnetwork", "jwt")

	if _, err := os.Stat(jwtPath); errors.Is(err, os.ErrNotExist) {
		returnErr = fmt.Errorf("jwt not found at %s. Please run 'urnetwork auth' first.", jwtPath)
		return
	}

	byJwtBytes, err := os.ReadFile(jwtPath)
	if err != nil {
		returnErr = err
		return
	}
	byClientJwt = string(byJwtBytes)

	// verify jwt
	parser := gojwt.NewParser()
	token, _, err := parser.ParseUnverified(byClientJwt, gojwt.MapClaims{})
	if err != nil {
		returnErr = err
		return
	}

	if claims, ok := token.Claims.(gojwt.MapClaims); ok {
		if sub, ok := claims["sub"].(string); ok {
			clientId, err = connect.IdFromHexString(sub)
			if err != nil {
				returnErr = err
				return
			}
		} else {
			returnErr = errors.New("jwt missing sub claim")
			return
		}
	} else {
		returnErr = errors.New("invalid jwt claims")
		return
	}

	return
}

func readProxySettings() (allProxySettings []*connect.ProxySettings) {
	home, err := os.UserHomeDir()
	if err != nil {
		panic(err)
	}
	proxyDir := filepath.Join(home, ".urnetwork", "proxy")
	entries, err := os.ReadDir(proxyDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		panic(err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		proxyPath := filepath.Join(proxyDir, entry.Name())
		proxyBytes, err := os.ReadFile(proxyPath)
		if err != nil {
			panic(err)
		}

		var proxySettings connect.ProxySettings
		err = json.Unmarshal(proxyBytes, &proxySettings)
		if err != nil {
			panic(err)
		}

		allProxySettings = append(allProxySettings, &proxySettings)
	}

	return
}

func proxyAuthAdd(opts docopt.Opts) {
	home, err := os.UserHomeDir()
	if err != nil {
		panic(err)
	}
	proxyAuthDir := filepath.Join(home, ".urnetwork", "proxy_auth")
	if err := os.MkdirAll(proxyAuthDir, 0700); err != nil {
		panic(err)
	}

	key, _ := opts.String("<key>")
	user, _ := opts.String("<proxy_user>")
	password, _ := opts.String("<proxy_password>")

	proxyAuth := connect.ProxyAuth{
		User:     user,
		Password: password,
	}
	proxyAuthBytes, err := json.Marshal(proxyAuth)
	if err != nil {
		panic(err)
	}

	proxyAuthPath := filepath.Join(proxyAuthDir, key)
	if _, err := os.Stat(proxyAuthPath); !errors.Is(err, os.ErrNotExist) {
		if force, _ := opts.Bool("-f"); !force {
			fmt.Printf("%s exists. Overwrite? [yN]\n", proxyAuthPath)

			reader := bufio.NewReader(os.Stdin)
			confirm, _ := reader.ReadString('\n')
			confirm = strings.TrimSpace(confirm)
			if confirm != "y" && confirm != "Y" {
				os.Exit(0)
			}
		}
	}

	err = os.WriteFile(proxyAuthPath, proxyAuthBytes, 0700)
	if err != nil {
		panic(err)
	}
	fmt.Printf("Proxy auth written to %s\n", proxyAuthPath)
}

func proxyAuthRemove(opts docopt.Opts) {
	home, err := os.UserHomeDir()
	if err != nil {
		panic(err)
	}
	proxyAuthDir := filepath.Join(home, ".urnetwork", "proxy_auth")

	if all, _ := opts.Bool("--all"); all {
		err := os.RemoveAll(proxyAuthDir)
		if err != nil {
			panic(err)
		}
		fmt.Printf("All proxy auth removed\n")
		return
	}

	key, _ := opts.String("<key>")
	proxyAuthPath := filepath.Join(proxyAuthDir, key)
	err = os.Remove(proxyAuthPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			fmt.Printf("Proxy auth %s not found\n", key)
			return
		}
		panic(err)
	}
	fmt.Printf("Proxy auth %s removed\n", key)
}

func proxyAdd(opts docopt.Opts) {
	home, err := os.UserHomeDir()
	if err != nil {
		panic(err)
	}
	proxyDir := filepath.Join(home, ".urnetwork", "proxy")
	if err := os.MkdirAll(proxyDir, 0700); err != nil {
		panic(err)
	}

	var proxyAddresses []string
	if proxyFile, _ := opts.String("--proxy_file"); proxyFile != "" {
		f, err := os.Open(proxyFile)
		if err != nil {
			panic(err)
		}
		defer f.Close()

		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line != "" {
				proxyAddresses = append(proxyAddresses, line)
			}
		}
	} else {
		proxyAddresses, _ = opts.StringList("<key_address>")
	}

	for _, proxyAddress := range proxyAddresses {
		var key string
		var address string
		var user string
		var password string

		parts := strings.Split(proxyAddress, "@")
		if len(parts) == 2 {
			key = parts[0]
			proxyAddress = parts[1]
		}

		parts = strings.Split(proxyAddress, ":")
		if len(parts) >= 2 {
			address = strings.Join(parts[0:2], ":")
			if len(parts) >= 3 {
				user = parts[2]
			}
			if len(parts) >= 4 {
				password = parts[3]
			}
		} else {
			fmt.Fprintf(os.Stderr, "Error: invalid proxy address: %s\n", proxyAddress)
			continue
		}

		var auth *connect.ProxyAuth
		if user != "" || password != "" {
			auth = &connect.ProxyAuth{
				User:     user,
				Password: password,
			}
		} else {
			// check for stored auth
			proxyAuthDir := filepath.Join(home, ".urnetwork", "proxy_auth")
			proxyAuthPath := filepath.Join(proxyAuthDir, key)
			proxyAuthBytes, err := os.ReadFile(proxyAuthPath)
			if err == nil {
				var proxyAuth connect.ProxyAuth
				err = json.Unmarshal(proxyAuthBytes, &proxyAuth)
				if err == nil {
					auth = &proxyAuth
				}
			}
		}

		proxySettings := connect.ProxySettings{
			Address: address,
			Auth:    auth,
		}
		proxySettingsBytes, err := json.Marshal(proxySettings)
		if err != nil {
			panic(err)
		}

		proxyPath := filepath.Join(proxyDir, strings.ReplaceAll(address, ":", "_"))
		if _, err := os.Stat(proxyPath); !errors.Is(err, os.ErrNotExist) {
			if force, _ := opts.Bool("-f"); !force {
				fmt.Printf("%s exists. Overwrite? [yN]\n", proxyPath)

				reader := bufio.NewReader(os.Stdin)
				confirm, _ := reader.ReadString('\n')
				confirm = strings.TrimSpace(confirm)
				if confirm != "y" && confirm != "Y" {
					continue
				}
			}
		}

		err = os.WriteFile(proxyPath, proxySettingsBytes, 0700)
		if err != nil {
			panic(err)
		}
		fmt.Printf("Proxy %s added\n", address)
	}
}

func proxyRemove(opts docopt.Opts) {
	home, err := os.UserHomeDir()
	if err != nil {
		panic(err)
	}
	proxyDir := filepath.Join(home, ".urnetwork", "proxy")

	if all, _ := opts.Bool("--all"); all {
		err := os.RemoveAll(proxyDir)
		if err != nil {
			panic(err)
		}
		fmt.Printf("All proxies removed\n")
		return
	}

	proxyAddresses, _ := opts.StringList("<key_address>")
	for _, proxyAddress := range proxyAddresses {
		proxyPath := filepath.Join(proxyDir, strings.ReplaceAll(proxyAddress, ":", "_"))
		err = os.Remove(proxyPath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				fmt.Printf("Proxy %s not found\n", proxyAddress)
				continue
			}
			panic(err)
		}
		fmt.Printf("Proxy %s removed\n", proxyAddress)
	}
}

func initSHMLogger() {
	logPath := "/dev/shm/urnetwork.log"
	f, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		fmt.Printf("[shm] failed to open %s: %v; falling back to stdout\n", logPath, err)
		return
	}
	fmt.Printf("[shm] routing logs to %s (max 1MiB)\n", logPath)
	os.Stdout = f
	os.Stderr = f

	// Background worker to truncate log if it exceeds 1MiB
	go func() {
		ticker := time.NewTicker(1 * time.Minute)
		for range ticker.C {
			if stat, err := os.Stat(logPath); err == nil {
				if stat.Size() > 1024*1024 {
					os.Truncate(logPath, 0)
				}
			}
		}
	}()
}

func obfuscateUser(user string) string {
	if len(user) <= 4 {
		return "****"
	}
	return user[:2] + "****" + user[len(user)-2:]
}

func obfuscatePassword(pass string) string {
	return "****"
}

func RequireVersion() string {
	if Version == "" {
		return "v3.23.0-fix.14"
	}
	return Version
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

func runHealthHeartbeat(ctx context.Context, startTime time.Time, profile string) {
	if profile == "" {
		profile = "default"
	}

	intervalStr := os.Getenv("URNETWORK_HEALTH_INTERVAL")
	interval := 5 * time.Minute
	if intervalStr != "" {
		if d, err := time.ParseDuration(intervalStr); err == nil {
			if d >= 1*time.Minute {
				interval = d
			}
		}
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
