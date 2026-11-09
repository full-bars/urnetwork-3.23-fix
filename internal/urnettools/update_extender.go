package urnettools

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

const extenderSysctlPortStartPath = "/proc/sys/net/ipv4/ip_unprivileged_port_start"

var updateSysctlReadFile = os.ReadFile

// extenderSysctlRouteActive reports whether port 443 is available to unprivileged
// users via net.ipv4.ip_unprivileged_port_start <= 443 on Linux.
func extenderSysctlRouteActive(platform ...string) bool {
	plat := runtime.GOOS
	if len(platform) > 0 && platform[0] != "" {
		plat = platform[0]
	}
	if plat != "linux" {
		return false
	}
	data, err := updateSysctlReadFile(extenderSysctlPortStartPath)
	if err != nil {
		return false
	}
	val, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return false
	}
	return val >= 0 && val <= 443
}

// installProviderBinary restores the Linux extender capability after a swap.
// Tool self-updates use installBinary directly and do not need this capability.
func installProviderBinary(src, dst, user string) error {
	return installProviderBinaryWithRunner(src, dst, user, runtime.GOOS, os.Stderr, func(name string, args ...string) ([]byte, error) {
		return exec.Command(name, args...).CombinedOutput()
	})
}

func installProviderBinaryWithRunner(src, dst, user, platform string, warnings io.Writer, run func(string, ...string) ([]byte, error)) error {
	return installProviderBinaryWithRunnerAndStatus(src, dst, user, platform, warnings, run, "/proc/self/status")
}

func installProviderBinaryWithRunnerAndStatus(src, dst, user, platform string, warnings io.Writer, run func(string, ...string) ([]byte, error), statusPath string) error {
	sysctlActive := extenderSysctlRouteActive(platform)
	var hadCapability bool
	if platform == "linux" && !sysctlActive {
		hadCapability = extenderCapabilityPresent(dst, run)
	}
	if err := installBinary(src, dst, user); err != nil {
		return err
	}
	if platform != "linux" || sysctlActive {
		return nil
	}
	quoted := "'" + strings.ReplaceAll(dst, "'", "'\\''") + "'"
	if !hadCapability {
		fmt.Fprintf(warnings, "WARNING: previous extender capability absent or unverified; to enable it run: sudo setcap cap_net_bind_service=+ep %s\n", quoted)
		return nil
	}
	if data, err := os.ReadFile(statusPath); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) == 2 && fields[0] == "CapBnd:" {
				if mask, err := strconv.ParseUint(fields[1], 16, 64); err == nil && mask&(1<<10) == 0 {
					fmt.Fprintf(warnings, "WARNING: extender capability blocked by the process bounding set; after fixing that, run: sudo setcap cap_net_bind_service=+ep %s\n", quoted)
					return nil
				}
			}
		}
	}
	if extenderCapabilityPresent(dst, run) {
		return nil
	}

	setcapArgs := []string{"cap_net_bind_service=+ep", dst}
	name := "setcap"
	if os.Geteuid() != 0 {
		name = "sudo"
		setcapArgs = append([]string{"-n", "setcap"}, setcapArgs...)
	}
	_, _ = run(name, setcapArgs...)
	if !extenderCapabilityPresent(dst, run) {
		quoted := "'" + strings.ReplaceAll(dst, "'", "'\\''") + "'"
		fmt.Fprintf(warnings, "WARNING: extender capability missing or unverified for %q; run: sudo setcap cap_net_bind_service=+ep %s\n", dst, quoted)
	}
	return nil
}

func extenderCapabilityPresent(binary string, run func(string, ...string) ([]byte, error)) bool {
	output, err := run("getcap", binary)
	if err != nil {
		return false
	}
	for _, field := range strings.Fields(string(output)) {
		parts := strings.SplitN(field, "=", 2)
		if len(parts) != 2 {
			continue
		}
		for _, capability := range strings.Split(parts[0], ",") {
			if capability == "cap_net_bind_service" && strings.Contains(parts[1], "e") && strings.Contains(parts[1], "p") {
				return true
			}
		}
	}
	return false
}
