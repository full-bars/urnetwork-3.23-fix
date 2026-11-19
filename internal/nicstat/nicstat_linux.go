//go:build linux

package nicstat

import (
	"fmt"
	"net"
	"os"
)

// Read reads counters from procfs and flags from the interface table.
func Read() ([]Counters, error) {
	f, err := os.Open("/proc/net/dev")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	counters, err := parseLinuxCounters(f)
	if err != nil {
		return nil, err
	}
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	flags := make(map[string]net.Flags, len(interfaces))
	for _, iface := range interfaces {
		flags[iface.Name] = iface.Flags
	}
	for i := range counters {
		flag, ok := flags[counters[i].Name]
		if !ok {
			return nil, fmt.Errorf("interface %s disappeared during snapshot", counters[i].Name)
		}
		counters[i].Up = flag&net.FlagUp != 0
		counters[i].Loopback = flag&net.FlagLoopback != 0
	}
	return counters, nil
}

// DefaultRoute returns the lowest-metric IPv4 default route interface.
// IPv6-only defaults are not yet supported.
func DefaultRoute() (string, error) {
	f, err := os.Open("/proc/net/route")
	if err != nil {
		return "", err
	}
	defer f.Close()
	return parseLinuxRoute(f)
}
