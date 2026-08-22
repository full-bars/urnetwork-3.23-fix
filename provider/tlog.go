package main

import (
	"fmt"
	"time"
)

// tlog prints one of the fork's own [tag] status lines (e.g. [health],
// [traffic], [report], [proxy]) prefixed with a glog-style "MMDD HH:MM:SS"
// timestamp. The library's internal logging goes through glog, which stamps
// every line ("I0623 08:09:22.047661 ..."); the fork's status lines were
// written with bare fmt.Printf and so landed in the same stdout stream with no
// timestamp. tlog is a drop-in replacement for fmt.Printf at those call sites
// so both kinds of lines line up. The format mirrors glog's date/time fields
// (local clock, no microseconds/PID) to stay visually aligned without
// duplicating glog's heavier header.
func tlog(format string, args ...any) {
	fmt.Printf("%s "+format, append([]any{time.Now().Format("0102 15:04:05")}, args...)...)
}
