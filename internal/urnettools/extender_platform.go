package urnettools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// PlatformConfigurer defines the operations needed to configure and verify extender requirements.
type PlatformConfigurer interface {
	Platform() string
	CheckRequirement(binary, user string) (configured bool, desc string, err error)
	ApplyRequirement(binary, user string) (applied bool, desc string, err error)
	VerifyBind(binary, user string) (ok bool, err error)
}

// platformPlanner is an optional interface implemented by PlatformConfigurers
// that can report the exact commands ApplyRequirement will run.
type platformPlanner interface {
	PlanRequirement(binary, user string) ([]setupCommand, error)
}

// SystemPlatformConfigurer is the default implementation that talks to OS binaries and kernel APIs.
type SystemPlatformConfigurer struct {
	PlatformName string
	ExecCmd      func(name string, args ...string) ([]byte, error)
	ReadFile     func(name string) ([]byte, error)
	WriteFile    func(name string, data []byte, perm os.FileMode) error
	ListenTCP    func(network, address string) (net.Listener, error)
	LookupPath   func(file string) (string, error)
	IsRoot       func() bool
	// Lstat inspects the provider binary without following a symlink. Nil
	// skips the check, for test doubles that name no real file.
	Lstat func(name string) (os.FileInfo, error)
}

// NewSystemPlatformConfigurer returns a SystemPlatformConfigurer for the platform.
func NewSystemPlatformConfigurer(platform string) *SystemPlatformConfigurer {
	return &SystemPlatformConfigurer{
		PlatformName: platform,
		ExecCmd: func(name string, args ...string) ([]byte, error) {
			if name == "sudo" && len(args) > 0 && args[len(args)-1] == "--extender-bind-probe" {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				return exec.CommandContext(ctx, name, args...).CombinedOutput()
			}
			cmd := exec.Command(name, args...)
			return cmd.CombinedOutput()
		},
		ReadFile:   os.ReadFile,
		WriteFile:  os.WriteFile,
		ListenTCP:  net.Listen,
		LookupPath: exec.LookPath,
		Lstat:      os.Lstat,
		IsRoot: func() bool {
			return os.Geteuid() == 0
		},
	}
}

func (sp *SystemPlatformConfigurer) Platform() string {
	return sp.PlatformName
}

// CheckRequirement checks whether the host platform requirement is already satisfied.
func (sp *SystemPlatformConfigurer) CheckRequirement(binary, user string) (bool, string, error) {
	switch sp.PlatformName {
	case "linux":
		if user == "root" {
			return true, "provider running as root (no file capability needed)", nil
		}
		return sp.checkLinux(binary)
	case "freebsd":
		return sp.checkFreeBSD(user)
	case "windows":
		return sp.checkWindows()
	case "darwin":
		return sp.checkDarwin()
	default:
		return false, fmt.Sprintf("unsupported platform %s", sp.PlatformName), nil
	}
}

// PlanRequirement returns the exact commands that ApplyRequirement will run.
func (sp *SystemPlatformConfigurer) PlanRequirement(binary, user string) ([]setupCommand, error) {
	switch sp.PlatformName {
	case "linux":
		if user == "root" {
			return nil, nil
		}
		if binary == "" {
			return nil, fmt.Errorf("cannot apply cap_net_bind_service: no provider binary path known")
		}
		if sp.Lstat != nil {
			info, err := sp.Lstat(binary)
			if err != nil {
				return nil, fmt.Errorf("cannot apply cap_net_bind_service: %w", err)
			}
			if !info.Mode().IsRegular() {
				return nil, fmt.Errorf("refusing to set cap_net_bind_service on %s: it is %s, not a regular file; point setup at the real provider binary", binary, info.Mode().Type())
			}
		}
		if sp.ReadFile != nil {
			if status, err := sp.ReadFile("/proc/self/status"); err == nil && bindCapabilityBlocked(status) {
				return nil, fmt.Errorf("cap_net_bind_service is excluded from the process bounding set")
			}
		}
		return []setupCommand{{name: "setcap", args: []string{"cap_net_bind_service=+ep", binary}}}, nil
	case "freebsd":
		if user == "root" {
			return nil, nil
		}
		return []setupCommand{{name: "sysctl", args: []string{"net.inet.ip.portrange.reservedhigh=0"}}}, nil
	case "windows":
		return planWindowsRules(sp.ExecCmd), nil
	case "darwin":
		_, _, err := sp.checkDarwin()
		return nil, err
	default:
		return nil, fmt.Errorf("unsupported platform %s", sp.PlatformName)
	}
}

