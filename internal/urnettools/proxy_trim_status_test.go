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
		Configured: 1200, TrimCap: 500, TrimCapSource: "operator",
	}}, nil)

	out := captureStdout(t, func() {
		if err := printTrimStatus(p); err != nil {
			t.Fatal(err)
		}
	})
	for _, want := range []string{
		"Trim cap: 500 running proxies (operator)",
		"Configured pool: 1200, holding 700 back (operator cap)",
		"Running now: 480 up, 12 connecting, 0 dropped, 8 dead (500 total)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestProxyTrimNoArgsReportsNoCap(t *testing.T) {
	p := trimProvider(t, "")
	stubTrimSnapshot(t, &NodeSnapshot{Proxies: SnapshotProxies{Up: 1200, Configured: 1200}}, nil)

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
		{"lots", `Trim cap: none ("lots" in`},
	} {
		p := trimProvider(t, tc.file)
		stubTrimSnapshot(t, &NodeSnapshot{Proxies: SnapshotProxies{Up: 3, Configured: 3}}, nil)
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
