//go:build linux

package urnettools

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

// processStartedAfter reports whether process pid started at or after the
// wall-clock time swapTime. Both sides are measured since system boot, so no
// wall-clock comparison between different moments is needed: swapTime comes
// from `time.Now` right after installBinary, and the process starttime is
// converted from ticks since boot, while uptime comes from /proc/uptime.
// Assumes no reboot between the swap and the check; if the machine rebooted,
// no new process existed at the old swapTime anyway and the compare stays
// conservative.
//
// Using start time rather than /proc/pid/exe means a process whose binary
// carries file capabilities (non-dumpable, readlink-permission-denied for
// anyone but root) can still be verified.
func processStartedAfter(pid int, swapTime time.Time) (bool, error) {
	if pid <= 0 {
		return false, fmt.Errorf("processStartedAfter: invalid pid %d", pid)
	}
	uptime, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return false, fmt.Errorf("processStartedAfter: read /proc/uptime: %w", err)
	}
	uptimeFields := strings.Fields(string(uptime))
	if len(uptimeFields) < 1 {
		return false, fmt.Errorf("processStartedAfter: malformed /proc/uptime")
	}
	uptimeSec, err := strconv.ParseFloat(uptimeFields[0], 64)
	if err != nil {
		return false, fmt.Errorf("processStartedAfter: parse uptime: %w", err)
	}
	f, err := os.Open(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false, fmt.Errorf("processStartedAfter: %w", err)
	}
	defer f.Close()
	scanner := bufio.NewScanner(io.LimitReader(f, 4096))
	if !scanner.Scan() {
		return false, fmt.Errorf("processStartedAfter: failed to read /proc/%d/stat", pid)
	}
	line := scanner.Text()
	// The comm field is enclosed in parentheses; everything after the first
	// ')' begins with the state field. Zero-index 21 there is starttime.
	idx := strings.Index(line, ")")
	if idx < 0 {
		return false, fmt.Errorf("processStartedAfter: malformed /proc/%d/stat", pid)
	}
	fields := strings.Fields(line[idx+1:])
	if len(fields) < 20 {
		return false, fmt.Errorf("processStartedAfter: not enough fields in /proc/%d/stat", pid)
	}
	starttime, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return false, fmt.Errorf("processStartedAfter: parse starttime: %w", err)
	}
	// Default tick rate 100 Hz; reads from sysconf are skipped so no
	// architecture-dependent syscall constants are needed.
	const ticks = 100
	startSinceBoot := time.Duration(starttime) * time.Second / time.Duration(ticks)
	nowSinceBoot := time.Duration(uptimeSec * float64(time.Second))
	swapSinceBoot := nowSinceBoot - time.Since(swapTime)
	after := startSinceBoot >= swapSinceBoot
	return after, nil
}