// ApplyRequirement applies the privileged platform requirement.
func (sp *SystemPlatformConfigurer) ApplyRequirement(binary, user string) (bool, string, error) {
	switch sp.PlatformName {
	case "linux":
		if user == "root" {
			return false, "provider running as root (no file capability needed)", nil
		}
		return sp.applyLinux(binary)
	case "freebsd":
		return sp.applyFreeBSD(user)
	case "windows":
		return sp.applyWindows()
	case "darwin":
		return sp.applyDarwin()
	default:
		return false, "", fmt.Errorf("unsupported platform %s", sp.PlatformName)
	}
}

// VerifyBind runs the provider's probe as its user; false without error is unverified.
func (sp *SystemPlatformConfigurer) VerifyBind(binary, user string) (bool, error) {
	if sp.PlatformName != "linux" || binary == "" || user == "" || sp.LookupPath == nil || sp.ExecCmd == nil {
		return false, nil
	}
	if _, err := sp.LookupPath("sudo"); err != nil {
		return false, nil
	}
	out, err := sp.ExecCmd("sudo", "-n", "-u", user, "--", binary, "--extender-bind-probe")
	text := strings.TrimSpace(string(out))
	if err == nil && text == "extender-bind-probe: proven" {
		return true, nil
	}
	if strings.HasPrefix(text, "extender-bind-probe: failed:") {
		return false, fmt.Errorf("%s", text)
	}
	return false, nil
}

// unitNoNewPrivileges is implemented by configurers that can ask systemd
// whether a unit sets NoNewPrivileges. It is optional so test doubles of
// PlatformConfigurer need not grow a method.
type unitNoNewPrivileges interface {
	UnitNoNewPrivileges(unit string) (bool, error)
}

// UnitNoNewPrivileges reports whether the systemd unit has NoNewPrivileges
// set. With it, the kernel ignores file capabilities on exec, so the
// cap_net_bind_service that setup grants does not reach the service.
func (sp *SystemPlatformConfigurer) UnitNoNewPrivileges(unit string) (bool, error) {
	if sp.PlatformName != "linux" || unit == "" || sp.ExecCmd == nil {
		return false, nil
	}
	out, err := sp.ExecCmd("systemctl", "show", "-p", "NoNewPrivileges", "--value", unit)
	if err != nil {
		return false, err
	}
	switch strings.TrimSpace(string(out)) {
	case "yes", "true", "1", "on":
		return true, nil
	}
	return false, nil
}

// bindCapabilityBlocked reports whether CapBnd in /proc/self/status excludes
// CAP_NET_BIND_SERVICE (bit 10).
func bindCapabilityBlocked(status []byte) bool {
	for _, line := range strings.Split(string(status), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "CapBnd:" {
			if mask, err := strconv.ParseUint(fields[1], 16, 64); err == nil && mask&(1<<10) == 0 {
				return true
			}
		}
	}
	return false
}

// --- Linux implementation ---

func (sp *SystemPlatformConfigurer) checkLinux(binary string) (bool, string, error) {
	if binary == "" {
		return false, "no provider binary path known", nil
	}
	out, err := sp.ExecCmd("getcap", binary)
	if err != nil {
		return false, fmt.Sprintf("getcap %s failed: %v", binary, err), nil
	}
	capOut := string(out)
	if strings.Contains(capOut, "cap_net_bind_service=ep") || strings.Contains(capOut, "cap_net_bind_service+ep") {
		return true, fmt.Sprintf("cap_net_bind_service=+ep already set on %s", binary), nil
	}
	return false, fmt.Sprintf("cap_net_bind_service=+ep missing on %s", binary), nil
}

