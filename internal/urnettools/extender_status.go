package urnettools

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// extender_status.go reads the provider's extender picture from its control
// socket and renders it for `urnet-tools status`, `urnet-tools top` and
// `urnet-tools extender status`. Nothing is inferred from files or logs: the
// provider answers, or the extender is reported as unavailable. Unavailable is
// never shown as "off", because a provider that cannot be reached tells you
// nothing about whether its extender is on.

const (
	SourceControlSocket = "control socket"
	SourceUnknown       = "unavailable"
)

// ExtenderPort is one carrier port.
type ExtenderPort struct {
	Name     string `json:"name"`
	Up       bool   `json:"up"`
	Error    string `json:"error,omitempty"`
	Disabled bool   `json:"disabled,omitempty"`
	// Reason says why a port that is not up is down: address_in_use,
	// permission_denied, unknown or not_requested. Providers that predate it
	// leave it empty.
	Reason string `json:"reason,omitempty"`
}

// extenderPortReasonText is the plain-words form of a port's Reason. The
// second result is true when the reason is no fault, so a renderer can show it
// as information and not as a warning.
func extenderPortReasonText(reason string) (text string, informational bool) {
	switch reason {
	case "":
		return "", false
	case "address_in_use":
		return "address already in use", false
	case "permission_denied":
		return "permission denied", false
	case "not_requested":
		return "not requested", true
	default:
		return "bind failed, cause unknown", false
	}
}

// ExtenderFamilyPort is one carrier's platform state on one address family.
type ExtenderFamilyPort struct {
	Name  string `json:"name"`
	State string `json:"state"`
}

// ExtenderFamily is one address family's activation state.
type ExtenderFamily struct {
	IpVersion         int                  `json:"ip_version"`
	State             string               `json:"state"`
	Ip                string               `json:"ip,omitempty"`
	Detail            string               `json:"detail,omitempty"`
	Ports             []ExtenderFamilyPort `json:"ports,omitempty"`
	LastAttemptUnixMs int64                `json:"last_attempt_unix_ms,omitempty"`
	NextAttemptUnixMs int64                `json:"next_attempt_unix_ms,omitempty"`
	ExpireUnixMs      int64                `json:"expire_unix_ms,omitempty"`
}

// ExtenderServed is requests served for clients over three windows.
type ExtenderServed struct {
	Last24h  uint64 `json:"last_24h"`
	Last7d   uint64 `json:"last_7d"`
	Lifetime uint64 `json:"lifetime"`
}

type ExtenderDialStats struct {
	Attempts uint64 `json:"attempts"`
	Timeout  uint64 `json:"timeout"`
	Refused  uint64 `json:"refused"`
	Canceled uint64 `json:"canceled"`
	Other    uint64 `json:"other"`
}

type ExtenderDiscovery struct {
	Requested             string             `json:"requested"`
	State                 string             `json:"state"`
	Failure               string             `json:"failure,omitempty"`
	PeerId                string             `json:"peer_id"`
	Peers                 int                `json:"peers"`
	FeedSubscribers       int                `json:"feed_subscribers"`
	ActiveRecords         *int               `json:"active_records,omitempty"`
	ConnectedPeers        *int               `json:"connected_peers,omitempty"`
	Connecting            *bool              `json:"connecting,omitempty"`
	DialStats             *ExtenderDialStats `json:"dial_stats,omitempty"`
	MessagesAccepted      *uint64            `json:"messages_accepted,omitempty"`
	ApplyErrors           *uint64            `json:"apply_errors,omitempty"`
	LastAcceptedUnixMilli int64              `json:"last_accepted_unix_milli,omitempty"`
}

// ExtenderCarrierStats holds cumulative served and refused request counts for one carrier.
type ExtenderCarrierStats struct {
	Carrier         string            `json:"carrier"`
	Served          uint64            `json:"served"`
	Refused         uint64            `json:"refused"`
	RefusalsByStage map[string]uint64 `json:"refusals_by_stage,omitempty"`
}

