package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Every extender log line opens with the bridge, so `grep 🌉` finds all of them
// (and `grep '🌉❌'` every failure), however busy the log is. This scans the
// source for a log literal that opens with the bare tag instead. The one
// legitimate bare tag is the important-log routing marker, which must stay bare
// so it matches lines that carry any outcome mark.
func TestExtenderLogLinesCarryTheBridge(t *testing.T) {
	roots := []string{"..", "../extender", "../gossip", "."}
	var offenders []string
	for _, root := range roots {
		files, err := filepath.Glob(filepath.Join(root, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, file := range files {
			if strings.HasSuffix(file, "_test.go") || filepath.Base(file) == "important_log.go" {
				continue
			}
			b, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			for i, line := range strings.Split(string(b), "\n") {
				if strings.Contains(line, `"[extender]`) || strings.Contains(line, `"[health][extender]`) {
					offenders = append(offenders, file+":"+strconv.Itoa(i+1)+": "+strings.TrimSpace(line))
				}
			}
		}
	}
	if len(offenders) != 0 {
		t.Fatalf("extender log lines must open with the bridge (🌉, then an outcome mark where one applies):\n%s", strings.Join(offenders, "\n"))
	}
}

func TestExtenderImportantMarkerStillMatchesEveryOutcomeMark(t *testing.T) {
	for _, line := range []string{
		"🌉 [extender] stopping: x",
		"🌉✅ [extender] started",
		"🌉❌ [extender] CANNOT USE tcp 443",
		"🌉⛔ [extender] IPv4: refused",
		"🌉⚠️ [extender] ignoring",
		"🌉❤️ [health][extender] serving",
	} {
		if !isImportantLogLine(line) {
			t.Errorf("%q would not reach the important log", line)
		}
	}
}
