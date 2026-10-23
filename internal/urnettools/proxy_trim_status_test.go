package urnettools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stubTrimSnapshot swaps the snapshot seam for the trim report tests.
func stubTrimSnapshot(t *testing.T, snap *NodeSnapshot, err error) {
	t.Helper()
	orig := fetchSnapshotFn
	t.Cleanup(func() { fetchSnapshotFn = orig })
	fetchSnapshotFn = func(Provider) (*NodeSnapshot, json.RawMessage, error) {
		if err != nil {
			return nil, nil, err
		}
		return snap, nil, nil
	}
}

func trimProvider(t *testing.T, trimFile string) Provider {
	t.Helper()
	dir := t.TempDir()
	if trimFile != "" {
		if err := os.WriteFile(filepath.Join(dir, "proxy_trim"), []byte(trimFile), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return Provider{User: "op", Unit: "urnetwork.service", StateDir: dir}
}

func TestProxyTrimNoArgsReportsCapAndReduction(t *testing.T) {
	p := trimProvider(t, "500\n")
	stubTrimSnapshot(t, &NodeSnapshot{Proxies: SnapshotProxies{
		Up: 480, Connecting: 12, Dead: 8,
		TrimReported: true, Configured: 1200, TrimCap: 500, TrimCapSource: "operator",
	}}, nil)

	out := captureStdout(t, func() {
		if err := printTrimStatus(p); err != nil {
			t.Fatal(err)
		}
	})
	for _, want := range []string{
		"Trim cap: 500 running proxies (operator cap)",
		"Configured pool: 1200, holding 700 back",
		"Running now: 480 up, 12 connecting, 0 dropped, 8 dead (500 total)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestProxyTrimNoArgsReportsNoCap(t *testing.T) {
	p := trimProvider(t, "")
	stubTrimSnapshot(t, &NodeSnapshot{Proxies: SnapshotProxies{Up: 1200, TrimReported: true, Configured: 1200}}, nil)

	out := captureStdout(t, func() {
		if err := printTrimStatus(p); err != nil {
			t.Fatal(err)
		}
	})
	for _, want := range []string{
		"Trim cap: none, this provider runs its full pool",
		"Configured pool: 1200, no cap applied",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestProxyTrimNoArgsReportsOffAndGarbage(t *testing.T) {
	for _, tc := range []struct{ file, want string }{
		{"off\n", "Trim cap: none, this provider runs its full pool"},
		{"0\n", "Trim cap: none, this provider runs its full pool"},
	} {
		p := trimProvider(t, tc.file)
		stubTrimSnapshot(t, &NodeSnapshot{Proxies: SnapshotProxies{Up: 3, TrimReported: true, Configured: 3}}, nil)
		out := captureStdout(t, func() {
			if err := printTrimStatus(p); err != nil {
				t.Fatal(err)
			}
		})
		if !strings.Contains(out, tc.want) {
			t.Errorf("file %q: output missing %q:\n%s", tc.file, tc.want, out)
		}
	}
}

func TestProxyTrimNoArgsStillReportsCapWhenProviderIsDown(t *testing.T) {
	p := trimProvider(t, "250\n")
	stubTrimSnapshot(t, nil, errSnapshotUnavailable)

	out := captureStdout(t, func() {
		if err := printTrimStatus(p); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "Trim cap: 250 running proxies (operator)") {
		t.Errorf("cap missing when provider is down:\n%s", out)
	}
	if !strings.Contains(out, "not answering") {
		t.Errorf("expected the unavailable notice:\n%s", out)
	}
}

// A provider too old to report trim state falls back to the operator file, and
// an unparseable file is called out rather than read as a cap.
func TestProxyTrimNoArgsGarbageFileOnOldProvider(t *testing.T) {
	p := trimProvider(t, "lots")
	stubTrimSnapshot(t, &NodeSnapshot{Proxies: SnapshotProxies{Up: 3}}, nil)
	out := captureStdout(t, func() {
		if err := printTrimStatus(p); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, `Trim cap: none ("lots" in`) {
		t.Errorf("garbage file not reported:\n%s", out)
	}
}

// An unreadable operator file must not hide a cap the provider is enforcing:
// the provider's report is authoritative and needs no file access.
func TestProxyTrimNoArgsUnreadableFileWithReportingProvider(t *testing.T) {
	p := trimProvider(t, "")
	// A directory at the file path makes the read fail for a reason other
	// than "not set".
	if err := os.Mkdir(filepath.Join(p.StateDir, "proxy_trim"), 0o755); err != nil {
		t.Fatal(err)
	}
	stubTrimSnapshot(t, &NodeSnapshot{Proxies: SnapshotProxies{
		Up: 500, TrimReported: true, Configured: 1200, TrimCap: 500, TrimCapSource: "operator",
	}}, nil)

	out := captureStdout(t, func() {
		if err := printTrimStatus(p); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "Trim cap: 500 running proxies (operator cap)") {
		t.Errorf("provider cap not reported over an unreadable file:\n%s", out)
	}
	if strings.Contains(out, "could not read") {
		t.Errorf("reported a file error even though the provider answered:\n%s", out)
	}
}

// A reporting provider with an empty pool still counts as reporting: the
// presence flag keeps "no cap on zero proxies" from looking like "unreported".
func TestProxyTrimNoArgsReportedEmptyPool(t *testing.T) {
	p := trimProvider(t, "500")
	stubTrimSnapshot(t, &NodeSnapshot{Proxies: SnapshotProxies{TrimReported: true}}, nil)
	out := captureStdout(t, func() {
		if err := printTrimStatus(p); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "Trim cap: none, this provider runs its full pool") {
		t.Errorf("reported provider with an empty pool misread:\n%s", out)
	}
	if strings.Contains(out, "500") {
		t.Errorf("fell back to the file despite a provider report:\n%s", out)
	}
}
