package nicstat

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// parseLinuxCounters accepts /proc/net/dev text, independently of the host OS.
func parseLinuxCounters(r io.Reader) ([]Counters, error) {
	s := bufio.NewScanner(r)
	var result []Counters
	line := 0
	for s.Scan() {
		line++
		if line <= 2 {
			continue
		}
		text := s.Text()
		i := strings.LastIndexByte(text, ':')
		if i < 0 {
			return nil, fmt.Errorf("invalid interface row %d", line)
		}
		name := strings.TrimSpace(text[:i])
		fields := strings.Fields(text[i+1:])
		if name == "" || len(fields) < 16 {
			return nil, fmt.Errorf("invalid interface row %d", line)
		}
		rx, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid rx counter on row %d: %w", line, err)
		}
		tx, err := strconv.ParseUint(fields[8], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid tx counter on row %d: %w", line, err)
		}
		result = append(result, Counters{Name: name, RxBytes: rx, TxBytes: tx})
	}
	if err := s.Err(); err != nil {
		return nil, err
	}
	if line < 2 {
		return nil, fmt.Errorf("invalid interface header")
	}
	return result, nil
}

// parseLinuxRoute selects the lowest-metric up IPv4 default route.
func parseLinuxRoute(r io.Reader) (string, error) {
	s := bufio.NewScanner(r)
	line := 0
	var name string
	var best uint64
	for s.Scan() {
		line++
		if line == 1 {
			continue
		}
		f := strings.Fields(s.Text())
		if len(f) < 8 {
			return "", fmt.Errorf("invalid route row %d", line)
		}
		flags, err := strconv.ParseUint(f[3], 16, 32)
		if err != nil {
			return "", fmt.Errorf("invalid route flags on row %d: %w", line, err)
		}
		if f[1] != "00000000" || f[7] != "00000000" || flags&1 == 0 {
			continue
		}
		metric, err := strconv.ParseUint(f[6], 10, 64)
		if err != nil {
			return "", fmt.Errorf("invalid route metric on row %d: %w", line, err)
		}
		if name == "" || metric < best {
			name, best = f[0], metric
		}
	}
	if err := s.Err(); err != nil {
		return "", err
	}
	if line == 0 {
		return "", fmt.Errorf("invalid route header")
	}
	if name == "" {
		return "", ErrNoDefaultRoute
	}
	return name, nil
}
