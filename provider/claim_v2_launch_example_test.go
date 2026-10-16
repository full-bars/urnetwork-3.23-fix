package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/urfoundation/sn/ss58"
)

// E-5, §10.1 config, VERSION2-LAUNCH.md step 13: the launch example of the v2
// claim daemon config, its "<...>" placeholders filled with synthetic values,
// passes the real loader with exactly those values; unfilled it is refused.
func TestClaimV2LaunchConfigExample(t *testing.T) {
	raw, err := os.ReadFile("testdata/claim-daemon-v2.example.yml")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	key := filepath.Join(dir, "relayer.key")
	if err := os.WriteFile(key, []byte(strings.Repeat("01", 32)), 0o600); err != nil {
		t.Fatal(err)
	}
	coldkey, err := ss58.Encode(claimV2TestColdkey, ss58.BittensorPrefix)
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]string{"operator-api-url": "https://operator-api.example.test", "O2-evm-rpc-url": "http://127.0.0.1:9944", "vault2-address": claimV2TestVault,
		"claim-batcher-address": "0x00000000000000000000000000000000000b4b4b", "O22-coldkey-ss58": coldkey, "O22-relayer-key-file": key, "O22-state-dir": filepath.Join(dir, "claims-v2")}
	found := map[string]bool{}
	filled := regexp.MustCompile(`"<[^"<>]+>"`).ReplaceAllFunc(raw, func(token []byte) []byte {
		key := string(token[2 : len(token)-2])
		if _, ok := values[key]; !ok {
			t.Fatalf("no test value for <%s>", key)
		}
		found[key] = true
		encoded, _ := json.Marshal(values[key])
		return encoded
	})
	if len(found) != len(values) {
		t.Fatalf("example has placeholders %v; the test fills %d", found, len(values))
	}
	load := func(raw []byte) (*ClaimDaemonV2Config, error) {
		path := filepath.Join(dir, "claim-daemon-v2.yml")
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		return LoadClaimDaemonV2Config(path)
	}
	cfg, err := load(filled)
	if err != nil || cfg.SchemaVersion != 2 || cfg.APIURL != values["operator-api-url"] || !slices.Equal(cfg.RPC, []string{values["O2-evm-rpc-url"]}) || cfg.ChainID != 964 || cfg.Vault != claimV2TestVault ||
		cfg.Batcher != values["claim-batcher-address"] || !slices.Equal(cfg.Coldkeys, []string{coldkey}) || len(cfg.coldkeys) != 1 || cfg.coldkeys[0] != claimV2TestColdkey || cfg.KeyFile != key || cfg.StateDir != values["O22-state-dir"] || cfg.PollSeconds != 30 {
		t.Fatalf("filled example: %+v %v", cfg, err)
	}
	if cfg, err := load(raw); cfg != nil || err == nil || err.Error() != "invalid v2 claim daemon configuration" {
		t.Fatalf("unfilled example: %+v %v", cfg, err)
	}
}
