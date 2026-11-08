package urnettools

import (
	"errors"
	"fmt"
	"net"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// ExtenderPortNames maps the standard ports to their role in extender.
const (
	ExtenderPortTCP443  = 443
	ExtenderPortUDP443  = 443
	ExtenderPortUDP4053 = 4053
)

// PortHolder describes the holder of a network port.
type PortHolder struct {
	Port    int    `json:"port"`
	Proto   string `json:"proto"` // "tcp" or "udp"
	Held    bool   `json:"held"`
	Process string `json:"process,omitempty"`
	PID     int    `json:"pid,omitempty"`
}

// ExtenderPorts holds the occupancy status of the extender carrier ports.
type ExtenderPorts struct {
	TCP443  PortHolder `json:"tcp_443"`
	UDP443  PortHolder `json:"udp_443"`
	UDP4053 PortHolder `json:"udp_4053"`
}

// AnyUDPHeld returns true if either UDP 443 or UDP 4053 is held.
func (p ExtenderPorts) AnyUDPHeld() bool {
	return p.UDP443.Held || p.UDP4053.Held
}

// UDPWarnings returns human-readable warnings for held UDP ports.
func (p ExtenderPorts) UDPWarnings() []string {
	var warnings []string
	if p.UDP443.Held {
		proc := p.UDP443.Process
		if proc == "" && p.UDP443.PID > 0 {
			proc = fmt.Sprintf("PID %d", p.UDP443.PID)
		} else if proc != "" && p.UDP443.PID > 0 {
			proc = fmt.Sprintf("%s (PID %d)", proc, p.UDP443.PID)
		}
		if proc != "" {
			warnings = append(warnings, fmt.Sprintf("UDP 443 is held by %s; extender will operate on TCP alone (QUIC carrier disabled)", proc))
		} else {
			warnings = append(warnings, "UDP 443 is held; extender will operate on TCP alone (QUIC carrier disabled)")
		}
	}
	if p.UDP4053.Held {
		proc := p.UDP4053.Process
		if proc == "" && p.UDP4053.PID > 0 {
			proc = fmt.Sprintf("PID %d", p.UDP4053.PID)
		} else if proc != "" && p.UDP4053.PID > 0 {
			proc = fmt.Sprintf("%s (PID %d)", proc, p.UDP4053.PID)
		}
		if proc != "" {
			warnings = append(warnings, fmt.Sprintf("UDP 4053 is held by %s; extender will operate on TCP alone (DNS carrier disabled)", proc))
		} else {
			warnings = append(warnings, "UDP 4053 is held; extender will operate on TCP alone (DNS carrier disabled)")
		}
	}
	return warnings
}

// PortChecker abstracts port queries across platforms and testing harnesses.
type PortChecker struct {
	Platform string
	ExecCmd  func(name string, args ...string) ([]byte, error)
}

// DefaultPortChecker returns a PortChecker configured for the current system.
func DefaultPortChecker(platform string) *PortChecker {
	return &PortChecker{
		Platform: platform,
		ExecCmd: func(name string, args ...string) ([]byte, error) {
			cmd := exec.Command(name, args...)
			return cmd.CombinedOutput()
		},
	}
}

// CheckPorts discovers who holds TCP 443, UDP 443 and UDP 4053.
func (c *PortChecker) CheckPorts() (ExtenderPorts, error) {
	result := ExtenderPorts{
		TCP443:  PortHolder{Port: ExtenderPortTCP443, Proto: "tcp"},
		UDP443:  PortHolder{Port: ExtenderPortUDP443, Proto: "udp"},
		UDP4053: PortHolder{Port: ExtenderPortUDP4053, Proto: "udp"},
	}

	var err error
	switch c.Platform {
	case "windows":
		err = c.checkWindows(&result)
	case "freebsd":
		err = c.checkFreeBSD(&result)
	case "darwin":
		err = c.checkBSDOrDarwin(&result)
	default: // "linux" and other unix
		err = c.checkLinux(&result)
	}

	return result, err
}

// ssUserRegex parses `users:(("name",pid=123,fd=4))` from ss output.
var ssUserRegex = regexp.MustCompile(`users:\(\("([^"]+)",pid=(\d+)`)

func (c *PortChecker) checkLinux(ports *ExtenderPorts) error {
	// TCP 443 check
	if out, err := c.ExecCmd("ss", "-tlnp"); err == nil {
		lines := strings.Split(string(out), "\n")
		for _, line := range lines {
			if parseSSLineForPort(line, 443) {
				ports.TCP443.Held = true
				if match := ssUserRegex.FindStringSubmatch(line); len(match) > 2 {
					ports.TCP443.Process = match[1]
					ports.TCP443.PID, _ = strconv.Atoi(match[2])
				}
				break
			}
		}
	} else {
		return fmt.Errorf("ss TCP inspection: %w", err)
	}

	// UDP 443 and 4053 check
	if out, err := c.ExecCmd("ss", "-ulnp"); err == nil {
		lines := strings.Split(string(out), "\n")
		for _, line := range lines {
			if !ports.UDP443.Held && parseSSLineForPort(line, 443) {
				ports.UDP443.Held = true
				if match := ssUserRegex.FindStringSubmatch(line); len(match) > 2 {
					ports.UDP443.Process = match[1]
					ports.UDP443.PID, _ = strconv.Atoi(match[2])
				}
			}
			if !ports.UDP4053.Held && parseSSLineForPort(line, 4053) {
				ports.UDP4053.Held = true
				if match := ssUserRegex.FindStringSubmatch(line); len(match) > 2 {
					ports.UDP4053.Process = match[1]
					ports.UDP4053.PID, _ = strconv.Atoi(match[2])
				}
			}
		}
	} else {
		return fmt.Errorf("ss UDP inspection: %w", err)
	}
	return nil
}

func parseSSLineForPort(line string, targetPort int) bool {
	fields := strings.Fields(line)
	if len(fields) < 4 {
		return false
	}
	// Local Address is typically field 3 (0-indexed: State, Recv-Q, Send-Q, Local Address:Port)
	localAddr := fields[3]
	idx := strings.LastIndex(localAddr, ":")
	if idx < 0 {
		return false
	}
	portStr := localAddr[idx+1:]
	p, err := strconv.Atoi(portStr)
	return err == nil && p == targetPort
}

func (c *PortChecker) checkFreeBSD(ports *ExtenderPorts) error {
	tcp, err := c.ExecCmd("sockstat", "-46l", "-P", "tcp", "-p", "443")
	if err != nil {
		return fmt.Errorf("sockstat TCP inspection: %w", err)
	}
	parseSockstatOutput(string(tcp), &ports.TCP443, 443, "tcp")
	udp, err := c.ExecCmd("sockstat", "-46", "-P", "udp", "-p", "443,4053")
	if err != nil {
		return fmt.Errorf("sockstat UDP inspection: %w", err)
	}
	parseSockstatOutput(string(udp), &ports.UDP443, 443, "udp")
	parseSockstatOutput(string(udp), &ports.UDP4053, 4053, "udp")
	return nil
}

func parseSockstatOutput(output string, holder *PortHolder, port int, proto string) {
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 7 || (fields[4] != proto+"4" && fields[4] != proto+"6" && fields[4] != proto+"46") {
			continue
		}
		local := fields[5]
		colon := strings.LastIndex(local, ":")
		if colon < 0 || local[colon+1:] != strconv.Itoa(port) {
			continue
		}
		holder.Held = true
		holder.Process = fields[1]
		holder.PID, _ = strconv.Atoi(fields[2])
		return
	}
}