// ExtenderCarrierBytes holds the relayed traffic of one carrier: the bytes this
// extender relayed on it, the rate now, the connections open now and opened
// since start, and the last 30 one minute buckets of ingress plus egress as
// whole KiB.
type ExtenderCarrierBytes struct {
	Carrier        string   `json:"carrier"`
	IngressBytes   uint64   `json:"ingress_bytes"`
	EgressBytes    uint64   `json:"egress_bytes"`
	RateInBps      uint64   `json:"rate_in_bps,omitempty"`
	RateOutBps     uint64   `json:"rate_out_bps,omitempty"`
	LiveConns      uint64   `json:"live_conns"`
	ConnsTotal     uint64   `json:"conns_total"`
	Minutes        []uint32 `json:"minutes,omitempty"`
	IngressBytesV4 uint64   `json:"ingress_bytes_v4,omitempty"`
	IngressBytesV6 uint64   `json:"ingress_bytes_v6,omitempty"`
	EgressBytesV4  uint64   `json:"egress_bytes_v4,omitempty"`
	EgressBytesV6  uint64   `json:"egress_bytes_v6,omitempty"`
	LiveConnsV4    uint64   `json:"live_conns_v4,omitempty"`
	LiveConnsV6    uint64   `json:"live_conns_v6,omitempty"`
	ConnsTotalV4   uint64   `json:"conns_total_v4,omitempty"`
	ConnsTotalV6   uint64   `json:"conns_total_v6,omitempty"`
}

// ExtenderReading mirrors the provider's extenderControlReading.
type ExtenderReading struct {
	// Source is where the reading came from; set by this package.
	Source string `json:"-"`

	Known         bool             `json:"known"`
	Setting       string           `json:"setting"`
	SettingSource string           `json:"setting_source,omitempty"`
	Running       bool             `json:"running"`
	RoleState     string           `json:"role_state,omitempty"`
	OffReason     string           `json:"off_reason,omitempty"`
	Note          string           `json:"note,omitempty"`
	Activating    bool             `json:"activating"`
	Ports         []ExtenderPort   `json:"ports,omitempty"`
	DnsPorts      []int            `json:"dns_ports,omitempty"`
	TcpRetries    int64            `json:"tcp_retries,omitempty"`
	Families      []ExtenderFamily `json:"families,omitempty"`
	LiveClients   int              `json:"live_clients"`
	Served        ExtenderServed   `json:"served"`

	RelayBytesIn  int64 `json:"relay_bytes_in,omitempty"`
	RelayBytesOut int64 `json:"relay_bytes_out,omitempty"`

	RefusalsByStage map[string]uint64      `json:"refusals_by_stage,omitempty"`
	Carriers        []ExtenderCarrierStats `json:"carriers,omitempty"`
	CarrierBytes    []ExtenderCarrierBytes `json:"carrier_bytes,omitempty"`
	Discovery       *ExtenderDiscovery     `json:"discovery,omitempty"`

	BindRoute string `json:"bind_route,omitempty"`
}

var (
	portRoutePlatformFn = func() string { return runtime.GOOS }
	portRouteReadFileFn = os.ReadFile
	portRouteExecCmdFn  = func(name string, args ...string) ([]byte, error) {
		return exec.Command(name, args...).CombinedOutput()
	}
)

func udp53ThresholdStatus(platform string, readFile func(string) ([]byte, error)) string {
	if platform != "linux" {
		return "UDP 53 sysctl threshold: not applicable on this platform"
	}
	data, err := readFile(extenderSysctlPortStartPath)
	if err != nil {
		return "UDP 53 sysctl threshold: unknown"
	}
	value, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || value < 0 || value > 65535 {
		return "UDP 53 sysctl threshold: unknown"
	}
	if value <= 53 {
		return fmt.Sprintf("UDP 53 sysctl threshold: in force (live value %d)", value)
	}
	return fmt.Sprintf("UDP 53 sysctl threshold: not in force (live value %d)", value)
}

func detectBindRoute(binary, user string) string {
	return detectBindRouteWith(portRoutePlatformFn(), binary, user, portRouteReadFileFn, portRouteExecCmdFn)
}

