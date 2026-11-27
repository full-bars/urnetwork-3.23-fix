package docs

import (
	"os"
	"strings"
	"testing"
)

func TestP2PMonitoringGuidance(t *testing.T) {
	data, err := os.ReadFile("Monitoring.md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	var zero, rejected string
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "| `") && strings.Contains(line, "decoded") && strings.Contains(line, "busy node") {
			zero = line
		}
		if strings.HasPrefix(line, "| `") && strings.Contains(line, "rejected") && strings.Contains(line, "rises") {
			rejected = line
		}
	}
	if !strings.Contains(zero, "count mode throughout the observation period") {
		t.Errorf("zero-count guidance must require count mode throughout the observation period: %q", zero)
	}
	if !strings.Contains(rejected, "does not by itself establish spoofing") || !strings.Contains(rejected, "older sequence") {
		t.Errorf("rejected guidance must explain ordinary discards and avoid claiming spoofing: %q", rejected)
	}
	if strings.Contains(text, `outcome="forwarded"`) || strings.Contains(text, "| `forwarded` |") {
		t.Error("monitoring must not describe an unexported forwarded outcome")
	}
	changelog, err := os.ReadFile("../CHANGELOG.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(changelog), "\n") {
		if strings.Contains(line, "Peer-to-peer source metrics") && strings.Contains(line, "`forwarded`") {
			t.Error("changelog must not describe an unexported forwarded outcome")
		}
	}
}
