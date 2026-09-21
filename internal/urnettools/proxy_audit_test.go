package urnettools

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFormatProxyAuditStatus(t *testing.T) {
	now := time.Date(2026, 9, 20, 22, 0, 0, 0, time.UTC)

	cases := []struct {
		name string
		as   *ProxyAuditStatus
		want []string // substrings that must appear, in order, one per line
	}{
		{"nil prints nothing", nil, nil},
		{
			"observing",
			&ProxyAuditStatus{Acting: false, WouldPark: 3},
			[]string{"proxy audit: observing only, 3 would be parked (turn proxy audit on to act: urnet-tools proxy audit on)"},
		},
		{
			"observing because audit is off",
			&ProxyAuditStatus{Acting: false, WouldPark: 3, NotActingReason: "audit-off"},
			[]string{"proxy audit: observing only, 3 would be parked (proxy audit is off; turn it on to act: urnet-tools proxy audit on)"},
		},
		{
			"observing because hot restart is off",
			&ProxyAuditStatus{Acting: false, WouldPark: 2, NotActingReason: "hot-restart-off"},
			[]string{"proxy audit: observing only, 2 would be parked (proxy audit is on, but parking also needs hot restart: urnet-tools hot-restart on)"},
		},
		{
			"paused",
			&ProxyAuditStatus{Acting: true, Paused: true, PausedSince: now.Add(-90 * time.Minute)},
			[]string{"proxy audit: acting, none parked, 0 parks in the last 24h", "proxy audit: PAUSED for 1h30m: the paid proxy list is unreadable or empty, so nothing is parked until it can be read again"},
		},
		{
			"observing with nothing to do",
			&ProxyAuditStatus{Acting: false},
			[]string{"proxy audit: observing only, nothing would be parked"},
		},
		{
			"acting with parks",
			&ProxyAuditStatus{
				Acting:   true,
				Parks24h: 2,
				Parked: []ProxyAuditParked{
					{Addr: "10.0.0.1:1080", Until: now.Add(5*time.Hour + 59*time.Minute)},
					{Addr: "10.0.0.2:1080", Until: now.Add(30 * time.Hour)},
				},
			},
			[]string{
				"proxy audit: acting, 2 parked, 2 parks in the last 24h",
				"  parked 10.0.0.1:1080 for another 5h59m",
				"  parked 10.0.0.2:1080 for another 30h0m",
			},
		},
		{
			"acting with none parked",
			&ProxyAuditStatus{Acting: true},
			[]string{"proxy audit: acting, none parked, 0 parks in the last 24h"},
		},
		{
			"distrusted pass explained",
			&ProxyAuditStatus{Acting: true, Distrusted: true},
			[]string{"proxy audit: acting", "  last pass distrusted"},
		},
		{
			"thin pass explained",
			&ProxyAuditStatus{Acting: true, Thin: true},
			[]string{"proxy audit: acting", "  last pass thin"},
		},
		{
			"an already-lapsed backoff is not shown as negative",
			&ProxyAuditStatus{Acting: true, Parked: []ProxyAuditParked{{Addr: "10.0.0.3:1080", Until: now.Add(-time.Minute)}}},
			[]string{"proxy audit: acting, 1 parked", "  parked 10.0.0.3:1080 for another 0m"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := formatProxyAuditStatus(tc.as, now)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d lines %q, want %d", len(got), got, len(tc.want))
			}
			for i, w := range tc.want {
				if !strings.Contains(got[i], w) {
					t.Fatalf("line %d = %q, want it to contain %q", i, got[i], w)
				}
			}
		})
	}
}

// startAuditSocket serves one canned reply on sockPath.
func startAuditSocket(t *testing.T, sockPath string, replyHandler func(req controlRequest) string) {
	t.Helper()
	l, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				sc := bufio.NewScanner(c)
				for sc.Scan() {
					var req controlRequest
					_ = json.Unmarshal(sc.Bytes(), &req)
					resp := replyHandler(req)
					_, _ = c.Write([]byte(resp + "\n"))
				}
			}(conn)
		}
	}()
}

func shortSocketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ug")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func TestCmdProxyAuditStatus_PrintsAuditLines(t *testing.T) {
	stateDir := shortSocketDir(t)
	startAuditSocket(t, filepath.Join(stateDir, "provider.sock"), func(req controlRequest) string {
		return `{"ok":true,"proxy_audit":{"acting":true,"parks_24h":1,"parked":[{"addr":"10.0.0.9:1080","until":"2999-01-01T00:00:00Z"}]}}`
	})

	origP, origS := discoverProcessesFn, discoverStoppedFn
	discoverProcessesFn = func() []Provider {
		return []Provider{{User: "test-user", StateDir: stateDir, Unit: "urnetwork.service", Running: true}}
	}
	discoverStoppedFn = func([]Provider) []Provider { return nil }
	defer func() { discoverProcessesFn, discoverStoppedFn = origP, origS }()

	out := captureStdout(t, func() {
		if err := cmdProxy([]string{"audit", "status", "--state-dir", stateDir}, false, false); err != nil {
			t.Errorf("audit status: %v", err)
		}
	})
	if !strings.Contains(out, "proxy audit: acting, 1 parked") || !strings.Contains(out, "10.0.0.9:1080") {
		t.Fatalf("status should print audit lines, got %q", out)
	}
}

func TestCmdProxyAuditOnAndOff_ConfirmsToUser(t *testing.T) {
	stateDir := shortSocketDir(t)
	startAuditSocket(t, filepath.Join(stateDir, "provider.sock"), func(req controlRequest) string {
		if req.Cmd == "audit" && req.Action == "on" {
			return `{"ok":true,"value":"enabled"}`
		}
		if req.Cmd == "audit" && req.Action == "off" {
			return `{"ok":true,"value":"disabled"}`
		}
		return `{"ok":false,"error":"unknown"}`
	})

	origP, origS := discoverProcessesFn, discoverStoppedFn
	discoverProcessesFn = func() []Provider {
		return []Provider{{User: "test-user", StateDir: stateDir, Unit: "urnetwork.service", Running: true}}
	}
	discoverStoppedFn = func([]Provider) []Provider { return nil }
	defer func() { discoverProcessesFn, discoverStoppedFn = origP, origS }()

	onOut := captureStdout(t, func() {
		if err := cmdProxy([]string{"audit", "on", "--state-dir", stateDir}, false, false); err != nil {
			t.Errorf("audit on: %v", err)
		}
	})
	if !strings.Contains(onOut, "✓ Proxy audit enabled") {
		t.Fatalf("expected affirmative confirmation for on, got %q", onOut)
	}

	offOut := captureStdout(t, func() {
		if err := cmdProxy([]string{"audit", "off", "--state-dir", stateDir}, false, false); err != nil {
			t.Errorf("audit off: %v", err)
		}
	})
	if !strings.Contains(offOut, "✓ Proxy audit disabled") {
		t.Fatalf("expected affirmative confirmation for off, got %q", offOut)
	}
}

func TestCmdProxyAuditRelease_ConfirmsToUser(t *testing.T) {
	stateDir := shortSocketDir(t)
	startAuditSocket(t, filepath.Join(stateDir, "provider.sock"), func(req controlRequest) string {
		if req.Cmd == "audit" && req.Action == "release" {
			if req.Address == "--all" {
				return `{"ok":true,"value":"released 2 proxies"}`
			}
			return fmt.Sprintf(`{"ok":true,"value":"released proxy %s"}`, req.Address)
		}
		return `{"ok":false,"error":"unknown"}`
	})

	origP2, origS2 := discoverProcessesFn, discoverStoppedFn
	discoverProcessesFn = func() []Provider {
		return []Provider{{User: "test-user", StateDir: stateDir, Unit: "urnetwork.service", Running: true}}
	}
	discoverStoppedFn = func([]Provider) []Provider { return nil }
	defer func() { discoverProcessesFn, discoverStoppedFn = origP2, origS2 }()

	relOut := captureStdout(t, func() {
		if err := cmdProxy([]string{"audit", "release", "1.2.3.4:1080", "--state-dir", stateDir}, false, false); err != nil {
			t.Errorf("audit release: %v", err)
		}
	})
	if !strings.Contains(relOut, "✓ Released proxy 1.2.3.4:1080") {
		t.Fatalf("expected affirmative confirmation for single release, got %q", relOut)
	}

	allOut := captureStdout(t, func() {
		if err := cmdProxy([]string{"audit", "release", "--all", "--state-dir", stateDir}, false, false); err != nil {
			t.Errorf("audit release --all: %v", err)
		}
	})
	if !strings.Contains(allOut, "✓ Released all parked proxies") {
		t.Fatalf("expected affirmative confirmation for release all, got %q", allOut)
	}
}
