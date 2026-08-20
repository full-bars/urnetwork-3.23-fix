package connect

import (
	"fmt"
	"runtime"
	"strings"
	"sync"
	"time"
)

// ErrorCategory classifies errors by subsystem.
type ErrorCategory string

const (
	ErrorTransport ErrorCategory = "transport"
	ErrorIP        ErrorCategory = "ip"
	ErrorProxy     ErrorCategory = "proxy"
	ErrorWebRTC    ErrorCategory = "webrtc"
)

// RateLimiter allows at most `limit` events per `window`. Excess calls are throttled.
type RateLimiter struct {
	mu     sync.Mutex
	window time.Duration
	limit  int
	counts []time.Time
}

func NewRateLimiter(limit int, window time.Duration) *RateLimiter {
	return &RateLimiter{window: window, limit: limit}
}

// Allow reports whether an event may proceed under the current rate limit.
func (r *RateLimiter) Allow() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	cutoff := now.Add(-r.window)
	kept := r.counts[:0]
	for _, t := range r.counts {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= r.limit {
		r.counts = kept
		return false
	}
	r.counts = append(kept, now)
	return true
}

// ErrorTracker records categorized, rate-limited errors with stack traces.
type ErrorTracker struct {
	mu     sync.Mutex
	errors map[ErrorCategory][]string
	recent map[ErrorCategory]*RateLimiter
	lastAt map[ErrorCategory]time.Time
}

var globalErrorTracker = &ErrorTracker{
	errors: map[ErrorCategory][]string{},
	recent: map[ErrorCategory]*RateLimiter{
		ErrorTransport: NewRateLimiter(100, time.Minute),
		ErrorIP:        NewRateLimiter(100, time.Minute),
		ErrorProxy:     NewRateLimiter(100, time.Minute),
		ErrorWebRTC:    NewRateLimiter(100, time.Minute),
	},
	lastAt: map[ErrorCategory]time.Time{},
}

// Record logs an error under a category. Stack trace is captured. Rate-limited.
func RecordError(cat ErrorCategory, msg string) {
	if !globalErrorTracker.recent[cat].Allow() {
		return
	}
	globalErrorTracker.mu.Lock()
	defer globalErrorTracker.mu.Unlock()
	stack := captureStack(3)
	globalErrorTracker.errors[cat] = append(globalErrorTracker.errors[cat], fmt.Sprintf("%s (%s)", msg, stack))
	if cap(globalErrorTracker.errors[cat]) > 100 {
		globalErrorTracker.errors[cat] = globalErrorTracker.errors[cat][len(globalErrorTracker.errors[cat])-100:]
	}
	globalErrorTracker.lastAt[cat] = time.Now()
}

// ErrorMetrics returns a JSON-friendly snapshot of tracked errors.
func ErrorMetrics() map[string]interface{} {
	globalErrorTracker.mu.Lock()
	defer globalErrorTracker.mu.Unlock()
	out := map[string]interface{}{}
	for cat, list := range globalErrorTracker.errors {
		out[string(cat)] = map[string]interface{}{
			"count":   len(list),
			"recent":  list,
			"last_at": globalErrorTracker.lastAt[cat].Format(time.RFC3339),
		}
	}
	return out
}

func captureStack(skip int) string {
	var sb strings.Builder
	for i := skip; i < skip+4 && i < 8; i++ {
		_, file, line, ok := runtime.Caller(i)
		if !ok {
			break
		}
		if idx := strings.LastIndex(file, "/"); idx >= 0 {
			file = file[idx+1:]
		}
		sb.WriteString(fmt.Sprintf("%s:%d ", file, line))
	}
	return strings.TrimSpace(sb.String())
}
