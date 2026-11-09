package urnettools

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	Name  string `json:"name"`
	Up    bool   `json:"up"`
	Error string `json:"error,omitempty"`
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

	RefusalsByStage map[string]uint64 `json:"refusals_by_stage,omitempty"`

	BindRoute string `json:"bind_route,omitempty"`
}

var (
	portRoutePlatformFn = func() string { return runtime.GOOS }
	portRouteReadFileFn = os.ReadFile
	portRouteExecCmdFn  = func(name string, args ...string) ([]byte, error) {
		return exec.Command(name, args...).CombinedOutput()
	}
)

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
				return fmt.Sprintf("sysctl: port is available without a file capability (ip_unprivileged_port_start=%d)", val)
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
		return
	}
	fmt.Fprintln(w, "\nExtender:")
	setting := r.Setting
	if r.SettingSource != "" {
		setting += "  (" + r.SettingSource + ")"
	}
	fmt.Fprintf(w, "  Setting:           %s\n", setting)
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
		case port.Up:
			ports = append(ports, port.Name+" bound")
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
}

// printExtenderBlock appends the extender block to `urnet-tools status`.
func printExtenderBlock(p Provider) {
	formatExtenderBlock(os.Stdout, ReadExtenderStatus(p), time.Now())
}
