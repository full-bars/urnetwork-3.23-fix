package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/docopt/docopt-go"
	"github.com/urnetwork/connect"
)

func TestProxySecretCatchAllFlow(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	// ---- Add proxy via command line with canary credentials ----
	canaryUser := "canary-user-7731"
	canaryPass := "canary-pass-9042"
	canaryToken := "canary-token-5518"

	line := "example.com:1080:" + canaryUser + ":" + canaryPass
	_ = captureOutput(t, func() {
		proxyAdd(docopt.Opts{"<key_address>": []string{line}, "-f": true})
	})
	if _, ok := readProxyConfig().Servers[line]; !ok {
		t.Fatal("proxy was not added")
	}

	// ---- Import proxy from file with canary credentials ----
	source := filepath.Join(t.TempDir(), "input")
	if err := os.WriteFile(source, []byte(line+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	_ = captureOutput(t, func() {
		proxyAdd(docopt.Opts{"<key_address>": []string{}, "--file": source, "-f": true})
	})
	if _, ok := readProxyConfig().Servers[line]; !ok {
		t.Fatal("proxy was not imported")
	}

	// ---- Register the canary proxy in connect and drive control requests ----
	address := "socks5://" + canaryUser + ":" + canaryPass + "@example.com:1080?token=" + canaryToken
	const index = 991733
	connect.RegisterProxy(index, address, address)
	defer connect.UnregisterProxy(index)

	// Collect all control reply output
	var controlBuilder strings.Builder
	for _, cmd := range []string{"status", "snapshot", "proxy_cells", "proxy_cell_detail", "audit"} {
		resp := handleControlRequest(newControlState(), controlRequest{Cmd: cmd})
		data, err := json.Marshal(resp)
		if err != nil {
			t.Fatalf("marshal %s: %v", cmd, err)
		}
		controlBuilder.WriteString(string(data))
	}

	// ---- Scan control output for canary strings ----
	for _, secret := range []string{canaryUser, canaryPass, canaryToken} {
		if strings.Contains(controlBuilder.String(), secret) {
			t.Errorf("%s found in control output", secret)
		}
	}

	// ---- Walk test home for any files containing canary strings ----
	privateConfig := filepath.Join(home, ".urnetwork", "proxy")
	filepath.WalkDir(home, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		if path == privateConfig {
			// Private proxy configuration holds credentials by design and must be mode 0600.
			info, err := os.Stat(path)
			if err != nil {
				return err
			}
			if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
				t.Errorf("private proxy configuration is not mode 0600: %s", path)
			}
			if !strings.Contains(string(data), canaryPass) {
				t.Errorf("private proxy configuration does not hold the imported credentials: %s", path)
			}
			return nil
		}
		for _, secret := range []string{canaryUser, canaryPass, canaryToken} {
			if strings.Contains(string(data), secret) {
				t.Errorf("%s contains a canary secret", path)
			}
		}
		return nil
	})
}
