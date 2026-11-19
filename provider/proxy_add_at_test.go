package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/docopt/docopt-go"
)

func TestProxyAddAtInPassword(t *testing.T) {
	for _, fileBacked := range []bool{false, true} {
		t.Run(map[bool]string{false: "internal", true: "file"}[fileBacked], func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("HOME", dir)
			t.Setenv("USERPROFILE", dir)
			source := filepath.Join(dir, "source")
			inputs := []string{"192.0.2.1:1080:user:p@ss", "key@192.0.2.2:1080:user:pass", "192.0.2.3:1080:user:pass"}
			opts := docopt.Opts{"<key_address>": inputs, "-f": true}
			if fileBacked {
				proxyAddFileBacked(source, opts)
				b, err := os.ReadFile(source)
				if err != nil {
					t.Fatal(err)
				}
				for _, want := range []string{inputs[0], "192.0.2.2:1080:user:pass", inputs[2]} {
					if !strings.Contains(string(b), want+"\n") {
						t.Errorf("missing expected line")
					}
				}
			} else {
				proxyAdd(opts)
				servers := readProxyConfig().Servers
				for address, key := range map[string]string{inputs[0]: "", "192.0.2.2:1080:user:pass": "key", inputs[2]: ""} {
					if got, ok := servers[address]; !ok || got != key {
						t.Errorf("missing entry or key")
					}
				}
			}
		})
	}
}