func detectBindRouteWith(platform, binary, user string, readFile func(string) ([]byte, error), execCmd func(string, ...string) ([]byte, error)) string {
	if platform != "linux" {
		return ""
	}
	if user == "root" {
		return "root (no capability or sysctl needed)"
	}
	// 1. Sysctl
	if readFile != nil {
		if data, err := readFile("/proc/sys/net/ipv4/ip_unprivileged_port_start"); err == nil {
			if val, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && val >= 0 && val <= 443 {
				route := fmt.Sprintf("sysctl: port is available without a file capability (ip_unprivileged_port_start=%d)", val)
				if binary != "" && execCmd != nil {
					if out, err := execCmd("getcap", binary); err == nil && linuxHasBindCapability(binary, string(out)) {
						cmd := setupCommand{name: "sudo", args: []string{"setcap", "-r", binary}}.String()
						route += "; warning: binary still has a file capability, so a provider started with it is non-dumpable and may be unreadable to the updater. You must restart or hand off the provider after removing it. " + cmd
						if strings.TrimSpace(string(out)) != binary+" cap_net_bind_service=ep" {
							route += " removes ALL file capabilities; inspect getcap -n first and preserve other capabilities"
						}
					}
				}
				return route
			}
		}
	}
	// 2. Capability on binary
	if binary != "" && execCmd != nil {
		if out, err := execCmd("getcap", binary); err == nil {
			if linuxHasBindCapability(binary, string(out)) {
				return "capability on the binary (cap_net_bind_service)"
			}
		}
	}
	// 3. Neither
	return "neither"
}

var socketExtenderFn = queryExtenderFromSocket

func queryExtenderFromSocket(p Provider) (*ExtenderReading, error) {
	if p.StateDir == "" {
		return nil, errors.New("no state directory")
	}
	sockPath := filepath.Join(p.StateDir, "provider.sock")
	resp, err := sendSocketRequest(sockPath, controlRequest{Cmd: "extender"})
	if err != nil {
		return nil, extenderSocketError(sockPath, err)
	}
	if !resp.OK || resp.Extender == nil || !resp.Extender.Known {
		return nil, errors.New("this provider build does not report extender status (update it)")
	}
	reading := *resp.Extender
	reading.Source = SourceControlSocket
	return &reading, nil
}

// errExtenderProviderNotRunning marks "there is no provider to ask", as opposed
// to a provider that is up and cannot answer. A change queued while the provider
// is down is a success; one that cannot be confirmed from a live provider is not.
var errExtenderProviderNotRunning = errors.New("provider not running")

func extenderSocketError(sockPath string, err error) error {
	_, statErr := os.Stat(sockPath)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(statErr, os.ErrNotExist) {
		return fmt.Errorf("%w: no control socket: %v", errExtenderProviderNotRunning, err)
	}
	return fmt.Errorf("control socket unavailable: %w", err)
}

// ReadExtenderStatus asks the provider; a failure is the unavailable reading.
func ReadExtenderStatus(p Provider) ExtenderReading {
	reading := ExtenderReading{Source: SourceUnknown}
	if r, err := socketExtenderFn(p); err == nil && r != nil {
		reading = *r
	}
	if reading.BindRoute == "" {
		reading.BindRoute = detectBindRoute(p.Binary, p.User)
	}
	return reading
}

func extenderFamilyName(ipVersion int) string {
	if ipVersion == 6 {
		return "IPv6"
	}
	return "IPv4"
}

// extenderTcpDown reports whether the tcp carrier is down, with its error.
func extenderTcpDown(r ExtenderReading) (bool, string) {
	for _, port := range r.Ports {
		if strings.HasPrefix(port.Name, "tcp") && !port.Up {
			return true, port.Error
		}
	}
	return false, ""
}

// extenderFamilyLine is the one-line state of a family.
func extenderFamilyLine(f ExtenderFamily, now time.Time) string {
	switch f.State {
	case "activated":
		line := "ACTIVE"
		if f.Ip != "" {
			line += " " + f.Ip
		}
		if f.ExpireUnixMs != 0 {
			if left := time.UnixMilli(f.ExpireUnixMs).Sub(now); 0 < left {
				line += fmt.Sprintf(" (listing renews within %s)", left.Round(time.Minute))
			}
		}
		return line
	case "held":
		return "not active: held until tcp 443 is bound"
	case "waiting":
		line := "waiting"
		if f.Detail != "" {
			line += " (" + f.Detail + ")"
		}
		return line + extenderNextTry(f, now)
	default:
		line := "not active"
		if f.State != "" {
			line += " (" + f.State + ")"
		}
		if f.Detail != "" {
			line += ": " + f.Detail
		}
		return line + extenderNextTry(f, now)
	}
}

func extenderNextTry(f ExtenderFamily, now time.Time) string {
	if f.NextAttemptUnixMs == 0 {
		return ""
	}
	next := time.UnixMilli(f.NextAttemptUnixMs)
	if !next.After(now) {
		return ", next try due now"
	}
	return ", next try " + next.Format("15:04")
}

