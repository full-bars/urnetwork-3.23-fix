package main

// network_show_test.go — `provider choose_network --show`, the insecure
// network warning, the network switch notice, the incomplete-config error and
// the atomic write of network.json. Ported from sn's miner/network_test.go.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/docopt/docopt-go"
)

func TestNetworkShowDefault(t *testing.T) {
	withTempHome(t)
	text, err := networkShowText()
	if err != nil {
		t.Fatalf("networkShowText: unexpected error: %s", err)
	}
	for _, want := range []string{
		"no custom network saved; using the main network",
		"api_url: " + DefaultApiUrl,
		"connect_url: " + DefaultConnectUrl,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("default show text missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "warning") {
		t.Errorf("default show text must not warn:\n%s", text)
	}
}

func TestNetworkShowCustomNamesTheFileAndTheUrls(t *testing.T) {
	home := withTempHome(t)
	if err := writeNetworkConfig("https://api.example.com", "wss://connect.example.com"); err != nil {
		t.Fatal(err)
	}
	text, err := networkShowText()
	if err != nil {
		t.Fatalf("networkShowText: unexpected error: %s", err)
	}
	for _, want := range []string{
		"custom network saved at " + filepath.Join(home, ".urnetwork", "network.json"),
		"api_url: https://api.example.com",
		"connect_url: wss://connect.example.com",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("custom show text missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "warning") {
		t.Errorf("an encrypted network must not warn:\n%s", text)
	}
}

func TestNetworkShowWarnsAboutPlaintext(t *testing.T) {
	withTempHome(t)
	// A saved plaintext network stays accepted (a local test backend); it is
	// said out loud instead of refused.
	if err := writeNetworkConfig("http://127.0.0.1:8080", "ws://127.0.0.1:8081"); err != nil {
		t.Fatal(err)
	}
	text, err := networkShowText()
	if err != nil {
		t.Fatalf("networkShowText: unexpected error: %s", err)
	}
	if !strings.Contains(text, "warning: api_url uses http and connect_url uses ws.") {
		t.Errorf("show text must warn about both plaintext URLs:\n%s", text)
	}
}

func TestChooseNetworkShowFlagPrintsAndDoesNotChangeTheConfig(t *testing.T) {
	withTempHome(t)
	if err := writeNetworkConfig("https://api.example.com", "wss://connect.example.com"); err != nil {
		t.Fatal(err)
	}
	out := captureOutput(t, func() {
		chooseNetworkCmd(docopt.Opts{"choose_network": true, "--show": true})
	})
	if !strings.Contains(out, "api_url: https://api.example.com") {
		t.Errorf("--show output missing the saved api_url:\n%s", out)
	}
	cfg, ok, err := readNetworkConfig()
	if err != nil || !ok || cfg.ApiUrl != "https://api.example.com" {
		t.Errorf("--show must not change the saved network, got %+v ok=%v err=%v", cfg, ok, err)
	}
}

func TestChooseNetworkShowReportsAnUnreadableConfig(t *testing.T) {
	home := withTempHome(t)
	dir := filepath.Join(home, ".urnetwork")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "network.json"), []byte("{not json"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := networkShowText(); err == nil {
		t.Fatal("networkShowText: expected an error for a corrupt network.json")
	}
}

func TestReadNetworkConfigRejectsAnIncompleteFile(t *testing.T) {
	for name, body := range map[string]string{
		"empty object":  `{}`,
		"null":          `null`,
		"api_url only":  `{"api_url":"https://api.example.com"}`,
		"connect only":  `{"connect_url":"wss://connect.example.com"}`,
		"empty strings": `{"api_url":"","connect_url":""}`,
	} {
		t.Run(name, func(t *testing.T) {
			home := withTempHome(t)
			dir := filepath.Join(home, ".urnetwork")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "network.json"), []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			if _, ok, err := readNetworkConfig(); err == nil || ok {
				t.Fatalf("readNetworkConfig(%s): want an error and ok=false, got ok=%v err=%v", body, ok, err)
			}
		})
	}
}

func TestInsecureNetworkWarning(t *testing.T) {
	cases := []struct {
		api, connect string
		want         string
	}{
		{"https://a.example", "wss://c.example", ""},
		{"http://a.example", "wss://c.example", "api_url uses http."},
		{"https://a.example", "ws://c.example", "connect_url uses ws."},
		{"http://a.example", "ws://c.example", "api_url uses http and connect_url uses ws."},
	}
	for _, c := range cases {
		got := insecureNetworkWarning(c.api, c.connect)
		if c.want == "" {
			if got != "" {
				t.Errorf("insecureNetworkWarning(%q, %q) = %q, want none", c.api, c.connect, got)
			}
			continue
		}
		if !strings.Contains(got, c.want) || !strings.HasPrefix(got, "warning: ") {
			t.Errorf("insecureNetworkWarning(%q, %q) = %q, want it to contain %q", c.api, c.connect, got, c.want)
		}
	}
}

func TestNetworkSwitchNoticeOnlyWhenAJwtExists(t *testing.T) {
	dir := t.TempDir()
	if got := networkSwitchNotice(dir); got != "" {
		t.Errorf("no jwt: want no notice, got %q", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "jwt"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	got := networkSwitchNotice(dir)
	if !strings.Contains(got, "issued by the previously selected network") || !strings.Contains(got, "provider auth") {
		t.Errorf("with a jwt: want the switch notice, got %q", got)
	}
}

func TestChooseNetworkPrintsTheNoticesAfterASave(t *testing.T) {
	home := withTempHome(t)
	dir := filepath.Join(home, ".urnetwork")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "jwt"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	out := captureOutput(t, func() {
		chooseNetworkCmd(docopt.Opts{"choose_network": true, "<api_url>": "http://127.0.0.1:1", "<connect_url>": "ws://127.0.0.1:2"})
	})
	if !strings.Contains(out, "network saved:") || !strings.Contains(out, "warning: api_url uses http") ||
		!strings.Contains(out, "previously selected network") {
		t.Errorf("save output must carry the saved line, the plaintext warning and the switch notice:\n%s", out)
	}
}

func TestWriteNetworkConfigLeavesNoTempFile(t *testing.T) {
	home := withTempHome(t)
	if err := writeNetworkConfig("https://api.example.com", "wss://connect.example.com"); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(home, ".urnetwork"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("leftover temp file %q after an atomic write", e.Name())
		}
	}
}