func (sp *SystemPlatformConfigurer) applyLinux(binary string) (bool, string, error) {
	if binary == "" {
		return false, "", fmt.Errorf("cannot apply cap_net_bind_service: no provider binary path known")
	}
	// setcap runs as root on a path the provider user may own. setcap follows
	// a symlink, so a link swapped in for the binary would put the capability
	// on whatever it points at. Refuse anything that is not a regular file.
	if sp.Lstat != nil {
		info, err := sp.Lstat(binary)
		if err != nil {
			return false, "", fmt.Errorf("cannot apply cap_net_bind_service: %w", err)
		}
		if !info.Mode().IsRegular() {
			return false, "", fmt.Errorf("refusing to set cap_net_bind_service on %s: it is %s, not a regular file; point setup at the real provider binary", binary, info.Mode().Type())
		}
	}
	// Applying a capability outside the bounding set makes exec fail.
	if sp.ReadFile != nil {
		if status, err := sp.ReadFile("/proc/self/status"); err == nil && bindCapabilityBlocked(status) {
			return false, "", fmt.Errorf("cap_net_bind_service is excluded from the process bounding set")
		}
	}
	// Apply capability
	if out, err := sp.ExecCmd("setcap", "cap_net_bind_service=+ep", binary); err != nil {
		return false, "", fmt.Errorf("setcap cap_net_bind_service=+ep %s failed: %s: %w", binary, strings.TrimSpace(string(out)), err)
	}

	// Verify re-reading getcap
	out, err := sp.ExecCmd("getcap", binary)
	if err != nil {
		return false, "", fmt.Errorf("re-reading getcap %s failed: %w", binary, err)
	}
	capOut := string(out)
	if !strings.Contains(capOut, "cap_net_bind_service=ep") && !strings.Contains(capOut, "cap_net_bind_service+ep") {
		return false, "", fmt.Errorf("re-reading getcap %s: capability cap_net_bind_service=+ep not present (got %q)", binary, strings.TrimSpace(capOut))
	}

	return true, fmt.Sprintf("applied cap_net_bind_service=+ep on %s (verified with getcap)", binary), nil
}

// --- FreeBSD implementation ---

func (sp *SystemPlatformConfigurer) checkFreeBSD(user string) (bool, string, error) {
	if user == "root" {
		return true, "provider running as root (no portrange.reservedhigh sysctl needed)", nil
	}
	out, err := sp.ExecCmd("sysctl", "-n", "net.inet.ip.portrange.reservedhigh")
	if err != nil {
		return false, fmt.Sprintf("sysctl net.inet.ip.portrange.reservedhigh failed: %v", err), nil
	}
	val := strings.TrimSpace(string(out))
	if val != "0" {
		return false, fmt.Sprintf("net.inet.ip.portrange.reservedhigh is %s (needs 0)", val), nil
	}

	confBytes, err := sp.ReadFile("/etc/sysctl.conf")
	if err != nil {
		return false, "net.inet.ip.portrange.reservedhigh=0 live, but /etc/sysctl.conf unreadable", nil
	}
	if !sysctlConfSetsReservedHighZero(string(confBytes)) {
		return false, "net.inet.ip.portrange.reservedhigh=0 live, but not persisted in /etc/sysctl.conf", nil
	}

	return true, "net.inet.ip.portrange.reservedhigh=0 (live and persisted in /etc/sysctl.conf)", nil
}

// sysctlConfSetsReservedHighZero reports whether /etc/sysctl.conf, as the boot
// scripts read it, leaves net.inet.ip.portrange.reservedhigh at 0. Only live
// assignment lines count: a commented-out line does not persist anything, and
// when the file assigns the name more than once the last assignment wins.
func sysctlConfSetsReservedHighZero(content string) bool {
	const name = "net.inet.ip.portrange.reservedhigh"
	set := false
	for _, line := range strings.Split(content, "\n") {
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) != name {
			continue
		}
		set = strings.TrimSpace(val) == "0"
	}
	return set
}