// formatExtenderBlock renders the reading. `now` is a parameter so the output
// is deterministic in tests.
func formatExtenderBlock(w io.Writer, r ExtenderReading, now time.Time) {
	if r.Source != SourceControlSocket {
		fmt.Fprintln(w, "\nExtender: unavailable (the provider did not answer on its control socket)")
		if r.BindRoute != "" {
			fmt.Fprintf(w, "  Port route:        %s\n", r.BindRoute)
		}
		return
	}
	fmt.Fprintln(w, "\nExtender:")
	setting := r.Setting
	if r.SettingSource != "" {
		setting += "  (" + r.SettingSource + ")"
	}
	fmt.Fprintf(w, "  Setting:           %s\n", setting)
	if r.Discovery != nil {
		d := r.Discovery
		fmt.Fprintf(w, "  Discovery:         %s (requested: %s)\n", d.State, d.Requested)
		fmt.Fprintf(w, "    Peer id: %s; mesh peers: %d; feed subscribers: %d\n", d.PeerId, d.Peers, d.FeedSubscribers)
		if d.ActiveRecords != nil {
			fmt.Fprintf(w, "    Active directory records: %d\n", *d.ActiveRecords)
		}
		if d.ConnectedPeers != nil {
			fmt.Fprintf(w, "    Gossip connections: %d", *d.ConnectedPeers)
			if d.Connecting != nil && *d.Connecting {
				fmt.Fprint(w, "; connecting")
			}
			fmt.Fprintln(w)
		}
		if d.DialStats != nil {
			dials := d.DialStats
			fmt.Fprintf(w, "    Gossip dials: %d attempts; failures timeout=%d refused=%d canceled=%d other=%d\n", dials.Attempts, dials.Timeout, dials.Refused, dials.Canceled, dials.Other)
		} else {
			fmt.Fprintln(w, "    Gossip dials: not reported")
		}
		if d.MessagesAccepted != nil {
			fmt.Fprintf(w, "    Gossip: %d valid peer messages received", *d.MessagesAccepted)
			if d.ApplyErrors != nil {
				fmt.Fprintf(w, "; %d apply errors", *d.ApplyErrors)
			}
			if d.LastAcceptedUnixMilli > 0 {
				fmt.Fprintf(w, "; last received %s", time.UnixMilli(d.LastAcceptedUnixMilli).Local().Format(time.RFC3339))
			}
			fmt.Fprintln(w)
		}
		switch d.State {
		case "waiting":
			fmt.Fprintln(w, "    Discovery is waiting for the platform's root keys.")
		case "failed":
			fmt.Fprintf(w, "    Discovery failed: %s; it will retry.\n", d.Failure)
		}
	}
	if !r.Running {
		reason := r.OffReason
		if reason == "" {
			reason = "not running"
		}
		fmt.Fprintf(w, "  State:             OFF: %s\n", reason)
		if r.BindRoute != "" {
			fmt.Fprintf(w, "  Port route:        %s\n", r.BindRoute)
		}
		fmt.Fprintf(w, "  Requests served:   24h %d, 7d %d, lifetime %d\n", r.Served.Last24h, r.Served.Last7d, r.Served.Lifetime)
		formatExtenderCarrierStats(w, r.Carriers)
		formatExtenderTraffic(w, r)
		return
	}
	state := strings.ReplaceAll(r.RoleState, "_", " ")
	if !r.Activating {
		state += " (waiting for the direct slot to sign in before asking to be listed)"
	}
	fmt.Fprintf(w, "  State:             %s\n", state)
	if r.BindRoute != "" {
		fmt.Fprintf(w, "  Port route:        %s\n", r.BindRoute)
	}

	var ports []string
	for _, port := range r.Ports {
		switch {
		case port.Disabled:
			ports = append(ports, port.Name+" disabled by setting")
		case port.Up:
			ports = append(ports, port.Name+" bound")
		case port.Reason == "not_requested":
			ports = append(ports, port.Name+" not requested")
		case port.Reason != "":
			text, _ := extenderPortReasonText(port.Reason)
			ports = append(ports, fmt.Sprintf("%s not bound (%s)", port.Name, text))
		case port.Error != "":
			ports = append(ports, fmt.Sprintf("%s not bound (%s)", port.Name, port.Error))
		default:
			ports = append(ports, port.Name+" not bound")
		}
	}
	fmt.Fprintf(w, "  Ports:             %s\n", strings.Join(ports, ", "))
	if down, _ := extenderTcpDown(r); down {
		fmt.Fprintf(w, "  Problem:           tcp 443 is down, so this host is NOT listed and gets no clients. Free the port; it is retried every 3 minutes.\n")
	}
	for _, f := range r.Families {
		fmt.Fprintf(w, "  %-18s %s\n", extenderFamilyName(f.IpVersion)+":", extenderFamilyLine(f, now))
		for _, port := range f.Ports {
			fmt.Fprintf(w, "    %s: %s\n", port.Name, port.State)
		}
	}
	fmt.Fprintf(w, "  Clients now:       %d\n", r.LiveClients)
	fmt.Fprintf(w, "  Requests served:   24h %d, 7d %d, lifetime %d\n", r.Served.Last24h, r.Served.Last7d, r.Served.Lifetime)
	formatExtenderCarrierStats(w, r.Carriers)
	formatExtenderTraffic(w, r)
}

