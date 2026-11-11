package urnettools

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProxyHealthBoundedOutput(t *testing.T) {
	if dir := os.Getenv("PROXY_HEALTH_TEST_DIR"); dir != "" {
		if err := cmdProxyHealthTarget(Provider{StateDir: dir}); err != nil {
			t.Fatal(err)
		}
		return
	}
	dir := t.TempDir()
	state := "dead=3 degraded=2\n"
	if err := os.WriteFile(filepath.Join(dir, "proxy_health.state"), []byte(state), 0o644); err != nil {
		t.Fatal(err)
	}
	var log strings.Builder
	log.WriteString("outside-tail RETAIN\n")
	for i := 0; i < 17; i++ {
		fmt.Fprintf(&log, "event-%02d\n", i)
	}
	log.WriteString("RETAIN noisy\nretain_ack noisy\nlast-event")
	logPath := filepath.Join(dir, "proxy_health.log")
	if err := os.WriteFile(logPath, []byte(log.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestProxyHealthBoundedOutput$")
	cmd.Env = append(os.Environ(), "PROXY_HEALTH_TEST_DIR="+dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("health command did not exit successfully: %v (context: %v), output: %s", err, ctx.Err(), out)
	}
	text := string(out)
	if !strings.Contains(text, state) || !strings.Contains(text, "event-00\n") || !strings.Contains(text, "last-event\n(2 RETAIN/retain_ack lines omitted)") {
		t.Fatalf("missing snapshot, tail or omission count: %s", text)
	}
	if strings.Index(text, state) > strings.Index(text, "event-00") || strings.Contains(text, "outside-tail") || strings.Contains(text, "RETAIN noisy") || strings.Contains(text, "retain_ack noisy") {
		t.Fatalf("snapshot order, tail bound or filtering incorrect: %s", text)
	}
	got, err := os.ReadFile(logPath)
	if err != nil || string(got) != log.String() {
		t.Fatalf("event log changed: %v", err)
	}
}
