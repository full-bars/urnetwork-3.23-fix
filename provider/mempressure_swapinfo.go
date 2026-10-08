package main

import "strings"

// FreeBSD swap accounting, as a PURE parser.
//
// The parsing lives here, untagged, so it can be tested on any platform —
// the syscall wrapper that feeds it cannot. That matters because the wrapper
// only runs on a FreeBSD kernel, which is exactly where these tests are
// hardest to execute.

// parseSwapinfo extracts (totalKB, usedKB) from `swapinfo -k` output.
//
// The output is a device table, one row per swap device, with a final "Total"
// row when more than one device is configured:
//
//	Device          1K-blocks     Used    Avail Capacity
//	/dev/ada0p8      1048576     12345  1036231    2%
//	Total             1048576     12345  1036231    2%
//
// With a single device there is no Total row, so the single row is summed.
// Values are in kilobytes because of -k, and both sides come from the same
// units, so the ratio needs no conversion.
//
// Returns ok=false when no usable row was found — an empty table means no swap
// is configured, which is a legitimate state and not an error.
func parseSwapinfo(out []byte) (totalKB, usedKB uint64, ok bool) {
	var sumTotal, sumUsed uint64
	rows := 0
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		// Header row, and the Total row which restates what we just summed
		// (counting it would double every device's contribution).
		if len(fields) < 3 || fields[0] == "Device" || fields[0] == "Total" {
			continue
		}
		total, totalOK := parseUint(fields[1])
		used, usedOK := parseUint(fields[2])
		if !totalOK || !usedOK {
			continue
		}
		sumTotal += total
		sumUsed += used
		rows++
	}
	if rows == 0 {
		return 0, 0, false
	}
	return sumTotal, sumUsed, true
}

// parseUint parses an unsigned decimal field, reporting false rather than
// coercing: a "-" or blank cell in swapinfo output means "not reported", and
// treating that as 0 would understate both the total and the used count.
func parseUint(s string) (uint64, bool) {
	if s == "" {
		return 0, false
	}
	var v uint64
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
		v = v*10 + uint64(c-'0')
	}
	return v, true
}