func formatExtenderCarrierStats(w io.Writer, carriers []ExtenderCarrierStats) {
	if len(carriers) == 0 {
		return
	}
	fmt.Fprintln(w, "  Carriers:")
	for _, c := range carriers {
		line := fmt.Sprintf("    %-9s served %d, refused %d", c.Carrier+":", c.Served, c.Refused)
		if len(c.RefusalsByStage) > 0 {
			stages := make([]string, 0, len(c.RefusalsByStage))
			for stage := range c.RefusalsByStage {
				stages = append(stages, stage)
			}
			slices.SortFunc(stages, func(a, b string) int {
				if c.RefusalsByStage[a] != c.RefusalsByStage[b] {
					if c.RefusalsByStage[a] > c.RefusalsByStage[b] {
						return -1
					}
					return 1
				}
				return strings.Compare(a, b)
			})
			parts := make([]string, 0, len(stages))
			for _, stage := range stages {
				parts = append(parts, fmt.Sprintf("%s %d", stage, c.RefusalsByStage[stage]))
			}
			line += " (" + strings.Join(parts, ", ") + ")"
		}
		fmt.Fprintln(w, line)
	}
}

// formatExtenderTraffic renders the per-carrier relayed traffic table. The
// bytes are what THIS extender relayed, summed over the life of the server for
// the totals and measured on the last sample for the rates.
func formatExtenderTraffic(w io.Writer, r ExtenderReading) {
	if len(r.CarrierBytes) == 0 {
		return
	}
	fmt.Fprintln(w, "  Traffic:")
	fmt.Fprintf(w, "    %-10s %10s %10s %12s %12s %s\n", "carrier", "in", "out", "rate in", "rate out", "connections")
	for _, c := range r.CarrierBytes {
		fmt.Fprintf(w, "    %-10s %10s %10s %12s %12s %d live, %d total\n",
			c.Carrier+":",
			formatBytes(float64(c.IngressBytes)),
			formatBytes(float64(c.EgressBytes)),
			formatBytes(float64(c.RateInBps))+"/s",
			formatBytes(float64(c.RateOutBps))+"/s",
			c.LiveConns,
			c.ConnsTotal,
		)
		if c.IngressBytesV4 > 0 || c.EgressBytesV4 > 0 || c.LiveConnsV4 > 0 || c.ConnsTotalV4 > 0 ||
			c.IngressBytesV6 > 0 || c.EgressBytesV6 > 0 || c.LiveConnsV6 > 0 || c.ConnsTotalV6 > 0 {
			fmt.Fprintf(w, "      %-8s %10s %10s %12s %12s %d live, %d total\n",
				"ipv4:",
				formatBytes(float64(c.IngressBytesV4)),
				formatBytes(float64(c.EgressBytesV4)),
				"-",
				"-",
				c.LiveConnsV4,
				c.ConnsTotalV4,
			)
			fmt.Fprintf(w, "      %-8s %10s %10s %12s %12s %d live, %d total\n",
				"ipv6:",
				formatBytes(float64(c.IngressBytesV6)),
				formatBytes(float64(c.EgressBytesV6)),
				"-",
				"-",
				c.LiveConnsV6,
				c.ConnsTotalV6,
			)
		}
	}
	fmt.Fprintf(w, "    %-10s %10s %10s\n", "total:",
		formatBytes(float64(r.RelayBytesIn)),
		formatBytes(float64(r.RelayBytesOut)))
}

// printExtenderBlock appends the extender block to `urnet-tools status`.
func printExtenderBlock(p Provider) {
	formatExtenderBlock(os.Stdout, ReadExtenderStatus(p), time.Now())
}