func (c *PortChecker) checkBSDOrDarwin(ports *ExtenderPorts) error {
	out, err := c.ExecCmd("lsof", "-nP", "-iTCP:443", "-sTCP:LISTEN")
	if err != nil && !lsofNoMatches(out, err) {
		return fmt.Errorf("lsof TCP inspection: %w", err)
	}
	parseLsofOutput(string(out), &ports.TCP443, 443)
	out, err = c.ExecCmd("lsof", "-nP", "-iUDP:443,4053")
	if err != nil && !lsofNoMatches(out, err) {
		return fmt.Errorf("lsof UDP inspection: %w", err)
	}
	parseLsofOutput(string(out), &ports.UDP443, 443)
	parseLsofOutput(string(out), &ports.UDP4053, 4053)
	return nil
}

func lsofNoMatches(out []byte, err error) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr) && exitErr.ExitCode() == 1 && len(strings.TrimSpace(string(out))) == 0
}

func parseLsofOutput(out string, holder *PortHolder, targetPort int) {
	lines := strings.Split(out, "\n")
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) < 9 || fields[0] == "COMMAND" {
			continue
		}
		local, _, _ := strings.Cut(fields[8], "->")
		colon := strings.LastIndex(local, ":")
		if colon >= 0 && local[colon+1:] == strconv.Itoa(targetPort) {
			holder.Held = true
			holder.Process = fields[0]
			if pid, err := strconv.Atoi(fields[1]); err == nil {
				holder.PID = pid
			}
			return
		}
	}
}

