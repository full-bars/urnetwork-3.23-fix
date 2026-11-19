package urnettools

import (
	"bytes"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestParseExtenderEventsArgs(t *testing.T) {
	f, err := parseExtenderEventsArgs([]string{"events", "--since", "1h", "--kind", "peer,reject,state,dial", "--json", "--unit", "urnetwork.service"})
	if err != nil {
		t.Fatalf("parseExtenderEventsArgs: %v", err)
	}
	if f.Since != time.Hour || !f.JSONOutput || f.Target.Unit != "urnetwork.service" {
		t.Fatalf("parsed filter = %+v", f)
	}
	for _, k := range []string{"peer", "reject", "state", "dial"} {
		if !f.Kinds[k] {
			t.Fatalf("missing kind %q in %+v", k, f.Kinds)
		}
	}

	fEq, err := parseExtenderEventsArgs([]string{"events", "--since=30m", "--kind=cap,publish"})
	if err != nil {
		t.Fatalf("parseExtenderEventsArgs (=): %v", err)
	}
	if fEq.Since != 30*time.Minute || !fEq.Kinds["cap"] || !fEq.Kinds["publish"] {
		t.Fatalf("parsed (=) filter = %+v", fEq)
	}

	for _, bad := range [][]string{
		{"events", "--since"},
		{"events", "--since", "0s"},
		{"events", "--since", "-5m"},
		{"events", "--since", "notaduration"},
		{"events", "--kind"},
		{"events", "--kind", ""},
		{"events", "--kind", "peer,unknown"},
		{"events", "--force"},
		{"events", "extra"},
	} {
		if _, err := parseExtenderEventsArgs(bad); err == nil {
			t.Fatalf("expected error for args %v", bad)
		}
	}
}

func TestFormatAndFilterExtenderGossipEvents(t *testing.T) {
	now := time.Date(2026, 11, 18, 6, 0, 0, 0, time.UTC)
	events := []ExtenderGossipEvent{
		{Time: now.Add(-2 * time.Hour), Kind: "state", Reason: "waiting"},
		{Time: now.Add(-45 * time.Minute), Kind: "peer", Peer: "12D3KooW", Direction: "in", Reason: "connect"},
		{Time: now.Add(-30 * time.Minute), Kind: "dial", Peer: "12D3KooX", Direction: "out", Reason: "timeout"},
		{Time: now.Add(-10 * time.Minute), Kind: "reject", Peer: "12D3KooY", Direction: "in", Reason: "signature", Size: 184},
		{Time: now.Add(-5 * time.Minute), Kind: "cap", Direction: "in", Reason: "subscriber_cap"},
	}

	filter, err := parseExtenderEventsArgs([]string{"events", "--since", "1h", "--kind", "peer,reject,state,dial"})
	if err != nil {
		t.Fatal(err)
	}

	var textBuf bytes.Buffer
	if err := formatExtenderGossipEvents(&textBuf, events, filter, now); err != nil {
		t.Fatal(err)
	}
	text := textBuf.String()
	// 2h-old state event and cap event are excluded; peer, dial and reject within 1h are included.
	if strings.Contains(text, "waiting") || strings.Contains(text, "subscriber_cap") {
		t.Fatalf("unexpected filtered events in text:\n%s", text)
	}
	for _, want := range []string{
		"2026-11-18T05:15:00Z  peer     peer=12D3KooW  dir=in  reason=connect",
		"2026-11-18T05:30:00Z  dial     peer=12D3KooX  dir=out  reason=timeout",
		"2026-11-18T05:50:00Z  reject   peer=12D3KooY  dir=in  reason=signature  size=184",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in text:\n%s", want, text)
		}
	}

	filter.JSONOutput = true
	var jsonBuf bytes.Buffer
	if err := formatExtenderGossipEvents(&jsonBuf, events, filter, now); err != nil {
		t.Fatal(err)
	}
	var decoded []ExtenderGossipEvent
	if err := json.Unmarshal(jsonBuf.Bytes(), &decoded); err != nil {
		t.Fatalf("unmarshal json: %v\n%s", err, jsonBuf.String())
	}
	if len(decoded) != 3 || decoded[0].Kind != "peer" || decoded[1].Kind != "dial" || decoded[2].Kind != "reject" {
		t.Fatalf("decoded json = %+v", decoded)
	}

	// Empty result formats cleanly in both text and JSON modes.
	var emptyText bytes.Buffer
	filter.JSONOutput = false
	filter.Since = time.Minute
	if err := formatExtenderGossipEvents(&emptyText, events, filter, now); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(emptyText.String(), "No gossip events.") {
		t.Fatalf("empty text = %q", emptyText.String())
	}
	var emptyJSON bytes.Buffer
	filter.JSONOutput = true
	if err := formatExtenderGossipEvents(&emptyJSON, events, filter, now); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(emptyJSON.String()) != "[]" {
		t.Fatalf("empty json = %q, want []", emptyJSON.String())
	}
}

func TestQueryExtenderEventsFromSocket(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix socket")
	}
	dir, err := os.MkdirTemp("/tmp", "ev")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sockPath := filepath.Join(dir, "provider.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	wantTime := time.Date(2026, 11, 18, 6, 30, 0, 0, time.UTC)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		var req controlRequest
		if err := json.NewDecoder(conn).Decode(&req); err != nil {
			return
		}
		if req.Cmd != "extender_events" {
			_ = json.NewEncoder(conn).Encode(controlResponse{OK: false, Error: "unexpected cmd " + req.Cmd})
			return
		}
		_ = json.NewEncoder(conn).Encode(controlResponse{
			OK: true,
			GossipEvents: []ExtenderGossipEvent{
				{Time: wantTime, Kind: "state", Reason: "running"},
			},
		})
	}()

	got, err := queryExtenderEventsFromSocket(Provider{StateDir: dir})
	if err != nil {
		t.Fatalf("queryExtenderEventsFromSocket: %v", err)
	}
	if len(got) != 1 || got[0].Kind != "state" || got[0].Reason != "running" || !got[0].Time.Equal(wantTime) {
		t.Fatalf("got events = %+v", got)
	}
}
