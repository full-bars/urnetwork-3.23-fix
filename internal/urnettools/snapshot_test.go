package urnettools

import (
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
)

const livestatusFixtures = "../../testdata/livestatus"

func loadSnapshotFixture(t *testing.T, name string) *NodeSnapshot {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(livestatusFixtures, name))
	if err != nil {
		t.Fatal(err)
	}
	// Decode through controlResponse, as the wire does, so the field tag
	// on controlResponse is covered too.
	wrapped := append(append([]byte(`{"ok":true,"snapshot":`), b...), '}')
	var resp controlResponse
	if err := json.Unmarshal(wrapped, &resp); err != nil {
		t.Fatalf("unmarshal %s: %v", name, err)
	}
	if resp.Snapshot == nil {
		t.Fatalf("%s: snapshot not decoded", name)
	}
	return resp.Snapshot
}

func TestSnapshotContractFull(t *testing.T) {
	s := loadSnapshotFixture(t, "node_snapshot_v1.json")
	if s.V != 1 || s.Version != "v3.23.0-fix.32.1" || s.State != "flowing" || !s.Busy || s.RestartPending {
		t.Fatalf("identity/state wrong: %+v", s)
	}
	if s.PreviousVersion == nil || *s.PreviousVersion != "v3.23.0-fix.32.0" {
		t.Fatalf("previous_version = %v", s.PreviousVersion)
	}
	if s.UptimeSeconds != 273871 {
		t.Fatalf("uptime = %v", s.UptimeSeconds)
	}
	if s.Rate.NowBps != 40058201 || s.Rate.Avg1mBps != 43012770 || s.Rate.Avg5mBps != 37400115 {
		t.Fatalf("rate = %+v", s.Rate)
	}
	if s.Rate.HistoryIntervalSeconds != 1 || len(s.Rate.HistoryBps) != 12 || s.Rate.HistoryBps[0] != 33500000 {
		t.Fatalf("history = %+v", s.Rate)
	}
	if s.Clients != 212 || s.Sessions.PQE != 200 || s.Sessions.Classical != 12 {
		t.Fatalf("clients/sessions = %d %+v", s.Clients, s.Sessions)
	}
	if s.Proxies.Up != 58 || s.Proxies.Degraded != 2 || s.Proxies.Connecting != 0 || s.Proxies.Dead != 0 {
		t.Fatalf("proxies = %+v", s.Proxies)
	}
	if s.Pressure != 0.21 || s.Restart.Reason != "update" || !s.Restart.CleanShutdown {
		t.Fatalf("pressure/restart = %v %+v", s.Pressure, s.Restart)
	}
	r := s.Resources
	if r.HeapInuseBytes != 1932735283 || r.Goroutines != 1204 {
		t.Fatalf("resources = %+v", r)
	}
	if r.MemLimitBytes == nil || *r.MemLimitBytes != 4294967296 ||
		r.RSSBytes == nil || *r.RSSBytes != 2254857830 ||
		r.OpenFDs == nil || *r.OpenFDs != 310 ||
		r.FDLimit == nil || *r.FDLimit != 65536 {
		t.Fatalf("optional resources not decoded: %+v", r)
	}
	if s.IdleHint != nil {
		t.Fatalf("idle_hint = %q on a flowing node", *s.IdleHint)
	}
}

func TestSnapshotContractMinimalLeavesOptionalsNil(t *testing.T) {
	s := loadSnapshotFixture(t, "node_snapshot_v1_idle_minimal.json")
	if s.State != "idle" || s.Restart.Reason != "unclean" || s.Restart.CleanShutdown {
		t.Fatalf("state/restart = %s %+v", s.State, s.Restart)
	}
	if s.PreviousVersion != nil || s.Resources.RSSBytes != nil || s.Resources.OpenFDs != nil || s.Resources.FDLimit != nil {
		t.Fatalf("absent optionals must stay nil: %+v prev=%v", s.Resources, s.PreviousVersion)
	}
	if s.Resources.MemLimitBytes == nil || *s.Resources.MemLimitBytes != 4294967296 {
		t.Fatalf("mem_limit_bytes = %v", s.Resources.MemLimitBytes)
	}
	if s.IdleHint == nil || *s.IdleHint != "all 58 proxies dead or connecting" {
		t.Fatalf("idle_hint = %v", s.IdleHint)
	}
	if s.Proxies.Dead != 46 || s.Proxies.Connecting != 12 || s.Rate.NowBps != 0 {
		t.Fatalf("proxies/rate = %+v %+v", s.Proxies, s.Rate)
	}
}

func TestSnapshotIgnoresUnknownFields(t *testing.T) {
	in := `{"ok":true,"snapshot":{"v":2,"state":"flowing","future_field":{"a":1},"rate":{"now_bps":5,"extra":true}}}`
	var resp controlResponse
	if err := json.Unmarshal([]byte(in), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Snapshot == nil || resp.Snapshot.V != 2 || resp.Snapshot.Rate.NowBps != 5 {
		t.Fatalf("got %+v", resp.Snapshot)
	}
}

// serveOnce answers one control request on a fresh unix socket with reply.
func serveOnce(t *testing.T, reply string) Provider {
	t.Helper()
	dir, err := os.MkdirTemp("", "snap")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	ln, err := net.Listen("unix", filepath.Join(dir, "provider.sock"))
	if err != nil {
		t.Skipf("unix sockets unavailable: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		buf := make([]byte, 256)
		n, _ := c.Read(buf)
		if string(buf[:n]) != `{"cmd":"snapshot"}`+"\n" {
			reply = `{"ok":false,"error":"bad request ` + string(buf[:n]) + `"}`
		}
		c.Write([]byte(reply + "\n"))
	}()
	return Provider{StateDir: dir}
}

func TestFetchSnapshotRoundTrip(t *testing.T) {
	b, _ := os.ReadFile(filepath.Join(livestatusFixtures, "node_snapshot_v1.json"))
	compact := new(json.RawMessage)
	if err := json.Unmarshal(b, compact); err != nil {
		t.Fatal(err)
	}
	line, _ := json.Marshal(compact)
	p := serveOnce(t, `{"ok":true,"snapshot":`+string(line)+`}`)
	s, err := fetchSnapshot(p)
	if err != nil {
		t.Fatal(err)
	}
	if s.State != "flowing" || s.Clients != 212 {
		t.Fatalf("got %+v", s)
	}
}

func TestFetchSnapshotOldProviderIsUnavailable(t *testing.T) {
	p := serveOnce(t, `{"ok":false,"error":"unknown command \"snapshot\""}`)
	s, err := fetchSnapshot(p)
	if s != nil || !errors.Is(err, errSnapshotUnavailable) {
		t.Fatalf("got %v, %v; want nil, errSnapshotUnavailable", s, err)
	}
}

func TestFetchSnapshotUnreachableIsUnavailable(t *testing.T) {
	s, err := fetchSnapshot(Provider{StateDir: t.TempDir()})
	if s != nil || !errors.Is(err, errSnapshotUnavailable) {
		t.Fatalf("got %v, %v; want nil, errSnapshotUnavailable", s, err)
	}
	s, err = fetchSnapshot(Provider{})
	if s != nil || !errors.Is(err, errSnapshotUnavailable) {
		t.Fatalf("empty state dir: got %v, %v", s, err)
	}
}
