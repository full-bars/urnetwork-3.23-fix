package connect

import (
	"fmt"
	"runtime"
	"strings"
	"sync"
	"time"
)

// ErrorCategory identifies the subsystem an error originates from. A small
// fixed set keeps the /metrics/errors view readable.
type ErrorCategory string

const (
	ErrorTransport ErrorCategory = "transport"
	ErrorIP        ErrorCategory = "ip"
	ErrorProxy     ErrorCategory = "proxy"
	ErrorWebRTC    ErrorCategory = "webrtc"
)

// maxRecentErrors bounds how many error strings each category retains for the
// /metrics/errors view, so the in-process buffer cannot grow without bound.
const maxRecentErrors = 100

// ErrorTracker holds a per-category recent-error ring buffer. Rate limiting
// reuses the fork's logThrottle (lock-free, O(1)) instead of a bespoke
// limiter, keeping this consistent with how the rest of the code base
// throttles high-volume log lines.
type ErrorTracker struct {
	mu         sync.Mutex
	recent     map[ErrorCategory][]string
	throttles  map[ErrorCategory]*logThrottle
	throttleMu sync.Mutex
}

var globalErrorTracker = &ErrorTracker{
	recent:    map[ErrorCategory][]string{},
	throttles: map[ErrorCategory]*logThrottle{},
}

// throttleFor returns (creating on first use) the rate limiter for a
// category. Guarded so first-use from concurrent goroutines cannot race.
func (t *ErrorTracker) throttleFor(cat ErrorCategory) *logThrottle {
	t.throttleMu.Lock()
	defer t.throttleMu.Unlock()
	th, ok := t.throttles[cat]
	if !ok {
		th = newLogThrottle(time.Minute)
		t.throttles[cat] = th
	}
	return th
}

// RecordError records a rate-limited categorized error with a truncated stack
// trace. Errors that exceed the category's rate limit are suppressed (counted
// by logThrottle) and not buffered. Recording never panics for an unknown
// category: an unknown category gets its own limiter and buffer slot.
func RecordError(cat ErrorCategory, msg string) {
	// capture the stack before the rate-limit check so suppressed errors do
	// not pay the stack-walk cost.
	stack := captureStack(3, 8)
	allowed, _ := globalErrorTracker.throttleFor(cat).Allow(time.Now())
	if !allowed {
		return
	}
	entry := fmt.Sprintf("%s (%s)", msg, stack)
	globalErrorTracker.mu.Lock()
	list := globalErrorTracker.recent[cat]
	list = append(list, entry)
	if len(list) > maxRecentErrors {
		list = list[len(list)-maxRecentErrors:]
	}
	globalErrorTracker.recent[cat] = list
	globalErrorTracker.mu.Unlock()
}

// ErrorMetrics exposes the recent-error buffer as a JSON-friendly map, keyed
// by category, for the /metrics/errors endpoint. The returned slices are
// copies, so concurrent RecordError appends cannot race with serialization.
func (t *ErrorTracker) ErrorMetrics() map[string]any {
	t.mu.Lock()
	out := map[string]any{}
	for cat, list := range t.recent {
		cp := append([]string(nil), list...)
		out[string(cat)] = map[string]any{
			"count":  len(list),
			"recent": cp,
		}
	}
	t.mu.Unlock()
	return out
}

// captureStack returns a compact "file:line file:line ..." stack trace with at
// most maxDepth frames, skipping skip callers. It never allocates unless there
// is a real stack to walk.
func captureStack(skip, maxDepth int) string {
	pcs := make([]uintptr, maxDepth)
	n := runtime.Callers(skip, pcs)
	if n == 0 {
		return ""
	}
	frames := runtime.CallersFrames(pcs[:n])
	var b strings.Builder
	count := 0
	for {
		frame, more := frames.Next()
		if count > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(fmt.Sprintf("%s:%d", shortFile(frame.File), frame.Line))
		count++
		if !more || count >= maxDepth {
			break
		}
	}
	return b.String()
}

// shortFile trims the leading directory path from a file path so stack
// entries stay compact.
func shortFile(path string) string {
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[i+1:]
	}
	return path
}

// ErrorMetrics is the package-level view of the global tracker, for the
// provider's /metrics/errors handler.
func ErrorMetrics() map[string]any {
	return globalErrorTracker.ErrorMetrics()
}
