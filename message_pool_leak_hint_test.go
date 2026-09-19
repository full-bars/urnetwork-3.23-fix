package connect

import (
	"strings"
	"testing"
)

// TestMessagePoolLeakHintAttribution verifies the leak-reporting snapshot
// surfaced in the [health][pool][leak] heartbeat line. It is the operator
// hook that turns "buffers were taken and never given back" (LA7, 4.19M
// buffers) into a named call site: with URNETWORK_POOL_DEBUG_TAGS=1, a buffer
// that is acquired but never returned must show up as a tag whose leaked
// count grows while its returned ratio stays below 100%.
func TestMessagePoolLeakHintAttribution(t *testing.T) {
	// debugTags is permanently on; just make sure the leak hint reflects a
	// deliberately-leaked buffer so the heartbeat can name the caller.
	if !debugTags {
		t.Fatal("debugTags must be true (on by default)")
	}

	// Sanity: with tags on, a clean get+return cycle must not leak.
	ResetMessagePoolStats()
	b := MessagePoolGet(64)
	MessagePoolReturn(b)
	hints := MessagePoolLeakHint(10)
	for _, h := range hints {
		if h.Leaked == 0 {
			continue
		}
		t.Fatalf("clean get+return must not leak, but tag=%d caller=%s leaked=%d", h.Tag, h.Caller, h.Leaked)
	}

	// Now actually leak one: acquire and never return. The snapshot must
	// reflect it (leaked > 0) so the heartbeat can name the caller.
	ResetMessagePoolStats()
	leaked := MessagePoolGet(2048)
	if leaked == nil {
		t.Fatal("MessagePoolGet(2048) returned nil")
	}
	// Cleanup runs after the assertions, so the leak stays visible to
	// MessagePoolLeakHint while the pool and its counters end up balanced.
	t.Cleanup(func() { MessagePoolReturn(leaked) })
	hints = MessagePoolLeakHint(10)
	found := false
	for _, h := range hints {
		if h.Leaked > 0 {
			found = true
			if h.Caller == "" {
				t.Errorf("leaky tag %d has empty caller (debugTag not stamped?)", h.Tag)
			}
		}
	}
	if !found {
		t.Fatal("deliberately leaked buffer did not appear in MessagePoolLeakHint")
	}
}

// TestMessagePoolLeakHintRequiresDebugTags is retained as documentation of the
// no-tags behavior guard: even if debugTags were ever disabled, the leak hint
// must return nothing rather than a meaningless tag-0 aggregate.
func TestMessagePoolLeakHintRequiresDebugTags(t *testing.T) {
	debugTags = false
	t.Cleanup(func() { debugTags = true })
	ResetMessagePoolStats()
	off := MessagePoolGet(64)
	t.Cleanup(func() { MessagePoolReturn(off) })
	if hints := MessagePoolLeakHint(5); len(hints) != 0 {
		t.Fatalf("MessagePoolLeakHint must be empty when debugTags is off, got %v", hints)
	}
}

// The leak report is only useful if the caller it names is the code that
// acquired the buffer, not a pool wrapper frame. Two distinct call sites of
// the same public getter must also get distinct tags.
func TestMessagePoolLeakHintNamesExternalCaller(t *testing.T) {
	ResetMessagePoolStats()
	first := MessagePoolGet(2048)
	t.Cleanup(func() { MessagePoolReturn(first) })
	second := MessagePoolGet(2048)
	t.Cleanup(func() { MessagePoolReturn(second) })

	hints := MessagePoolLeakHint(10)
	tags := map[uint8]bool{}
	for _, h := range hints {
		if h.Leaked == 0 {
			continue
		}
		tags[h.Tag] = true
		if !strings.Contains(h.Caller, "message_pool_leak_hint_test.go") {
			t.Errorf("tag %d caller %q does not name the acquiring test file", h.Tag, h.Caller)
		}
		if strings.Contains(h.Caller, "message_pool.go") {
			t.Errorf("tag %d caller %q names a pool wrapper frame", h.Tag, h.Caller)
		}
	}
	if len(tags) != 2 {
		t.Fatalf("two distinct MessagePoolGet call sites must get two tags, got %d: %v", len(tags), hints)
	}
}

// A hash collision between two call sites must yield distinct tags so the
// per-tag leak counters stay attributable.
func TestRegisterDebugTagLockedAvoidsCollisions(t *testing.T) {
	debugStateLock.Lock()
	defer debugStateLock.Unlock()

	// Register two fake sites that hash to the same tag.
	const hashed = uint8(200)
	a := registerDebugTagLocked([2]uintptr{0xA1, 0xA2}, [2]uintptr{}, hashed)
	b := registerDebugTagLocked([2]uintptr{0xB1, 0xB2}, [2]uintptr{}, hashed)
	t.Cleanup(func() {
		delete(tagCallers, a)
		delete(tagCallers, b)
	})
	if a == 0 || b == 0 {
		t.Fatalf("tag 0 is reserved for untagged buffers, got %d and %d", a, b)
	}
	if a == b {
		t.Fatalf("colliding call sites must get distinct tags, both got %d", a)
	}
}

// Tags with no outstanding buffers are not leak offenders and must not appear
// in the report, even when fewer than the limit have real leaks.
func TestMessagePoolLeakHintOmitsBalancedTags(t *testing.T) {
	ResetMessagePoolStats()
	b := MessagePoolGet(64)
	MessagePoolReturn(b)
	if hints := MessagePoolLeakHint(10); len(hints) != 0 {
		t.Fatalf("balanced get/return must not be reported, got %v", hints)
	}
}
