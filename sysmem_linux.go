//go:build linux

package connect

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

// HostMemoryTotalBytes is the host's physical RAM from /proc/meminfo
// MemTotal, ignoring any cgroup limit. ok is false when it cannot be read.
func HostMemoryTotalBytes() (int64, bool) {
	return readMeminfoBytes("MemTotal:")
}

// HostMemoryAvailableBytes is the kernel's estimate of memory available
// without swapping (MemAvailable), ignoring any cgroup limit. ok is false
// when it cannot be read.
func HostMemoryAvailableBytes() (int64, bool) {
	return readMeminfoBytes("MemAvailable:")
}

func readMeminfoBytes(key string) (int64, bool) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, false
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, key) {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0, false
		}
		kb, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0, false
		}
		return kb * 1024, true
	}
	return 0, false
}
