package urnettools

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExtenderDiscoveryControlKey(t *testing.T) {
	for _, name := range []string{"extender-discovery", "extender_discovery"} {
		k, ok := canonicalControlKey(name)
		if !ok || k != "extender_discovery" {
			t.Fatalf("%s: %s %v", name, k, ok)
		}
	}
	for _, v := range []string{"off", "on", "feed-only", "ON"} {
		if err := validateControlValue("extender_discovery", v); err != nil {
			t.Fatal(err)
		}
	}
	if validateControlValue("extender_discovery", "yes") == nil {
		t.Fatal("invalid mode accepted")
	}
	found := false
	for _, line := range setKeyHelps {
		if strings.Contains(line, "extender-discovery") && strings.Contains(line, "feed-only") {
			found = true
		}
	}
	if !found {
		t.Fatal("missing set help")
	}
}

func TestExtenderDiscoveryQueue(t *testing.T) {
	dir := t.TempDir()
	p := Provider{StateDir: dir}
	if err := applySetOverride(p, "extender-discovery", "feed-only", false); err != nil {
		t.Fatal(err)
	}
	if err := applySetOverride(p, "extender-discovery", "off", false); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "pending_overrides.json"))
	if err != nil {
		t.Fatal(err)
	}
	var ops []pendingOp
	if err := json.Unmarshal(data, &ops); err != nil {
		t.Fatal(err)
	}
	if len(ops) != 2 || ops[0].Key != "extender_discovery" || ops[0].Value != "feed-only" || ops[1].Op != "set" || ops[1].Value != "off" {
		t.Fatalf("queue: %+v", ops)
	}
}

func TestExtenderDiscoveryStatusBlock(t *testing.T) {
	for _, state := range []string{"off", "waiting", "running", "feed-only", "failed"} {
		var b bytes.Buffer
		r := ExtenderReading{Source: SourceControlSocket, Running: true, Discovery: &ExtenderDiscovery{Requested: "on", State: state, Failure: "bad key", PeerId: "peer", Peers: 3, FeedSubscribers: 4}}
		formatExtenderBlock(&b, r, time.Time{})
		for _, want := range []string{state, "requested: on", "peer", "mesh peers: 3", "feed subscribers: 4"} {
			if !strings.Contains(b.String(), want) {
				t.Errorf("%s missing %s", state, want)
			}
		}
		if state == "waiting" && !strings.Contains(b.String(), "root keys") {
			t.Error("no waiting explanation")
		}
		if state == "failed" && (!strings.Contains(b.String(), "bad key") || !strings.Contains(b.String(), "retry")) {
			t.Error("no failure explanation")
		}
	}
}

func TestExtenderDiscoveryCounterCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name, data string
		present    bool
	}{
		{"older provider", `{"state":"running","peers":1}`, false},
		{"new provider", `{"state":"running","peers":1,"messages_accepted":0,"apply_errors":0}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var d ExtenderDiscovery
			if err := json.Unmarshal([]byte(tc.data), &d); err != nil {
				t.Fatal(err)
			}
			if (d.MessagesAccepted != nil) != tc.present {
				t.Fatalf("counter presence = %+v", d.MessagesAccepted)
			}
			var b bytes.Buffer
			formatExtenderBlock(&b, ExtenderReading{Source: SourceControlSocket, Running: true, Discovery: &d}, time.Time{})
			if strings.Contains(b.String(), "valid peer messages received") != tc.present {
				t.Fatalf("unexpected counter display: %s", b.String())
			}
		})
	}
}