func (sp *SystemPlatformConfigurer) applyFreeBSD(user string) (bool, string, error) {
	sysctlConf := "/etc/sysctl.conf"
	data, err := sp.ReadFile(sysctlConf)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, "", fmt.Errorf("reading %s: %w", sysctlConf, err)
	}
	curOut, err := sp.ExecCmd("sysctl", "-n", "net.inet.ip.portrange.reservedhigh")
	if err != nil {
		return false, "", fmt.Errorf("reading sysctl net.inet.ip.portrange.reservedhigh: %w", err)
	}
	curVal := strings.TrimSpace(string(curOut))

	if out, err := sp.ExecCmd("sysctl", "net.inet.ip.portrange.reservedhigh=0"); err != nil {
		return false, "", fmt.Errorf("sysctl net.inet.ip.portrange.reservedhigh=0 failed: %s: %w", strings.TrimSpace(string(out)), err)
	}

	afterOut, err := sp.ExecCmd("sysctl", "-n", "net.inet.ip.portrange.reservedhigh")
	if err != nil {
		return false, "", fmt.Errorf("re-reading sysctl net.inet.ip.portrange.reservedhigh failed: %w", err)
	}
	afterVal := strings.TrimSpace(string(afterOut))

	content := string(data)
	if !sysctlConfSetsReservedHighZero(content) {
		var buf bytes.Buffer
		buf.WriteString(content)
		if len(content) > 0 && !strings.HasSuffix(content, "\n") {
			buf.WriteString("\n")
		}
		buf.WriteString("# Extender role: allow unprivileged binding of port 443\nnet.inet.ip.portrange.reservedhigh=0\n")
		if err := sp.WriteFile(sysctlConf, buf.Bytes(), 0644); err != nil {
			return false, "", fmt.Errorf("failed to persist in %s: %w", sysctlConf, err)
		}
	}

	desc := fmt.Sprintf("net.inet.ip.portrange.reservedhigh changed from %s to %s and persisted in %s", curVal, afterVal, sysctlConf)
	return true, desc, nil
}

// --- Windows implementation ---

func (sp *SystemPlatformConfigurer) checkWindows() (bool, string, error) {
	tcp := windowsRuleExists(sp.ExecCmd, "URnetwork Extender")
	udp := windowsRuleExists(sp.ExecCmd, "URnetwork Extender UDP")
	return tcp && udp, fmt.Sprintf("Windows firewall rules: TCP present=%t, UDP present=%t", tcp, udp), nil
}

func (sp *SystemPlatformConfigurer) applyWindows() (bool, string, error) {
	applied, err := applyWindowsRules(sp.ExecCmd)
	if err != nil {
		return applied, "", err
	}
	return applied, "Windows Firewall TCP 443 and UDP 443,4053 rules configured", nil
}

// --- macOS (darwin) implementation ---

func (sp *SystemPlatformConfigurer) checkDarwin() (bool, string, error) {
	return false, "", fmt.Errorf("macOS extender setup cannot verify or install a root LaunchDaemon; configure and verify the provider service manually")
}

func (sp *SystemPlatformConfigurer) applyDarwin() (bool, string, error) {
	return sp.checkDarwin()
}

// UserCanBindTCP443 determines whether the current user on the platform can bind port 443.
func UserCanBindTCP443(cfg PlatformConfigurer, user string) bool {
	switch cfg.Platform() {
	case "windows":
		return true
	case "darwin":
		return user == "root" || os.Geteuid() == 0
	case "freebsd":
		if user == "root" || os.Geteuid() == 0 {
			return true
		}
		configured, _, _ := cfg.CheckRequirement("", user)
		return configured
	default: // "linux"
		if user == "root" || os.Geteuid() == 0 {
			return true
		}
		configured, _, _ := cfg.CheckRequirement("", user)
		return configured
	}
}
