package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The retention writer used to do a full open/stat/write/close cycle PER
// EVENT. On a slow disk the single writer could not drain the 256-slot buffer
// as fast as events arrived, so it went permanently full and the fleet saw a
// steady stream of "retention event buffer was full, dropped N event(s)".
// These tests pin the actual fix: batched appends behind one held handle.

// Every event handed to the writer must land in the file, in order, one line
// each. Batching must not lose, reorder or merge lines.
func TestRetentionWriterWritesEveryEventInOrder(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("URNETWORK_PROXY_HEALTH_DIR", dir)

	w := newRetentionLogWriter()
	defer w.close()
	const n = 500
	for i := 0; i < n; i += 7 { // ragged batches, as the real drain produces
		batch := make([]string, 0, 7)
		for j := i; j < i+7 && j < n; j++ {
			batch = append(batch, "event-"+strings.Repeat("x", j%5)+itoa(j))
		}
		w.write(batch)
	}
	b, err := os.ReadFile(filepath.Join(dir, "proxy_health.log"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) != n {
		t.Fatalf("wrote %d lines, want %d", len(lines), n)
	}
	for i, ln := range lines {
		if !strings.Contains(ln, "RETAIN") {
			t.Fatalf("line %d is not a RETAIN row: %q", i, ln)
		}
		if !strings.Contains(ln, "event-") {
			t.Fatalf("line %d lost its event text: %q", i, ln)
		}
	}
	// Order is preserved: the Nth event appears on the Nth line.
	for i, ln := range lines {
		if !strings.Contains(ln, "event-"+strings.Repeat("x", i%5)+itoa(i)) {
			t.Fatalf("line %d is out of order: %q", i, ln)
		}
	}
}

// The handle must be reused across batches, not reopened per batch: that reuse
// IS the fix. An empty batch must not open anything either.
func TestRetentionWriterReusesTheHandleAcrossBatches(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("URNETWORK_PROXY_HEALTH_DIR", dir)

	w := newRetentionLogWriter()
	if w.f != nil {
		t.Fatal("a fresh writer must hold no handle before its first write")
	}
	w.write([]string{"a"})
	first := w.f
	if first == nil {
		t.Fatal("the first write must open the log")
	}
	w.write([]string{"b"})
	if w.f != first {
		t.Fatal("the handle must be reused across batches; reopening per batch is the bug being fixed")
	}
	w.write([]string{"c"})
	if w.f != first {
		t.Fatal("the handle must still be reused")
	}
	// An empty batch is a no-op and must not disturb the handle.
	w.write(nil)
	if w.f != first {
		t.Fatal("an empty batch must not reopen the log")
	}
	w.close()
	if w.f != nil {
		t.Fatal("close must release the handle")
	}
	// Writing after close reopens rather than writing into a dead handle.
	w.write([]string{"d"})
	if w.f == nil {
		t.Fatal("a write after close must reopen the log")
	}
	w.close()
}

// The batch size is what turns a full buffer into one write; pin it so a
// future "optimisation" back to one-event-per-write is caught.
func TestRetentionBatchDrainsMoreThanOneEvent(t *testing.T) {
	if retentionEventBatch < 8 {
		t.Fatalf("retentionEventBatch = %d; batching by one or two defeats the fix", retentionEventBatch)
	}
	if retentionEventBatch > retentionEventBuffer {
		t.Fatalf("retentionEventBatch (%d) exceeds the channel buffer (%d); it can never be filled",
			retentionEventBatch, retentionEventBuffer)
	}
}

// Rotation must not lose writes: after a rotation the writer reopens, so lines
// written afterwards land in the NEW file rather than the renamed generation.
func TestRetentionWriterReopensAfterRotation(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("URNETWORK_PROXY_HEALTH_DIR", dir)
	path := filepath.Join(dir, "proxy_health.log")

	w := newRetentionLogWriter()
	defer w.close()
	w.write([]string{"before-rotation"})
	if b, err := os.ReadFile(path); err != nil || !strings.Contains(string(b), "before-rotation") {
		t.Fatalf("pre-rotation write missing: %v", err)
	}
	// Push the writer's own byte count to the threshold. Rotation is driven by
	// that count rather than a stat, so this is the real trigger, not a stub.
	w.bytes = proxyHealthLogMaxBytes - 1
	w.write([]string{"crossing-line"}) // the write that trips the threshold
	w.bytes = 0                        // already rotated and reset
	w.write([]string{"after-rotation"})

	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("rotation did not happen: %v", err)
	}
	// The new generation must carry the post-rotation line, and the old handle
	// must not still be writing into the renamed file.
	w.close()
	current, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(current), "after-rotation") {
		t.Fatalf("the post-rotation line is not in the live log: %q", current)
	}
	if strings.Contains(string(current), "before-rotation") {
		t.Fatalf("the pre-rotation line must live in the rotated generation, not the live log")
	}
	rotated, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rotated), "before-rotation") {
		t.Fatalf("rotation lost the pre-rotation line: %q", rotated)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
