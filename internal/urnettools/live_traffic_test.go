package urnettools

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
)

// serveCmdOnce answers one control request for cmd on a fresh unix socket.
func serveCmdOnce(t *testing.T, cmd, reply string) Provider {
	t.Helper()
	dir, err := os.MkdirTemp("", "traf")
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
		if string(buf[:n]) != `{"cmd":"`+cmd+`"}`+"\n" {
			reply = `{"ok":false,"error":"bad request ` + string(buf[:n]) + `"}`
		}
		c.Write([]byte(reply + "\n"))
	}()
	return Provider{StateDir: dir}
}

func TestFetchLiveTrafficRoundTrip(t *testing.T) {
	p := serveCmdOnce(t, "traffic", `{"ok":true,"traffic":{"at_unix_nano":1790000000123456789,"billable_bytes":700,"total_bytes":2100}}`)
	lt, err := fetchLiveTraffic(p)
	if err != nil {
		t.Fatal(err)
	}
	if lt.AtUnixNano != 1790000000123456789 || lt.BillableBytes != 700 || lt.TotalBytes != 2100 {
		t.Fatalf("got %+v", lt)
	}
}

// A provider that predates the command answers "unknown command". top must tell
// that apart from a silent provider, so it can keep going on the snapshot's own
// rates instead of reporting the provider lost.
func TestFetchLiveTrafficOldProviderIsUnsupportedNotUnavailable(t *testing.T) {
	p := serveCmdOnce(t, "traffic", `{"ok":false,"error":"unknown command \"traffic\""}`)
	_, err := fetchLiveTraffic(p)
	if !errors.Is(err, errTrafficUnsupported) || errors.Is(err, errSnapshotUnavailable) {
		t.Fatalf("err = %v, want errTrafficUnsupported", err)
	}
}

func TestFetchLiveTrafficOtherFailuresAreUnavailable(t *testing.T) {
	for name, reply := range map[string]string{
		"provider error": `{"ok":false,"error":"boom"}`,
		"no payload":     `{"ok":true}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := fetchLiveTraffic(serveCmdOnce(t, "traffic", reply))
			if !errors.Is(err, errSnapshotUnavailable) || errors.Is(err, errTrafficUnsupported) {
				t.Fatalf("err = %v, want errSnapshotUnavailable", err)
			}
		})
	}
	if _, err := fetchLiveTraffic(Provider{}); !errors.Is(err, errSnapshotUnavailable) {
		t.Fatalf("no state dir: err = %v", err)
	}
}

// The full fixture carries the traffic block; the minimal one stands for a
// provider that predates it and must decode with none.
func TestSnapshotDecodesTrafficBlockWhenPresent(t *testing.T) {
	full := loadSnapshotFixture(t, "node_snapshot_v1.json")
	if full.Traffic == nil || full.Traffic.TotalBytes != 96636764160 || full.Traffic.LifetimeBillableBytes == nil || len(full.Traffic.TotalHistoryBps) == 0 {
		t.Fatalf("traffic block not decoded: %+v", full.Traffic)
	}
	old := loadSnapshotFixture(t, "node_snapshot_v1_idle_minimal.json")
	if old.Traffic != nil {
		t.Fatalf("an older provider's snapshot decoded a traffic block: %+v", old.Traffic)
	}
}
