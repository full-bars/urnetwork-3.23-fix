package urnettools

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// extenderCapabilityPlan inspects a provider file and plans a capability grant.
func extenderCapabilityPlan(binary string, status []byte, run func(string, ...string) ([]byte, error)) ([]setupCommand, error) {
	if !filepath.IsAbs(binary) {
		return nil, fmt.Errorf("provider binary path must be absolute")
	}
	info, err := os.Lstat(binary)
	if err != nil {
		return nil, fmt.Errorf("inspect provider binary: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("provider binary is not a regular file: %s", binary)
	}
	out, err := run("getcap", binary)
	if err != nil {
		return nil, fmt.Errorf("cannot check provider capability: %w", err)
	}
	if extenderCapabilityPresent(binary, func(string, ...string) ([]byte, error) { return out, nil }) {
		return nil, nil
	}
	// setcap replaces the complete capability set. Do not discard existing bits.
	if strings.TrimSpace(string(out)) != "" {
		return nil, fmt.Errorf("provider has existing capabilities; add cap_net_bind_service manually without removing them")
	}
	for _, line := range strings.Split(string(status), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[0] != "CapBnd:" {
			continue
		}
		mask, err := strconv.ParseUint(fields[1], 16, 64)
		if err != nil {
			return nil, fmt.Errorf("cannot parse capability bounding set: %w", err)
		}
		if mask&(1<<10) == 0 {
			return nil, fmt.Errorf("cap_net_bind_service is excluded from the capability bounding set")
		}
		return []setupCommand{{name: "setcap", args: []string{"cap_net_bind_service=+ep", binary}}}, nil
	}
	return nil, fmt.Errorf("cannot verify capability bounding set")
}