func (c *PortChecker) checkWindows(ports *ExtenderPorts) error {
	// TCP
	if out, err := c.ExecCmd("netstat", "-ano", "-p", "tcp"); err == nil {
		parseNetstatOutput(string(out), &ports.TCP443, 443, "TCP", "LISTENING")
		if ports.TCP443.Held && ports.TCP443.PID > 0 {
			ports.TCP443.Process = c.lookupWindowsProcessName(ports.TCP443.PID)
		}
	} else {
		return fmt.Errorf("netstat TCP inspection: %w", err)
	}

	// UDP
	if out, err := c.ExecCmd("netstat", "-ano", "-p", "udp"); err == nil {
		parseNetstatOutput(string(out), &ports.UDP443, 443, "UDP", "")
		if ports.UDP443.Held && ports.UDP443.PID > 0 {
			ports.UDP443.Process = c.lookupWindowsProcessName(ports.UDP443.PID)
		}
		parseNetstatOutput(string(out), &ports.UDP4053, 4053, "UDP", "")
		if ports.UDP4053.Held && ports.UDP4053.PID > 0 {
			ports.UDP4053.Process = c.lookupWindowsProcessName(ports.UDP4053.PID)
		}
	} else {
		return fmt.Errorf("netstat UDP inspection: %w", err)
	}
	return nil
}

func parseNetstatOutput(out string, holder *PortHolder, targetPort int, proto, wantState string) {
	lines := strings.Split(out, "\n")
	portSuffix := fmt.Sprintf(":%d", targetPort)
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		if !strings.EqualFold(fields[0], proto) {
			continue
		}
		localAddr := fields[1]
		if !strings.HasSuffix(localAddr, portSuffix) {
			continue
		}
		if wantState != "" {
			isListening := false
			for _, f := range fields {
				if strings.EqualFold(f, wantState) {
					isListening = true
					break
				}
			}
			if !isListening {
				continue
			}
		}
		holder.Held = true
		pidStr := fields[len(fields)-1]
		if pid, err := strconv.Atoi(pidStr); err == nil {
			holder.PID = pid
		}
		return
	}
}

func (c *PortChecker) lookupWindowsProcessName(pid int) string {
	if pid <= 0 {
		return ""
	}
	out, err := c.ExecCmd("tasklist", "/FI", fmt.Sprintf("PID eq %d", pid), "/FO", "CSV", "/NH")
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(string(out))
	if strings.HasPrefix(line, `"`) {
		parts := strings.Split(line, `","`)
		if len(parts) > 0 {
			return strings.Trim(parts[0], `"`)
		}
	}
	return ""
}

// CanBindTCP443 checks if the current user can bind TCP 443 directly.
// This is non-destructive: it attempts to listen and closes immediately if successful.
func CanBindTCP443(bindFunc func(network, address string) (net.Listener, error)) (bool, error) {
	if bindFunc == nil {
		bindFunc = net.Listen
	}
	ln, err := bindFunc("tcp", ":443")
	if err == nil {
		_ = ln.Close()
		return true, nil
	}
	errStr := err.Error()
	if strings.Contains(errStr, "permission denied") || strings.Contains(errStr, "operation not permitted") {
		return false, nil
	}
	return false, err
}
