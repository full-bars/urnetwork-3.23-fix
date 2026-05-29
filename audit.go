//go:build linux

package connect

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Golden Values for a high-performance provider.
// Based on the user's fleet (Arch/Debian) optimization profiles.
const (
	GoldenUlimit          = 1048576
	GoldenConntrackMax    = 1048576 // Scaled dynamically, but this is the "Warning" floor
	GoldenConntrackTimeout = 5400    // 90 minutes
)

// RunSystemAudit checks the host's system limits and logs warnings if they
// are suboptimal for a high-volume provider. This is a passive check that
// does not require root. Returns (slowDisk, lowSpace).
func RunSystemAudit(skipDiskTest bool) (slowDisk bool, lowSpace bool) {
	if runtime.GOOS != "linux" {
		return false, false
	}

	// 1. Check File Descriptors (Ulimit)
	var rLimit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rLimit); err == nil {
		if rLimit.Cur < 65535 {
			fmt.Printf("[audit] File Descriptors: %d (Suboptimal! Target: %d+)\n", rLimit.Cur, GoldenUlimit)
		}
	}

	// 2. Check Conntrack Max
	if max, err := readSysctlInt("/proc/sys/net/netfilter/nf_conntrack_max"); err == nil {
		// Based on fleet observation, even 1GB boxes (texas, losangeles) run 2M entries.
		// We'll set a high target of 2M for all systems.
		target := int64(2097152)

		if int64(max) < target {
			fmt.Printf("[audit] Conntrack Max: %d (Suboptimal! Target: %d)\n", max, target)
		}
	} else if _, err := os.Stat("/proc/sys/net/netfilter"); os.IsNotExist(err) {
		fmt.Printf("[audit] Warning: Conntrack kernel module not found/loaded.\n")
	}

	// 3. Check TCP Established Timeout
	if timeout, err := readSysctlInt("/proc/sys/net/netfilter/nf_conntrack_tcp_timeout_established"); err == nil {
		if timeout > 14400 { // > 4 hours
			fmt.Printf("[audit] Conntrack Timeout: %ds (Suboptimal! Target: %ds)\n", timeout, GoldenConntrackTimeout)
		}
	}

	// 4. Recommendation Hint
	// We check if we are likely in Docker or a binary to tailor the hint
	isDocker := false
	if _, err := os.Stat("/.dockerenv"); err == nil {
		isDocker = true
	}

	suboptimal := false
	if rLimit.Cur < 65535 {
		suboptimal = true
	}
	if max, err := readSysctlInt("/proc/sys/net/netfilter/nf_conntrack_max"); err == nil && int64(max) < 1048576 {
		suboptimal = true
	}
	if timeout, err := readSysctlInt("/proc/sys/net/netfilter/nf_conntrack_tcp_timeout_established"); err == nil && timeout > 14400 {
		suboptimal = true
	}

	if suboptimal {
		if isDocker {
			fmt.Printf("[audit] Hint: Container is isolated from host network optimizations.\n")
			fmt.Printf("[audit] Hint: Add the optimized sysctls from the README to your Docker command to fix.\n")
		} else {
			fmt.Printf("[audit] Hint: System is not optimized for high volume. Run 'urnet-tools optimize' as root to fix.\n")
		}
	}

	// 5. Check Disk I/O (Free Space & Latency)
	if skipDiskTest {
		_, lowSpace = CheckDiskSpace()
		return false, lowSpace
	}

	return RunDiskAudit()
}

// CheckDiskSpace returns (freeMiB, lowSpaceFlag)
func CheckDiskSpace() (int64, bool) {
	var stat syscall.Statfs_t
	wd, _ := os.Getwd()
	if err := syscall.Statfs(wd, &stat); err != nil {
		return 0, false
	}

	freeBytes := int64(stat.Bavail) * int64(stat.Bsize)
	freeMiB := freeBytes / 1024 / 1024

	if freeMiB < 1024 { // < 1GB
		fmt.Printf("[audit] CRITICAL: Low disk space (%d MiB available). Provider may fail.\n", freeMiB)
		return freeMiB, true
	}
	return freeMiB, false
}

// RunDiskAudit performs a robust cache-busting write test to detect slow
// disk I/O and checks for sufficient free space. Returns (slowDisk, lowSpace).
func RunDiskAudit() (slowDisk bool, lowSpace bool) {
	// 1. Check Free Space
	freeMiB, lowSpace := CheckDiskSpace()

	// 2. Determine Test File Size
	// Target 1/4 of free space, capped at 1024MB (1GB), floor 16MB.
	testSizeMiB := freeMiB / 4
	if testSizeMiB > 1024 {
		testSizeMiB = 1024
	}
	if testSizeMiB < 16 {
		testSizeMiB = 16
	}

	// 3. Performance Test (Sync Write)
	testFile := ".io-test"
	data := make([]byte, 1024*1024) // 1MB buffer
	for i := range data {
		data[i] = byte(i % 255)
	}

	start := time.Now()
	f, err := os.OpenFile(testFile, os.O_WRONLY|os.O_CREATE|os.O_SYNC, 0600)
	if err != nil {
		return false, lowSpace
	}
	defer os.Remove(testFile)

	for i := int64(0); i < testSizeMiB; i++ {
		if _, err := f.Write(data); err != nil {
			fmt.Printf("[audit] Disk I/O test failed: %v (likely out of disk space)\n", err)
			f.Close()
			return true, lowSpace // Treat as slow/broken disk to trigger RAM logging
		}
	}
	f.Close()

	elapsed := time.Since(start)
	mbps := float64(testSizeMiB) / elapsed.Seconds()

	fmt.Printf("[audit] Disk write speed: %.1f MB/s (%dMB sync test)\n", mbps, testSizeMiB)

	// Fleet threshold: < 50MB/s is considered slow for high-volume logs.
	if mbps < 50 {
		slowDisk = true
		fmt.Printf("[audit] Slow disk I/O detected (%.1f MB/s for %dMB sync write).\n", mbps, testSizeMiB)
		fmt.Printf("[audit] Hint: Suggest enabling RAM logging to eliminate I/O bottlenecks: 'urnet-tools ramlogs on'.\n")
	}

	return slowDisk, lowSpace
}

func readSysctlInt(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(data)))
}
