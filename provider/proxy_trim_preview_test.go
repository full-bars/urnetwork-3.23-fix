package main

import (
	"strings"
	"testing"

	"github.com/urnetwork/connect"
)

// `proxy trim N --preview` used to compute the running set in the CLI's own
// short-lived process, where the health registry is empty, so it always said
// "running=0 <= N, nothing to shed" no matter how many proxies the provider
// ran. The preview must be computed by the RUNNING provider and asked for over
// the control socket.
func TestTrimPreviewText_ListsWorstAndReportsCounts(t *testing.T) {
	state := &ProxyState{Proxies: map[string]ProxyEntry{
		"a:1": {Health: "up"},
		"b:1": {Health: "dead"},
		"c:1": {Health: "up"},
	}}
	got := trimPreviewText(state, nil, []string{"a:1", "b:1", "c:1"}, map[string]uint64{"a:1": 100}, 2)
	if !strings.Contains(got, "preview: 3 running; would shed 1 worst-graded to reach 2:") {
		t.Fatalf("header wrong:\n%s", got)
	}
	if !strings.Contains(got, "b:1") || strings.Contains(got, "a:1") {
		t.Fatalf("must shed the dead proxy and keep the earner:\n%s", got)
	}
	if none := trimPreviewText(state, nil, []string{"a:1"}, nil, 5); !strings.Contains(none, "running=1 <= 5, nothing to shed") {
		t.Fatalf("under-cap text wrong: %q", none)
	}
}

func TestControlTrimPreview_ComputedInsideTheRunningProvider(t *testing.T) {
	withTempHome(t)
	connect.ResetProxyHealthForTesting()
	t.Cleanup(connect.ResetProxyHealthForTesting)
	for i, addr := range []string{"10.0.0.1:1080", "10.0.0.2:1080", "10.0.0.3:1080"} {
		registerBandwidthProxy(i+1, addr, addr, 0)
	}
	if err := writeProxyState(&ProxyState{Proxies: map[string]ProxyEntry{
		"10.0.0.1:1080": {Health: "up"}, "10.0.0.2:1080": {Health: "dead"}, "10.0.0.3:1080": {Health: "up"},
	}}); err != nil {
		t.Fatal(err)
	}

	resp := handleControlRequest(newControlState(), controlRequest{Cmd: "trim_preview", Value: "2"})
	if !resp.OK {
		t.Fatalf("trim_preview failed: %s", resp.Error)
	}
	if !strings.Contains(resp.Value, "preview: 3 running; would shed 1 worst-graded to reach 2:") {
		t.Fatalf("live preview must see the 3 running proxies, got:\n%s", resp.Value)
	}

	for _, bad := range []string{"", "abc", "0", "-3"} {
		if r := handleControlRequest(newControlState(), controlRequest{Cmd: "trim_preview", Value: bad}); r.OK {
			t.Fatalf("trim_preview %q must be rejected", bad)
		}
	}
}

// The direct transport registers itself in the health registry as "direct", but
// the live trim never counts or sheds it (reload skips directProxyKey, and the
// cap applies to non-direct proxies only). The preview must agree: with two
// proxies plus direct and a cap of 2, the real trim sheds nothing, so the
// preview must not claim it would shed one.
func TestTrimPreviewIgnoresTheDirectTransport(t *testing.T) {
	withTempHome(t)
	connect.ResetProxyHealthForTesting()
	t.Cleanup(connect.ResetProxyHealthForTesting)
	connect.RegisterProxy(0, "direct", directProxyKey)
	registerBandwidthProxy(1, "10.0.0.1:1080", "10.0.0.1:1080", 0)
	registerBandwidthProxy(2, "10.0.0.2:1080", "10.0.0.2:1080", 0)
	if err := writeProxyState(&ProxyState{Proxies: map[string]ProxyEntry{
		"10.0.0.1:1080": {Health: "up"}, "10.0.0.2:1080": {Health: "up"},
	}}); err != nil {
		t.Fatal(err)
	}

	resp := handleControlRequest(newControlState(), controlRequest{Cmd: "trim_preview", Value: "2"})
	if !resp.OK {
		t.Fatalf("trim_preview failed: %s", resp.Error)
	}
	if !strings.Contains(resp.Value, "running=2 <= 2, nothing to shed") {
		t.Fatalf("2 proxies plus direct under a cap of 2 sheds nothing; preview said:\n%s", resp.Value)
	}
	if strings.Contains(resp.Value, directProxyKey+"\n") {
		t.Fatalf("the direct transport must never appear as a shed candidate:\n%s", resp.Value)
	}
}
