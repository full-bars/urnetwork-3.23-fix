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

// Rotation must not lose writes, and must not put the crossing batch into the
// generation that is about to be renamed. The check happens BEFORE the write
// (an earlier version checked after, and its own comment claimed the opposite),
// so the batch that trips the cap goes into the NEW file.
func TestRetentionWriterRotatesBeforeTheCrossingWrite(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("URNETWORK_PROXY_HEALTH_DIR", dir)
	path := filepath.Join(dir, "proxy_health.log")

	w := newRetentionLogWriter()
	defer w.close()
	w.write([]string{"before-rotation"})
	if b, err := os.ReadFile(path); err != nil || !strings.Contains(string(b), "before-rotation") {
		t.Fatalf("pre-rotation write missing: %v", err)
	}

	// Fill the live file to the cap (sparse, so it costs no disk), then write the
	// batch that trips it. The writer reads the live size before each batch, so
	// the file itself has to be full: setting w.bytes would be overwritten.
	if err := os.Truncate(path, proxyHealthLogMaxBytes); err != nil {
		t.Fatal(err)
	}
	w.write([]string{"crossing-line"})

	// Rotation must already have happened, and the crossing line must be in the
	// NEW file, not in the one that was renamed.
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("rotation did not happen before the crossing write: %v", err)
	}
	w.close()
	live, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(live), "crossing-line") {
		t.Fatalf("the crossing batch must land in the NEW live log, got %q", live)
	}
	if strings.Contains(string(live), "before-rotation") {
		t.Fatal("the pre-rotation line belongs to the rotated generation, not the live log")
	}
	rotated, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rotated), "before-rotation") {
		t.Fatalf("rotation lost the pre-rotation line: %q", rotated)
	}

	// Writing after the rotation must keep working against the new file.
	w2 := newRetentionLogWriter()
	defer w2.close()
	w2.write([]string{"after-rotation"})
	live2, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(live2), "after-rotation") {
		t.Fatalf("writes after a rotation must reach the new log, got %q", live2)
	}
	if !strings.Contains(string(live2), "crossing-line") {
		t.Fatal("the crossing line must still be in the live log after a later write")
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

// The heartbeat writer (writeProxyHealthEvents) rotates proxy_health.log by
// renaming it while the retention writer still holds an open handle. Comparing
// the path string cannot see that: the path is unchanged, the handle now points
// at the renamed generation, and on POSIX writes to it still succeed. Every
// later RETAIN line then went to .1, and the next rotation replaced .1 and
// deleted them. The writer must notice the handle no longer names the live file
// and reopen it.
func TestRetentionWriterReopensAfterTheHeartbeatRotatesTheFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("URNETWORK_PROXY_HEALTH_DIR", dir)
	path := filepath.Join(dir, "proxy_health.log")

	w := newRetentionLogWriter()
	defer w.close()
	w.write([]string{"before-rotation"})

	// the other writer rotates the file and starts a fresh one
	rotateIfNeeded(path, 0)
	if err := os.WriteFile(path, []byte("| heartbeat line |\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	w.write([]string{"after-rotation"})

	live, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(live), "after-rotation") {
		t.Fatalf("a RETAIN line written after the rotation must land in the live file, live=%q", live)
	}
	rotated, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rotated), "after-rotation") {
		t.Fatalf("a RETAIN line written after the rotation went into the rotated generation: %q", rotated)
	}
	if !strings.Contains(string(rotated), "before-rotation") {
		t.Fatalf("the rotated generation must keep what was written before it: %q", rotated)
	}
}

// The byte count that triggers rotation is the writer's own; appends by the
// heartbeat writer make it stale. Re-reading the live size under the lock keeps
// the 20 MB cap honest.
func TestRetentionWriterRefreshesItsSizeFromTheLiveFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("URNETWORK_PROXY_HEALTH_DIR", dir)
	path := filepath.Join(dir, "proxy_health.log")

	w := newRetentionLogWriter()
	defer w.close()
	w.write([]string{"first"})

	// the heartbeat writer appends through its own handle
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(strings.Repeat("h", 4096) + "\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	w.write([]string{"second"})

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if w.bytes < info.Size()-int64(len("| second |")) {
		t.Fatalf("writer counts %d bytes but the live file is %d", w.bytes, info.Size())
	}
}
